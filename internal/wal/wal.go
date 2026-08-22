// Package wal implements strata's write-ahead log.
//
// Everything the engine promises about durability is enforced here. A write is
// acknowledged only after its bytes are in this log, so recovery is the log
// replayed forward -- which means the log's framing has to survive the machine
// dying in the middle of a write.
//
// # Physical format
//
// The log is a sequence of fixed 32 KiB blocks. Logical records are chopped to
// fit inside blocks, and each fragment carries its own header:
//
//	+---------+--------+------+---------- ... ----------+
//	| crc (4) | len(2) | type | payload (len bytes)     |
//	+---------+--------+------+---------- ... ----------+
//
//	type: FULL   -- the whole record is in this fragment
//	      FIRST  -- the record starts here and continues
//	      MIDDLE -- neither the start nor the end
//	      LAST   -- the record ends here
//
// The block structure exists for exactly one reason: it bounds the blast
// radius of corruption. Without it, a single bad byte early in a multi-gigabyte
// log makes everything after it unparseable, because the reader has no way to
// resynchronise. With it, a reader that hits a bad block can skip to the next
// 32 KiB boundary and pick up from there.
//
// If fewer than 7 bytes remain in a block there is no room for a header, so the
// remainder is zero-filled and the next record starts in the next block. A
// zero-filled tail is therefore normal, not damage.
//
// # Checksum masking
//
// The stored checksum is not a raw CRC. It is rotated and offset first:
//
//	masked = rotate_right(crc, 15) + 0xa282ead8
//
// The reason is subtle and worth understanding. CRC32 has the property that
// computing a CRC over data that already contains its own CRC produces a
// predictable, weak result -- the checksum of a block plus its checksum is
// (nearly) constant. Real systems checksum buffers that already contain
// checksums all the time. Masking destroys that algebraic relationship, so a
// CRC computed over already-checksummed data is still a strong check.
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ramkirangaruda/strata/internal/crc"
)

const (
	// BlockSize is the resynchronisation granularity of the log.
	BlockSize = 32 * 1024

	// HeaderSize is crc(4) + length(2) + type(1).
	HeaderSize = 7
)

type recordType uint8

const (
	recZero   recordType = 0 // never written; marks preallocated/padding space
	recFull   recordType = 1
	recFirst  recordType = 2
	recMiddle recordType = 3
	recLast   recordType = 4
)

// ErrCorrupt reports that a record's framing or checksum did not validate.
//
// Recovery treats this as end-of-log rather than as a fatal error, because the
// overwhelmingly likely cause is a torn final write: the process died between
// the kernel accepting some of a record's bytes and all of them. Those bytes
// were never acknowledged to a caller, so dropping them loses nothing. What
// would be unacceptable is dropping records BEFORE the corruption -- which is
// why the reader stops at the damage rather than trying to scan past it.
var ErrCorrupt = errors.New("strata/wal: corrupt record")

// ErrTruncated reports that the log ended in the middle of a record.
var ErrTruncated = errors.New("strata/wal: truncated record")

// Writer appends logical records to a log file.
type Writer struct {
	w           io.Writer
	blockOffset int // bytes already written into the current block
	scratch     [HeaderSize]byte
}

// NewWriter returns a Writer that starts at the beginning of a block.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// NewWriterAt returns a Writer resuming at an existing file offset, used when
// reopening a log that recovery decided to keep appending to.
func NewWriterAt(w io.Writer, offset int64) *Writer {
	return &Writer{w: w, blockOffset: int(offset % BlockSize)}
}

// Append writes one logical record, fragmenting it across blocks as needed.
//
// A zero-length record is legal and round-trips as a zero-length record.
func (w *Writer) Append(p []byte) error {
	first := true
	for {
		avail := BlockSize - w.blockOffset
		if avail < HeaderSize {
			// Not enough room for even a header: pad the block out with zeros.
			// The reader recognises an all-zero header as padding and moves on.
			if avail > 0 {
				var pad [HeaderSize]byte
				if _, err := w.w.Write(pad[:avail]); err != nil {
					return err
				}
			}
			w.blockOffset = 0
			avail = BlockSize
		}

		space := avail - HeaderSize
		n := len(p)
		end := n <= space
		if !end {
			n = space
		}

		var t recordType
		switch {
		case first && end:
			t = recFull
		case first:
			t = recFirst
		case end:
			t = recLast
		default:
			t = recMiddle
		}

		if err := w.emit(t, p[:n]); err != nil {
			return err
		}
		p = p[n:]
		first = false
		if end {
			return nil
		}
	}
}

func (w *Writer) emit(t recordType, payload []byte) error {
	// The checksum covers the type byte as well as the payload. Including the
	// type is what stops a corrupted type field from silently converting a
	// FULL record into a FIRST record and swallowing the record after it.
	binary.LittleEndian.PutUint32(w.scratch[0:4], crc.Masked([]byte{byte(t)}, payload))
	binary.LittleEndian.PutUint16(w.scratch[4:6], uint16(len(payload)))
	w.scratch[6] = byte(t)

	if _, err := w.w.Write(w.scratch[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.w.Write(payload); err != nil {
			return err
		}
	}
	w.blockOffset += HeaderSize + len(payload)
	return nil
}

// Reader reassembles logical records from a log file.
type Reader struct {
	r      io.Reader
	block  [BlockSize]byte
	buf    []byte // unconsumed bytes of the current block
	eof    bool
	record []byte // reassembly buffer, reused across calls
}

// NewReader returns a Reader over r, which must be positioned at a block
// boundary (in practice, at the start of the file).
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Next returns the next logical record.
//
// The returned slice is only valid until the following call to Next. It
// returns io.EOF at a clean end of log, and ErrCorrupt or ErrTruncated if the
// log ends in damage -- callers doing recovery should treat all three as
// "the log ends here" but should log the latter two, because a torn tail in
// the middle of a file (rather than at the end) means something worse happened.
func (r *Reader) Next() ([]byte, error) {
	r.record = r.record[:0]
	inFragment := false

	for {
		if len(r.buf) < HeaderSize {
			if err := r.nextBlock(); err != nil {
				if inFragment {
					// We saw a FIRST or MIDDLE fragment and the log ended
					// before its LAST. The record was never complete on disk,
					// so it was never durable, so it was never acknowledged.
					return nil, ErrTruncated
				}
				return nil, err
			}
			continue
		}

		hdr := r.buf[:HeaderSize]
		storedCRC := binary.LittleEndian.Uint32(hdr[0:4])
		length := int(binary.LittleEndian.Uint16(hdr[4:6]))
		t := recordType(hdr[6])

		if t == recZero && storedCRC == 0 && length == 0 {
			// Zero padding at the tail of a block. Skip to the next block.
			r.buf = nil
			continue
		}

		if HeaderSize+length > len(r.buf) {
			// The header claims a payload that runs past the end of the block.
			// A well-formed log never does this, so either the length field is
			// damaged or the file was truncated mid-record.
			if inFragment {
				return nil, ErrTruncated
			}
			return nil, ErrCorrupt
		}

		payload := r.buf[HeaderSize : HeaderSize+length]
		if crc.Value([]byte{byte(t)}, payload) != crc.Unmask(storedCRC) {
			return nil, ErrCorrupt
		}
		r.buf = r.buf[HeaderSize+length:]

		switch t {
		case recFull:
			if inFragment {
				return nil, ErrCorrupt // FULL in the middle of a fragmented record
			}
			r.record = append(r.record, payload...)
			return r.record, nil
		case recFirst:
			if inFragment {
				return nil, ErrCorrupt
			}
			r.record = append(r.record, payload...)
			inFragment = true
		case recMiddle:
			if !inFragment {
				return nil, ErrCorrupt
			}
			r.record = append(r.record, payload...)
		case recLast:
			if !inFragment {
				return nil, ErrCorrupt
			}
			r.record = append(r.record, payload...)
			return r.record, nil
		default:
			return nil, fmt.Errorf("%w: unknown record type %d", ErrCorrupt, t)
		}
	}
}

func (r *Reader) nextBlock() error {
	if r.eof {
		return io.EOF
	}
	n, err := io.ReadFull(r.r, r.block[:])
	switch {
	case err == io.EOF:
		r.eof = true
		return io.EOF
	case err == io.ErrUnexpectedEOF:
		// A partial final block is normal: the log is a file being appended to,
		// not a file of whole blocks.
		r.eof = true
		if n == 0 {
			return io.EOF
		}
	case err != nil:
		return err
	}
	r.buf = r.block[:n]
	return nil
}
