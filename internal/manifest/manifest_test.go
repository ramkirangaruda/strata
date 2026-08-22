package manifest

import (
	"bytes"
	"testing"
)

func TestVersionEditRoundTrip(t *testing.T) {
	in := &VersionEdit{
		Comparator: "strata.BytewiseComparator", HasComparator: true,
		LogNumber: 42, HasLogNumber: true,
		NextFileNumber: 99, HasNextFileNumber: true,
		LastSequence: 123456, HasLastSequence: true,
		Deleted: []DeletedFile{{Level: 0, Number: 7}, {Level: 2, Number: 8}},
		New: []NewFile{
			{Level: 0, Meta: FileMeta{Number: 10, Size: 4096, Smallest: []byte("aaa\x00\x00\x00\x00\x00\x00\x01"), Largest: []byte("zzz\x00\x00\x00\x00\x00\x00\x01")}},
			{Level: 1, Meta: FileMeta{Number: 11, Size: 8192, Smallest: []byte("b"), Largest: []byte("y")}},
		},
	}

	out, err := Decode(in.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Comparator != in.Comparator || out.LogNumber != in.LogNumber ||
		out.NextFileNumber != in.NextFileNumber || out.LastSequence != in.LastSequence {
		t.Errorf("scalar fields did not round trip: %+v", out)
	}
	if len(out.Deleted) != 2 || out.Deleted[1] != in.Deleted[1] {
		t.Errorf("deleted files did not round trip: %+v", out.Deleted)
	}
	if len(out.New) != 2 {
		t.Fatalf("got %d new files, want 2", len(out.New))
	}
	for i := range in.New {
		a, b := in.New[i], out.New[i]
		if a.Level != b.Level || a.Meta.Number != b.Meta.Number || a.Meta.Size != b.Meta.Size ||
			!bytes.Equal(a.Meta.Smallest, b.Meta.Smallest) || !bytes.Equal(a.Meta.Largest, b.Meta.Largest) {
			t.Errorf("new file %d did not round trip: %+v vs %+v", i, a, b)
		}
	}
}

func TestEmptyEditRoundTrips(t *testing.T) {
	out, err := Decode((&VersionEdit{}).Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.HasLogNumber || out.HasComparator || len(out.New) != 0 {
		t.Errorf("empty edit decoded as %+v", out)
	}
}

func TestTruncatedEditIsRejected(t *testing.T) {
	full := (&VersionEdit{
		New: []NewFile{{Level: 0, Meta: FileMeta{Number: 1, Size: 2, Smallest: []byte("aaaa"), Largest: []byte("bbbb")}}},
	}).Encode()
	for cut := 1; cut < len(full); cut++ {
		if _, err := Decode(full[:cut]); err == nil {
			t.Fatalf("Decode accepted a manifest record truncated to %d/%d bytes", cut, len(full))
		}
	}
}
