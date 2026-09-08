# Contributing

This is primarily a solo learning project (see `docs/DESIGN.md` for the
build plan across phases A1-A6), but issues and PRs are welcome, especially
if you find a case where the code and `docs/DESIGN.md` disagree - per that
doc's own rule, that's a bug in one of them.

## Before sending a PR

```bash
make vet
make test
make test-race
```

All three have to pass. `test-race` matters more here than in most repos:
the memtable is a lock-free skiplist with one writer and many concurrent
readers (see `internal/skiplist/skiplist.go`), and that's exactly the kind
of code where a data race is a real bug, not a false positive.

## Scope

Phase A1 (the write path, durability, and recovery) is meant to be small
and correct rather than fast or feature-complete. If you're looking to add
compaction, a block cache, or transactions, check `docs/DESIGN.md` §7 first
- those are scoped as later phases on purpose, so a PR that jumps ahead is
likely to collide with how a later phase is meant to build on this one.

Bug fixes, test coverage, documentation, and the deployment/tooling side of
the repo (`cmd/server`, CI, Docker) are fair game any time.
