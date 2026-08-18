package worktree

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The change-scoped capture's one obligation is byte-identity: whatever the
// pre-pass decides, the patch must be exactly what the full `add -A` stage
// would have produced. These tests assert that equality against a reference
// implementation of the pre-scoping sequence, per worktree mutation shape,
// rather than asserting on patch contents by eye.

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// captureWorktree stands up a local-mode branch worktree seeded with a couple
// of tracked files, so mutation cases have tracked material to modify, delete,
// and rename. The seed commit is local-only, which captureUncommitted never
// looks at — it diffs against whatever HEAD is.
func captureWorktree(t *testing.T, rootKey string) string {
	t.Helper()
	withTestHome(t)
	upstream := makeTestUpstream(t)
	wtDir, err := CreateForBranch(context.Background(), "acme", "repo", upstream, "main", "aa/feature", rootKey)
	if err != nil {
		t.Fatalf("CreateForBranch: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtDir, rootKey) })

	for rel, content := range map[string]string{
		"tracked.txt":              "tracked content\n",
		"nested/inner.txt":         "nested content\n",
		"nested/deeper/leaf.txt":   "leaf content\n",
		"renameable/original.txt":  "renameable content\n",
		"deletable/condemned.txt":  "condemned content\n",
		"reversible/pendulum.txt":  "resting state\n",
		"binaryish/blob.bin":       "\x00\x01\x02not text\x03\n",
		"spaced/has space name.md": "spaces are paths too\n",
	} {
		p := filepath.Join(wtDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	gitIn(t, wtDir, "add", ".")
	gitIn(t, wtDir, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "seed tracked files")
	return wtDir
}

// referenceUncommittedPatch reproduces the pre-scoping capture sequence — a
// throwaway HEAD-seeded index, the full `git add -A`, the _tfac and
// .claude/skills drops, one binary diff — so the scoped implementation is
// asserted against the semantics of record rather than against itself.
func referenceUncommittedPatch(t *testing.T, wtPath string) []byte {
	t.Helper()
	idx, err := os.CreateTemp("", "tf-ref-index-*")
	if err != nil {
		t.Fatalf("ref temp index: %v", err)
	}
	idxName := idx.Name()
	_ = idx.Close()
	t.Cleanup(func() { _ = os.Remove(idxName) })

	env := append(gitBaseEnv(), "GIT_INDEX_FILE="+idxName)
	mustGit := func(args ...string) []byte {
		out, gErr := gitCapture(context.Background(), wtPath, env, args...)
		if gErr != nil {
			t.Fatalf("reference git %v: %v", args, gErr)
		}
		return out
	}
	mustGit("read-tree", "HEAD")
	mustGit("add", "-A")
	mustGit("reset", "-q", "--", ScratchDir)
	if changed := mustGit("diff", "--no-ext-diff", "--no-textconv", "--cached", "--name-only", "HEAD", "--", ".claude/skills"); len(bytes.TrimSpace(changed)) > 0 {
		mustGit("reset", "-q", "--", ".claude/skills")
	}
	patch := mustGit("diff", "--no-ext-diff", "--no-textconv", "--cached", "--binary", "HEAD")
	if len(bytes.TrimSpace(patch)) == 0 {
		return nil
	}
	return patch
}

func assertScopedMatchesReference(t *testing.T, wtDir string) {
	t.Helper()
	want := referenceUncommittedPatch(t, wtDir)
	got, err := captureUncommitted(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("captureUncommitted: %v", err)
	}
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !bytes.Equal(got, want) {
		t.Errorf("scoped capture diverged from the full stage.\nscoped (%d bytes):\n%s\nfull (%d bytes):\n%s",
			len(got), got, len(want), want)
	}
}

func TestCaptureUncommitted_ScopedMatchesFullStage(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, wt string)
	}{
		{"clean tree", func(t *testing.T, wt string) {}},
		{"unstaged modify", func(t *testing.T, wt string) {
			if err := os.WriteFile(filepath.Join(wt, "tracked.txt"), []byte("edited\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"staged modify", func(t *testing.T, wt string) {
			if err := os.WriteFile(filepath.Join(wt, "nested", "inner.txt"), []byte("staged edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, wt, "add", "nested/inner.txt")
		}},
		{"untracked file", func(t *testing.T, wt string) {
			if err := os.WriteFile(filepath.Join(wt, "brand-new.txt"), []byte("new\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"untracked dir", func(t *testing.T, wt string) {
			dir := filepath.Join(wt, "newdir", "sub")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"a.txt", "b.txt"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(f+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"deletion", func(t *testing.T, wt string) {
			if err := os.Remove(filepath.Join(wt, "deletable", "condemned.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"staged rename", func(t *testing.T, wt string) {
			gitIn(t, wt, "mv", "renameable/original.txt", "renameable/renamed.txt")
		}},
		{"staged but reverted", func(t *testing.T, wt string) {
			p := filepath.Join(wt, "reversible", "pendulum.txt")
			if err := os.WriteFile(p, []byte("swung\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, wt, "add", "reversible/pendulum.txt")
			if err := os.WriteFile(p, []byte("resting state\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"binary change", func(t *testing.T, wt string) {
			if err := os.WriteFile(filepath.Join(wt, "binaryish", "blob.bin"), []byte("\x00\xff\xfe\x03\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"glob metacharacter filename", func(t *testing.T, wt string) {
			if runtime.GOOS == "windows" {
				t.Skip("'*' is not a legal filename character on windows")
			}
			if err := os.WriteFile(filepath.Join(wt, "f*o.txt"), []byte("literal star\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"untracked .claude with skills and a sibling", func(t *testing.T, wt string) {
			// The collapsed `?? .claude/` status entry: the scoped stage must
			// keep skills out via its :(exclude) pathspec while still
			// capturing the sibling — matching the full stage's
			// add-everything-then-reset-skills result.
			skill := filepath.Join(wt, ".claude", "skills", "step-0", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(skill, []byte("---\nname: step-0\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, ".claude", "keep.md"), []byte("sibling\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"tracked _tfac file modified", func(t *testing.T, wt string) {
			// A repo that legitimately TRACKS files under the scratch dir: the
			// full stage restores HEAD's entry via reset, the scoped stage
			// never stages it — both must keep the change out of the patch
			// without recording a deletion.
			p := filepath.Join(wt, ScratchDir, "pinned.txt")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("pinned\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, wt, "add", "-f", ScratchDir+"/pinned.txt")
			gitIn(t, wt, "-c", "user.email=t@e.c", "-c", "user.name=T", "commit", "-q", "-m", "track scratch file")
			if err := os.WriteFile(p, []byte("locally modified\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"staged new file deleted from worktree", func(t *testing.T, wt string) {
			// The one shape whose pathspec matches nothing in the temp-index
			// world (the file is in neither HEAD nor the worktree), so the
			// scoped stage fails and the fallback must produce the answer.
			p := filepath.Join(wt, "ghost.txt")
			if err := os.WriteFile(p, []byte("staged then gone\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, wt, "add", "ghost.txt")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}},
		{"everything at once", func(t *testing.T, wt string) {
			if err := os.WriteFile(filepath.Join(wt, "tracked.txt"), []byte("edited\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(wt, "deletable", "condemned.txt")); err != nil {
				t.Fatal(err)
			}
			gitIn(t, wt, "mv", "renameable/original.txt", "renameable/relocated.txt")
			if err := os.WriteFile(filepath.Join(wt, "fresh.txt"), []byte("fresh\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, "spaced", "has space name.md"), []byte("still spaced\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtDir := captureWorktree(t, "scoped-"+sanitizeRootKey(tc.name))
			tc.mutate(t, wtDir)
			assertScopedMatchesReference(t, wtDir)
		})
	}
}

// sanitizeRootKey keeps subtest names usable as worktree root keys (dir names).
func sanitizeRootKey(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch r {
		case ' ', '*', '/':
			out = append(out, '-')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// TestCaptureUncommitted_FallbackOverPathspecLimit: past the limit the
// pre-pass declares itself unusable and the full stage answers — same patch,
// just paid for at O(tree).
func TestCaptureUncommitted_FallbackOverPathspecLimit(t *testing.T) {
	prev := changeScopedPathspecLimit
	changeScopedPathspecLimit = 2
	t.Cleanup(func() { changeScopedPathspecLimit = prev })

	wtDir := captureWorktree(t, "scoped-over-limit")
	for _, f := range []string{"one.txt", "two.txt", "three.txt"} {
		if err := os.WriteFile(filepath.Join(wtDir, f), []byte(f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	assertScopedMatchesReference(t, wtDir)

	got, err := captureUncommitted(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("captureUncommitted: %v", err)
	}
	for _, f := range []string{"one.txt", "two.txt", "three.txt"} {
		if !bytes.Contains(got, []byte(f)) {
			t.Errorf("fallback patch is missing %s", f)
		}
	}
}

// TestChangedPathsVsHEAD_CleanAndDirty pins the pre-pass's two answers that
// captureUncommitted branches on: a clean tree is (empty, ok) — the skip-
// everything fast path — and a staged rename yields both pathnames, so the
// scoped stage carries the content and the deletion alike.
func TestChangedPathsVsHEAD_CleanAndDirty(t *testing.T) {
	wtDir := captureWorktree(t, "scoped-prepass")

	paths, ok := changedPathsVsHEAD(context.Background(), wtDir)
	if !ok || len(paths) != 0 {
		t.Fatalf("clean tree: got (%v, %v), want ([], true)", paths, ok)
	}

	gitIn(t, wtDir, "mv", "renameable/original.txt", "renameable/renamed.txt")
	paths, ok = changedPathsVsHEAD(context.Background(), wtDir)
	if !ok {
		t.Fatal("dirty tree: pre-pass reported unusable")
	}
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	if !seen["renameable/original.txt"] || !seen["renameable/renamed.txt"] {
		t.Errorf("rename must contribute both pathnames, got %v", paths)
	}
}

// TestCaptureWorkspaceGit_CleanTreeStillCarriesHead: the clean-tree fast path
// skips the patch, never the delta — restore still needs HEAD (and the branch)
// to rebuild the worktree at the right commit.
func TestCaptureWorkspaceGit_CleanTreeStillCarriesHead(t *testing.T) {
	wtDir := captureWorktree(t, "scoped-clean-delta")
	delta, err := CaptureWorkspaceGit(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit: %v", err)
	}
	if delta == nil || delta.Head == "" || delta.Branch == "" {
		t.Fatalf("clean-tree delta must still carry head+branch, got %+v", delta)
	}
	if delta.Patch != nil {
		t.Errorf("clean tree produced a patch: %q", delta.Patch)
	}
}
