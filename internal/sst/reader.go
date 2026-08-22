package sst

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/ramkirangaruda/strata/internal/crc"
	"github.com/ramkirangaruda/strata/internal/keys"
)

// Reader gives random access to a finished table.
//
// The index and filter blocks are read once at open and held in memory; data
// blocks are read on demand. That is the whole caching policy for now. A real
// engine puts a bounded LRU in front of data blocks (phase A2's block cache);
// until then, every data-block hit is a syscall, which is honest but slow, and
// makes the effect of the bloom filter very visible in benchmarks.
type Reader struct {
	f      io.ReaderAt
	size   int64
	index  *block
	filter []byte
}

// Open parses a table's footer, index and filter.
func Open(f io.ReaderAt, size int64) (*Reader, error) {
	if size < FooterSize {
		return nil, fmt.Errorf("%w: file is %d bytes, smaller than a footer", ErrBadFormat, size)
	}
	buf := make([]byte, FooterSize)
	if _, err := f.ReadAt(buf, size-FooterSize); err != nil {
		return nil, err
	}
	ft, err := decodeFooter(buf)
	if err != nil {
		return nil, err
	}

	r := &Reader{f: f, size: size}
	if r.filter, err = r.readRawBlock(ft.filter); err != nil {
		return nil, fmt.Errorf("reading filter block: %w", err)
	}
	indexBytes, err := r.readRawBlock(ft.index)
	if err != nil {
		return nil, fmt.Errorf("reading index block: %w", err)
	}
	if r.index, err = newBlock(indexBytes); err != nil {
		return nil, err
	}
	return r, nil
}

// readRawBlock reads a block and verifies its checksum.
//
// The checksum is verified on EVERY read, not sampled. This is the layer that
// turns silent disk corruption -- a flipped bit in a sector, a misdirected
// write landing in the wrong place -- into a loud error instead of a wrong
// answer returned to an application. Skipping it to save CPU is a trade
// nobody who has debugged silent corruption makes twice.
func (r *Reader) readRawBlock(h BlockHandle) ([]byte, error) {
	if h.Offset+h.Size+blockTrailerSize > uint64(r.size) {
		return nil, fmt.Errorf("%w: block handle points past the end of the file", ErrBadFormat)
	}
	buf := make([]byte, h.Size+blockTrailerSize)
	if _, err := r.f.ReadAt(buf, int64(h.Offset)); err != nil {
		return nil, err
	}
	contents := buf[:h.Size]
	trailer := buf[h.Size:]
	if trailer[0] != compressionNone {
		return nil, fmt.Errorf("%w: unknown compression codec %d", ErrBadFormat, trailer[0])
	}
	stored := binary.LittleEndian.Uint32(trailer[1:])
	if got := crc.Masked(contents, trailer[:1]); got != stored {
		return nil, fmt.Errorf("%w: block checksum mismatch at offset %d (stored %08x, computed %08x)",
			ErrBadFormat, h.Offset, stored, got)
	}
	return contents, nil
}

// Get looks up the newest version of ikey's user key that is visible at
// ikey's sequence number.
//
// It returns found=false both when the key is absent and when the newest
// visible version is a tombstone -- the caller distinguishes those through
// kind, because "deleted here" must stop the search rather than let an older
// table answer with a resurrected value.
func (r *Reader) Get(ikey []byte) (value []byte, kind keys.Kind, found bool, err error) {
	ukey := keys.UserKey(ikey)
	if !bloomMayContain(r.filter, ukey) {
		return nil, 0, false, nil
	}

	idx := r.index.iter()
	idx.SeekGE(ikey)
	if !idx.Valid() {
		return nil, 0, false, idx.Error()
	}
	h, _, err := decodeHandle(idx.Value())
	if err != nil {
		return nil, 0, false, err
	}
	contents, err := r.readRawBlock(h)
	if err != nil {
		return nil, 0, false, err
	}
	b, err := newBlock(contents)
	if err != nil {
		return nil, 0, false, err
	}

	it := b.iter()
	it.SeekGE(ikey)
	if !it.Valid() {
		return nil, 0, false, it.Error()
	}
	// Sort order does the work: the first entry at or after the seek key with
	// a matching user key is the newest version visible at that sequence.
	if !keys.Valid(it.Key()) || string(keys.UserKey(it.Key())) != string(ukey) {
		return nil, 0, false, nil
	}
	k := keys.KindOf(it.Key())
	if k == keys.KindDelete {
		return nil, k, true, nil
	}
	return append([]byte(nil), it.Value()...), k, true, nil
}

// Iterator walks every entry in the table in internal-key order. It is a
// two-level iterator: one cursor over the index, one over the current data
// block.
type Iterator struct {
	r    *Reader
	idx  *blockIter
	data *blockIter
	err  error
}

// NewIter returns an iterator positioned before the first entry.
func (r *Reader) NewIter() *Iterator { return &Iterator{r: r, idx: r.index.iter()} }

func (i *Iterator) Valid() bool   { return i.data != nil && i.data.Valid() }
func (i *Iterator) Key() []byte   { return i.data.Key() }
func (i *Iterator) Value() []byte { return i.data.Value() }

// Error reports the first error encountered. A nil error with Valid() == false
// means a clean end of iteration; a non-nil error means the table is damaged
// and the iteration is incomplete.
func (i *Iterator) Error() error {
	if i.err != nil {
		return i.err
	}
	if i.data != nil {
		return i.data.Error()
	}
	return i.idx.Error()
}

func (i *Iterator) loadCurrentBlock() bool {
	if !i.idx.Valid() {
		i.data = nil
		return false
	}
	h, _, err := decodeHandle(i.idx.Value())
	if err != nil {
		i.err, i.data = err, nil
		return false
	}
	contents, err := i.r.readRawBlock(h)
	if err != nil {
		i.err, i.data = err, nil
		return false
	}
	b, err := newBlock(contents)
	if err != nil {
		i.err, i.data = err, nil
		return false
	}
	i.data = b.iter()
	return true
}

// First positions the iterator at the smallest entry in the table.
func (i *Iterator) First() {
	i.idx.First()
	if !i.loadCurrentBlock() {
		return
	}
	i.data.First()
	i.skipEmptyForward()
}

// SeekGE positions the iterator at the first entry >= ikey.
func (i *Iterator) SeekGE(ikey []byte) {
	i.idx.SeekGE(ikey)
	if !i.loadCurrentBlock() {
		return
	}
	i.data.SeekGE(ikey)
	i.skipEmptyForward()
}

// Next advances one entry, crossing into the next data block when needed.
func (i *Iterator) Next() {
	if i.data == nil {
		return
	}
	i.data.Next()
	i.skipEmptyForward()
}

// skipEmptyForward walks past data blocks that the cursor has run off the end
// of. It is a loop rather than a single step because a table may legitimately
// contain an empty block after a future compaction change.
func (i *Iterator) skipEmptyForward() {
	for i.data != nil && !i.data.Valid() {
		if i.data.Error() != nil {
			i.err = i.data.Error()
			return
		}
		i.idx.Next()
		if !i.loadCurrentBlock() {
			return
		}
		i.data.First()
	}
}
