//go:build linux

package delegate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCaptureWorkspaceGit_LocalModeSkipsIsolation pins the dispatcher's routing:
// in local mode captureWorkspaceGit captures in-process and must NOT route
// through the dropped-privilege child (which needs privilege and a re-owned
// tree local mode never produces). Guards against inverting the mode gate.
// captureViaSandbox is stubbed to fail loudly if the isolated path is taken.
func TestCaptureWorkspaceGit_LocalModeSkipsIsolation(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	called := false
	orig := captureViaSandbox
	captureViaSandbox = func(ctx context.Context, wtPath string) ([]byte, error) {
		called = true
		return nil, fmt.Errorf("isolated child spawned in local mode")
	}
	t.Cleanup(func() { captureViaSandbox = orig })

	dir := newCaptureTestRepo(t)
	delta, err := captureWorkspaceGit(context.Background(), dir)
	if err != nil {
		t.Fatalf("captureWorkspaceGit (local): %v", err)
	}
	if called {
		t.Error("local mode routed through the isolated child; want in-process capture")
	}
	if delta == nil || delta.Head == "" {
		t.Errorf("local capture returned %+v, want a delta with a head", delta)
	}
}

// TestCaptureIsolated_DecodesDeltaAndNull pins the wrapper's decode
// contract against the privileged capture op: empty or literal-"null"
// output means "not a git worktree" (nil delta, nil error); a JSON delta
// decodes; garbage is a clear error rather than a zero-value delta.
func TestCaptureIsolated_DecodesDeltaAndNull(t *testing.T) {
	orig := captureViaSandbox
	t.Cleanup(func() { captureViaSandbox = orig })

	for _, tc := range []struct {
		name    string
		raw     []byte
		wantNil bool
		wantErr bool
		head    string
	}{
		{name: "empty output", raw: nil, wantNil: true},
		{name: "null delta", raw: []byte("null\n"), wantNil: true},
		{name: "real delta", raw: []byte(`{"Head":"abc123"}`), head: "abc123"},
		{name: "garbage", raw: []byte("not-json"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureViaSandbox = func(ctx context.Context, wtPath string) ([]byte, error) {
				return tc.raw, nil
			}
			delta, err := captureIsolated(context.Background(), "/w")
			if tc.wantErr {
				if err == nil {
					t.Fatal("want decode error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("captureIsolated: %v", err)
			}
			if tc.wantNil {
				if delta != nil {
					t.Errorf("delta = %+v, want nil", delta)
				}
				return
			}
			if delta == nil || delta.Head != tc.head {
				t.Errorf("delta = %+v, want Head %q", delta, tc.head)
			}
		})
	}
}

// newCaptureTestRepo makes a git repo with one commit and an uncommitted change,
// enough for CaptureWorkspaceGit to produce a non-empty delta.
func newCaptureTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "work")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
