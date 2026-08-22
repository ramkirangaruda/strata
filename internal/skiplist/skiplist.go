// Package skiplist implements the ordered in-memory structure behind strata's
// memtable.
//
// Why a skiplist and not a balanced tree: the concurrency model. strata allows
// exactly one writer at a time (serialised by the DB mutex) but any number of
// concurrent readers, with no reader ever taking a lock. A skiplist supports
// that directly -- an insert publishes a node by storing a single pointer at
// each level, and a reader that races with the insert either sees the new node
// or does not. Neither outcome is corrupt. A rebalancing tree has no such
// property: rotations move nodes readers are standing on.
//
// The structure is append-only. Overwrites and deletions are new entries with
// higher sequence numbers, not mutations, which is what lets readers hold a
// stable snapshot without copying anything.
package skiplist

import (
	"math/rand"
	"sync/atomic"

	"github.com/ramkirangaruda/strata/internal/keys"
)

const (
	// maxHeight caps the tower. 12 levels with p=1/4 addresses roughly 4^12 =
	// 16M entries before the top level stops helping, which is far above the
	// number of entries any sane memtable holds before it is flushed.
	maxHeight = 12

	// branching is 1/p: a node gets another level with probability 1/4.
	branching = 4
)

type node struct {
	key   []byte
	value []byte
	// next has exactly the node's height. Entries are atomic because readers
	// traverse them without holding the writer's lock.
	next []atomic.Pointer[node]
}

// List is an ordered set of internal keys. It is safe for one writer and any
// number of concurrent readers. It is NOT safe for concurrent writers; the DB
// serialises them.
type List struct {
	head   *node
	height atomic.Int32 // current number of levels in use, >= 1

	rnd *rand.Rand

	// memUsage is an approximation of the bytes this list is keeping alive. It
	// drives memtable rotation, so it deliberately counts pointer overhead and
	// not just payload -- undercounting here is how an engine OOMs while
	// believing its memtable is small.
	memUsage atomic.Int64
	count    atomic.Int64
}

// New returns an empty list. seed makes tower heights deterministic, which the
// simulation tests in phase A5 depend on: a bug that only reproduces at one
// particular skiplist shape is useless if the shape is not reproducible.
func New(seed int64) *List {
	l := &List{
		head: &node{next: make([]atomic.Pointer[node], maxHeight)},
		rnd:  rand.New(rand.NewSource(seed)),
	}
	l.height.Store(1)
	return l
}

func (l *List) randomHeight() int {
	h := 1
	for h < maxHeight && l.rnd.Intn(branching) == 0 {
		h++
	}
	return h
}

// MemUsage returns the approximate retained size in bytes.
func (l *List) MemUsage() int64 { return l.memUsage.Load() }

// Len returns the number of entries, including tombstones and superseded
// versions. It is not the number of distinct user keys.
func (l *List) Len() int64 { return l.count.Load() }

// Insert adds an entry. The caller must hold the write lock, and ikey must not
// already be present -- sequence numbers are unique per entry, so a duplicate
// internal key means the caller assigned the same sequence number twice, which
// is a bug worth panicking over rather than silently tolerating.
func (l *List) Insert(ikey, value []byte) {
	var prev [maxHeight + 1]*node
	x := l.findSpliceAll(ikey, &prev)
	if x != nil && keys.Compare(x.key, ikey) == 0 {
		panic("strata/skiplist: duplicate internal key (duplicate sequence number?)")
	}

	h := l.randomHeight()
	if cur := int(l.height.Load()); h > cur {
		// Levels above the old height start out pointing at nothing, so the
		// head is the correct predecessor for all of them.
		for i := cur; i < h; i++ {
			prev[i] = l.head
		}
		// Publishing the new height before the node is linked is safe: a reader
		// that sees the taller list finds nil at the new levels and descends.
		l.height.Store(int32(h))
	}

	n := &node{key: ikey, value: value, next: make([]atomic.Pointer[node], h)}

	// Link bottom-up. Level 0 is the level that defines membership: once
	// prev[0].next[0] points at n, the entry exists as far as any reader is
	// concerned. Higher levels are pure acceleration, so a reader that races
	// ahead of them still finds n by descending to level 0.
	for i := 0; i < h; i++ {
		n.next[i].Store(prev[i].next[i].Load())
		prev[i].next[i].Store(n)
	}

	// 8 bytes of pointer per level, plus the node header, plus the payload.
	l.memUsage.Add(int64(len(ikey) + len(value) + h*8 + 48))
	l.count.Add(1)
}

// findSpliceAll locates, for every level, the last node whose key sorts before
// ikey. It returns the first node at level 0 that is >= ikey, or nil.
func (l *List) findSpliceAll(ikey []byte, prev *[maxHeight + 1]*node) *node {
	x := l.head
	h := int(l.height.Load())
	for i := h - 1; i >= 0; i-- {
		for {
			next := x.next[i].Load()
			if next == nil || keys.Compare(next.key, ikey) >= 0 {
				break
			}
			x = next
		}
		prev[i] = x
	}
	return x.next[0].Load()
}

// seekGE returns the first node whose key is >= ikey, or nil if none is.
func (l *List) seekGE(ikey []byte) *node {
	x := l.head
	for i := int(l.height.Load()) - 1; i >= 0; i-- {
		for {
			next := x.next[i].Load()
			if next == nil || keys.Compare(next.key, ikey) >= 0 {
				break
			}
			x = next
		}
	}
	return x.next[0].Load()
}

// Iterator walks the list in internal-key order. It is a read-only view and is
// safe to use while a writer is inserting; entries added after the iterator
// passed a position simply will not be seen.
type Iterator struct {
	list *List
	n    *node
}

// NewIterator returns an iterator positioned before the first entry.
func (l *List) NewIterator() *Iterator { return &Iterator{list: l} }

// Valid reports whether the iterator is positioned at an entry.
func (i *Iterator) Valid() bool { return i.n != nil }

// Key returns the internal key at the current position. The bytes alias the
// list's memory and must not be modified.
func (i *Iterator) Key() []byte { return i.n.key }

// Value returns the value at the current position, aliasing list memory.
func (i *Iterator) Value() []byte { return i.n.value }

// SeekGE positions the iterator at the first entry >= ikey.
func (i *Iterator) SeekGE(ikey []byte) { i.n = i.list.seekGE(ikey) }

// First positions the iterator at the smallest entry.
func (i *Iterator) First() { i.n = i.list.head.next[0].Load() }

// Next advances the iterator.
func (i *Iterator) Next() {
	if i.n != nil {
		i.n = i.n.next[0].Load()
	}
}
