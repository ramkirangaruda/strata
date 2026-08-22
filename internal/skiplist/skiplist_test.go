package skiplist

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/ramkirangaruda/strata/internal/keys"
)

func ik(u string, seq uint64) []byte { return keys.Encode(nil, []byte(u), seq, keys.KindSet) }

func TestInsertAndIterateInOrder(t *testing.T) {
	l := New(1)
	rnd := rand.New(rand.NewSource(2))
	const n = 5000
	perm := rnd.Perm(n)
	for _, i := range perm {
		l.Insert(ik(fmt.Sprintf("key%06d", i), uint64(i+1)), []byte(fmt.Sprintf("v%d", i)))
	}
	if l.Len() != n {
		t.Fatalf("Len = %d, want %d", l.Len(), n)
	}

	it := l.NewIterator()
	count := 0
	var prev []byte
	for it.First(); it.Valid(); it.Next() {
		if prev != nil && keys.Compare(prev, it.Key()) >= 0 {
			t.Fatalf("out of order at entry %d", count)
		}
		prev = append([]byte(nil), it.Key()...)
		count++
	}
	if count != n {
		t.Fatalf("iterated %d, inserted %d", count, n)
	}
}

func TestSeekGE(t *testing.T) {
	l := New(3)
	for i := 0; i < 100; i++ {
		l.Insert(ik(fmt.Sprintf("k%03d", i*2), uint64(i+1)), nil)
	}
	it := l.NewIterator()

	it.SeekGE(ik("k000", keys.MaxSequence))
	if !it.Valid() || string(keys.UserKey(it.Key())) != "k000" {
		t.Fatalf("SeekGE to the first key landed on %q", keys.UserKey(it.Key()))
	}
	// A key that does not exist must land on the next one that does.
	it.SeekGE(ik("k001", keys.MaxSequence))
	if !it.Valid() || string(keys.UserKey(it.Key())) != "k002" {
		t.Fatalf("SeekGE(k001) landed on %q, want k002", keys.UserKey(it.Key()))
	}
	it.SeekGE(ik("zzz", keys.MaxSequence))
	if it.Valid() {
		t.Error("SeekGE past the end returned a valid position")
	}
}

// One writer, many readers, no locks. Run this under -race: the point is not
// that the readers see everything, but that they never see anything torn.
func TestConcurrentReadersSeeConsistentEntries(t *testing.T) {
	l := New(7)
	const n = 20000

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				it := l.NewIterator()
				var prev []byte
				for it.First(); it.Valid(); it.Next() {
					// Every entry a reader observes must be a complete, well
					// formed pair -- never a key with someone else's value.
					if !keys.Valid(it.Key()) {
						t.Error("reader saw a malformed key")
						return
					}
					want := "v" + string(keys.UserKey(it.Key()))
					if string(it.Value()) != want {
						t.Errorf("reader saw key %q paired with value %q", keys.UserKey(it.Key()), it.Value())
						return
					}
					if prev != nil && keys.Compare(prev, it.Key()) >= 0 {
						t.Error("reader saw entries out of order")
						return
					}
					prev = append(prev[:0], it.Key()...)
				}
			}
		}()
	}

	for i := 0; i < n; i++ {
		u := fmt.Sprintf("k%06d", i)
		l.Insert(ik(u, uint64(i+1)), []byte("v"+u))
	}
	close(stop)
	wg.Wait()
}

func TestDuplicateSequenceNumberPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("inserting a duplicate internal key did not panic")
		}
	}()
	l := New(1)
	l.Insert(ik("k", 1), nil)
	l.Insert(ik("k", 1), nil)
}
