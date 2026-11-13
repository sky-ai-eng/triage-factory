package instance

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// would silently abandon every row stamped with the real id (its claims
// would come back only through lease takeover, counted as lost
// engagements), so the operator must decide — restore the file or delete
// it to knowingly re-mint.
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

// TestEnsureIdentity_IDReadableWhileLocked pins that the lock does not hide
// the id from other handles, which a mandatory-lock platform would do if it
// locked the byte range the id lives in.
func TestEnsureIdentity_IDReadableWhileLocked(t *testing.T) {
	if !flockEnforced {
		t.Skip("this platform's flockExclusive is a no-op — nothing is locked, see lock_other.go")
	}
	root := t.TempDir()
	first, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	defer first.Close()

	raw, err := os.ReadFile(filepath.Join(root, idFileName))
	if err != nil {
		t.Fatalf("read id file through a second handle while locked: %v", err)
	}
	if string(raw) != first.ID {
		t.Fatalf("second handle read %q, want %q", raw, first.ID)
	}
}

const lockHolderEnv = "INSTANCE_TEST_LOCK_HOLDER_ROOT"

// TestHelperHoldIdentity is the child half of
// TestEnsureIdentity_SecondOSProcessFailsFast: it takes the identity, reports
// readiness on stdout and holds the lock until stdin closes.
func TestHelperHoldIdentity(t *testing.T) {
	root := os.Getenv(lockHolderEnv)
	if root == "" {
		t.Skip("helper for TestEnsureIdentity_SecondOSProcessFailsFast, not a test of its own")
	}
	id, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("holder EnsureIdentity: %v", err)
	}
	defer id.Close()
	fmt.Println("HELD " + id.ID)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestEnsureIdentity_SecondOSProcessFailsFast(t *testing.T) {
	if !flockEnforced {
		t.Skip("this platform's flockExclusive is a no-op — a second opener never fails fast, see lock_other.go")
	}
	root := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestHelperHoldIdentity$", "-test.v")
	child.Env = append(os.Environ(), lockHolderEnv+"="+root)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin: %v", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("start holder process: %v", err)
	}
	defer func() {
		stdin.Close()
		_ = child.Wait()
	}()

	var heldID string
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "HELD "); ok {
			heldID = rest
			break
		}
	}
	if heldID == "" {
		t.Fatalf("holder process never reported holding the lock (scan err: %v)", sc.Err())
	}

	_, err = EnsureIdentity(root)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked while another OS process holds the lock, got: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, idFileName))
	if err != nil || string(raw) != heldID {
		t.Fatalf("id file read while another process holds the lock = %q, %v; want %q", raw, err, heldID)
	}

	stdin.Close()
	if err := child.Wait(); err != nil {
		t.Fatalf("holder process: %v", err)
	}
	again, err := EnsureIdentity(root)
	if err != nil {
		t.Fatalf("expected the lock to be free once the holder exited, got: %v", err)
	}
	again.Close()
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
