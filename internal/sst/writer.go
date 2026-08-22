package sst

import (
	"bytes"
	"fmt"
	"io"

	"github.com/ramkirangaruda/strata/internal/crc"
	"github.com/ramkirangaruda/strata/internal/keys"
)

// DefaultBlockSize is the target size of an uncompressed data block.
//
// 4 KiB matches the page size, which is the granularity the kernel and the
// device are going to move anyway. Smaller blocks mean a fatter index; larger
// blocks mean a point lookup reads more bytes than it needs. Almost every
// engine lands between 4 and 64 KiB, biased large for scan-heavy workloads.
const DefaultBlockSize = 4 * 1024

// Metadata describes a finished table. The manifest stores exactly this, and
// the version set uses Smallest/Largest to decide which files a lookup or a
// compaction has to touch without opening them.
type Metadata struct {
	Number   uint64
	Size     uint64
	Smallest []byte // smallest internal key in the file
	Largest  []byte // largest internal key in the file
	Entries  int
}

// Writer builds an SSTable in a single forward pass.
type Writer struct {
	w      io.Writer
	offset uint64
	err    error

	blockSize int

	data  blockBuilder
	index blockBuilder

	ukeys   [][]byte // user keys, deduplicated, for the bloom filter
	lastKey []byte   // last internal key added
	first   []byte   // smallest internal key added
	entries int

	finished bool
}

// NewWriter returns a Writer that appends a table to w.
func NewWriter(w io.Writer, blockSize int) *Writer {
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	tw := &Writer{w: w, blockSize: blockSize}
	tw.data.reset()
	tw.index.reset()
	return tw
}

// Add appends one entry. Internal keys must arrive in strictly ascending
// order, which for a memtable flush they naturally do.
func (w *Writer) Add(ikey, value []byte) error {
	if w.err != nil {
		return w.err
	}
	if w.finished {
		return fmt.Errorf("strata/sst: Add after Finish")
	}
	if w.entries > 0 && keys.Compare(w.lastKey, ikey) >= 0 {
		return w.fail(fmt.Errorf("strata/sst: keys added out of order"))
	}

	// Flush BEFORE adding, not after, so that w.lastKey is still the final key
	// of the block being flushed when the index entry is written.
	if w.data.sizeEstimate() >= w.blockSize {
		if err := w.flushDataBlock(); err != nil {
			return err
		}
	}

	if w.entries == 0 {
		w.first = append([]byte(nil), ikey...)
	}

	ukey := keys.UserKey(ikey)
	if len(w.ukeys) == 0 || !bytes.Equal(w.ukeys[len(w.ukeys)-1], ukey) {
		// Several versions of one user key share a bloom entry, so only the
		// first is recorded. Adding them all would inflate the filter without
		// improving it at all.
		w.ukeys = append(w.ukeys, append([]byte(nil), ukey...))
	}

	w.data.add(ikey, value)
	w.lastKey = append(w.lastKey[:0], ikey...)
	w.entries++
	return nil
}

func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = err
	}
	return w.err
}

// writeRawBlock appends contents plus a trailer and returns its handle.
//
// The trailer is one compression byte and a masked CRC32C over the contents
// AND that byte. Checksumming per block rather than per file is deliberate: a
// reader touches one block at a time, and a per-file checksum could only be
// verified by reading the entire file, which defeats the point of an index.
func (w *Writer) writeRawBlock(contents []byte) (BlockHandle, error) {
	h := BlockHandle{Offset: w.offset, Size: uint64(len(contents))}
	if _, err := w.w.Write(contents); err != nil {
		return h, w.fail(err)
	}
	trailer := [blockTrailerSize]byte{compressionNone}
	sum := crc.Masked(contents, trailer[:1])
	trailer[1] = byte(sum)
	trailer[2] = byte(sum >> 8)
	trailer[3] = byte(sum >> 16)
	trailer[4] = byte(sum >> 24)
	if _, err := w.w.Write(trailer[:]); err != nil {
		return h, w.fail(err)
	}
	w.offset += uint64(len(contents)) + blockTrailerSize
	return h, nil
}

func (w *Writer) flushDataBlock() error {
	if w.data.empty() {
		return nil
	}
	contents := w.data.finish()
	h, err := w.writeRawBlock(contents)
	if err != nil {
		return err
	}
	// The index entry keys the block by its LAST key. A seek for K therefore
	// binary-searches the index for the first entry whose key is >= K, which
	// is by construction the only block that can contain K.
	//
	// A production engine would instead store the shortest string that
	// separates this block's last key from the next block's first key
	// ("user:2" rather than "user:1999999"), which shrinks the index
	// substantially on long keys. That optimisation is deferred: it changes
	// nothing about correctness, and doing it here would obscure why the
	// index works.
	w.index.add(w.lastKey, h.AppendTo(nil))
	w.data.reset()
	return nil
}

// Finish writes the filter block, the index block and the footer, and returns
// the table's metadata. The Writer is unusable afterwards.
func (w *Writer) Finish() (*Metadata, error) {
	if w.err != nil {
		return nil, w.err
	}
	if w.finished {
		return nil, fmt.Errorf("strata/sst: Finish called twice")
	}
	if err := w.flushDataBlock(); err != nil {
		return nil, err
	}
	w.finished = true

	filterHandle, err := w.writeRawBlock(buildBloom(w.ukeys))
	if err != nil {
		return nil, err
	}
	indexHandle, err := w.writeRawBlock(w.index.finish())
	if err != nil {
		return nil, err
	}
	if _, err := w.w.Write(footer{filter: filterHandle, index: indexHandle}.encode()); err != nil {
		return nil, w.fail(err)
	}
	w.offset += FooterSize

	return &Metadata{
		Size:     w.offset,
		Smallest: w.first,
		Largest:  append([]byte(nil), w.lastKey...),
		Entries:  w.entries,
	}, nil
}
