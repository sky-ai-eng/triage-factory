// Package kbstore owns the object-store key convention for a knowledge base
// (KB) and is the ONLY place those keys are built. It wraps a
// storage.Storage so callers address blobs by (orgID, projectID, filename)
// without ever hand-assembling a key.
//
// Key convention:
//
//	<orgID>/projects/<projectID>/kb/<filename>
//
// orgID is always the first segment, consistent with the existing snapshot
// convention (<orgID>/<blueprint_run_id>/workspace.tar). The KB layout is
// contractually FLAT — one level, matching local mode's on-disk
// knowledge-base/ directory — so a filename with a path separator is not a
// valid KB name and is rejected here rather than silently nesting.
//
// This package is currently orphaned: its former consumers (the projects
// panel, the classifier, project bundle import/export) were removed along
// with the "projects" concept. It is kept deliberately, for a later ticket
// to re-key it onto team-scoped storage and wire it into delegated runs.
package kbstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// ErrInvalidName is returned when a KB filename is empty, contains a path
// separator, or is a traversal segment. Callers that stream a batch of
// files (the syncer) skip an offending file and continue; a single handler
// call surfaces it as a client error.
var ErrInvalidName = errors.New("kbstore: invalid knowledge-base filename")

// ErrNotFound re-exports storage.ErrNotFound so callers branch on the
// kbstore surface without also importing internal/storage just for the
// sentinel.
var ErrNotFound = storage.ErrNotFound

// FileInfo is one KB blob's listing entry: the flat filename (no prefix, no
// directory component), its byte size, and its last-modified time. It is the
// object-store analogue of the os.FileInfo the local-disk lister produces.
type FileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Store is the KB blob seam. It holds no per-project state — every method
// takes (orgID, projectID) so one process-wide Store serves every tenant.
type Store struct {
	blobs storage.Storage
}

// New wraps a storage.Storage as a KB store. Currently unused — see the
// package doc — but a future re-key would build one storage.Storage at boot
// and hand it here once, shared by every KB consumer in the process.
func New(blobs storage.Storage) *Store {
	return &Store{blobs: blobs}
}

// prefix is the key namespace for one project's KB. Always ends in a
// trailing slash so a List over it can't match a sibling project whose id is
// a prefix of this one.
func prefix(orgID, projectID string) string {
	return orgID + "/projects/" + projectID + "/kb/"
}

// key builds the full object key for one KB file after validating the name.
// This is the single choke point for KB key construction.
func key(orgID, projectID, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return prefix(orgID, projectID) + name, nil
}

// ValidateName enforces the flat-layout contract, preserving the
// path-traversal guards the local-disk handler applies: a name must be
// non-empty after trimming, must not be a "." / ".." traversal segment,
// must not contain a path separator, and must not start with a dot (dot
// files are the agent's scratch state, never user-visible KB). Exported so
// the syncer and handlers can pre-check a name before deciding whether to
// skip or reject it.
//
// Both '/' (the object-store key separator, always rejected) AND the OS path
// separator are rejected: on Windows filepath.Base treats '\' as a separator,
// so a name containing '\' that passed here but not sanitizeKnowledgeFilename
// would round-trip inconsistently and leave the object unreachable from the
// per-file endpoints. Rejecting both keeps kbstore in lockstep with the
// local-disk sanitizer on every platform.
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if trimmed == "." || trimmed == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if strings.ContainsRune(trimmed, '/') || strings.ContainsRune(trimmed, filepath.Separator) {
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidName, name)
	}
	if strings.HasPrefix(trimmed, ".") {
		return fmt.Errorf("%w: %q starts with a dot", ErrInvalidName, name)
	}
	return nil
}

// List returns every KB file for the project, flat. Nested keys (which
// should never exist given ValidateName on Put) and dot-prefixed keys are
// filtered so a listing only ever surfaces names the per-file Get/Delete can
// round-trip — the same actionable-set guarantee the local lister makes. A
// project with no KB is an empty slice, not an error.
func (s *Store) List(ctx context.Context, orgID, projectID string) ([]FileInfo, error) {
	pfx := prefix(orgID, projectID)
	objs, err := s.blobs.List(ctx, pfx)
	if err != nil {
		return nil, err
	}
	out := make([]FileInfo, 0, len(objs))
	for _, o := range objs {
		name := strings.TrimPrefix(o.Key, pfx)
		if name == o.Key {
			continue // key didn't carry the prefix — defensive, shouldn't happen
		}
		if ValidateName(name) != nil {
			continue // nested or dot-prefixed — not a flat, user-visible KB file
		}
		out = append(out, FileInfo{Name: name, Size: o.Size, ModTime: o.ModTime})
	}
	return out, nil
}

// Stat returns one KB file's size and mod time, or ErrNotFound. It is a single
// HEAD on the exact key — the serve handler calls it on every ranged GET (video
// scrubbing), so it must not list the whole project prefix per request.
func (s *Store) Stat(ctx context.Context, orgID, projectID, name string) (FileInfo, error) {
	k, err := key(orgID, projectID, name)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := s.blobs.Stat(ctx, k)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Name: name, Size: info.Size, ModTime: info.ModTime}, nil
}

// Get opens a KB file for streaming reads. A missing file returns
// ErrNotFound.
func (s *Store) Get(ctx context.Context, orgID, projectID, name string) (io.ReadCloser, error) {
	k, err := key(orgID, projectID, name)
	if err != nil {
		return nil, err
	}
	return s.blobs.Get(ctx, k)
}

// GetRange opens a byte range of a KB file. See storage.Storage.GetRange for
// the offset/length contract (length < 0 is "to end").
func (s *Store) GetRange(ctx context.Context, orgID, projectID, name string, offset, length int64) (io.ReadCloser, error) {
	k, err := key(orgID, projectID, name)
	if err != nil {
		return nil, err
	}
	return s.blobs.GetRange(ctx, k, offset, length)
}

// Put writes (overwriting) a KB file, streaming r to the store.
func (s *Store) Put(ctx context.Context, orgID, projectID, name string, r io.Reader) error {
	k, err := key(orgID, projectID, name)
	if err != nil {
		return err
	}
	return s.blobs.Put(ctx, k, r)
}

// Exists reports whether a KB file is present — used by the upload handler to
// preserve the local mode's reject-on-conflict semantics.
func (s *Store) Exists(ctx context.Context, orgID, projectID, name string) (bool, error) {
	k, err := key(orgID, projectID, name)
	if err != nil {
		return false, err
	}
	return s.blobs.Exists(ctx, k)
}

// Delete removes a KB file. Deleting a missing file is not an error.
func (s *Store) Delete(ctx context.Context, orgID, projectID, name string) error {
	k, err := key(orgID, projectID, name)
	if err != nil {
		return err
	}
	return s.blobs.Delete(ctx, k)
}

// DeletePrefix removes every KB blob for a project — the project-deletion
// cleanup. Best-effort per key: it deletes what it can and returns the first
// error so the caller can surface a cleanup warning without leaving the rest
// of the project's KB behind.
func (s *Store) DeletePrefix(ctx context.Context, orgID, projectID string) error {
	pfx := prefix(orgID, projectID)
	objs, err := s.blobs.List(ctx, pfx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, o := range objs {
		if delErr := s.blobs.Delete(ctx, o.Key); delErr != nil && firstErr == nil {
			firstErr = delErr
		}
	}
	return firstErr
}

// Diff is the by-name+size comparison of a local flat directory against the
// project's KB prefix in the store. Each field names what an action in one
// direction would touch, so materialize (store→disk) reads Pull+RemoveLocal
// and reconcile (disk→store) reads Push+RemoveRemote. A file present on both
// sides with a differing size lands in BOTH Pull and Push; the two directions
// run at different points in a turn's lifecycle, so that is not a conflict.
type Diff struct {
	// Pull: in the store but missing locally or a different size.
	Pull []FileInfo
	// RemoveLocal: on disk but absent from the store.
	RemoveLocal []string
	// Push: on disk but missing from the store or a different size.
	Push []string
	// RemoveRemote: in the store but absent on disk.
	RemoveRemote []string
}

// DiffManifest compares the flat contents of localDir against the project's
// KB in the store, by name+size. Directories, symlinks, and dot-prefixed /
// invalid names on disk are skipped (they are never KB files), matching the
// local lister. A missing localDir is treated as empty. Size, not content, is
// the discriminator — the incremental syncer does content hashing for
// suppression; this whole-directory diff is the turn-bracket backstop where a
// name+size mismatch is a strong enough signal to move the file.
func (s *Store) DiffManifest(ctx context.Context, orgID, projectID, localDir string) (Diff, error) {
	remote, err := s.List(ctx, orgID, projectID)
	if err != nil {
		return Diff{}, err
	}
	remoteByName := make(map[string]int64, len(remote))
	remoteInfo := make(map[string]FileInfo, len(remote))
	for _, f := range remote {
		remoteByName[f.Name] = f.Size
		remoteInfo[f.Name] = f
	}

	localByName, err := scanLocalDir(localDir)
	if err != nil {
		return Diff{}, err
	}

	var d Diff
	// Disk-side: what to push up (missing/size-changed remotely) and what to
	// delete locally (no remote counterpart).
	for name, size := range localByName {
		rs, ok := remoteByName[name]
		if !ok {
			d.Push = append(d.Push, name)
			d.RemoveLocal = append(d.RemoveLocal, name)
			continue
		}
		if rs != size {
			d.Push = append(d.Push, name)
		}
	}
	// Store-side: what to pull down (missing/size-changed locally) and what to
	// delete remotely (no local counterpart).
	for name, f := range remoteInfo {
		ls, ok := localByName[name]
		if !ok {
			d.Pull = append(d.Pull, f)
			d.RemoveRemote = append(d.RemoveRemote, name)
			continue
		}
		if ls != f.Size {
			d.Pull = append(d.Pull, f)
		}
	}
	return d, nil
}

// scanLocalDir returns the flat, KB-eligible files in dir as name→size. A
// missing dir is an empty map. Directories, non-regular files, symlinks, and
// names that fail ValidateName (dot files, traversal) are skipped.
func scanLocalDir(dir string) (map[string]int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int64{}, nil
		}
		return nil, err
	}
	out := make(map[string]int64, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if ValidateName(name) != nil {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		out[name] = info.Size()
	}
	return out, nil
}
