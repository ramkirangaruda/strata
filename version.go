package strata

import (
	"sort"

	"github.com/ramkirangaruda/strata/internal/keys"
	"github.com/ramkirangaruda/strata/internal/manifest"
)

// NumLevels is the depth of the tree. Phase A1 only ever writes level 0;
// levels 1..6 exist so that compaction in phase A3 has somewhere to put its
// output without changing the manifest format.
const NumLevels = 7

// version is an immutable snapshot of which files are live.
//
// Immutability is what makes lock-free reads possible. A reader takes the
// current *version pointer under the mutex and then releases it; the version
// it holds can never change underneath it, so it can spend as long as it likes
// doing I/O while writers install new versions. Compaction deleting a file
// cannot pull the rug out, because deletion of the underlying file is deferred
// until no version references it.
type version struct {
	files [NumLevels][]*manifest.FileMeta
}

// apply returns a NEW version with the edit applied. The receiver is unchanged
// and remains valid for any reader still holding it.
func (v *version) apply(e *manifest.VersionEdit) *version {
	next := &version{}
	deleted := make(map[uint64]bool, len(e.Deleted))
	for _, d := range e.Deleted {
		deleted[d.Number] = true
	}

	for lvl := 0; lvl < NumLevels; lvl++ {
		for _, f := range v.files[lvl] {
			if !deleted[f.Number] {
				next.files[lvl] = append(next.files[lvl], f)
			}
		}
	}
	for _, n := range e.New {
		if n.Level < 0 || n.Level >= NumLevels {
			continue
		}
		m := n.Meta
		next.files[n.Level] = append(next.files[n.Level], &m)
	}

	next.sortLevels()
	return next
}

func (v *version) sortLevels() {
	// Level 0 is special: its files come straight from memtable flushes, so
	// their key ranges OVERLAP arbitrarily. There is no ordering by key that
	// would let a lookup pick one file, which is why L0 lookups must consult
	// every overlapping file. Sorting newest-first at least means the search
	// finds the newest version and stops.
	sort.SliceStable(v.files[0], func(i, j int) bool {
		return v.files[0][i].Number > v.files[0][j].Number
	})
	// Levels 1 and below are maintained by compaction so that files within a
	// level never overlap. That invariant is what turns a lookup into a binary
	// search over one file per level.
	for lvl := 1; lvl < NumLevels; lvl++ {
		files := v.files[lvl]
		sort.SliceStable(files, func(i, j int) bool {
			return keys.Compare(files[i].Smallest, files[j].Smallest) < 0
		})
	}
}

// snapshotEdit encodes the whole version as a single edit, used when writing a
// fresh manifest at open. Replaying one self-contained edit is cheaper and far
// easier to reason about than replaying a manifest that has accumulated every
// flush since the database was created.
func (v *version) snapshotEdit() *manifest.VersionEdit {
	e := &manifest.VersionEdit{}
	for lvl := 0; lvl < NumLevels; lvl++ {
		for _, f := range v.files[lvl] {
			e.New = append(e.New, manifest.NewFile{Level: lvl, Meta: *f})
		}
	}
	return e
}

// liveFiles returns the numbers of every table any level references.
func (v *version) liveFiles() map[uint64]bool {
	out := make(map[uint64]bool)
	for lvl := 0; lvl < NumLevels; lvl++ {
		for _, f := range v.files[lvl] {
			out[f.Number] = true
		}
	}
	return out
}

// mayContain reports whether ukey falls inside f's key range. Comparing user
// keys rather than internal keys is deliberate: the file's bounds carry
// sequence numbers, and a lookup wants to know whether ANY version of the user
// key could be in this file.
func mayContain(f *manifest.FileMeta, ukey []byte) bool {
	return keys.CompareUser(ukey, keys.UserKey(f.Smallest)) >= 0 &&
		keys.CompareUser(ukey, keys.UserKey(f.Largest)) <= 0
}
