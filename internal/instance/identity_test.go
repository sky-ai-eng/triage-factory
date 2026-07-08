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
