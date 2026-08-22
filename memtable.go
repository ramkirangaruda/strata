package strata

import (
	"github.com/ramkirangaruda/strata/internal/keys"
	"github.com/ramkirangaruda/strata/internal/skiplist"
)

// memtable is the writable head of the LSM tree: a sorted in-memory structure
// that absorbs writes until it is large enough to be worth turning into a file.
//
// It is append-only. An overwrite or a delete inserts a NEW entry with a higher
// sequence number rather than modifying or removing anything, so the memtable
// accumulates versions and tombstones exactly the way the on-disk levels do.
// That uniformity is not an accident -- it is what lets one merging iterator
// read across memory and disk without caring which is which.
type memtable struct {
	list *skiplist.List
}

func newMemtable(seed int64) *memtable {
	return &memtable{list: skiplist.New(seed)}
}

func (m *memtable) add(seq uint64, kind keys.Kind, ukey, value []byte) {
	// The memtable must OWN its memory, and this is not a stylistic
	// preference -- it is a correctness requirement that is easy to get wrong.
	//
	// Entries here outlive the buffers they arrive in. The log reader hands
	// out a record buffer that it reuses on the very next call, so a memtable
	// that stored that slice would find every one of its values silently
	// rewritten to the contents of the last record replayed. (That is not
	// hypothetical: this engine had exactly that bug, and it presented as
	// every key in the database returning the value of the final write after
	// a reopen -- reads that were individually plausible and collectively
	// nonsense, which is the worst way for a storage bug to present.)
	//
	// keys.Encode already builds a fresh key. The value has to be copied
	// explicitly.
	m.list.Insert(keys.Encode(nil, ukey, seq, kind), append([]byte(nil), value...))
}

// get returns the newest version of ukey visible at snapshot.
//
// found=true with kind==KindDelete means "this key is deleted as of this
// snapshot" -- a definitive answer that must stop the search. If the caller
// treated a tombstone as a miss and fell through to the tables below, deleted
// keys would come back from the dead.
func (m *memtable) get(ukey []byte, snapshot uint64) (value []byte, kind keys.Kind, found bool) {
	it := m.list.NewIterator()
	it.SeekGE(keys.Encode(nil, ukey, snapshot, keys.KindSeek))
	if !it.Valid() {
		return nil, 0, false
	}
	if string(keys.UserKey(it.Key())) != string(ukey) {
		return nil, 0, false
	}
	k := keys.KindOf(it.Key())
	if k == keys.KindDelete {
		return nil, k, true
	}
	return it.Value(), k, true
}

func (m *memtable) memUsage() int64 { return m.list.MemUsage() }
func (m *memtable) empty() bool     { return m.list.Len() == 0 }

func (m *memtable) iter() *skiplist.Iterator { return m.list.NewIterator() }
