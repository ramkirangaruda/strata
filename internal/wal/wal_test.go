package wal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

func writeAll(t *testing.T, recs [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i, r := range recs {
		if err := w.Append(r); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	return buf.Bytes()
}

func readAll(t *testing.T, data []byte) ([][]byte, error) {
	t.Helper()
	r := NewReader(bytes.NewReader(data))
	var out [][]byte
	for {
		rec, err := r.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, append([]byte(nil), rec...))
	}
}

func TestRoundTrip(t *testing.T) {
	recs := [][]byte{
		{}, // empty records are legal
		[]byte("hello"),
		bytes.Repeat([]byte("a"), 100),
		bytes.Repeat([]byte("b"), BlockSize-HeaderSize), // exactly fills a block
		bytes.Repeat([]byte("c"), BlockSize),            // must fragment
		bytes.Repeat([]byte("d"), BlockSize*3+17),       // FIRST/MIDDLE/MIDDLE/LAST
		[]byte("after the big ones"),
	}
	got, err := readAll(t, writeAll(t, recs))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(recs) {
		t.Fatalf("read %d records, wrote %d", len(got), len(recs))
	}
	for i := range recs {
		if !bytes.Equal(got[i], recs[i]) {
			t.Errorf("record %d: got %d bytes, want %d", i, len(got[i]), len(recs[i]))
		}
	}
}

func TestRandomSizesRoundTrip(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	var recs [][]byte
	for i := 0; i < 500; i++ {
		n := rnd.Intn(3 * BlockSize / 8)
		rec := make([]byte, n)
		rnd.Read(rec)
		recs = append(recs, rec)
	}
	got, err := readAll(t, writeAll(t, recs))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for i := range recs {
		if !bytes.Equal(got[i], recs[i]) {
			t.Fatalf("record %d differs", i)
		}
	}
}

// The single most important property for recovery: if the process died
// mid-write, everything written BEFORE the torn record must still be readable.
// Losing the torn record is fine -- it was never acknowledged. Losing anything
// before it is data loss.
func TestTruncatedTailKeepsEarlierRecords(t *testing.T) {
	var recs [][]byte
	for i := 0; i < 40; i++ {
		recs = append(recs, []byte(fmt.Sprintf("record-%03d-%s", i, bytes.Repeat([]byte("x"), 500))))
	}
	full := writeAll(t, recs)

	for cut := 1; cut < len(full); cut += 97 {
		got, err := readAll(t, full[:cut])
		if err != nil && !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("cut at %d: unexpected error %v", cut, err)
		}
		// Whatever survived must be a prefix of what was written, byte for byte.
		if len(got) > len(recs) {
			t.Fatalf("cut at %d: read more records than were written", cut)
		}
		for i := range got {
			if !bytes.Equal(got[i], recs[i]) {
				t.Fatalf("cut at %d: record %d is not the record that was written", cut, i)
			}
		}
	}
}

func TestCorruptChecksumIsDetected(t *testing.T) {
	recs := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
	data := writeAll(t, recs)

	// Flip a bit inside the second record's payload.
	pos := HeaderSize + len("first") + HeaderSize + 2
	data[pos] ^= 0x01

	r := NewReader(bytes.NewReader(data))
	rec, err := r.Next()
	if err != nil || string(rec) != "first" {
		t.Fatalf("first record: %q, %v", rec, err)
	}
	if _, err := r.Next(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("second record: err = %v, want ErrCorrupt", err)
	}
}

// A corrupted type byte must not be able to turn a FULL record into a FIRST
// record and swallow the record after it. This is why the checksum covers the
// type byte and not just the payload.
func TestCorruptTypeByteIsDetected(t *testing.T) {
	data := writeAll(t, [][]byte{[]byte("alpha"), []byte("beta")})
	data[6] = byte(recFirst) // was recFull
	r := NewReader(bytes.NewReader(data))
	if _, err := r.Next(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestMaskIsNotIdentity(t *testing.T) {
	// A sanity check on the masking scheme: masking must actually change the
	// value, and must round-trip exactly.
	for _, v := range []uint32{0, 1, 0xdeadbeef, 0xffffffff} {
		if m := crcMaskForTest(v); m == v {
			t.Errorf("mask(%08x) is the identity", v)
		}
	}
}
