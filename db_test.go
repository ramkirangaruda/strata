package strata_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ramkirangaruda/strata"
)

func open(t *testing.T, dir string, opts strata.Options) *strata.DB {
	t.Helper()
	db, err := strata.Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func mustGet(t *testing.T, db *strata.DB, key, want string) {
	t.Helper()
	v, err := db.Get([]byte(key))
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	if string(v) != want {
		t.Fatalf("Get(%q) = %q, want %q", key, v, want)
	}
}

func mustMiss(t *testing.T, db *strata.DB, key string) {
	t.Helper()
	if _, err := db.Get([]byte(key)); !errors.Is(err, strata.ErrNotFound) {
		t.Fatalf("Get(%q): err = %v, want ErrNotFound", key, err)
	}
}

func TestPutGetDelete(t *testing.T) {
	db := open(t, t.TempDir(), strata.Options{})
	defer db.Close()

	if err := db.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	mustGet(t, db, "a", "1")
	mustMiss(t, db, "b")

	// An overwrite is a new version, and the newest must win.
	if err := db.Put([]byte("a"), []byte("2")); err != nil {
		t.Fatal(err)
	}
	mustGet(t, db, "a", "2")

	if err := db.Delete([]byte("a")); err != nil {
		t.Fatal(err)
	}
	mustMiss(t, db, "a")

	// Writing after a delete must resurrect the key -- the tombstone is just
	// another version, not a permanent gravestone.
	if err := db.Put([]byte("a"), []byte("3")); err != nil {
		t.Fatal(err)
	}
	mustGet(t, db, "a", "3")
}

func TestEmptyValueIsNotADeletion(t *testing.T) {
	db := open(t, t.TempDir(), strata.Options{})
	defer db.Close()
	if err := db.Put([]byte("k"), []byte{}); err != nil {
		t.Fatal(err)
	}
	v, err := db.Get([]byte("k"))
	if err != nil {
		t.Fatalf("an empty value came back as %v; empty and absent are different things", err)
	}
	if len(v) != 0 {
		t.Fatalf("Get = %q, want empty", v)
	}
}

func TestBatchIsAtomicAndOrdered(t *testing.T) {
	db := open(t, t.TempDir(), strata.Options{})
	defer db.Close()

	var b strata.Batch
	b.Put([]byte("x"), []byte("first"))
	b.Put([]byte("x"), []byte("second")) // later entry, higher sequence, wins
	b.Put([]byte("y"), []byte("yes"))
	b.Delete([]byte("z"))
	if err := db.Write(&b); err != nil {
		t.Fatal(err)
	}
	mustGet(t, db, "x", "second")
	mustGet(t, db, "y", "yes")
	mustMiss(t, db, "z")
}

func TestReopenReplaysTheLog(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, strata.Options{})
	for i := 0; i < 500; i++ {
		if err := db.Put([]byte(fmt.Sprintf("k%04d", i)), []byte(fmt.Sprintf("v%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = open(t, dir, strata.Options{})
	defer db.Close()
	for i := 0; i < 500; i++ {
		mustGet(t, db, fmt.Sprintf("k%04d", i), fmt.Sprintf("v%04d", i))
	}
}

func TestFlushProducesTablesAndReadsStillWork(t *testing.T) {
	dir := t.TempDir()
	// A tiny memtable forces many flushes, so the read path has to go through
	// level 0 files rather than answering everything from memory.
	db := open(t, dir, strata.Options{MemtableSize: 32 << 10, BlockSize: 512})
	const n = 4000
	value := make([]byte, 100)
	for i := 0; i < n; i++ {
		if err := db.Put([]byte(fmt.Sprintf("key%06d", i)), append(value[:0:0], []byte(fmt.Sprintf("v%06d", i))...)); err != nil {
			t.Fatal(err)
		}
	}

	tables, _ := filepath.Glob(filepath.Join(dir, "*.sst"))
	if len(tables) == 0 {
		t.Fatal("no tables were written; the memtable never flushed")
	}
	t.Logf("%d level-0 tables", len(tables))

	for i := 0; i < n; i++ {
		mustGet(t, db, fmt.Sprintf("key%06d", i), fmt.Sprintf("v%06d", i))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2 := open(t, dir, strata.Options{MemtableSize: 32 << 10, BlockSize: 512})
	defer db2.Close()
	for i := 0; i < n; i++ {
		mustGet(t, db2, fmt.Sprintf("key%06d", i), fmt.Sprintf("v%06d", i))
	}
}

func TestOverwritesAcrossFlushesResolveToTheNewestVersion(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, strata.Options{MemtableSize: 16 << 10, BlockSize: 512})

	const keys = 50
	for round := 0; round < 40; round++ {
		for k := 0; k < keys; k++ {
			v := fmt.Sprintf("round%02d", round)
			if err := db.Put([]byte(fmt.Sprintf("k%03d", k)), []byte(v)); err != nil {
				t.Fatal(err)
			}
		}
		// Pad, to push the memtable over its limit between rounds.
		for p := 0; p < 200; p++ {
			if err := db.Put([]byte(fmt.Sprintf("pad-%02d-%04d", round, p)), make([]byte, 100)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for k := 0; k < keys; k++ {
		mustGet(t, db, fmt.Sprintf("k%03d", k), "round39")
	}
	db.Close()

	db2 := open(t, dir, strata.Options{MemtableSize: 16 << 10, BlockSize: 512})
	defer db2.Close()
	for k := 0; k < keys; k++ {
		mustGet(t, db2, fmt.Sprintf("k%03d", k), "round39")
	}
}

func TestDeletesSurviveFlush(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, strata.Options{MemtableSize: 16 << 10, BlockSize: 512})

	for i := 0; i < 400; i++ {
		if err := db.Put([]byte(fmt.Sprintf("k%04d", i)), make([]byte, 200)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 400; i += 2 {
		if err := db.Delete([]byte(fmt.Sprintf("k%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// More writes, to force the tombstones down into a table.
	for i := 0; i < 800; i++ {
		if err := db.Put([]byte(fmt.Sprintf("z%04d", i)), make([]byte, 200)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	db2 := open(t, dir, strata.Options{MemtableSize: 16 << 10, BlockSize: 512})
	defer db2.Close()
	for i := 0; i < 400; i++ {
		k := fmt.Sprintf("k%04d", i)
		if i%2 == 0 {
			// A tombstone in a newer file must shadow the value in an older
			// one. Getting this wrong resurrects deleted keys, which is the
			// classic LSM bug.
			mustMiss(t, db2, k)
		} else if _, err := db2.Get([]byte(k)); err != nil {
			t.Fatalf("Get(%q): %v", k, err)
		}
	}
}

// A torn write at the tail of the log must lose only the records it damaged.
// Everything written before the tear must still be there, and reopening must
// succeed rather than error.
func TestTruncatedLogLosesOnlyASuffix(t *testing.T) {
	for _, cut := range []int{1, 9, 64, 501, 3000, 9999} {
		t.Run(fmt.Sprintf("cut%d", cut), func(t *testing.T) {
			dir := t.TempDir()
			db := open(t, dir, strata.Options{MemtableSize: 1 << 30}) // never flush
			const n = 300
			for i := 0; i < n; i++ {
				if err := db.Put([]byte(fmt.Sprintf("k%04d", i)), []byte(fmt.Sprintf("v%04d", i))); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()

			logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
			if len(logs) != 1 {
				t.Fatalf("expected one log file, found %d", len(logs))
			}
			st, err := os.Stat(logs[0])
			if err != nil {
				t.Fatal(err)
			}
			size := st.Size() - int64(cut)
			if size < 0 {
				t.Skip("log shorter than the cut")
			}
			if err := os.Truncate(logs[0], size); err != nil {
				t.Fatal(err)
			}

			db2, err := strata.Open(dir, strata.Options{})
			if err != nil {
				t.Fatalf("reopen after truncation failed: %v", err)
			}
			defer db2.Close()

			// Find the first missing key, then assert everything below it is
			// present and everything above it is absent -- i.e. exactly a
			// prefix survived.
			firstMissing := n
			for i := 0; i < n; i++ {
				if _, err := db2.Get([]byte(fmt.Sprintf("k%04d", i))); errors.Is(err, strata.ErrNotFound) {
					firstMissing = i
					break
				}
			}
			for i := 0; i < firstMissing; i++ {
				mustGet(t, db2, fmt.Sprintf("k%04d", i), fmt.Sprintf("v%04d", i))
			}
			for i := firstMissing; i < n; i++ {
				if _, err := db2.Get([]byte(fmt.Sprintf("k%04d", i))); !errors.Is(err, strata.ErrNotFound) {
					t.Fatalf("key %d is present but key %d (written earlier) is not: the log did not lose a clean suffix", i, firstMissing)
				}
			}
			t.Logf("cut %d bytes: kept %d/%d records", cut, firstMissing, n)
		})
	}
}

// The gate for phase A1: kill the process with SIGKILL, repeatedly, and assert
// that every write it acknowledged is still there afterwards.
func TestKillMinusNineLosesNoAcknowledgedWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: builds a helper binary and kills it repeatedly")
	}

	bin := filepath.Join(t.TempDir(), "crashwriter")
	build := exec.Command("go", "build", "-o", bin, "./cmd/crashwriter")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building crashwriter: %v\n%s", err, out)
	}

	dir := t.TempDir()
	highestAcked := -1

	const rounds = 8
	for round := 0; round < rounds; round++ {
		cmd := exec.Command(bin, dir, "200")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		acked := make(chan int, 1)
		go func() {
			last := -1
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				if n, err := strconv.Atoi(sc.Text()); err == nil {
					last = n
				}
			}
			acked <- last
		}()

		time.Sleep(250 * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil { // SIGKILL: no cleanup, no flush
			t.Fatal(err)
		}
		_ = cmd.Wait()
		last := <-acked
		if last < 0 {
			t.Fatalf("round %d: the writer acknowledged nothing", round)
		}
		if last > highestAcked {
			highestAcked = last
		}
		t.Logf("round %d: killed after %d acknowledged writes", round, last+1)

		db, err := strata.Open(dir, strata.Options{Sync: true, MemtableSize: 256 << 10})
		if err != nil {
			t.Fatalf("round %d: reopen after kill -9: %v", round, err)
		}
		for i := 0; i <= last; i++ {
			key := fmt.Sprintf("key%08d", i)
			if _, err := db.Get([]byte(key)); err != nil {
				t.Fatalf("round %d: %q was acknowledged before the kill but is gone after recovery: %v", round, key, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Every key acknowledged in any round must still be readable at the end,
	// after the database has been crashed and recovered eight times.
	db, err := strata.Open(dir, strata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i <= highestAcked; i++ {
		if _, err := db.Get([]byte(fmt.Sprintf("key%08d", i))); err != nil {
			t.Fatalf("after %d crashes, key %d is gone: %v", rounds, i, err)
		}
	}
	t.Logf("survived %d kill -9 cycles with %d writes intact", rounds, highestAcked+1)
}

func TestCorruptManifestIsRefusedNotIgnored(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, strata.Options{MemtableSize: 16 << 10})
	for i := 0; i < 500; i++ {
		if err := db.Put([]byte(fmt.Sprintf("k%04d", i)), make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	manifests, _ := filepath.Glob(filepath.Join(dir, "MANIFEST-*"))
	if len(manifests) != 1 {
		t.Fatalf("expected one manifest, found %d", len(manifests))
	}
	data, err := os.ReadFile(manifests[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 20 {
		t.Skip("manifest too small to corrupt meaningfully")
	}
	data[len(data)-3] ^= 0xff
	if err := os.WriteFile(manifests[0], data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Silently ignoring a damaged manifest would mean opening a database whose
	// file set is wrong, and answering reads with missing data. Refusing is
	// the only safe behaviour.
	if _, err := strata.Open(dir, strata.Options{}); err == nil {
		t.Fatal("opened a database with a corrupt manifest instead of refusing")
	}
}

func TestConcurrentReadersDuringWrites(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, strata.Options{MemtableSize: 64 << 10, BlockSize: 512})
	defer db.Close()

	const n = 3000
	done := make(chan error, 4)
	for r := 0; r < 4; r++ {
		go func() {
			for i := 0; i < n; i++ {
				// Racing the writer: a key may or may not exist yet, but a
				// read must never return an error other than ErrNotFound and
				// must never return a torn value.
				v, err := db.Get([]byte(fmt.Sprintf("key%06d", i)))
				if err != nil && !errors.Is(err, strata.ErrNotFound) {
					done <- err
					return
				}
				if err == nil && string(v) != fmt.Sprintf("v%06d", i) {
					done <- fmt.Errorf("key %d has value %q", i, v)
					return
				}
			}
			done <- nil
		}()
	}
	for i := 0; i < n; i++ {
		if err := db.Put([]byte(fmt.Sprintf("key%06d", i)), []byte(fmt.Sprintf("v%06d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for r := 0; r < 4; r++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
