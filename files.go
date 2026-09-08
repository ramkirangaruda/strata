package strata

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// File naming. Every file in a strata directory is identified by a number
// drawn from a single monotonically increasing counter recorded in the
// manifest, which is what makes "is this file still referenced?" answerable
// after a crash without scanning file contents.
//
//	CURRENT           text: the name of the live manifest, newline-terminated
//	MANIFEST-000007   the version-edit log
//	000004.log        a write-ahead log
//	000009.sst        an immutable table

type fileKind int

const (
	fileUnknown fileKind = iota
	fileCurrent
	fileManifest
	fileLog
	fileTable
	fileTemp
)

func currentPath(dir string) string { return filepath.Join(dir, "CURRENT") }
func manifestPath(dir string, n uint64) string {
	return filepath.Join(dir, fmt.Sprintf("MANIFEST-%06d", n))
}
func logPath(dir string, n uint64) string   { return filepath.Join(dir, fmt.Sprintf("%06d.log", n)) }
func tablePath(dir string, n uint64) string { return filepath.Join(dir, fmt.Sprintf("%06d.sst", n)) }
func tempPath(dir string, n uint64) string  { return filepath.Join(dir, fmt.Sprintf("%06d.tmp", n)) }

func parseFileName(name string) (uint64, fileKind) {
	switch {
	case name == "CURRENT":
		return 0, fileCurrent
	case strings.HasPrefix(name, "MANIFEST-"):
		n, err := strconv.ParseUint(name[len("MANIFEST-"):], 10, 64)
		if err != nil {
			return 0, fileUnknown
		}
		return n, fileManifest
	}
	ext := filepath.Ext(name)
	n, err := strconv.ParseUint(strings.TrimSuffix(name, ext), 10, 64)
	if err != nil {
		return 0, fileUnknown
	}
	switch ext {
	case ".log":
		return n, fileLog
	case ".sst":
		return n, fileTable
	case ".tmp":
		return n, fileTemp
	}
	return 0, fileUnknown
}

// syncDir fsyncs a directory.
//
// This is the step everybody forgets. fsync on a newly created file makes its
// CONTENTS durable, but the directory entry that gives the file a name lives
// in the parent directory, and that is a separate piece of metadata with its
// own writeback. Without this call a crash can leave a perfectly durable file
// that no longer appears in any directory -- which, for a table the manifest
// now references, means the database will not open.
//
// Windows has no equivalent of this call, and pretending otherwise is worse
// than admitting it. FlushFileBuffers on a directory handle returns
// ACCESS_DENIED -- Windows simply does not expose directory-entry durability
// as a syncable operation the way POSIX does. Rather than fail every Open on
// Windows for a guarantee the platform cannot give, this is a deliberate
// no-op there. That is a real gap against docs/DESIGN.md section 4, which
// describes POSIX behavior: on Windows a crash at exactly the wrong instant
// could in principle leave a durable file with no durable directory entry.
// Recorded here rather than hidden, since a silently platform-dependent
// durability guarantee is exactly the kind of thing this engine's tests
// exist to catch.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// setCurrent atomically points the database at a manifest file.
//
// Write to a temporary name, fsync it, rename it over CURRENT, then fsync the
// directory. rename(2) is atomic, so there is no window in which CURRENT is
// half-written or names a file that does not exist. Writing CURRENT in place
// would create exactly that window, and a crash inside it leaves a database
// that cannot be opened at all.
func setCurrent(dir string, manifestNum uint64) error {
	tmp := filepath.Join(dir, "CURRENT.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "MANIFEST-%06d\n", manifestNum); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, currentPath(dir)); err != nil {
		return err
	}
	return syncDir(dir)
}

func readCurrent(dir string) (uint64, error) {
	b, err := os.ReadFile(currentPath(dir))
	if err != nil {
		return 0, err
	}
	name := strings.TrimSpace(string(b))
	n, kind := parseFileName(name)
	if kind != fileManifest {
		return 0, fmt.Errorf("strata: CURRENT names %q, which is not a manifest", name)
	}
	return n, nil
}
