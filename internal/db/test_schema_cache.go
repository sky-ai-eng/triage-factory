package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// sharedHeadImage returns the page image of an empty database after the
// whole SQLite migration tree, the image every other test schema is built
// from. Replaying the tree is the slow part of every SQLite-backed test
// binary, and its result depends on nothing but the tree and the modules
// that apply it, so binaries share it through a file under the OS temp
// directory: the first to need it builds it, holding a lock, and the rest
// read it. What Migrate does after goose, and the tenant rows, are written
// from Go code, so they are never part of this file; each process applies
// them over it.
//
// Any failure to read, lock or write the file falls back to building in
// the process, so the cache can only save time. Tests-only.
func sharedHeadImage(head int64) ([]byte, error) {
	migratedImagesMu.Lock()
	image, ok := migratedImages[head]
	migratedImagesMu.Unlock()
	if ok {
		return image, nil
	}

	path, ok := headImageCachePath()
	if !ok {
		return migratedImageAt(head)
	}
	if image, ok := readHeadImage(path, head); ok {
		return image, nil
	}

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return migratedImageAt(head)
	}
	defer func() { _ = lock.Close() }()
	if err := lockImageCache(lock); err != nil {
		return migratedImageAt(head)
	}
	// Another binary may have written it while this one waited.
	if image, ok := readHeadImage(path, head); ok {
		return image, nil
	}

	image, err = migratedImageAt(head)
	if err != nil {
		return nil, err
	}
	writeHeadImage(path, image)
	return image, nil
}

// readHeadImage reads the cached image at path and returns it only if a
// database restored from it passes SQLite's integrity check and stands at
// head, so an unreadable, damaged or foreign file is treated as absent and
// rebuilt over. The integrity check reads every page: a damaged page outside
// goose_db_version would pass the version read and fail the first fixture
// that touched its table. A hit is also cached in the process, where
// migratedImageAt finds it.
func readHeadImage(path string, head int64) ([]byte, bool) {
	image, err := os.ReadFile(path)
	if err != nil || len(image) == 0 {
		return nil, false
	}
	template, err := openTemplate(image)
	if err != nil {
		return nil, false
	}
	defer func() { _ = template.Close() }()
	var integrity string
	if err := template.QueryRow(`PRAGMA integrity_check(1)`).Scan(&integrity); err != nil || integrity != "ok" {
		return nil, false
	}
	var version int64
	if err := template.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil || version != head {
		return nil, false
	}

	migratedImagesMu.Lock()
	defer migratedImagesMu.Unlock()
	migratedImages[head] = image
	return image, true
}

// writeHeadImage writes image to path through a temporary file renamed into
// place, so a reader never sees a partial image. Best-effort: a failure
// leaves no file, and the next binary builds its own.
func writeHeadImage(path string, image []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return
	}
	_, writeErr := tmp.Write(image)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

// headImageCachePath names the cache file for this build's migration tree
// and modules. ok is false when the key cannot be computed, and the caller
// builds in the process instead.
func headImageCachePath() (string, bool) {
	key, ok := headImageCacheKey()
	if !ok {
		return "", false
	}
	dir := filepath.Join(os.TempDir(), "tf-test-sqlite-images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false
	}
	return filepath.Join(dir, "head-"+key+".img"), true
}

// headImageCacheModules are the modules whose code decides the image's
// bytes: the SQLite engine and its C runtime, and goose, which writes
// goose_db_version.
var headImageCacheModules = map[string]bool{
	"modernc.org/sqlite":          true,
	"modernc.org/libc":            true,
	"github.com/pressly/goose/v3": true,
}

// headImageCacheKey hashes everything the head image depends on: the
// embedded SQLite migration tree, the DSN the image is built under, and how
// go.mod pins each module in headImageCacheModules. A test binary's build
// info lists no dependencies, so the pins come from the go.mod beside this
// file's source; ok is false when that cannot be read (a -trimpath build, or
// a binary run away from its source tree) or headImageCachePins refuses it.
func headImageCacheKey() (string, bool) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	gomod, err := os.ReadFile(filepath.Join(filepath.Dir(self), "..", "..", "go.mod"))
	if err != nil {
		return "", false
	}
	pins, ok := headImageCachePins(gomod)
	if !ok {
		return "", false
	}
	h := sha256.New()
	fmt.Fprintf(h, "dsn %s\n%s", TestDSNMemory, pins)
	err = fs.WalkDir(migrationsSQLiteFS, "migrations-sqlite", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(migrationsSQLiteFS, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "file %s %d\n", path, len(body))
		_, _ = h.Write(body)
		return nil
	})
	if err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// headImageCachePins renders, one line per module in headImageCacheModules
// and in a fixed order, the version gomod requires and any replacement it
// names. ok is false when gomod does not parse, when it requires one of the
// modules not at all, and when it replaces one with a directory, whose code
// can change without go.mod doing so.
func headImageCachePins(gomod []byte) (string, bool) {
	file, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return "", false
	}
	pins := map[string]string{}
	for _, r := range file.Require {
		if headImageCacheModules[r.Mod.Path] {
			pins[r.Mod.Path] = r.Mod.Version
		}
	}
	if len(pins) != len(headImageCacheModules) {
		return "", false
	}
	for _, r := range file.Replace {
		if !headImageCacheModules[r.Old.Path] {
			continue
		}
		// modfile gives a directory replacement no version.
		if r.New.Version == "" {
			return "", false
		}
		pins[r.Old.Path] += fmt.Sprintf(" replace %s %s => %s %s", r.Old.Path, r.Old.Version, r.New.Path, r.New.Version)
	}
	mods := make([]string, 0, len(pins))
	for mod := range pins {
		mods = append(mods, mod)
	}
	sort.Strings(mods)
	var b strings.Builder
	for _, mod := range mods {
		fmt.Fprintf(&b, "module %s %s\n", mod, pins[mod])
	}
	return b.String(), true
}
