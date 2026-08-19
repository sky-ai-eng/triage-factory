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
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
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
	captureViaSandbox = func(ctx context.Context, wtPath, stagingDir, sessionID string) ([]byte, error) {
		called = true
		return nil, fmt.Errorf("isolated child spawned in local mode")
	}
	t.Cleanup(func() { captureViaSandbox = orig })

	dir := newCaptureTestRepo(t)
	state, _, err := captureWorkspaceGit(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("captureWorkspaceGit (local): %v", err)
	}
	if called {
		t.Error("local mode routed through the isolated child; want in-process capture")
	}
	if state.Delta == nil || state.Delta.Head == "" {
		t.Errorf("local capture returned %+v, want a delta with a head", state.Delta)
	}
}

// TestCaptureIsolated_DecodesState pins the wrapper's decode contract against
// the privileged capture op's worktree.CapturedState envelope: empty output
// means nothing captured; a nil-delta object means "not a git worktree" (nil
// delta, nil error); a delta decodes; a staged transcript path rides alongside
// even when the delta is nil (the Jira/Slack lazy shape); garbage is a clear error.
func TestCaptureIsolated_DecodesState(t *testing.T) {
	orig := captureViaSandbox
	t.Cleanup(func() { captureViaSandbox = orig })

	for _, tc := range []struct {
		name       string
		manifest   func(string) []byte
		wantNil    bool
		wantErr    bool
		head       string
		transcript string
	}{
		{name: "empty output", wantNil: true},
		{name: "nil-delta object", manifest: func(string) []byte { return []byte(`{"delta":null}`) }, wantNil: true},
		{name: "real delta", manifest: func(string) []byte { return []byte(`{"delta":{"Head":"abc123"}}`) }, head: "abc123"},
		{name: "transcript only", manifest: func(dir string) []byte {
			path := filepath.Join(dir, worktree.CaptureTranscriptFile)
			if err := os.WriteFile(path, []byte("hi"), 0o602); err != nil {
				t.Fatal(err)
			}
			return []byte(fmt.Sprintf(`{"delta":null,"transcript_path":%q}`, path))
		}, wantNil: true, transcript: "hi"},
		{name: "foreign member path", manifest: func(string) []byte {
			return []byte(`{"delta":null,"transcript_path":"/etc/passwd"}`)
		}, wantErr: true},
		{name: "buffered member", manifest: func(string) []byte {
			return []byte(`{"delta":null,"transcript":"aGk="}`)
		}, wantErr: true},
		{name: "garbage", manifest: func(string) []byte { return []byte("not-json") }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureViaSandbox = func(ctx context.Context, wtPath, stagingDir, sessionID string) ([]byte, error) {
				if tc.manifest == nil {
					return nil, nil
				}
				return tc.manifest(stagingDir), nil
			}
			state, cleanup, err := captureIsolated(context.Background(), "/w", "")
			if tc.wantErr {
				if err == nil {
					t.Fatal("want decode error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("captureIsolated: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}
			if tc.wantNil {
				if state.Delta != nil {
					t.Errorf("delta = %+v, want nil", state.Delta)
				}
			} else if state.Delta == nil || state.Delta.Head != tc.head {
				t.Errorf("delta = %+v, want Head %q", state.Delta, tc.head)
			}
			if tc.transcript != "" {
				transcript, err := os.ReadFile(state.TranscriptPath)
				if err != nil || string(transcript) != tc.transcript {
					t.Errorf("transcript = %q, %v, want %q", transcript, err, tc.transcript)
				}
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
