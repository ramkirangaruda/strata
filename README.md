# strata

An LSM-tree storage engine in Go, built from scratch — write-ahead log, sorted
string tables with bloom filters, MVCC through sequence numbers, and crash
recovery.

**Status: phase A1 of 6.** The write path, durability and recovery are done and
tested. Compaction, a block cache, transactions and deterministic simulation
testing are not built yet — see [`docs/DESIGN.md` §7](docs/DESIGN.md) for the
list of what is deliberately missing.

```bash
go test ./...          # all tests
go test -race ./...    # the concurrency and crash tests want this
go test -v -run Kill   # the kill -9 durability test, on its own
```

## Reading order

The packages depend on each other in roughly this order, and reading them in it
means never hitting a concept before the thing that defines it.

| # | File | ~lines | What to take from it |
|---|------|--------|----------------------|
| 1 | [`docs/DESIGN.md`](docs/DESIGN.md) | — | Read this first. Everything else is an implementation of it. |
| 2 | `internal/keys/keys.go` | 190 | The sort order the whole engine rests on. If only one file makes sense to you, make it this one. |
| 3 | `internal/skiplist/skiplist.go` | 200 | Why one-writer-many-readers needs no locks. |
| 4 | `internal/wal/wal.go` | 290 | Block framing, checksum masking, and what happens to a record torn by a crash. |
| 5 | `internal/sst/format.go` | 130 | The table layout, and why the footer is at the end. |
| 6 | `internal/sst/block.go` | 300 | Prefix compression, and the restart points that make binary search possible again. |
| 7 | `internal/sst/bloom.go` | 140 | Double hashing, and why the filter must fail *open*. |
| 8 | `internal/sst/writer.go` | 200 | One forward pass, bounded memory. |
| 9 | `internal/sst/reader.go` | 250 | Footer → index → one data block. Two-level iteration. |
| 10 | `internal/manifest/manifest.go` | 230 | Why the file set is itself a log. |
| 11 | `files.go` | 150 | Atomic `CURRENT` updates, and the directory fsync everyone forgets. |
| 12 | `version.go` | 130 | Immutable versions, and why L0 is special. |
| 13 | `memtable.go`, `batch.go` | 200 | Ownership rules, and why a tombstone has no value field. |
| 14 | `db.go` | 550 | Where all of it is assembled. Read `recover` and `makeRoomForWrite` slowly. |

## Questions worth being able to answer

Not a quiz — these are the things an interviewer probes, and each one has an
answer somewhere in the code.

**Keys and ordering**
1. Why do sequence numbers sort *descending* within a user key?
2. Why is `KindSeek` defined as the largest valid kind rather than the smallest?
3. What breaks if a delete and an empty value are encoded the same way?

**Durability**
4. Why does the WAL checksum cover the type byte and not just the payload?
5. Why are checksums masked before being stored?
6. Why is a torn record at the tail of the WAL *not* an error, while a torn
   record in the MANIFEST *is*?
7. What exactly does `Sync: false` still guarantee?
8. Why fsync the directory as well as the file?

**Tables**
9. Why does prefix compression require restart points to stay useful?
10. Why does the bloom filter index user keys rather than internal keys?
11. Why must a corrupt bloom filter answer "maybe present"?
12. Why is the footer at the end of the file?

**The tree**
13. Why must a level-0 lookup check *every* overlapping file, when levels below
    it need only one?
14. What read amplification does L0 cost, and why does that make compaction
    non-optional?
15. Why does `Close` deliberately not flush the memtable?

**Concurrency**
16. Why can readers traverse the skiplist with no lock at all?
17. Why does `Get` capture the version pointer under the mutex and then release
    it before doing any I/O?

## A bug this code already had

Worth knowing about, because it is representative of the whole genre.

The WAL reader reuses one buffer across calls to `Next` — documented, and
correct. Recovery decoded a batch out of that buffer and inserted the resulting
value slices straight into the memtable, which outlives the buffer.

Every value in the database silently became the value of the last record
replayed. Individually each read looked plausible. Collectively they were
nonsense. Nothing crashed, no checksum failed, and unit tests on every
individual package passed — only a test that closed and reopened the database
caught it.

The fix is one `append([]byte(nil), value...)` in `memtable.add`, and the
comment there explains the ownership rule that makes it necessary. This is the
class of bug phase A5's fault injection exists to find in bulk.

## Next

Phase A2: a block cache, a merging iterator across memtable and levels, and
snapshots as a public API.
