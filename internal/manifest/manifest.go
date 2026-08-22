// Package manifest records which SSTables make up the database.
//
// An LSM tree's files are immutable, so "the state of the database" is exactly
// the set of live files and which level each sits in. That set changes on every
// flush and every compaction, and those changes must be atomic: a reader must
// never observe a moment where a compaction's output exists but its inputs are
// already gone, or vice versa.
//
// The manifest solves this the way filesystems solve it -- with a log. Each
// change is a VersionEdit appended to the MANIFEST file: files added, files
// removed, and bookkeeping like the next file number. The current state is the
// edits replayed from the beginning. A crash mid-append truncates the log at a
// record boundary (the WAL framing guarantees that), so recovery either sees an
// edit entirely or not at all.
//
// One indirection remains: which MANIFEST file is current. That is what the
// CURRENT file holds, and it is updated by writing a temporary file and
// rename(2)-ing it into place. rename is atomic on POSIX filesystems, so there
// is no instant at which CURRENT names a file that does not exist.
package manifest

import (
	"encoding/binary"
	"fmt"
)

// Tags identify fields inside an encoded VersionEdit. Encoding fields by tag
// rather than by position means an older binary can skip fields it does not
// understand instead of misparsing the rest of the record -- the same reason
// protocol buffers are tagged.
type tag uint32

const (
	tagComparator     tag = 1
	tagLogNumber      tag = 2
	tagNextFileNumber tag = 3
	tagLastSequence   tag = 4
	tagDeletedFile    tag = 5
	tagNewFile        tag = 6
)

// FileMeta describes one SSTable.
//
// Smallest and Largest are internal keys, and they are the reason a lookup can
// skip most files without opening them: if the target key falls outside a
// file's range, that file provably cannot contain it.
type FileMeta struct {
	Number   uint64
	Size     uint64
	Smallest []byte
	Largest  []byte
}

// DeletedFile identifies a file being removed from a level.
type DeletedFile struct {
	Level  int
	Number uint64
}

// NewFile identifies a file being added to a level.
type NewFile struct {
	Level int
	Meta  FileMeta
}

// VersionEdit is one atomic change to the set of live files.
type VersionEdit struct {
	Comparator    string
	HasComparator bool

	// LogNumber is the oldest WAL file whose contents are not yet durable in
	// an SSTable. Recovery replays this log and everything after it, and
	// anything older can be deleted. Getting this field wrong is how engines
	// silently lose the last few seconds of writes.
	LogNumber    uint64
	HasLogNumber bool

	NextFileNumber    uint64
	HasNextFileNumber bool

	LastSequence    uint64
	HasLastSequence bool

	Deleted []DeletedFile
	New     []NewFile
}

func appendString(dst []byte, s []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// Encode serialises the edit.
func (e *VersionEdit) Encode() []byte {
	var b []byte
	if e.HasComparator {
		b = binary.AppendUvarint(b, uint64(tagComparator))
		b = appendString(b, []byte(e.Comparator))
	}
	if e.HasLogNumber {
		b = binary.AppendUvarint(b, uint64(tagLogNumber))
		b = binary.AppendUvarint(b, e.LogNumber)
	}
	if e.HasNextFileNumber {
		b = binary.AppendUvarint(b, uint64(tagNextFileNumber))
		b = binary.AppendUvarint(b, e.NextFileNumber)
	}
	if e.HasLastSequence {
		b = binary.AppendUvarint(b, uint64(tagLastSequence))
		b = binary.AppendUvarint(b, e.LastSequence)
	}
	for _, d := range e.Deleted {
		b = binary.AppendUvarint(b, uint64(tagDeletedFile))
		b = binary.AppendUvarint(b, uint64(d.Level))
		b = binary.AppendUvarint(b, d.Number)
	}
	for _, n := range e.New {
		b = binary.AppendUvarint(b, uint64(tagNewFile))
		b = binary.AppendUvarint(b, uint64(n.Level))
		b = binary.AppendUvarint(b, n.Meta.Number)
		b = binary.AppendUvarint(b, n.Meta.Size)
		b = appendString(b, n.Meta.Smallest)
		b = appendString(b, n.Meta.Largest)
	}
	return b
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint(what string) uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = fmt.Errorf("strata/manifest: truncated %s", what)
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) bytes(what string) []byte {
	n := d.uvarint(what + " length")
	if d.err != nil {
		return nil
	}
	if uint64(len(d.b)) < n {
		d.err = fmt.Errorf("strata/manifest: %s claims %d bytes, %d remain", what, n, len(d.b))
		return nil
	}
	out := append([]byte(nil), d.b[:n]...)
	d.b = d.b[n:]
	return out
}

// Decode parses an edit produced by Encode.
func Decode(b []byte) (*VersionEdit, error) {
	e := &VersionEdit{}
	d := &decoder{b: b}
	for len(d.b) > 0 && d.err == nil {
		switch tag(d.uvarint("tag")) {
		case tagComparator:
			e.Comparator, e.HasComparator = string(d.bytes("comparator")), true
		case tagLogNumber:
			e.LogNumber, e.HasLogNumber = d.uvarint("log number"), true
		case tagNextFileNumber:
			e.NextFileNumber, e.HasNextFileNumber = d.uvarint("next file number"), true
		case tagLastSequence:
			e.LastSequence, e.HasLastSequence = d.uvarint("last sequence"), true
		case tagDeletedFile:
			df := DeletedFile{Level: int(d.uvarint("deleted file level"))}
			df.Number = d.uvarint("deleted file number")
			e.Deleted = append(e.Deleted, df)
		case tagNewFile:
			nf := NewFile{Level: int(d.uvarint("new file level"))}
			nf.Meta.Number = d.uvarint("new file number")
			nf.Meta.Size = d.uvarint("new file size")
			nf.Meta.Smallest = d.bytes("new file smallest key")
			nf.Meta.Largest = d.bytes("new file largest key")
			e.New = append(e.New, nf)
		default:
			// An unknown tag cannot be skipped, because tags do not carry
			// their own length. Rather than guess, refuse the record: a
			// manifest written by a newer binary is not something an older
			// one should half-understand.
			if d.err == nil {
				d.err = fmt.Errorf("strata/manifest: unknown tag in version edit")
			}
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	return e, nil
}
