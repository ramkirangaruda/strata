package keys

import (
	"sort"
	"testing"
)

func TestEncodeRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		ukey string
		seq  uint64
		kind Kind
	}{
		{"", 0, KindSet},
		{"a", 1, KindDelete},
		{"some/longer/key", MaxSequence, KindSet},
	} {
		ik := Encode(nil, []byte(tc.ukey), tc.seq, tc.kind)
		if !Valid(ik) {
			t.Fatalf("%q: encoded key is not Valid", tc.ukey)
		}
		u, s, k, err := Split(ik)
		if err != nil {
			t.Fatalf("%q: %v", tc.ukey, err)
		}
		if string(u) != tc.ukey || s != tc.seq || k != tc.kind {
			t.Errorf("round trip of (%q,%d,%d) gave (%q,%d,%d)", tc.ukey, tc.seq, tc.kind, u, s, k)
		}
	}
}

// The ordering contract, stated as a test so that changing Compare breaks
// loudly rather than producing an engine that returns stale values.
func TestSortOrder(t *testing.T) {
	mk := func(u string, seq uint64) []byte { return Encode(nil, []byte(u), seq, KindSet) }

	got := [][]byte{mk("b", 1), mk("a", 1), mk("a", 5), mk("a", 3), mk("c", 9)}
	sort.Slice(got, func(i, j int) bool { return Compare(got[i], got[j]) < 0 })

	want := []struct {
		u   string
		seq uint64
	}{
		{"a", 5}, // user keys ascending, sequence numbers DESCENDING within a key
		{"a", 3},
		{"a", 1},
		{"b", 1},
		{"c", 9},
	}
	for i, w := range want {
		if string(UserKey(got[i])) != w.u || Seq(got[i]) != w.seq {
			t.Fatalf("position %d: got (%q,%d), want (%q,%d)",
				i, UserKey(got[i]), Seq(got[i]), w.u, w.seq)
		}
	}
}

// A seek key at sequence s must sort before every version at or below s, and
// after every version above s. This is the property point lookups depend on.
func TestSeekKeyStraddlesTheSnapshot(t *testing.T) {
	seek := Encode(nil, []byte("k"), 10, KindSeek)

	for _, seq := range []uint64{11, 12, 100} {
		for _, kind := range []Kind{KindSet, KindDelete} {
			if Compare(Encode(nil, []byte("k"), seq, kind), seek) >= 0 {
				t.Errorf("version at seq %d (kind %d) does not sort before a seek at seq 10", seq, kind)
			}
		}
	}
	// Versions at or below the snapshot must sort AT OR AFTER the seek key, so
	// that a SeekGE lands on them. "At" is not a rounding error: a KindSet
	// record at exactly seq 10 encodes the same trailer as the seek key,
	// because KindSeek is defined to be KindSet. Requiring a strict inequality
	// here would be requiring the engine to skip the very version the snapshot
	// is supposed to read.
	for _, seq := range []uint64{10, 9, 0} {
		for _, kind := range []Kind{KindSet, KindDelete} {
			if Compare(Encode(nil, []byte("k"), seq, kind), seek) < 0 {
				t.Errorf("version at seq %d (kind %d) sorts before a seek at seq 10", seq, kind)
			}
		}
	}
}

func TestMalformedKeysDoNotBreakTheComparator(t *testing.T) {
	good := Encode(nil, []byte("k"), 1, KindSet)
	short := []byte("abc")
	if Compare(short, good) >= 0 {
		t.Error("a malformed key must sort before a well-formed one")
	}
	if Compare(good, short) <= 0 {
		t.Error("comparator is not antisymmetric for malformed keys")
	}
	if _, _, _, err := Split(short); err == nil {
		t.Error("Split accepted a key shorter than a trailer")
	}
}
