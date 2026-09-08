// Package strata is a log-structured merge-tree storage engine.
//
// The shape of the thing, in one paragraph: writes go into a write-ahead log
// for durability and a sorted in-memory table for reading. When that memtable
// grows past a threshold it is frozen and written out as an immutable sorted
// file, and a fresh memtable takes over. Reads consult the memtable first,
// then the files, newest to oldest, stopping at the first answer -- which is
// correct because every record carries a sequence number and the sort order
// puts newer versions first. Nothing is ever updated in place; an overwrite is
// a new record and a delete is a tombstone.
//
// That design trades read cost for write cost. Writes are a sequential log
// append and an in-memory insert, which is about as cheap as durable writes
// get. Reads may have to check several files, which is why bloom filters and
// compaction exist. Everything else in this engine is a consequence of that
// one trade.
//
// # Phase status
//
// This is phase A1 of the build plan: the write path, durability, and
// recovery. Deliberately absent, in the order they arrive:
//
//	A2  a block cache and a merging iterator across levels
//	A3  compaction -- so today every flush lands in level 0 and stays there,
//	    which means read amplification grows without bound
//	A4  transactions and snapshot isolation as a public API
//	A5  deterministic simulation testing
//
// The flush in this phase is synchronous and holds the write lock, so a flush
// stalls every writer. That is a real write stall, just a crude one; making it
// a background job with proper backpressure is part of A3.
package strata

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ramkirangaruda/strata/internal/keys"
	"github.com/ramkirangaruda/strata/internal/manifest"
	"github.com/ramkirangaruda/strata/internal/sst"
	"github.com/ramkirangaruda/strata/internal/wal"
)

// ErrNotFound means the key has no visible value -- either it was never
// written, or the newest version visible to this read is a tombstone.
var ErrNotFound = errors.New("strata: key not found")

// ErrClosed means the database has been closed.
var ErrClosed = errors.New("strata: database is closed")

// Options configures a database.
type Options struct {
	// MemtableSize is the approximate memory a memtable may occupy before it
	// is frozen and flushed. Bigger memtables mean fewer, larger files and
	// less write amplification, at the cost of longer recovery (more log to
	// replay) and a larger stall when the flush happens.
	MemtableSize int64

	// BlockSize is the target size of a data block inside a table.
	BlockSize int

	// Sync makes every write fsync the log before returning.
	//
	// This is THE durability knob, and it is worth being precise about what
	// each setting promises. With Sync, a write that returned survives a power
	// cut. Without it, a write that returned survives the PROCESS dying (the
	// bytes are in the kernel's page cache, which outlives the process) but
	// not the machine dying. Most engines default to false and most users do
	// not realise which guarantee they are getting.
	Sync bool

	// SkiplistSeed makes memtable structure deterministic, for tests.
	SkiplistSeed int64
}

func (o Options) withDefaults() Options {
	if o.MemtableSize <= 0 {
		o.MemtableSize = 4 << 20
	}
	if o.BlockSize <= 0 {
		o.BlockSize = sst.DefaultBlockSize
	}
	return o
}

type tableHandle struct {
	file   *os.File
	reader *sst.Reader
}

// DB is a strata database. It is safe for concurrent use.
type DB struct {
	dir  string
	opts Options

	mu  sync.Mutex
	mem *memtable
	imm *memtable // frozen, being flushed
	ver *version  // immutable; replaced, never mutated
	seq uint64

	logNumber uint64
	logFile   *os.File
	log       *wal.Writer

	manifestNumber uint64
	manifestFile   *os.File
	manifestLog    *wal.Writer

	nextFileNumber uint64
	closed         bool

	cacheMu sync.Mutex
	cache   map[uint64]*tableHandle
}

// Open opens or creates a database in dir.
func Open(dir string, opts Options) (*DB, error) {
	opts = opts.withDefaults()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db := &DB{
		dir:   dir,
		opts:  opts,
		ver:   &version{},
		cache: make(map[uint64]*tableHandle),
	}

	if _, err := os.Stat(currentPath(dir)); errors.Is(err, os.ErrNotExist) {
		if err := db.create(); err != nil {
			return nil, fmt.Errorf("strata: creating database: %w", err)
		}
	} else if err != nil {
		return nil, err
	}

	if err := db.recover(); err != nil {
		db.Close()
		return nil, fmt.Errorf("strata: recovering database: %w", err)
	}
	return db, nil
}

// create lays down a minimal valid database: manifest 1, no files, no log.
func (db *DB) create() error {
	e := &manifest.VersionEdit{
		Comparator: keys.ComparatorName, HasComparator: true,
		NextFileNumber: 2, HasNextFileNumber: true,
		LogNumber: 0, HasLogNumber: true,
		LastSequence: 0, HasLastSequence: true,
	}
	if err := writeManifest(db.dir, 1, e); err != nil {
		return err
	}
	return setCurrent(db.dir, 1)
}

func writeManifest(dir string, num uint64, edits ...*manifest.VersionEdit) error {
	f, err := os.OpenFile(manifestPath(dir, num), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := wal.NewWriter(f)
	for _, e := range edits {
		if err := w.Append(e.Encode()); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(dir)
}

// recover rebuilds in-memory state from disk and installs a fresh manifest.
//
// Recovery runs on EVERY open, including a clean one, because Close does not
// flush the memtable. That is deliberate: recovery code that only runs after a
// crash is recovery code that is never tested, and an engine whose recovery
// path is exercised once a year in production is an engine that loses data.
// Here, every reopen replays the log.
func (db *DB) recover() error {
	manifestNum, err := readCurrent(db.dir)
	if err != nil {
		return err
	}

	mf, err := os.Open(manifestPath(db.dir, manifestNum))
	if err != nil {
		return err
	}
	r := wal.NewReader(mf)
	for {
		rec, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			mf.Close()
			// A damaged manifest is NOT recoverable by ignoring it: the file
			// set would be wrong and reads would silently miss data. Refuse.
			return fmt.Errorf("manifest %d: %w", manifestNum, err)
		}
		e, err := manifest.Decode(rec)
		if err != nil {
			mf.Close()
			return err
		}
		if e.HasComparator && e.Comparator != keys.ComparatorName {
			mf.Close()
			return fmt.Errorf("strata: database was created with comparator %q, this binary uses %q",
				e.Comparator, keys.ComparatorName)
		}
		if e.HasLogNumber {
			db.logNumber = e.LogNumber
		}
		if e.HasNextFileNumber {
			db.nextFileNumber = e.NextFileNumber
		}
		if e.HasLastSequence {
			db.seq = e.LastSequence
		}
		db.ver = db.ver.apply(e)
	}
	mf.Close()
	if db.nextFileNumber < 2 {
		db.nextFileNumber = 2
	}

	// Replay every log at or after logNumber, oldest first.
	logs, err := db.logsToReplay()
	if err != nil {
		return err
	}
	db.mem = newMemtable(db.opts.SkiplistSeed)
	for _, num := range logs {
		if err := db.replayLog(num); err != nil {
			return err
		}
	}

	// Anything replayed lives only in memory, so flush it before the log that
	// contained it is dropped.
	var edits []*manifest.VersionEdit
	if !db.mem.empty() {
		meta, err := db.writeTable(db.mem)
		if err != nil {
			return err
		}
		edits = append(edits, &manifest.VersionEdit{
			New: []manifest.NewFile{{Level: 0, Meta: *meta}},
		})
		db.mem = newMemtable(db.opts.SkiplistSeed)
	}

	newLog := db.allocFileNumber()
	newManifest := db.allocFileNumber()

	for _, e := range edits {
		db.ver = db.ver.apply(e)
	}

	snapshot := db.ver.snapshotEdit()
	snapshot.Comparator, snapshot.HasComparator = keys.ComparatorName, true
	snapshot.LogNumber, snapshot.HasLogNumber = newLog, true
	snapshot.NextFileNumber, snapshot.HasNextFileNumber = db.nextFileNumber, true
	snapshot.LastSequence, snapshot.HasLastSequence = db.seq, true

	if err := writeManifest(db.dir, newManifest, snapshot); err != nil {
		return err
	}
	// Only after the new manifest is durable does CURRENT start pointing at
	// it. If the process dies between those two steps, the old manifest is
	// still the live one and the new one is garbage that cleanup removes.
	if err := setCurrent(db.dir, newManifest); err != nil {
		return err
	}
	db.manifestNumber = newManifest
	db.logNumber = newLog

	if err := db.openLog(newLog); err != nil {
		return err
	}
	if err := db.openManifestForAppend(newManifest); err != nil {
		return err
	}
	return db.removeObsoleteFiles()
}

func (db *DB) logsToReplay() ([]uint64, error) {
	ents, err := os.ReadDir(db.dir)
	if err != nil {
		return nil, err
	}
	var nums []uint64
	for _, e := range ents {
		n, kind := parseFileName(e.Name())
		if kind == fileLog && n >= db.logNumber {
			nums = append(nums, n)
		}
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	return nums, nil
}

func (db *DB) replayLog(num uint64) error {
	f, err := os.Open(logPath(db.dir, num))
	if err != nil {
		return err
	}
	defer f.Close()

	r := wal.NewReader(f)
	for {
		rec, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if errors.Is(err, wal.ErrCorrupt) || errors.Is(err, wal.ErrTruncated) {
			// The tail of the log was being written when the process died.
			// Those bytes were never a complete record, so the batch they
			// belonged to was never acknowledged to any caller, so stopping
			// here loses nothing that was ever promised. Everything before
			// this point has already been applied.
			return nil
		}
		if err != nil {
			return err
		}

		seq, entries, err := decodeBatch(rec)
		if err != nil {
			return err
		}
		for i, e := range entries {
			db.mem.add(seq+uint64(i), e.kind, e.key, e.value)
		}
		if last := seq + uint64(len(entries)) - 1; last > db.seq {
			db.seq = last
		}
	}
}

func (db *DB) allocFileNumber() uint64 {
	n := db.nextFileNumber
	db.nextFileNumber++
	return n
}

func (db *DB) openLog(num uint64) error {
	f, err := os.OpenFile(logPath(db.dir, num), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := syncDir(db.dir); err != nil {
		f.Close()
		return err
	}
	db.logFile, db.log, db.logNumber = f, wal.NewWriter(f), num
	return nil
}

func (db *DB) openManifestForAppend(num uint64) error {
	f, err := os.OpenFile(manifestPath(db.dir, num), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	db.manifestFile = f
	// The log framing is block-aligned, so a Writer resuming an existing file
	// has to know how far into the current block it is starting.
	db.manifestLog = wal.NewWriterAt(f, st.Size())
	return nil
}

// writeTable serialises a memtable into a new immutable table file.
//
// The file is written under its final name rather than a temporary one. That
// is safe because nothing references it until the manifest edit that names it
// becomes durable: a crash before that leaves an unreferenced file, which
// removeObsoleteFiles deletes on the next open. The alternative ordering --
// publishing the name before the bytes are durable -- is what produces a
// database that opens and then fails on a read.
func (db *DB) writeTable(m *memtable) (*manifest.FileMeta, error) {
	num := db.allocFileNumber()
	f, err := os.OpenFile(tablePath(db.dir, num), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	w := sst.NewWriter(f, db.opts.BlockSize)
	it := m.iter()
	for it.First(); it.Valid(); it.Next() {
		if err := w.Add(it.Key(), it.Value()); err != nil {
			f.Close()
			return nil, err
		}
	}
	meta, err := w.Finish()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := syncDir(db.dir); err != nil {
		return nil, err
	}

	return &manifest.FileMeta{
		Number:   num,
		Size:     meta.Size,
		Smallest: meta.Smallest,
		Largest:  meta.Largest,
	}, nil
}

// logAndApply makes an edit durable and then visible, in that order.
func (db *DB) logAndApply(e *manifest.VersionEdit) error {
	e.NextFileNumber, e.HasNextFileNumber = db.nextFileNumber, true
	e.LastSequence, e.HasLastSequence = db.seq, true
	if err := db.manifestLog.Append(e.Encode()); err != nil {
		return err
	}
	if err := db.manifestFile.Sync(); err != nil {
		return err
	}
	db.ver = db.ver.apply(e)
	return nil
}

// Write applies a batch atomically.
func (db *DB) Write(b *Batch) error {
	if b.Len() == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if err := db.makeRoomForWrite(); err != nil {
		return err
	}

	seq := db.seq + 1
	// The log first, always. If this returns an error the batch has not been
	// applied to the memtable, so a failed write leaves no trace -- which is
	// what makes retrying one safe.
	if err := db.log.Append(b.encode(seq)); err != nil {
		return err
	}
	if db.opts.Sync {
		if err := db.logFile.Sync(); err != nil {
			return err
		}
	}
	for i, e := range b.entries {
		db.mem.add(seq+uint64(i), e.kind, e.key, e.value)
	}
	db.seq = seq + uint64(b.Len()) - 1
	return nil
}

// Put writes a single key.
func (db *DB) Put(key, value []byte) error {
	var b Batch
	b.Put(key, value)
	return db.Write(&b)
}

// Delete writes a tombstone for a single key. Deleting a key that does not
// exist is not an error and still costs a record: the engine has no cheap way
// to know the key is absent, and finding out would cost a full lookup.
func (db *DB) Delete(key []byte) error {
	var b Batch
	b.Delete(key)
	return db.Write(&b)
}

func (db *DB) makeRoomForWrite() error {
	if db.mem.memUsage() < db.opts.MemtableSize {
		return nil
	}

	oldLog := db.logNumber
	oldLogFile := db.logFile

	db.imm, db.mem = db.mem, newMemtable(db.opts.SkiplistSeed)
	if err := db.openLog(db.allocFileNumber()); err != nil {
		return err
	}

	meta, err := db.writeTable(db.imm)
	if err != nil {
		return err
	}
	if err := db.logAndApply(&manifest.VersionEdit{
		LogNumber: db.logNumber, HasLogNumber: true,
		New: []manifest.NewFile{{Level: 0, Meta: *meta}},
	}); err != nil {
		return err
	}
	db.imm = nil

	// Only now is the old log redundant: its contents are durable in a table
	// that a durable manifest edit references.
	oldLogFile.Close()
	os.Remove(logPath(db.dir, oldLog))
	return nil
}

// Get returns the value for key.
func (db *DB) Get(key []byte) ([]byte, error) {
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return nil, ErrClosed
	}
	// Capture a consistent view, then do all the I/O outside the lock. The
	// memtable is safe to read while a writer inserts, and the version is
	// immutable, so nothing here can change underneath us.
	mem, imm, ver, snapshot := db.mem, db.imm, db.ver, db.seq
	db.mu.Unlock()

	for _, m := range []*memtable{mem, imm} {
		if m == nil {
			continue
		}
		if v, kind, ok := m.get(key, snapshot); ok {
			if kind == keys.KindDelete {
				return nil, ErrNotFound
			}
			return append([]byte(nil), v...), nil
		}
	}

	ikey := keys.Encode(nil, key, snapshot, keys.KindSeek)

	// Level 0 files overlap, so every file whose range covers the key has to
	// be consulted, newest first.
	for _, f := range ver.files[0] {
		if !mayContain(f, key) {
			continue
		}
		v, kind, found, err := db.searchTable(f.Number, ikey)
		if err != nil {
			return nil, err
		}
		if found {
			if kind == keys.KindDelete {
				return nil, ErrNotFound
			}
			return v, nil
		}
	}

	// Deeper levels do not overlap within a level, so at most one file per
	// level can contain the key. (Nothing reaches these levels until
	// compaction exists, but the search is written now so that A3 only has to
	// produce the files, not also teach reads about them.)
	for lvl := 1; lvl < NumLevels; lvl++ {
		files := ver.files[lvl]
		i := sort.Search(len(files), func(i int) bool {
			return keys.CompareUser(keys.UserKey(files[i].Largest), key) >= 0
		})
		if i >= len(files) || !mayContain(files[i], key) {
			continue
		}
		v, kind, found, err := db.searchTable(files[i].Number, ikey)
		if err != nil {
			return nil, err
		}
		if found {
			if kind == keys.KindDelete {
				return nil, ErrNotFound
			}
			return v, nil
		}
	}

	return nil, ErrNotFound
}

func (db *DB) searchTable(num uint64, ikey []byte) ([]byte, keys.Kind, bool, error) {
	h, err := db.table(num)
	if err != nil {
		return nil, 0, false, err
	}
	return h.reader.Get(ikey)
}

// table returns a cached reader for a table file, opening it on first use.
//
// Handles are never evicted in this phase, so a database with many files will
// eventually run out of file descriptors. A bounded LRU over both handles and
// decoded blocks is phase A2.
func (db *DB) table(num uint64) (*tableHandle, error) {
	db.cacheMu.Lock()
	defer db.cacheMu.Unlock()
	if h, ok := db.cache[num]; ok {
		return h, nil
	}
	f, err := os.Open(tablePath(db.dir, num))
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := sst.Open(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	h := &tableHandle{file: f, reader: r}
	db.cache[num] = h
	return h, nil
}

// removeObsoleteFiles deletes files no live version references.
func (db *DB) removeObsoleteFiles() error {
	live := db.ver.liveFiles()
	ents, err := os.ReadDir(db.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		n, kind := parseFileName(e.Name())
		remove := false
		switch kind {
		case fileLog:
			remove = n < db.logNumber
		case fileTable:
			remove = !live[n]
		case fileManifest:
			remove = n != db.manifestNumber
		case fileTemp:
			remove = true
		}
		if remove {
			os.Remove(filepath.Join(db.dir, e.Name()))
		}
	}
	return nil
}

// Sync flushes the write-ahead log to stable storage. Callers running with
// Sync: false use this to checkpoint durability explicitly rather than paying
// for an fsync on every write.
func (db *DB) Sync() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	return db.logFile.Sync()
}

// Close releases the database's files.
//
// It does NOT flush the memtable. Everything in it is already in the log, and
// leaving it there means the next Open exercises the recovery path. See the
// comment on recover.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true

	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if db.logFile != nil {
		record(db.logFile.Sync())
		record(db.logFile.Close())
	}
	if db.manifestFile != nil {
		record(db.manifestFile.Close())
	}
	db.cacheMu.Lock()
	for _, h := range db.cache {
		record(h.file.Close())
	}
	db.cache = nil
	db.cacheMu.Unlock()
	return firstErr
}
