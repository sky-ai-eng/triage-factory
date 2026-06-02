// Package storage is the durable blob seam: a tiny streamed,
// key-addressed object store with two interchangeable backends chosen by
// runtime mode. Local mode writes blobs to the on-disk state root
// (fsStorage); multi mode talks to an S3-compatible object store
// (objectStorage) so a blob outlives the executor that wrote it.
//
// The motivating consumer is the durable blueprint workspace: it snapshots
// a worktree tarball under one backend and rehydrates it under whichever
// executor next owns the shard. That snapshot/rehydrate logic — and the key
// conventions it uses — live above this package; here we provide only the
// put/get/delete/exists primitive over opaque, caller-namespaced keys.
package storage

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ErrNotFound is returned by Get when no blob exists at the key. Both
// backends map their native "missing object" signal — os.IsNotExist for
// the filesystem, the S3 NoSuchKey / 404 for the object store — onto this
// one sentinel, so callers branch with errors.Is regardless of backend.
// Exists reports the same condition as (false, nil); only Get surfaces it
// as an error.
var ErrNotFound = errors.New("storage: key not found")

// Storage is a streamed, key-addressed blob store. Keys are opaque,
// caller-namespaced strings (e.g. "<orgID>/<blueprint_run_id>/workspace.tar");
// this package assigns them no structure. Put and Get stream their bodies
// so a large worktree tarball never has to buffer whole in memory.
type Storage interface {
	// Put writes the full contents of r at key, overwriting any existing
	// blob. It reads r to EOF.
	Put(ctx context.Context, key string, r io.Reader) error
	// Get opens the blob at key for streaming reads. The caller must Close
	// the returned reader. A missing key returns ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the blob at key. Deleting a missing key is not an
	// error (idempotent, matching S3 DELETE semantics).
	Delete(ctx context.Context, key string) error
	// Exists reports whether a blob is present at key. A missing key is
	// (false, nil); only a backend failure returns a non-nil error.
	Exists(ctx context.Context, key string) (bool, error)
}

// New constructs the process-wide blob store for the current runtime mode.
// Local mode → fsStorage rooted at <StateRoot>/blobs, needing no external
// service. Multi mode → objectStorage, configured from the TF_BLOB_* env
// (endpoint, bucket, access key/secret); a missing or malformed config is
// a hard error so a misconfigured deployment fails fast at boot rather than
// on its first snapshot. Call once at startup.
func New() (Storage, error) {
	if runmode.Current() == runmode.ModeMulti {
		cfg, err := ObjectConfigFromEnv()
		if err != nil {
			return nil, err
		}
		return newObjectStorage(cfg)
	}
	root, err := paths.StateRootErr()
	if err != nil {
		return nil, err
	}
	return newFSStorage(filepath.Join(root, "blobs")), nil
}
