# strata — design

Phase A0 deliverable. This document describes the formats and invariants that
the code in phase A1 implements. If the code and this document ever disagree,
one of them is a bug.

---

## 1. What the engine is

A log-structured merge tree. Writes are absorbed by an in-memory sorted
structure and made durable by a sequential log; when the in-memory structure
gets large it is written out as an immutable sorted file. Reads consult memory
first, then files, newest to oldest.

Nothing is ever modified in place. An overwrite is a new record. A delete is a
new record. This is the single decision everything else follows from:

- It makes writes cheap — one sequential log append and one in-memory insert.
- It makes reads expensive — a key might be in any of several files, so a
  lookup may touch many of them. Bloom filters and compaction exist to claw
  that cost back.
- It makes concurrency cheap — nothing a reader is looking at can be mutated,
  so readers need no locks.
- It makes space expensive — superseded versions and tombstones occupy disk
  until compaction removes them.

An LSM tree is the right shape when writes dominate reads and when writes are
random in key space but you want them sequential on the device. It is the wrong
shape when you need bounded read latency without tuning, or when the working
set is read-mostly and updates are rare — a B-tree beats it comfortably there.

---

## 2. Internal keys

Every user key is stored wrapped:

```
internal_key = user_key || trailer
trailer      = uint64le( (sequence << 8) | kind )
kind         = 0 (delete)  |  1 (set)
```

56 bits of sequence, 8 bits of kind.

### Sort order

```
user key   ASCENDING
trailer    DESCENDING     ← newest version of a key sorts FIRST
```

This ordering is load-bearing for the entire engine. It is what makes a point
lookup a single seek:

> To read key `k` at snapshot `s`, seek for the internal key `(k, s, KindSeek)`.
> Every version of `k` newer than `s` has a larger trailer, so it sorts *before*
> the seek key and is skipped. The first entry the seek lands on is therefore
> the newest version of `k` visible at `s`.

`KindSeek` is defined to equal the largest valid kind (`KindSet`). If it were
smaller, a `KindSet` record at exactly sequence `s` would sort *before* the seek
key and be skipped — the snapshot would fail to see a write it should see.

### Consequences

- Deleting a key writes a tombstone, which occupies space and must be read
  through. A lookup that finds a tombstone must stop and report "absent"; if it
  fell through to older files, deleted keys would be resurrected.
- Sequence numbers are globally unique and monotonic. Two records with the same
  internal key is a bug (the memtable panics on it) because it means the same
  sequence number was handed out twice.

---

## 3. On-disk files

```
CURRENT             text; names the live manifest
MANIFEST-000007     log of version edits
000004.log          write-ahead log
000009.sst          immutable table
```

All numbers come from one monotonically increasing counter stored in the
manifest, so "is this file still referenced?" is answerable after a crash
without reading file contents.

### 3.1 Write-ahead log

Fixed 32 KiB blocks. Logical records are fragmented to fit:

```
+---------+--------+------+------------------+
| crc (4) | len(2) | type | payload          |
+---------+--------+------+------------------+

type: FULL | FIRST | MIDDLE | LAST
```

**Why blocks.** They bound the blast radius of corruption. Without them, one
bad byte early in the file makes everything after it unparseable, because a
reader has no way to resynchronise. With them, a reader can skip to the next
32 KiB boundary.

**Why the checksum covers the type byte.** A corrupted type field could
otherwise turn a `FULL` record into a `FIRST` record and silently swallow the
record that follows it.

**Why checksums are masked.** CRC32 is linear: the CRC of a buffer that already
ends in its own CRC is a fixed, content-independent value. Since real systems
checksum buffers that already contain checksums, the stored value is rotated
and offset first:

```
masked = rotate_right(crc, 15) + 0xa282ead8
```

**Tail handling.** A truncated or checksum-failing record at the end of the log
is treated as end-of-log, not as an error. Those bytes were never a complete
record, so the batch they belonged to was never acknowledged. What is *not*
acceptable is dropping records before the damage — so the reader stops at the
tear rather than trying to scan past it.

### 3.2 Batches

One batch is one log record, which is where write atomicity comes from — the
framing either reconstructs a record whole or rejects it.

```
sequence (8 LE) | count (4 LE) | entry*
entry := kind(1) uvarint(len(key)) key [uvarint(len(value)) value]
```

Entry *i* of a batch gets sequence `base + i`.

The value is **omitted entirely** for a tombstone rather than stored as an empty
string. Storing an empty value would make an empty string and a deletion
indistinguishable, and an engine that cannot tell those apart cannot implement
`Delete`.

### 3.3 Tables (SSTables)

```
+------------------+
| data block 0     |  sorted internal keys, prefix-compressed
| data block 1     |
| ...              |
+------------------+
| filter block     |  bloom filter over the file's USER keys
+------------------+
| index block      |  one entry per data block: last key -> handle
+------------------+
| footer (48 B)    |  two handles, then an 8-byte magic
+------------------+
```

The footer is at the **end** so the writer never seeks backwards: a table can be
produced in one forward pass with bounded memory.

**Block format:**

```
entry*
restart_offset*   (uint32 each)
num_restarts      (uint32)

entry := uvarint(shared) uvarint(unshared) uvarint(value_len)
         key_suffix[unshared] value[value_len]
```

Adjacent keys share long prefixes, so each entry stores only the bytes that
differ from its predecessor. The cost is that entries are no longer
independently decodable, which would make binary search inside a block
impossible. **Restart points** buy it back: every 16 entries stores its key in
full, and the offsets of those entries are recorded at the end of the block. A
seek binary-searches the restart array and then replays at most 16 entries.

Each block carries a trailer: one compression byte (only `none` today) and a
masked CRC32C, verified on **every** read. Per-block rather than per-file,
because a reader touches one block at a time.

**Bloom filter.** 10 bits per key, ~1% false positives, `k = 7` probes derived
from a single hash by double hashing. Filters on *user* keys, not internal keys
— filtering on internal keys would only answer "was this exact version
written", which is never the question. On a damaged filter the reader fails
*open* (says "maybe present"): a false positive costs a wasted block read, a
false negative would be data loss.

Known simplification: one filter for the whole file, rather than LevelDB's
per-2KB filter partitions. Also, index entries key blocks by their last key
rather than by the shortest separating string. Neither affects correctness.

### 3.4 Manifest

The set of live files *is* the state of the database, and it changes on every
flush and compaction. Those changes must be atomic — no reader may see a
compaction's output before its inputs are gone, or after.

Solved with a log. Each change is a `VersionEdit` appended to the MANIFEST,
using the same block framing as the WAL. State is the edits replayed from the
start.

Fields are **tagged**, not positional, so an older binary skips what it does not
understand rather than misparsing the remainder.

A damaged manifest is refused outright. Unlike the WAL, ignoring damage here
would mean opening a database whose file set is wrong and answering reads with
missing data.

### 3.5 CURRENT

Which manifest is live is itself a piece of state that must change atomically.
`CURRENT` is written by creating `CURRENT.tmp`, fsyncing it, `rename(2)`-ing it
into place, then fsyncing the directory. `rename` is atomic on POSIX, so there
is no instant at which `CURRENT` is half-written.

---

## 4. Durability rules

These are the rules a crash test is checking. Each one is a real bug if broken.

1. **Log before memtable.** A write is appended to the WAL before it is applied
   in memory. A failed append therefore leaves no trace, which is what makes
   retrying a failed write safe.
2. **fsync the file, then fsync the directory.** Syncing a new file makes its
   contents durable; the directory entry that gives it a name is separate
   metadata with its own writeback. Skipping the directory fsync can leave a
   durable file that no longer appears in any directory.
3. **Bytes before names.** A table is written and fsynced before the manifest
   edit that references it. A crash in between leaves an unreferenced file,
   which cleanup deletes. The reverse ordering produces a database that opens
   and then fails on a read.
4. **Durable before visible.** A version edit is appended and fsynced to the
   manifest before the new version is installed in memory.
5. **New manifest before CURRENT.** The new manifest is fully durable before
   `CURRENT` points at it.
6. **Old log deleted last.** A WAL file is removed only after the table
   containing its contents is referenced by a durable manifest edit.

### What `Sync` actually promises

| `Sync` | Survives process death | Survives power loss |
|--------|------------------------|---------------------|
| true   | yes                    | yes                 |
| false  | yes                    | **no**              |

With `Sync: false` the bytes are in the kernel page cache, which outlives the
process but not the machine. Most engines default to false and most users do not
realise which guarantee they are getting.

---

## 5. Recovery

Runs on **every** open, including a clean one, because `Close` deliberately does
not flush the memtable.

That is not laziness. Recovery code that only runs after a crash is recovery
code that is never tested, and an engine whose recovery path executes once a
year in production is an engine that loses data. Here, every reopen replays the
log.

```
1. read CURRENT                    -> manifest number
2. replay the manifest             -> live files, log number, next file number,
                                      last sequence
3. replay every log >= log number  -> memtable, highest sequence seen
4. flush the memtable if non-empty -> a new level-0 table
5. allocate a new log and a new manifest
6. write the new manifest (a full snapshot of the version)
7. fsync it, then point CURRENT at it
8. open the new log
9. delete: logs below the new log number, manifests other than the live one,
   tables no version references, leftover temp files
```

Step 6 writes a fresh snapshot manifest rather than appending, so replay is one
self-contained edit instead of every flush since the database was created.

---

## 6. Concurrency

One writer at a time (serialised by the DB mutex); any number of readers, none
of which take a lock while doing I/O.

This works because of two properties:

- **The memtable is append-only.** The skiplist supports one writer publishing
  nodes by storing a single pointer per level; a racing reader either sees a
  node or does not, and neither outcome is torn. A rebalancing tree has no such
  property — rotations move nodes readers are standing on.
- **Versions are immutable.** A reader takes the current `*version` under the
  mutex and releases it immediately. The version it holds can never change, so
  it can do disk I/O for as long as it likes.

A `Get` captures `(memtable, immutable memtable, version, sequence)` under the
lock and then does everything else outside it.

---

## 7. What phase A1 deliberately does not do

| | |
|---|---|
| **A2** | block cache; merging iterator across levels; public snapshots |
| **A3** | compaction — today every flush lands in L0 and stays there, so read amplification grows without bound |
| **A4** | transactions; optimistic conflict detection; documented isolation level |
| **A5** | deterministic simulation testing with injected faults |
| **A6** | YCSB benchmarks against Pebble, BadgerDB, SQLite |

Other known gaps, all of them deliberate rather than overlooked:

- The flush is **synchronous and holds the write lock**. That is a real write
  stall, just a crude one.
- Table handles are cached forever and never evicted, so a database with many
  files eventually exhausts file descriptors.
- Data blocks are re-read and re-parsed on every access — no block cache. This
  is slow, and it makes the effect of the bloom filter very visible in a
  profile, which is useful right now.
- There are no range scans in the public API, only point lookups. The iterators
  exist; nothing exposes them yet.

---

## 8. Invariants

Worth stating explicitly, because these are what a fault-injection harness
should assert in phase A5.

1. Every acknowledged write is readable after recovery.
2. Recovery never loses a record that precedes a surviving record — the log
   loses a clean *suffix* or nothing.
3. A read at sequence `s` never observes a write with sequence `> s`.
4. A tombstone always shadows every older version of its key.
5. Within one level ≥ 1, file key ranges never overlap. (Vacuously true until
   compaction exists.)
6. Every file the live version references exists on disk; every `.sst` on disk
   that no version references is garbage and may be deleted.
7. Sequence numbers are unique and monotonically increasing.
8. A block whose checksum fails is never returned as data.
