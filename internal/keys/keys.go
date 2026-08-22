// Package keys defines strata's internal key encoding.
//
// Every user-visible key is stored on disk wrapped in an *internal key*:
//
//	internal_key = user_key || trailer
//	trailer      = uint64le( (sequence << 8) | kind )
//
// The trailer is what makes MVCC possible. Two writes to the same user key
// produce two distinct internal keys that both live in the engine at once;
// which one a reader sees depends on the sequence number it is reading at.
//
// The sort order is the load-bearing part of the whole engine:
//
//	user key   ASCENDING   (so range scans work)
//	trailer    DESCENDING  (so the NEWEST version of a key sorts FIRST)
//
// Descending trailer order is why a point lookup is just "seek and take the
// first match": the first entry with a matching user key is by construction
// the newest version visible at the seek sequence number. Every iterator in
// the engine relies on this, so if you change Compare, you break everything.
package keys

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Kind tags what a record means. It occupies the low byte of the trailer, so
// it must fit in 8 bits and must never exceed KindMax.
type Kind uint8

const (
	// KindDelete is a tombstone: the key is absent as of this sequence number.
	// Tombstones are real records that occupy space until compaction can prove
	// no live snapshot needs them (see phase A3).
	KindDelete Kind = 0
	// KindSet carries a value.
	KindSet Kind = 1

	// KindMax is the largest valid kind.
	KindMax = KindSet
)

const (
	// TrailerLen is the fixed number of bytes appended to every user key.
	TrailerLen = 8

	// MaxSequence is the largest representable sequence number. The trailer
	// spends 8 bits on the kind, so sequence numbers get the remaining 56.
	// At a million writes per second that is ~2200 years, which is enough.
	MaxSequence uint64 = 1<<56 - 1
)

// KindSeek is the kind used when *constructing a seek key* rather than
// storing a record.
//
// To read user key k at snapshot s, we seek for the internal key
// (k, s, KindSeek). Because trailers sort descending, every version of k with
// a sequence number GREATER than s has a larger trailer and therefore sorts
// BEFORE our seek key -- so the seek skips exactly the versions the snapshot
// must not see, and lands on the newest version at or below s.
//
// That only works if KindSeek is the largest valid kind: at equal sequence
// numbers the seek key must not sort after the record we are looking for.
const KindSeek = KindMax

// ErrMalformed is returned when a byte slice is too short to be an internal
// key, or carries a kind the engine does not recognise. In practice this means
// corruption, so callers should treat it as a hard error rather than a miss.
var ErrMalformed = fmt.Errorf("strata/keys: malformed internal key")

// Encode appends the internal key for (ukey, seq, kind) to dst and returns the
// extended slice. Pass a nil dst to allocate a fresh key.
//
// Encoding into a caller-owned buffer matters more than it looks: point
// lookups build a seek key on every call, and the alternative is an allocation
// per Get on the hottest path in the engine.
func Encode(dst, ukey []byte, seq uint64, kind Kind) []byte {
	if seq > MaxSequence {
		panic("strata/keys: sequence number overflows 56 bits")
	}
	if kind > KindMax {
		panic("strata/keys: unknown kind")
	}
	dst = append(dst, ukey...)
	var trailer [TrailerLen]byte
	binary.LittleEndian.PutUint64(trailer[:], seq<<8|uint64(kind))
	return append(dst, trailer[:]...)
}

// Valid reports whether ik is long enough to be an internal key and carries a
// kind the engine understands.
func Valid(ik []byte) bool {
	if len(ik) < TrailerLen {
		return false
	}
	return Kind(ik[len(ik)-TrailerLen]) <= KindMax
}

// UserKey returns the user key portion of ik, aliasing ik's memory.
//
// The result is NOT a copy. If ik points into a memtable node or a decoded
// block, the user key stays valid only as long as that memory does. Copy it
// before handing it to a caller who outlives the iterator.
func UserKey(ik []byte) []byte {
	if len(ik) < TrailerLen {
		return nil
	}
	return ik[:len(ik)-TrailerLen]
}

// Trailer returns the raw 8-byte trailer as a uint64.
func Trailer(ik []byte) uint64 {
	if len(ik) < TrailerLen {
		return 0
	}
	return binary.LittleEndian.Uint64(ik[len(ik)-TrailerLen:])
}

// Seq returns the sequence number encoded in ik.
func Seq(ik []byte) uint64 { return Trailer(ik) >> 8 }

// KindOf returns the record kind encoded in ik.
func KindOf(ik []byte) Kind { return Kind(Trailer(ik) & 0xff) }

// Split decomposes an internal key. It is the checked counterpart to the
// individual accessors, for paths that are reading untrusted bytes off disk.
func Split(ik []byte) (ukey []byte, seq uint64, kind Kind, err error) {
	if !Valid(ik) {
		return nil, 0, 0, ErrMalformed
	}
	return UserKey(ik), Seq(ik), KindOf(ik), nil
}

// Compare orders two internal keys: user key ascending, then trailer
// descending. It returns a negative number, zero, or a positive number as
// a sorts before, equal to, or after b.
//
// Malformed keys (shorter than a trailer) sort before every well-formed key
// so that a corrupt entry cannot make the comparator inconsistent and send a
// binary search off the rails.
func Compare(a, b []byte) int {
	aok, bok := len(a) >= TrailerLen, len(b) >= TrailerLen
	switch {
	case !aok && !bok:
		return bytes.Compare(a, b)
	case !aok:
		return -1
	case !bok:
		return 1
	}
	if c := bytes.Compare(UserKey(a), UserKey(b)); c != 0 {
		return c
	}
	// Descending: the LARGER trailer (newer sequence number) sorts FIRST.
	at, bt := Trailer(a), Trailer(b)
	switch {
	case at > bt:
		return -1
	case at < bt:
		return 1
	default:
		return 0
	}
}

// CompareUser orders two user keys. Kept here so that every comparison in the
// engine flows through one package and one definition of ordering.
func CompareUser(a, b []byte) int { return bytes.Compare(a, b) }

// ComparatorName is written into the MANIFEST at database creation and checked
// on every open. Opening a database with a different comparator would silently
// produce wrong results on every read, so the engine refuses instead.
const ComparatorName = "strata.BytewiseComparator"
