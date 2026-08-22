package strata

import (
	"encoding/binary"
	"fmt"

	"github.com/ramkirangaruda/strata/internal/keys"
)

// Batch is a group of writes that become visible together.
//
// Atomicity comes from the log framing rather than from any locking: a batch is
// encoded as ONE log record, and the log reader either reconstructs a record
// whole or rejects it. There is no state in which half a batch is durable.
//
// Batches are also the unit of sequence-number assignment. The batch gets a
// base sequence number and entry i gets base+i, which is what makes it possible
// to write several keys "at the same time" while still giving every version a
// unique, totally ordered identity.
//
// # Encoding
//
//	sequence (8 bytes LE) | count (4 bytes LE) | entry*
//	entry := kind(1) uvarint(len(key)) key [uvarint(len(value)) value]
//
// The value is omitted entirely for a tombstone. It is tempting to store an
// empty value instead and keep the encoding uniform, but then an empty string
// and a deletion become indistinguishable, and an engine that cannot tell those
// apart cannot implement Delete correctly.
type Batch struct {
	entries []batchEntry
}

type batchEntry struct {
	kind  keys.Kind
	key   []byte
	value []byte
}

// Put records a key/value assignment.
func (b *Batch) Put(key, value []byte) {
	b.entries = append(b.entries, batchEntry{keys.KindSet, append([]byte(nil), key...), append([]byte(nil), value...)})
}

// Delete records a tombstone.
func (b *Batch) Delete(key []byte) {
	b.entries = append(b.entries, batchEntry{keys.KindDelete, append([]byte(nil), key...), nil})
}

// Len is the number of entries, and therefore the number of sequence numbers
// the batch will consume.
func (b *Batch) Len() int { return len(b.entries) }

// Reset empties the batch for reuse.
func (b *Batch) Reset() { b.entries = b.entries[:0] }

func (b *Batch) encode(seq uint64) []byte {
	buf := make([]byte, 12, 12+len(b.entries)*32)
	binary.LittleEndian.PutUint64(buf[0:8], seq)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(b.entries)))
	for _, e := range b.entries {
		buf = append(buf, byte(e.kind))
		buf = binary.AppendUvarint(buf, uint64(len(e.key)))
		buf = append(buf, e.key...)
		if e.kind == keys.KindSet {
			buf = binary.AppendUvarint(buf, uint64(len(e.value)))
			buf = append(buf, e.value...)
		}
	}
	return buf
}

func decodeBatch(rec []byte) (seq uint64, entries []batchEntry, err error) {
	if len(rec) < 12 {
		return 0, nil, fmt.Errorf("strata: batch record is %d bytes, shorter than its header", len(rec))
	}
	seq = binary.LittleEndian.Uint64(rec[0:8])
	count := int(binary.LittleEndian.Uint32(rec[8:12]))
	p := rec[12:]

	for i := 0; i < count; i++ {
		if len(p) < 1 {
			return 0, nil, fmt.Errorf("strata: batch claims %d entries, ran out at %d", count, i)
		}
		kind := keys.Kind(p[0])
		if kind > keys.KindMax {
			return 0, nil, fmt.Errorf("strata: batch entry %d has unknown kind %d", i, kind)
		}
		p = p[1:]

		klen, n := binary.Uvarint(p)
		if n <= 0 || uint64(len(p[n:])) < klen {
			return 0, nil, fmt.Errorf("strata: batch entry %d has a truncated key", i)
		}
		p = p[n:]
		key := p[:klen]
		p = p[klen:]

		var value []byte
		if kind == keys.KindSet {
			vlen, n := binary.Uvarint(p)
			if n <= 0 || uint64(len(p[n:])) < vlen {
				return 0, nil, fmt.Errorf("strata: batch entry %d has a truncated value", i)
			}
			p = p[n:]
			value = p[:vlen]
			p = p[vlen:]
		}
		entries = append(entries, batchEntry{kind, key, value})
	}
	return seq, entries, nil
}
