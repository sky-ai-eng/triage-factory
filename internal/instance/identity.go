// Package instance resolves this process's persistent instance identity —
// the id the fleet registry (internal/db InstanceStore, TFAC-577) keys
// every instances row on. See docs/specs/horizontal-scaling/README.md §4.1.
package instance

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// idFileName is the identity file's basename under the state root. Exact
// spelling matches the spec (§4.1) and MUST NOT change — existing installs
// key their persistent id off it.
const idFileName = "instance-id"

// Identity is this process's persistent instance id, resolved once at boot
// and held for the process lifetime via an exclusive file lock.
type Identity struct {
	ID string

	file *os.File
}

// EnsureIdentity mints (on first boot) or re-reads (on every later boot)
// the instance id file at <stateRoot>/instance-id, holding it flocked
// exclusive for the caller's lifetime. The id deliberately identifies the
// *state root*, not the host or the process (hostnames are recycled in
// container platforms; PIDs are meaningless) — ownership of rows is really
// ownership of the disk state those rows reference, so identity travels
// with the volume.
//
// The exclusive lock is the two-process guard: a second process pointed at
// the same state root fails here — loudly, at boot — instead of silently
// sharing an identity and corrupting the registry's per-instance
// invariants. Callers should Close the returned Identity at shutdown (or
// just let process exit release the lock).
func EnsureIdentity(stateRoot string) (*Identity, error) {
	if err := os.MkdirAll(stateRoot, 0o755); err != nil {
		return nil, fmt.Errorf("instance identity: ensure state root %s: %w", stateRoot, err)
	}
	path := filepath.Join(stateRoot, idFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("instance identity: open %s: %w", path, err)
	}
	if err := flockExclusive(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("instance identity: %s is locked by another process — two TF processes cannot share one state root: %w", path, err)
	}

	id, err := readOrMintID(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("instance identity: %s: %w", path, err)
	}
	return &Identity{ID: id, file: f}, nil
}

// readOrMintID reads the id already in f, or — when f is empty — mints a
// fresh uuid and writes it. f is already positioned at offset 0 (just
// opened); callers must not have read from or written to it yet.
func readOrMintID(f *os.File) (string, error) {
	raw, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if id := strings.TrimSpace(string(raw)); id != "" {
		return id, nil
	}

	id := uuid.New().String()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return "", fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.WriteString(id); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync: %w", err)
	}
	return id, nil
}

// Close releases the identity file's lock and closes its handle.
// Best-effort — safe to call once at shutdown; a nil Identity (a failed
// EnsureIdentity) or a double-Close is a no-op.
func (i *Identity) Close() error {
	if i == nil || i.file == nil {
		return nil
	}
	f := i.file
	i.file = nil
	return f.Close()
}

// ErrLocked is wrapped into the error EnsureIdentity returns when the
// identity file is already held by another process. Exposed for tests;
// production callers surface the wrapping error message directly.
var ErrLocked = errors.New("instance identity file is locked")
