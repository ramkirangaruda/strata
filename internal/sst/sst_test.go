package sst

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ramkirangaruda/strata/internal/keys"
)

func ikey(user string, seq uint64, kind keys.Kind) []byte {
	return keys.Encode(nil, []byte(user), seq, kind)
}

// buildTable writes n entries with small blocks so that the table is forced to
// contain many data blocks -- a single-block table would not exercise the
// index at all, which is where the interesting bugs live.
func buildTable(t *testing.T, n int, blockSize int) (*Reader, *Metadata) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf, blockSize)
	for i := 0; i < n; i++ {
		k := ikey(fmt.Sprintf("key%06d", i), uint64(i+1), keys.KindSet)
		if err := w.Add(k, []byte(fmt.Sprintf("value-%06d", i))); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	meta, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r, meta
}

func TestRoundTripPointLookups(t *testing.T) {
	const n = 5000
	r, meta := buildTable(t, n, 512)

	if meta.Entries != n {
		t.Fatalf("Entries = %d, want %d", meta.Entries, n)
	}
	if got := string(keys.UserKey(meta.Smallest)); got != "key000000" {
		t.Errorf("Smallest user key = %q", got)
	}
	if got := string(keys.UserKey(meta.Largest)); got != fmt.Sprintf("key%06d", n-1) {
		t.Errorf("Largest user key = %q", got)
	}

	for i := 0; i < n; i++ {
		// Read at a sequence far above every write, i.e. "read the latest".
		seek := ikey(fmt.Sprintf("key%06d", i), keys.MaxSequence, keys.KindSeek)
		v, kind, found, err := r.Get(seek)
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if !found {
			t.Fatalf("Get(%d): not found", i)
		}
		if kind != keys.KindSet {
			t.Fatalf("Get(%d): kind = %d", i, kind)
		}
		if want := fmt.Sprintf("value-%06d", i); string(v) != want {
			t.Fatalf("Get(%d) = %q, want %q", i, v, want)
		}
	}
}

func TestMissingKeys(t *testing.T) {
	r, _ := buildTable(t, 1000, 512)
	for _, k := range []string{"aaa", "key000000x", "zzz", "key999999"} {
		_, _, found, err := r.Get(ikey(k, keys.MaxSequence, keys.KindSeek))
		if err != nil {
			t.Fatalf("Get(%q): %v", k, err)
		}
		if found {
			t.Errorf("Get(%q): reported found for a key never written", k)
		}
	}
}

// A snapshot read must not see versions written after it. This is the property
// the descending-trailer sort order exists to provide, so it is worth an
// explicit test rather than trusting the comparator.
func TestSnapshotIsolationWithinATable(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, 256)
	// Three versions of one key, newest first in sort order.
	for _, seq := range []uint64{30, 20, 10} {
		if err := w.Add(ikey("k", seq, keys.KindSet), []byte(fmt.Sprintf("v%d", seq))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		at   uint64
		want string
	}{
		{35, "v30"}, {30, "v30"}, {29, "v20"}, {20, "v20"}, {15, "v10"}, {10, "v10"},
	} {
		v, _, found, err := r.Get(ikey("k", tc.at, keys.KindSeek))
		if err != nil || !found {
			t.Fatalf("read at seq %d: found=%v err=%v", tc.at, found, err)
		}
		if string(v) != tc.want {
			t.Errorf("read at seq %d = %q, want %q", tc.at, v, tc.want)
		}
	}

	// Below every write, the key does not exist yet.
	if _, _, found, _ := r.Get(ikey("k", 9, keys.KindSeek)); found {
		t.Error("read at seq 9 saw a key first written at seq 10")
	}
}

func TestTombstoneIsFoundButHasNoValue(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, 256)
	if err := w.Add(ikey("gone", 5, keys.KindDelete), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	r, _ := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	_, kind, found, err := r.Get(ikey("gone", keys.MaxSequence, keys.KindSeek))
	if err != nil || !found {
		t.Fatalf("tombstone lookup: found=%v err=%v", found, err)
	}
	if kind != keys.KindDelete {
		t.Errorf("kind = %d, want KindDelete", kind)
	}
}

func TestIteratorVisitsEveryEntryInOrder(t *testing.T) {
	const n = 3000
	r, _ := buildTable(t, n, 400)
	it := r.NewIter()
	count := 0
	var prev []byte
	for it.First(); it.Valid(); it.Next() {
		if prev != nil && keys.Compare(prev, it.Key()) >= 0 {
			t.Fatalf("iterator went backwards at entry %d", count)
		}
		prev = append([]byte(nil), it.Key()...)
		count++
	}
	if err := it.Error(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}
	if count != n {
		t.Fatalf("iterated %d entries, wrote %d", count, n)
	}
}

func TestIteratorSeekLandsOnTheRightEntry(t *testing.T) {
	r, _ := buildTable(t, 2000, 400)
	it := r.NewIter()
	for _, i := range []int{0, 1, 17, 512, 1999} {
		target := ikey(fmt.Sprintf("key%06d", i), keys.MaxSequence, keys.KindSeek)
		it.SeekGE(target)
		if !it.Valid() {
			t.Fatalf("SeekGE(%d): invalid", i)
		}
		if got, want := string(keys.UserKey(it.Key())), fmt.Sprintf("key%06d", i); got != want {
			t.Errorf("SeekGE(%d) landed on %q, want %q", i, got, want)
		}
	}
	// Past the end.
	it.SeekGE(ikey("zzzz", keys.MaxSequence, keys.KindSeek))
	if it.Valid() {
		t.Error("SeekGE past the last key returned a valid position")
	}
}

// Corruption must surface as an error, never as a plausible-looking wrong
// answer. This is the single most important property of the block trailer.
func TestChecksumCatchesBitFlips(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, 512)
	for i := 0; i < 200; i++ {
		if err := w.Add(ikey(fmt.Sprintf("key%04d", i), uint64(i+1), keys.KindSet), []byte("some value here")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()

	corrupted := 0
	for pos := 0; pos < len(good)-FooterSize; pos += 37 {
		bad := append([]byte(nil), good...)
		bad[pos] ^= 0x40
		r, err := Open(bytes.NewReader(bad), int64(len(bad)))
		if err != nil {
			corrupted++
			continue
		}
		sawError := false
		it := r.NewIter()
		for it.First(); it.Valid(); it.Next() {
		}
		if it.Error() != nil {
			sawError = true
		}
		for i := 0; i < 200 && !sawError; i++ {
			if _, _, _, err := r.Get(ikey(fmt.Sprintf("key%04d", i), keys.MaxSequence, keys.KindSeek)); err != nil {
				sawError = true
			}
		}
		if sawError {
			corrupted++
		}
	}
	if corrupted == 0 {
		t.Fatal("no injected bit flip was detected; the block checksums are not doing anything")
	}
	t.Logf("detected corruption in %d injected bit flips", corrupted)
}
