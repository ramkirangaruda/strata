// Command basic is the smallest possible strata program: open a database,
// write a few keys, read one back, delete one, and close. It exists purely
// as the thing to point someone at when they ask "okay, how do I actually
// use this as a library" - cmd/server answers the same question over HTTP,
// this answers it as five lines of Go.
package main

import (
	"errors"
	"fmt"
	"log"

	"github.com/ramkirangaruda/strata"
)

func main() {
	db, err := strata.Open("/tmp/strata-example", strata.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := db.Put([]byte("hello"), []byte("world")); err != nil {
		log.Fatal(err)
	}

	// Batches commit atomically: both keys become visible together, backed
	// by one log record.
	var b strata.Batch
	b.Put([]byte("a"), []byte("1"))
	b.Put([]byte("b"), []byte("2"))
	if err := db.Write(&b); err != nil {
		log.Fatal(err)
	}

	v, err := db.Get([]byte("hello"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("hello = %s\n", v)

	if err := db.Delete([]byte("hello")); err != nil {
		log.Fatal(err)
	}

	_, err = db.Get([]byte("hello"))
	fmt.Printf("after delete: %v\n", errors.Is(err, strata.ErrNotFound))
}
