package strata_test

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/ramkirangaruda/strata"
)

// These are not the benchmarks that go on a resume -- phase A6 is YCSB against
// Pebble, BadgerDB and SQLite, with error bars. These exist so that changes in
// A2 and A3 have a baseline to move against, and so the cost of Sync is a
// number rather than an intuition.
//
//	go test -bench . -benchmem -run '^$'

func benchDB(b *testing.B, sync bool) *strata.DB {
	b.Helper()
	db, err := strata.Open(b.TempDir(), strata.Options{
		Sync:         sync,
		MemtableSize: 4 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

func BenchmarkPutNoSync(b *testing.B) {
	db := benchDB(b, false)
	value := make([]byte, 100)
	b.ResetTimer()
	b.SetBytes(int64(len(value) + 16))
	for i := 0; i < b.N; i++ {
		if err := db.Put([]byte(fmt.Sprintf("key%09d", i)), value); err != nil {
			b.Fatal(err)
		}
	}
}

// The gap between this and BenchmarkPutNoSync is the price of durability
// against power loss. On a consumer SSD it is usually two orders of magnitude,
// which is why group commit exists and why almost nothing defaults to Sync.
func BenchmarkPutSync(b *testing.B) {
	db := benchDB(b, true)
	value := make([]byte, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Put([]byte(fmt.Sprintf("key%09d", i)), value); err != nil {
			b.Fatal(err)
		}
	}
}

// One batch is one log record and one fsync, so batching amortises the cost
// that BenchmarkPutSync pays per key.
func BenchmarkBatchedPutSync(b *testing.B) {
	for _, size := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("batch%d", size), func(b *testing.B) {
			db := benchDB(b, true)
			value := make([]byte, 100)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var batch strata.Batch
				for j := 0; j < size; j++ {
					batch.Put([]byte(fmt.Sprintf("key%09d", i*size+j)), value)
				}
				if err := db.Write(&batch); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(size), "keys/op")
		})
	}
}

func BenchmarkGetHit(b *testing.B) {
	db := benchDB(b, false)
	const n = 200000
	value := make([]byte, 100)
	for i := 0; i < n; i++ {
		if err := db.Put([]byte(fmt.Sprintf("key%09d", i)), value); err != nil {
			b.Fatal(err)
		}
	}
	rnd := rand.New(rand.NewSource(1))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Get([]byte(fmt.Sprintf("key%09d", rnd.Intn(n)))); err != nil {
			b.Fatal(err)
		}
	}
}

// Misses are the case bloom filters exist for. With no block cache yet, the
// gap between this and BenchmarkGetHit is roughly the filter doing its job.
func BenchmarkGetMiss(b *testing.B) {
	db := benchDB(b, false)
	const n = 200000
	value := make([]byte, 100)
	for i := 0; i < n; i++ {
		if err := db.Put([]byte(fmt.Sprintf("key%09d", i)), value); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Get([]byte(fmt.Sprintf("absent%09d", i)))
	}
}
