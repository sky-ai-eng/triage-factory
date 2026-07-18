//go:build linux

package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadFileConfined is the cross-run isolation guard for the snapshot-capture
// transcript read: a real file beneath the run root still reads, but an
// agent-planted symlink that points OUT of the root — at another run's tree
// (owned by the same shared agent uid) or a host file that uid can read — must
// be refused, not followed and exfiltrated into this run's snapshot.
func TestReadFileConfined(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("another run's data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "real.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A real file beneath root reads normally.
	if got, err := readFileConfined(root, "sub/real.txt"); err != nil || string(got) != "mine" {
		t.Fatalf("in-root read = (%q, %v), want (\"mine\", nil)", got, err)
	}

	cases := []struct {
		name, link, target string
	}{
		{"absolute symlink escape", "sub/evil.txt", outside},
		{"relative dotdot escape", "sub/dots.txt", "../../../../../../etc/hostname"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(root, c.link)
			if err := os.Symlink(c.target, p); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(p) })
			if got, err := readFileConfined(root, c.link); err == nil {
				t.Fatalf("escape read succeeded (got %q); RESOLVE_BENEATH must reject it", got)
			}
		})
	}

	// An intermediate directory symlink that escapes root is refused too — the
	// final component being real doesn't save it.
	if err := os.Symlink(outsideDir, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if got, err := readFileConfined(root, "linkdir/secret.txt"); err == nil {
		t.Fatalf("intermediate-symlink escape read succeeded (got %q); must be rejected", got)
	}
}

// TestReadSandboxSessionTranscript_ConfinedToRunRoot ties the sandbox transcript
// reader to the confinement: a real transcript at the run-root layout reads, but
// once the hostile agent swaps it for a symlink pointing out of its run root
// (at another run's tree or a host file the shared agent uid can read), the read
// is refused rather than exfiltrating the target into this run's snapshot.
func TestReadSandboxSessionTranscript_ConfinedToRunRoot(t *testing.T) {
	root := t.TempDir()
	const sess = "s1"
	p := SandboxClaudeSessionPath(root, sess)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("real transcript"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := ReadSandboxSessionTranscript(root, sess); !ok || string(got) != "real transcript" {
		t.Fatalf("legit read = (%q, %v), want (\"real transcript\", true)", got, ok)
	}

	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("another run's data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p); err != nil {
		t.Fatal(err)
	}
	if got, ok := ReadSandboxSessionTranscript(root, sess); ok {
		t.Fatalf("symlink-escape read succeeded (got %q); the transcript read must be confined to the run root", got)
	}
}
