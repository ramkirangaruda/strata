// Command crashwriter writes keys to a strata database forever, printing the
// index of every write the engine has ACKNOWLEDGED.
//
// It exists to be killed. The durability test starts it, lets it run, sends
// SIGKILL, and then asserts that every index it printed is readable after
// recovery. Nothing about that can be faked with an in-process test: only a
// real kill -9 leaves the process no opportunity to flush, close, or clean up,
// which is exactly the situation the write-ahead log exists for.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/ramkirangaruda/strata"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: crashwriter <dir> <valuesize>")
		os.Exit(2)
	}
	dir := os.Args[1]
	valueSize, _ := strconv.Atoi(os.Args[2])

	db, err := strata.Open(dir, strata.Options{
		// Sync on every write: an acknowledged write must survive the machine
		// dying, not merely the process dying. Without this the test would
		// pass trivially, because the page cache outlives a killed process.
		Sync:         true,
		MemtableSize: 256 << 10, // small, so the run crosses several flushes
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	value := make([]byte, valueSize)
	for i := range value {
		value[i] = byte('a' + i%26)
	}

	for i := 0; ; i++ {
		if err := db.Put([]byte(fmt.Sprintf("key%08d", i)), value); err != nil {
			fmt.Fprintln(os.Stderr, "put:", err)
			os.Exit(1)
		}
		// Unbuffered, so the parent never credits us with an ack we did not
		// actually complete. A buffered writer here would invalidate the test.
		os.Stdout.WriteString(strconv.Itoa(i) + "\n")
	}
}
