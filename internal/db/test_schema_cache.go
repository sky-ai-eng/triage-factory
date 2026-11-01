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
	"sync"

	"github.com/pressly/goose/v3"
	"golang.org/x/mod/modfile"
)

// imageCache shares the images migratedImageAt builds across test binaries.
// Replaying the migration tree is the slow part of every SQLite-backed test
// binary, and the image goose leaves at a version depends on nothing but the
// migrations up to it and the modules that apply them, so binaries share
// images through files under the OS temp directory: the first to need one
// builds it, holding a lock, and the rest read it. What Migrate does after
// goose, and the tenant rows, are written from Go code, so they are never
// part of these files; each process applies them over the restored image.
//
// Each file is keyed by the migrations up to its own version, so a new
// migration adds images without invalidating the ones below it. A nil cache
// reads nothing and writes nothing, and any failure to read, lock or write
// falls back to building in the process, so the cache can only save time.
// Tests-only.
type imageCache struct {
	dir string
	// keys maps each migration version in the tree to the key of the image
	// goose leaves there.
	keys map[int64]string
}

var (
	sharedImageCacheOnce sync.Once
	sharedImageCache     *imageCache
)

// openImageCache returns the cache under the OS temp directory for tree, or
// nil when its keys cannot be computed (see imageCacheKeys).
func openImageCache(treeFS fs.FS, tree goose.Migrations) *imageCache {
	sharedImageCacheOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "tf-test-sqlite-images")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return
		}
		if keys, ok := imageCacheKeys(treeFS, tree); ok {
			sharedImageCache = &imageCache{dir: dir, keys: keys}
		}
	})
	return sharedImageCache
}

// read returns the cached image at version only if a database restored from
// it passes SQLite's integrity check and stands at version, so an unreadable,
// damaged or foreign file is treated as absent and rebuilt over. The
// integrity check reads every page: a damaged page outside goose_db_version
// would pass the version read and fail the first fixture that touched its
// table.
func (c *imageCache) read(version int64) ([]byte, bool) {
	path, ok := c.path(version)
	if !ok {
		return nil, false
	}
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
	var got int64
	if err := template.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&got); err != nil || got != version {
		return nil, false
	}
	return image, true
}

// write stores image as the image at version, through a temporary file
// renamed into place, so a reader never sees a partial image. Best-effort: a
// failure leaves no file, and the next binary builds its own.
func (c *imageCache) write(version int64, image []byte) {
	path, ok := c.path(version)
	if !ok {
		return
	}
	tmp, err := os.CreateTemp(c.dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return
	}
	_, writeErr := tmp.Write(image)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

// lock blocks until this binary holds the cache's build lock, and returns
// the function that releases it. One lock covers every build, so binaries
// that start together wait for one replay from empty and then build what
// they need from the images it left. A lock that cannot be taken returns at
// once, and the caller builds unserialized.
func (c *imageCache) lock() (release func()) {
	if c == nil {
		return func() {}
	}
	f, err := os.OpenFile(filepath.Join(c.dir, "build.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	_ = lockImageCache(f)
	return func() { _ = f.Close() }
}

func (c *imageCache) path(version int64) (string, bool) {
	if c == nil {
		return "", false
	}
	key, ok := c.keys[version]
	if !ok {
		return "", false
	}
	return filepath.Join(c.dir, key+".img"), true
}

// imageCacheModules are the modules whose code decides an image's bytes: the
// SQLite engine and its C runtime, and goose, which writes goose_db_version.
var imageCacheModules = map[string]bool{
	"modernc.org/sqlite":          true,
	"modernc.org/libc":            true,
	"github.com/pressly/goose/v3": true,
}

// imageCacheKeys keys the image at each version of tree by everything it
// depends on: the DSN images are built under, how go.mod pins each module in
// imageCacheModules (sourceImageCachePins), and every migration up to that
// version. ok is false when the pins or a migration cannot be read.
func imageCacheKeys(treeFS fs.FS, tree goose.Migrations) (map[int64]string, bool) {
	pins, ok := sourceImageCachePins()
	if !ok {
		return nil, false
	}
	return imageCacheKeysUnder(pins, treeFS, tree)
}

// sourceImageCachePins reads the module pins from the go.mod beside this
// file's source: a test binary's build info lists no dependencies. ok is false
// when that cannot be read (a -trimpath build, or a binary run away from its
// source tree) or imageCachePins refuses it.
func sourceImageCachePins() (string, bool) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	gomod, err := os.ReadFile(filepath.Join(filepath.Dir(self), "..", "..", "go.mod"))
	if err != nil {
		return "", false
	}
	return imageCachePins(gomod)
}

// imageCacheKeysUnder chains the key of each version's image from the one
// below it: pins and the DSN first, then each migration's path and body, so a
// key covers the migrations up to its version and no others.
func imageCacheKeysUnder(pins string, treeFS fs.FS, tree goose.Migrations) (map[int64]string, bool) {
	keys := make(map[int64]string, len(tree))
	link := sha256.Sum256([]byte("dsn " + TestDSNMemory + "\n" + pins))
	for _, m := range tree {
		body, err := fs.ReadFile(treeFS, m.Source)
		if err != nil {
			return nil, false
		}
		h := sha256.New()
		_, _ = h.Write(link[:])
		fmt.Fprintf(h, "file %s %d\n", m.Source, len(body))
		_, _ = h.Write(body)
		copy(link[:], h.Sum(nil))
		keys[m.Version] = hex.EncodeToString(link[:])
	}
	return keys, true
}

// imageCachePins renders, one line per module in imageCacheModules and in a
// fixed order, the version gomod requires and any replacement it names. ok is
// false when gomod does not parse, when it requires one of the modules not at
// all, and when it replaces one with a directory, whose code can change
// without go.mod doing so.
func imageCachePins(gomod []byte) (string, bool) {
	file, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return "", false
	}
	pins := map[string]string{}
	for _, r := range file.Require {
		if imageCacheModules[r.Mod.Path] {
			pins[r.Mod.Path] = r.Mod.Version
		}
	}
	if len(pins) != len(imageCacheModules) {
		return "", false
	}
	for _, r := range file.Replace {
		if !imageCacheModules[r.Old.Path] {
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
