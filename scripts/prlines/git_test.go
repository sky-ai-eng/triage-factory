package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo builds a throwaway repo with a real remote so base resolution and
// the merge-base walk are exercised against git itself rather than a mock.
type testRepo struct {
	*repo
	t *testing.T
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// The user's own git config must not reach these commands: a global
	// init.defaultBranch or commit.gpgsign would change what the test builds.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")

	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	for _, d := range []string{remote, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(remote, "init", "--bare", "-b", "main")
	run(work, "init", "-b", "main")
	run(work, "remote", "add", "origin", remote)

	return &testRepo{repo: &repo{root: work}, t: t}
}

func (tr *testRepo) write(path, content string) {
	tr.t.Helper()
	full := filepath.Join(tr.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *testRepo) commit(msg string) {
	tr.t.Helper()
	if _, err := tr.git("add", "-A"); err != nil {
		tr.t.Fatal(err)
	}
	if _, err := tr.git("commit", "-m", msg); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *testRepo) mustGit(args ...string) string {
	tr.t.Helper()
	out, err := tr.git(args...)
	if err != nil {
		tr.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// TestMergeBaseIgnoresLaterBaseCommits is the claim the whole tool rests on:
// work that landed on the base branch after this branch was cut must not show
// up in the counts. Diffing against origin/main's tip would report other.go.
func TestMergeBaseIgnoresLaterBaseCommits(t *testing.T) {
	tr := newTestRepo(t)

	tr.write("seed.go", "package p\n")
	tr.commit("seed")
	tr.mustGit("push", "-q", "origin", "main")

	tr.mustGit("checkout", "-q", "-b", "feature")
	tr.write("mine.go", "package p\n\n// why this exists\nfunc Mine() int { return 1 }\n")
	tr.commit("feature work")
	featureHead := strings.TrimSpace(tr.mustGit("rev-parse", "HEAD"))

	// Someone else lands on main after the branch point.
	tr.mustGit("checkout", "-q", "main")
	tr.write("other.go", strings.Repeat("// noise\n", 50))
	tr.commit("unrelated work on main")
	tr.mustGit("push", "-q", "origin", "main")
	tr.mustGit("checkout", "-q", "feature")

	opts := options{remote: "origin", head: "HEAD", noFetch: true, noGH: true}
	base, _, err := resolveBase(tr.repo, opts, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if base.Ref != "origin/main" {
		t.Fatalf("base = %q, want origin/main", base.Ref)
	}

	mb, n, err := tr.mergeBaseOf(base.Ref, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("got %d merge bases, want 1", n)
	}
	if mb == strings.TrimSpace(tr.mustGit("rev-parse", "origin/main")) {
		t.Fatal("merge base is origin/main's tip; the later commit would be counted")
	}

	raw, err := tr.diff(mb, featureHead)
	if err != nil {
		t.Fatal(err)
	}
	files, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	res := count(tr.repo, files, mb, featureHead, false)

	if res.Files != 1 {
		t.Fatalf("got %d files, want only mine.go: %+v", res.Files, res.PerFile)
	}
	for _, f := range res.PerFile {
		if f.Path == "other.go" {
			t.Fatal("counted a commit that landed on the base after the branch point")
		}
	}
	if want := (pair{Added: 2}); res.Buckets.Code != want {
		t.Errorf("code = %+v, want %+v", res.Buckets.Code, want)
	}
	if want := (pair{Added: 1}); res.Buckets.Comments != want {
		t.Errorf("comments = %+v, want %+v", res.Buckets.Comments, want)
	}
	if want := (pair{Added: 1}); res.Blank != want {
		t.Errorf("blank = %+v, want %+v", res.Blank, want)
	}
}

func TestCountBucketsEndToEnd(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("seed.go", "package p\n")
	tr.commit("seed")
	tr.mustGit("push", "-q", "origin", "main")
	tr.mustGit("checkout", "-q", "-b", "feature")

	tr.write("app.go", "package p\n\n// why the guard is here\nfunc F() bool { return true }\n")
	tr.write("app_test.go", "package p\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { _ = F() }\n")
	tr.write("README.md", "# Title\n\nProse.\n")
	tr.write("go.sum", "example.com/m v1.0.0 h1:abc=\n")
	tr.commit("work")

	head := strings.TrimSpace(tr.mustGit("rev-parse", "HEAD"))
	mb, _, err := tr.mergeBaseOf("origin/main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tr.diff(mb, head)
	if err != nil {
		t.Fatal(err)
	}
	files, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	res := count(tr.repo, files, mb, head, false)

	if want := (pair{Added: 2}); res.Buckets.Code != want {
		t.Errorf("code = %+v, want %+v (package + func, comment excluded)", res.Buckets.Code, want)
	}
	if want := (pair{Added: 3}); res.Buckets.Tests != want {
		t.Errorf("tests = %+v, want %+v", res.Buckets.Tests, want)
	}
	if want := (pair{Added: 2}); res.Buckets.Documentation != want {
		t.Errorf("documentation = %+v, want %+v", res.Buckets.Documentation, want)
	}
	if want := (pair{Added: 1}); res.Buckets.Comments != want {
		t.Errorf("comments = %+v, want %+v", res.Buckets.Comments, want)
	}
	if want := (pair{Added: 1}); res.Generated != want {
		t.Errorf("generated = %+v, want %+v (go.sum excluded from the table)", res.Generated, want)
	}
}

func TestResolveBasePrefersExplicitAndConfig(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("seed.go", "package p\n")
	tr.commit("seed")
	tr.mustGit("push", "-q", "origin", "main")
	tr.mustGit("checkout", "-q", "-b", "parent")
	tr.write("parent.go", "package p\n")
	tr.commit("parent work")
	tr.mustGit("push", "-q", "origin", "parent")
	tr.mustGit("checkout", "-q", "-b", "child")

	opts := options{remote: "origin", head: "HEAD", noFetch: true, noGH: true}

	// Without a hint, a stacked branch can only fall back to the default
	// branch — which is exactly the ambiguity --base and tfBase exist to fix.
	base, _, err := resolveBase(tr.repo, opts, "child")
	if err != nil {
		t.Fatal(err)
	}
	if base.Ref != "origin/main" {
		t.Fatalf("fallback base = %q, want origin/main", base.Ref)
	}

	tr.mustGit("config", "branch.child.tfBase", "origin/parent")
	base, _, err = resolveBase(tr.repo, opts, "child")
	if err != nil {
		t.Fatal(err)
	}
	if base.Ref != "origin/parent" {
		t.Fatalf("configured base = %q, want origin/parent", base.Ref)
	}

	withFlag := opts
	withFlag.base = "origin/main"
	base, _, err = resolveBase(tr.repo, withFlag, "child")
	if err != nil {
		t.Fatal(err)
	}
	if base.Ref != "origin/main" {
		t.Fatalf("--base = %q, want it to win over tfBase", base.Ref)
	}
}

func TestDefaultBranchAmbiguityIsAnError(t *testing.T) {
	tr := newTestRepo(t)
	tr.write("seed.go", "package p\n")
	tr.commit("seed")
	tr.mustGit("push", "-q", "origin", "main")
	tr.mustGit("push", "-q", "origin", "main:master")
	tr.mustGit("fetch", "-q", "origin")

	// Two conventional names and no origin/HEAD: refusing beats guessing,
	// because a wrong base silently produces a plausible table.
	_, _, err := resolveBase(tr.repo, options{remote: "origin", noFetch: true, noGH: true}, "main")
	if err == nil {
		t.Fatal("expected an error when the default branch is ambiguous")
	}
	if !strings.Contains(err.Error(), "--base") {
		t.Errorf("error should point at the fix, got: %v", err)
	}
}

func TestParsePRInfo(t *testing.T) {
	pr, err := parsePRInfo(`{"number":812,"baseRefName":"main","state":"OPEN","isCrossRepository":false}`)
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.Number != 812 || pr.BaseRefName != "main" {
		t.Fatalf("got %+v", pr)
	}
	// gh emits an empty object for some non-PR states; that is "no PR", not an error.
	pr, err = parsePRInfo(`{}`)
	if err != nil || pr != nil {
		t.Fatalf("got %+v, %v; want nil, nil", pr, err)
	}
}
