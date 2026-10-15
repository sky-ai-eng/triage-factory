package snapshotcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// dirtyRoot builds a run root holding one checkout with a local-only commit and
// an uncommitted edit, plus scratch.
func dirtyRoot(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	co := filepath.Join(root, "acme", "app", "default")
	if err := os.MkdirAll(co, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, co, "init", "-q", "-b", "work")
	git(t, co, "config", "user.email", "t@example.com")
	git(t, co, "config", "user.name", "T")
	write(t, filepath.Join(co, "a.txt"), "line1\n")
	git(t, co, "add", "-A")
	git(t, co, "commit", "-q", "-m", "seed")
	write(t, filepath.Join(co, "a.txt"), "line1\nline2\n")
	write(t, filepath.Join(co, "u.txt"), "untracked\n")
	write(t, filepath.Join(root, worktree.ScratchDir, "notes.txt"), "scratch note")
	write(t, filepath.Join(root, worktree.ScratchDir, worktree.CILogsDir, "run1", "log.txt"), "big log")
	write(t, filepath.Join(root, worktree.ScratchDir, worktree.KnowledgeDir, "k.md"), "re-staged")
	return root
}

// readStream returns every member of a capture stream by name.
func readStream(t *testing.T, stream []byte) map[string]string {
	t.Helper()
	got := map[string]string{}
	r := sandbox.NewCaptureStreamReader(bytes.NewReader(stream))
	for {
		name, body, err := r.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("read member %s: %v", name, err)
		}
		if _, dup := got[name]; dup {
			t.Errorf("member %q written twice", name)
		}
		got[name] = string(data)
	}
}

func runCapture(t *testing.T, root string, checkouts ...string) (worktree.CaptureManifest, map[string]string) {
	t.Helper()
	var stream, out bytes.Buffer
	if err := Run(context.Background(), root, checkouts, &stream, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var man worktree.CaptureManifest
	if err := json.Unmarshal(out.Bytes(), &man); err != nil {
		t.Fatalf("decode manifest: %v (out=%q)", err, out.String())
	}
	return man, readStream(t, stream.Bytes())
}

// TestRun_StreamsCheckoutAndScratch: the large members travel in the stream
// under the names the orchestrator will hold them to, and the manifest carries
// only the small git metadata and the scratch's digest.
func TestRun_StreamsCheckoutAndScratch(t *testing.T) {
	root := dirtyRoot(t)
	man, members := runCapture(t, root, "acme/app/default")

	if len(man.Checkouts) != 1 || man.Checkouts[0].Rel != "acme/app/default" {
		t.Fatalf("manifest checkouts = %+v", man.Checkouts)
	}
	cc := man.Checkouts[0]
	if cc.Delta == nil || cc.Delta.Branch != "work" || cc.Delta.Head == "" {
		t.Fatalf("delta = %+v, want the branch and head", cc.Delta)
	}
	if len(cc.Delta.Bundle) != 0 || len(cc.Delta.Patch) != 0 {
		t.Error("members rode the manifest; want the stream")
	}
	if !cc.HasPatch {
		t.Error("the uncommitted edit was not captured")
	}
	if patch := members[worktree.CapturePatchMember("acme/app/default")]; !bytes.Contains([]byte(patch), []byte("+line2")) || !bytes.Contains([]byte(patch), []byte("u.txt")) {
		t.Errorf("patch member = %q, want the edit and the untracked file", patch)
	}
	if got := members[worktree.CaptureScratchPrefix+"notes.txt"]; got != "scratch note" {
		t.Errorf("scratch member = %q", got)
	}
	for name := range members {
		if name == worktree.CaptureScratchPrefix+worktree.CILogsDir+"/run1/log.txt" || name == worktree.CaptureScratchPrefix+worktree.KnowledgeDir+"/k.md" {
			t.Errorf("a scratch entry that comes back by its own route was carried: %s", name)
		}
	}
	if !man.CILogsOmitted || man.ScratchDigest == "" {
		t.Errorf("manifest = omitted %v digest %q, want ci-logs reported and a digest", man.CILogsOmitted, man.ScratchDigest)
	}
}

// TestRun_ReadOnlyTree: the capture writes nothing under the tree it reads. It
// runs in a jail that mounts the tree read-only, so a capture that needed to
// write there would fail every snapshot; this holds the property where it can
// be seen without a jail.
func TestRun_ReadOnlyTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through any mode")
	}
	root := dirtyRoot(t)
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(path, map[bool]os.FileMode{true: 0o555, false: 0o444}[d.IsDir()])
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	man, members := runCapture(t, root, "acme/app/default")
	if !man.Checkouts[0].HasPatch || members[worktree.CapturePatchMember("acme/app/default")] == "" {
		t.Error("a read-only tree captured no patch")
	}
}

// TestRun_NonGitCheckout: a checkout whose .git is gone reports no git state
// and writes no members, rather than failing the capture.
func TestRun_NonGitCheckout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "acme", "app", "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	man, members := runCapture(t, root, "acme/app/default")
	if man.Checkouts[0].Delta != nil || man.Checkouts[0].HasBundle || man.Checkouts[0].HasPatch {
		t.Errorf("non-git checkout = %+v, want no git state", man.Checkouts[0])
	}
	if len(members) != 0 {
		t.Errorf("members = %v, want none", members)
	}
}

// TestRun_CleanCheckoutHasNoPatchMember pins that a member exists only when git
// had bytes for it, so the manifest's flags and the stream cannot disagree.
func TestRun_CleanCheckoutHasNoPatchMember(t *testing.T) {
	root := t.TempDir()
	co := filepath.Join(root, "o", "r", "default")
	if err := os.MkdirAll(co, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, co, "init", "-q", "-b", "main")
	git(t, co, "config", "user.email", "t@example.com")
	git(t, co, "config", "user.name", "T")
	write(t, filepath.Join(co, "a"), "x\n")
	git(t, co, "add", "-A")
	git(t, co, "commit", "-q", "-m", "seed")
	man, members := runCapture(t, root, "o/r/default")
	if man.Checkouts[0].HasPatch {
		t.Error("a clean tree reported a patch")
	}
	if _, ok := members[worktree.CapturePatchMember("o/r/default")]; ok {
		t.Error("a clean tree wrote a patch member")
	}
}
