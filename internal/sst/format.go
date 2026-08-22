// Package sst implements strata's immutable on-disk table format.
//
// An SSTable ("sorted string table") is the unit that memtables flush into and
// that compaction rewrites. Once written it is never modified -- immutability
// is what lets readers use a file with no locking at all, and what makes
// compaction a matter of writing new files and atomically swapping which ones
// are live.
//
// # File layout
//
//	+------------------+
//	| data block 0     |   sorted internal keys, prefix-compressed
//	| data block 1     |
//	| ...              |
//	+------------------+
//	| filter block     |   bloom filter over the file's USER keys
//	+------------------+
//	| index block      |   one entry per data block: last key -> block handle
//	+------------------+
//	| footer (48 B)    |   handles for the two blocks above, then a magic number
//	+------------------+
//
// Reading is therefore: footer (fixed offset from the end) -> index -> the one
// data block that can contain the key. Two seeks, or one if the index is
// cached, which it always is in practice.
//
// The footer sits at the END of the file rather than the start for a reason
// worth internalising: it lets the writer stream data blocks out without ever
// seeking backwards, so an SSTable can be produced in a single forward pass
// with bounded memory even when the input does not fit in RAM.
package sst

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// magic identifies a strata SSTable and its format version. A file whose last
// eight bytes are not this is not ours, and the reader refuses it rather than
// interpreting arbitrary bytes as block handles.
var magic = [8]byte{'S', 't', 'r', 'a', 't', 'a', 0x00, 0x01}

const (
	// FooterSize is fixed so a reader can find the footer knowing only the
	// file length: two varint handles (at most 20 bytes each) plus the magic.
	FooterSize = 48

	// footerHandleSpace is the zero-padded region the handles live in.
	footerHandleSpace = FooterSize - 8

	// blockTrailerSize is one compression byte plus a masked CRC32C.
	blockTrailerSize = 5

	// compressionNone is the only codec strata implements today. The byte is
	// written anyway so that adding Snappy or Zstd later is a format-compatible
	// change rather than a new file version.
	compressionNone byte = 0
)

// ErrBadFormat means the bytes read are not a valid strata table.
var ErrBadFormat = errors.New("strata/sst: bad table format")

// BlockHandle points at a byte range in the file. It does not include the
// block trailer, which the reader adds back when it reads.
type BlockHandle struct {
	Offset uint64
	Size   uint64
}

// AppendTo encodes h as two varints.
func (h BlockHandle) AppendTo(dst []byte) []byte {
	dst = binary.AppendUvarint(dst, h.Offset)
	return binary.AppendUvarint(dst, h.Size)
}

func decodeHandle(b []byte) (BlockHandle, int, error) {
	off, n1 := binary.Uvarint(b)
	if n1 <= 0 {
		return BlockHandle{}, 0, ErrBadFormat
	}
	size, n2 := binary.Uvarint(b[n1:])
	if n2 <= 0 {
		return BlockHandle{}, 0, ErrBadFormat
	}
	return BlockHandle{Offset: off, Size: size}, n1 + n2, nil
}

type footer struct {
	filter BlockHandle
	index  BlockHandle
}

func (f footer) encode() []byte {
	buf := make([]byte, FooterSize)
	enc := f.filter.AppendTo(nil)
	enc = f.index.AppendTo(enc)
	if len(enc) > footerHandleSpace {
		panic("strata/sst: footer handles overflow reserved space")
	}
	copy(buf, enc)
	copy(buf[footerHandleSpace:], magic[:])
	return buf
}

func decodeFooter(b []byte) (footer, error) {
	if len(b) != FooterSize {
		return footer{}, fmt.Errorf("%w: footer is %d bytes, want %d", ErrBadFormat, len(b), FooterSize)
	}
	if string(b[footerHandleSpace:]) != string(magic[:]) {
		return footer{}, fmt.Errorf("%w: bad magic (not a strata table, or a different format version)", ErrBadFormat)
	}
	var f footer
	var err error
	var n int
	if f.filter, n, err = decodeHandle(b); err != nil {
		return footer{}, err
	}
	if f.index, _, err = decodeHandle(b[n:]); err != nil {
		return footer{}, err
	}
	return f, nil
}
