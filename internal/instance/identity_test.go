package instance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureIdentity_MintsOnFirstBoot(t *testing.T) {
	root := t.TempDir()
	id, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	defer id.Close()

	if id.ID == "" {
		t.Fatal("expected a non-empty minted id")
	}
	raw, err := os.ReadFile(filepath.Join(root, idFileName))
	if err != nil {
		t.Fatalf("read id file: %v", err)
	}
	if string(raw) != id.ID {
		t.Fatalf("id file content %q does not match minted id %q", raw, id.ID)
	}
}

func TestEnsureIdentity_RereadsSameIDAcrossBoots(t *testing.T) {
	root := t.TempDir()

	first, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	firstID := first.ID
	if err := first.Close(); err != nil {
		t.Fatalf("close first boot: %v", err)
	}

	second, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	defer second.Close()

	if second.ID != firstID {
		t.Fatalf("id changed across boots: first=%q second=%q", firstID, second.ID)
	}
}

// TestEnsureIdentity_EmptyFileReMints pins that a zero-byte identity file
// (the state O_CREATE leaves on true first boot, or a crash before the
// first write) re-mints rather than erroring — empty is unambiguous
// first-boot state, unlike corrupt content.
func TestEnsureIdentity_EmptyFileReMints(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, idFileName), nil, 0o600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	id, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("EnsureIdentity on empty file: %v", err)
	}
	defer id.Close()
	if id.ID == "" {
		t.Fatal("expected a freshly minted id from an empty file")
	}
}

// TestEnsureIdentity_CorruptFileFailsLoudly pins that non-UUID content is
// a boot error, not a silently adopted new identity: accepting garbage
// would permanently orphan every row stamped with the real id (there is
// no reaper to collect them), so the operator must decide — restore the
// file or delete it to knowingly re-mint.
func TestEnsureIdentity_CorruptFileFailsLoudly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, idFileName), []byte("d3adbeef-torn-write"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	_, err := EnsureIdentity(root)
	if err == nil {
		t.Fatal("expected an error for corrupt identity-file content")
	}
	if !errors.Is(err, ErrCorruptIDFile) {
		t.Fatalf("error = %v, want ErrCorruptIDFile", err)
	}
	// The lock must not be left held by the failed attempt: a subsequent
	// boot after the operator deletes the file succeeds.
	if err := os.Remove(filepath.Join(root, idFileName)); err != nil {
		t.Fatalf("remove corrupt file: %v", err)
	}
	id, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("EnsureIdentity after operator repair: %v", err)
	}
	id.Close()
}

func TestEnsureIdentity_SecondProcessFailsFast(t *testing.T) {
	if !flockEnforced {
		t.Skip("this platform's flockExclusive is a no-op — a second opener never fails fast, see lock_other.go")
	}
	root := t.TempDir()

	first, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("first process: %v", err)
	}
	defer first.Close()

	_, err = EnsureIdentity(root)
	if err == nil {
		t.Fatal("expected a second EnsureIdentity against the same state root to fail while the first holds the lock")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked, got: %v", err)
	}
}

func TestEnsureIdentity_LockReleasedAfterClose(t *testing.T) {
	if !flockEnforced {
		t.Skip("this platform's flockExclusive is a no-op — there is no real lock to release, see lock_other.go")
	}
	root := t.TempDir()

	first, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("first process: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("expected the lock to be free after Close, got: %v", err)
	}
	defer second.Close()
}

func TestIdentity_CloseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	id, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	if err := id.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := id.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestIdentity_NilCloseIsNoOp(t *testing.T) {
	var id *Identity
	if err := id.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}
