package sst

import (
	"encoding/binary"
	"fmt"

	"github.com/ramkirangaruda/strata/internal/keys"
)

// A block is the unit of I/O and of caching inside an SSTable. Both data
// blocks and the index block use this format.
//
// # Layout
//
//	entry_0 entry_1 ... entry_n-1
//	restart_offset_0 ... restart_offset_r-1   (uint32 each)
//	num_restarts                              (uint32)
//
//	entry := varint(shared) varint(unshared) varint(value_len)
//	         key_suffix[unshared] value[value_len]
//
// # Prefix compression, and why restart points exist
//
// Keys in a block are sorted, so adjacent keys usually share a long prefix
// ("user:1000001" after "user:1000000"). Each entry therefore stores only how
// many bytes it shares with its predecessor plus the bytes that differ. On
// real key distributions this is a large win -- often 3-5x on the key portion.
//
// The cost is that an entry is no longer independently decodable: to know the
// key at entry 500 you must replay entries 0..500. That would make a binary
// search inside a block impossible, which would make point lookups linear.
//
// Restart points buy it back. Every restartInterval entries, one entry stores
// its key in full (shared = 0), and the offsets of those entries are recorded
// in an array at the end of the block. A seek binary-searches that array -- the
// keys there are complete and self-describing -- and then replays at most
// restartInterval entries linearly. Sixteen is the usual choice: it keeps the
// scan short while spending only ~6% of entries on uncompressed keys.

const restartInterval = 16

type blockBuilder struct {
	buf      []byte
	restarts []uint32
	counter  int
	lastKey  []byte
	entries  int
}

func (b *blockBuilder) reset() {
	b.buf = b.buf[:0]
	b.restarts = append(b.restarts[:0], 0) // entry 0 is always a restart point
	b.counter = 0
	b.lastKey = b.lastKey[:0]
	b.entries = 0
}

func (b *blockBuilder) empty() bool { return b.entries == 0 }

// sizeEstimate is what the block will occupy once finished, used to decide
// when to flush. It has to include the restart array, or blocks overshoot the
// target size by a few percent on every flush.
func (b *blockBuilder) sizeEstimate() int {
	return len(b.buf) + len(b.restarts)*4 + 4
}

// add appends an entry. Keys must arrive in strictly increasing order; the
// caller (the table writer) is responsible for that, and a violation here
// silently produces a table whose binary searches return wrong answers, so it
// is checked rather than assumed.
func (b *blockBuilder) add(key, value []byte) {
	if b.entries > 0 && keys.Compare(b.lastKey, key) >= 0 {
		panic(fmt.Sprintf("strata/sst: block keys out of order: %q then %q", b.lastKey, key))
	}

	shared := 0
	if b.counter < restartInterval {
		// Share the longest common prefix with the previous key.
		n := min(len(b.lastKey), len(key))
		for shared < n && b.lastKey[shared] == key[shared] {
			shared++
		}
	} else {
		// Start a new restart point: store the key in full.
		b.restarts = append(b.restarts, uint32(len(b.buf)))
		b.counter = 0
	}

	b.buf = binary.AppendUvarint(b.buf, uint64(shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(key)-shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(value)))
	b.buf = append(b.buf, key[shared:]...)
	b.buf = append(b.buf, value...)

	b.lastKey = append(b.lastKey[:0], key...)
	b.counter++
	b.entries++
}

// finish appends the restart array and returns the block contents. The
// returned slice aliases the builder's buffer and is only valid until reset.
func (b *blockBuilder) finish() []byte {
	if b.entries == 0 {
		// An empty block still needs a well-formed restart array so the reader
		// does not have to special-case it.
		b.restarts = b.restarts[:0]
	}
	for _, r := range b.restarts {
		b.buf = binary.LittleEndian.AppendUint32(b.buf, r)
	}
	b.buf = binary.LittleEndian.AppendUint32(b.buf, uint32(len(b.restarts)))
	return b.buf
}

// block is a parsed, read-only block.
type block struct {
	data        []byte // entries only
	restarts    []byte // raw restart array, decoded on demand
	numRestarts int
}

func newBlock(data []byte) (*block, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("%w: block shorter than its restart count", ErrBadFormat)
	}
	n := int(binary.LittleEndian.Uint32(data[len(data)-4:]))
	restartBytes := n * 4
	if restartBytes+4 > len(data) {
		return nil, fmt.Errorf("%w: block claims %d restart points, which do not fit in %d bytes", ErrBadFormat, n, len(data))
	}
	end := len(data) - 4 - restartBytes
	return &block{
		data:        data[:end],
		restarts:    data[end : len(data)-4],
		numRestarts: n,
	}, nil
}

func (b *block) restartOffset(i int) int {
	return int(binary.LittleEndian.Uint32(b.restarts[i*4:]))
}

// blockIter walks a block in key order.
//
// Keys are reconstructed into an internal buffer as the iterator advances, so
// Key() is only valid until the next call to Next or SeekGE. Value() aliases
// the block itself and stays valid as long as the block does.
type blockIter struct {
	b     *block
	off   int // offset of the entry after the current one
	key   []byte
	val   []byte
	valid bool
	err   error
}

func (b *block) iter() *blockIter { return &blockIter{b: b} }

func (it *blockIter) Valid() bool   { return it.valid }
func (it *blockIter) Key() []byte   { return it.key }
func (it *blockIter) Value() []byte { return it.val }
func (it *blockIter) Error() error  { return it.err }

// decodeAt parses the entry starting at off, given the previous full key.
func (it *blockIter) decodeAt(off int, prevKey []byte) (key, val []byte, next int, ok bool) {
	d := it.b.data
	if off >= len(d) {
		return nil, nil, 0, false
	}
	shared, n1 := binary.Uvarint(d[off:])
	if n1 <= 0 {
		it.err = fmt.Errorf("%w: bad shared-prefix varint at offset %d", ErrBadFormat, off)
		return nil, nil, 0, false
	}
	unshared, n2 := binary.Uvarint(d[off+n1:])
	if n2 <= 0 {
		it.err = fmt.Errorf("%w: bad key-suffix varint at offset %d", ErrBadFormat, off)
		return nil, nil, 0, false
	}
	vlen, n3 := binary.Uvarint(d[off+n1+n2:])
	if n3 <= 0 {
		it.err = fmt.Errorf("%w: bad value-length varint at offset %d", ErrBadFormat, off)
		return nil, nil, 0, false
	}
	p := off + n1 + n2 + n3
	if uint64(shared) > uint64(len(prevKey)) {
		it.err = fmt.Errorf("%w: entry claims a %d-byte shared prefix of a %d-byte key", ErrBadFormat, shared, len(prevKey))
		return nil, nil, 0, false
	}
	if p+int(unshared)+int(vlen) > len(d) {
		it.err = fmt.Errorf("%w: entry at offset %d runs past the end of the block", ErrBadFormat, off)
		return nil, nil, 0, false
	}
	k := make([]byte, 0, int(shared)+int(unshared))
	k = append(k, prevKey[:shared]...)
	k = append(k, d[p:p+int(unshared)]...)
	p += int(unshared)
	return k, d[p : p+int(vlen)], p + int(vlen), true
}

// keyAtRestart returns the (complete) key stored at restart point i.
func (it *blockIter) keyAtRestart(i int) []byte {
	k, _, _, ok := it.decodeAt(it.b.restartOffset(i), nil)
	if !ok {
		return nil
	}
	return k
}

// First positions the iterator at the smallest key in the block.
func (it *blockIter) First() {
	if it.b.numRestarts == 0 {
		it.valid = false
		return
	}
	it.seekToOffset(it.b.restartOffset(0), nil)
}

func (it *blockIter) seekToOffset(off int, prevKey []byte) {
	k, v, next, ok := it.decodeAt(off, prevKey)
	if !ok {
		it.valid = false
		return
	}
	it.key, it.val, it.off, it.valid = k, v, next, true
}

// Next advances one entry.
func (it *blockIter) Next() {
	if !it.valid {
		return
	}
	it.seekToOffset(it.off, it.key)
}

// SeekGE positions the iterator at the first key >= target.
//
// Binary search over the restart array, then a linear replay of at most
// restartInterval entries. The binary search compares against restart keys
// only, which is the whole reason restart keys are stored uncompressed.
func (it *blockIter) SeekGE(target []byte) {
	if it.b.numRestarts == 0 {
		it.valid = false
		return
	}

	// Find the last restart point whose key sorts strictly before target.
	// Invariant: everything at or below lo is < target; everything above hi
	// is >= target.
	lo, hi := 0, it.b.numRestarts-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		k := it.keyAtRestart(mid)
		if k == nil {
			it.valid = false
			return
		}
		if keys.Compare(k, target) < 0 {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	it.seekToOffset(it.b.restartOffset(lo), nil)
	for it.valid && keys.Compare(it.key, target) < 0 {
		it.Next()
	}
}
