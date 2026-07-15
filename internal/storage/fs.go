package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// fsStorage is the local-mode backend: one file per key under a single
// root directory (<StateRoot>/blobs). Slash-separated key segments map to
// nested directories, created on Put. Local mode has no object store and
// no second executor, so the on-disk copy IS the durable copy — there is
// nothing to snapshot it to.
type fsStorage struct {
	root string
}

// newFSStorage roots a filesystem backend at dir. Construction touches no
// disk (the root is created lazily on the first Put), so it can't fail.
func newFSStorage(dir string) *fsStorage {
	return &fsStorage{root: dir}
}

// NewFS is the exported constructor for the filesystem backend, so a caller
// (a handler or syncer test) can build a real, disk-backed Storage over a
// temp dir without going through New's mode selection. Production still
// reaches the filesystem backend only via New in local mode.
func NewFS(root string) Storage {
	return newFSStorage(root)
}

func (s *fsStorage) Put(ctx context.Context, key string, r io.Reader) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("storage: create blob dir: %w", err)
	}
	// Write to a sibling temp file and rename into place: a concurrent
	// reader never observes a half-written blob, and a crash mid-write
	// can't truncate an existing one. CreateTemp in the target's own
	// directory keeps the rename on one filesystem, so it stays atomic.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return fmt.Errorf("storage: create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename below succeeds
	if _, err := io.Copy(tmp, &ctxReader{ctx: ctx, r: r}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: flush temp blob: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("storage: commit blob: %w", err)
	}
	return nil
}

func (s *fsStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

func (s *fsStorage) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	if length < 0 {
		return f, nil
	}
	// LimitReader caps the body at length; the file handle still needs
	// closing, so wrap the pair so the caller's single Close reaches the
	// underlying *os.File.
	return &limitedFile{Reader: io.LimitReader(f, length), f: f}, nil
}

// limitedFile pairs a length-bounded reader with the file handle it reads
// from so GetRange can return an io.ReadCloser whose Close reaches the
// underlying descriptor rather than leaking it.
type limitedFile struct {
	io.Reader
	f *os.File
}

func (l *limitedFile) Close() error { return l.f.Close() }

func (s *fsStorage) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	// The prefix names a subtree under the root; walk it and re-derive each
	// file's key by stripping the root back off. A prefix that resolves to a
	// missing dir is an empty listing, matching the object store's "no keys
	// under this prefix" (not an error). Reuse path()'s containment rules by
	// resolving the prefix as a key would be — but the prefix may name a
	// directory rather than a single object, so resolve against the root
	// directly here while keeping the ".." rejection.
	for _, seg := range strings.Split(prefix, "/") {
		if seg == ".." {
			return nil, fmt.Errorf("storage: invalid prefix %q (..)", prefix)
		}
	}
	root := filepath.Clean(s.root)
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(prefix, "/")), "/")
	start := root
	if rel != "" {
		start = filepath.Join(root, filepath.FromSlash(rel))
	}
	if !strings.HasPrefix(start, root) {
		return nil, fmt.Errorf("storage: invalid prefix %q", prefix)
	}
	var out []ObjectInfo
	err := filepath.WalkDir(start, func(full string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A missing start dir is "no objects", not a failure.
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil // raced deletion during the walk
			}
			return err
		}
		keyRel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		out = append(out, ObjectInfo{
			Key:     filepath.ToSlash(keyRel),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *fsStorage) Delete(ctx context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *fsStorage) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	p, err := s.path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: info.Size(), ModTime: info.ModTime()}, nil
}

func (s *fsStorage) Exists(ctx context.Context, key string) (bool, error) {
	p, err := s.path(key)
	if err != nil {
		return false, err
	}
	switch _, err := os.Stat(p); {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// path maps an opaque slash-separated key to an absolute filesystem path
// under the root. Keys are forward-slash relative paths; a ".." segment
// anywhere is rejected outright rather than silently clamped, so a
// malformed key (keys are trusted, caller-namespaced strings — a ".." is a
// bug) fails loudly instead of landing somewhere surprising. The empty /
// root-resolving cases are rejected too, and the prefix check is a
// belt-and-suspenders guard on top of the segment scan.
func (s *fsStorage) path(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("storage: empty key")
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return "", fmt.Errorf("storage: invalid key %q (..)", key)
		}
	}
	// Clean to collapse "." and redundant slashes, then strip the leading
	// separator so the remainder joins strictly UNDER the root as a relative
	// path. (filepath.Join would not actually discard the root for an
	// absolute second arg — Go cleans the concatenation rather than treating
	// it like Python's os.path.join — but joining a relative remainder makes
	// the containment obvious to a reader.)
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(key, "/")), "/")
	if rel == "" {
		return "", fmt.Errorf("storage: invalid key %q", key)
	}
	p := filepath.Join(s.root, filepath.FromSlash(rel))
	if root := filepath.Clean(s.root); !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("storage: invalid key %q", key)
	}
	return p, nil
}

// ctxReader aborts a streaming copy as soon as ctx is cancelled, checked
// before each underlying Read. It keeps Put honoring the context across a
// multi-gigabyte tarball without the copy loop having to special-case it.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}
