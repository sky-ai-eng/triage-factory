package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

func TestParseDiffSummaries(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/mod.go b/mod.go",
		"index 111..222 100644",
		"--- a/mod.go",
		"+++ b/mod.go",
		"@@ -1,2 +1,3 @@",
		" ctx",
		"-old",
		"+new1",
		"+new2",
		"diff --git a/added.go b/added.go",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/added.go",
		"@@ -0,0 +1,1 @@",
		"+hello",
		"diff --git a/gone.go b/gone.go",
		"deleted file mode 100644",
		"--- a/gone.go",
		"+++ /dev/null",
		"@@ -1,1 +0,0 @@",
		"-bye",
		"diff --git a/old_name.go b/new_name.go",
		"similarity index 90%",
		"rename from old_name.go",
		"rename to new_name.go",
		"diff --git a/img.png b/img.png",
		"index 333..444 100644",
		"Binary files a/img.png and b/img.png differ",
		"",
	}, "\n")

	got := parseDiffSummaries(diff)
	byPath := map[string]fileSummary{}
	for _, f := range got {
		byPath[f.Path] = f
	}
	if len(got) != 5 {
		t.Fatalf("want 5 file summaries, got %d: %+v", len(got), got)
	}
	if f := byPath["mod.go"]; f.Status != "modified" || f.Additions != 2 || f.Deletions != 1 {
		t.Errorf("mod.go = %+v, want modified +2/-1", f)
	}
	if f := byPath["added.go"]; f.Status != "added" || f.Additions != 1 {
		t.Errorf("added.go = %+v, want added +1", f)
	}
	if f := byPath["gone.go"]; f.Status != "removed" || f.Deletions != 1 {
		t.Errorf("gone.go = %+v, want removed -1", f)
	}
	if f := byPath["new_name.go"]; f.Status != "renamed" || f.PreviousFilename != "old_name.go" {
		t.Errorf("new_name.go = %+v, want renamed from old_name.go", f)
	}
	if f := byPath["img.png"]; !f.Binary {
		t.Errorf("img.png = %+v, want binary", f)
	}
}

func TestDiffHeaderNewPath(t *testing.T) {
	cases := map[string]string{
		"diff --git a/foo.go b/foo.go":         "foo.go",
		"diff --git a/dir/old.go b/dir/new.go": "dir/new.go",
		"diff --git a/only-a-side.go nonsense": "only-a-side.go",
	}
	for header, want := range cases {
		if got := diffHeaderNewPath(header); got != want {
			t.Errorf("diffHeaderNewPath(%q) = %q, want %q", header, got, want)
		}
	}
}

// gitInit builds a tiny repo with a base branch and a feature branch checked
// out, returning the worktree dir, the feature HEAD SHA, and the base (main tip)
// SHA. The feature branch adds one line to f.go so base...HEAD is a real
// one-file diff. baseSHA is the recorded base.sha a real PR carries, so callers
// can drive the worktree diff down its primary (recorded-base) path.
func gitInit(t *testing.T) (dir, headSHA, baseSHA string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")
	baseSHA = run("rev-parse", "HEAD")
	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "feature change")
	headSHA = run("rev-parse", "HEAD")
	return dir, headSHA, baseSHA
}

// TestPersistPRDiff_LocalCheckout pins the primary path: with a worktree the
// diff is framed against the local HEAD (not the live PR head), the manifest
// records source=local_checkout + head_sha=local HEAD, and the file rows come
// from the local diff.
func TestPersistPRDiff_LocalCheckout(t *testing.T) {
	dir, headSHA, baseSHA := gitInit(t)

	// The live PR head equals the local HEAD here (fresh checkout) → no staleness.
	srv := newPRDiffServer(t, prDiffBackend{prJSON: prJSON(t, headSHA, "main", baseSHA, 1, 0, 1)})
	client := ghclient.NewClient(srv.URL, "test-token")

	m, err := persistPRDiff(context.Background(), client, resolveLocalCheckout(dir), dir, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("persistPRDiff: %v", err)
	}
	if m.Source != diffSourceLocal {
		t.Errorf("Source = %q, want %q", m.Source, diffSourceLocal)
	}
	if m.HeadSHA != headSHA {
		t.Errorf("HeadSHA = %q, want local HEAD %q", m.HeadSHA, headSHA)
	}
	// The manifest records the recorded base.sha it framed against (TFAC-505) —
	// confirms the primary recorded-base path, not the stale-branch fallback.
	if m.BaseSHA != baseSHA {
		t.Errorf("BaseSHA = %q, want recorded base %q", m.BaseSHA, baseSHA)
	}
	if m.Stale {
		t.Errorf("fresh checkout should not be stale: %+v", m)
	}
	if len(m.Files) != 1 || m.Files[0].Path != "f.go" || m.Files[0].Additions != 1 {
		t.Errorf("local file rows mismatch: %+v", m.Files)
	}
	full, err := os.ReadFile(m.FullDiffPath)
	if err != nil {
		t.Fatalf("read full.diff: %v", err)
	}
	if !strings.Contains(string(full), "+line3") {
		t.Errorf("full.diff should contain the local change")
	}
}

// TestPersistPRDiff_LocalCheckout_StaleWarns pins that when the live PR head is
// ahead of the local checkout, the manifest carries the behind-by count and a
// git-pull warning while still diffing the local code.
func TestPersistPRDiff_LocalCheckout_StaleWarns(t *testing.T) {
	dir, headSHA, baseSHA := gitInit(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/compare/"):
			_, _ = w.Write([]byte(`{"ahead_by":3}`))
		default:
			// live head is a different (newer) commit than the local checkout
			_, _ = w.Write(prJSON(t, "live_newer_head", "main", baseSHA, 1, 0, 1))
		}
	}))
	t.Cleanup(srv.Close)
	client := ghclient.NewClient(srv.URL, "test-token")

	m, err := persistPRDiff(context.Background(), client, resolveLocalCheckout(dir), dir, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("persistPRDiff: %v", err)
	}
	if m.Source != diffSourceLocal || m.HeadSHA != headSHA {
		t.Errorf("should still diff the local checkout: source=%q head=%q", m.Source, m.HeadSHA)
	}
	if !m.Stale || m.BehindBy != 3 || m.RemoteHeadSHA != "live_newer_head" {
		t.Errorf("staleness mismatch: stale=%v behind=%d remote=%q", m.Stale, m.BehindBy, m.RemoteHeadSHA)
	}
	if !strings.Contains(m.Warning, "git pull") {
		t.Errorf("warning should point at git pull: %q", m.Warning)
	}
}

// gitInitInterveningMerge builds the TFAC-505 topology. main advances C0 → M,
// where M edits shared.go (an "already-merged" change). feature branches from M
// and edits only f.go (the PR's own change), so M is in feature's ancestry. The
// local origin/main tracking ref is then pinned to C0 — a clone-time-frozen ref
// that PREDATES M, the way a reused bare's base ref falls behind. Returns the
// worktree dir, the feature HEAD, the recorded base (M), and the stale ref (C0).
func gitInitInterveningMerge(t *testing.T) (dir, headSHA, baseSHA, staleSHA string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("shared.go", "package p\n\nconst V = 1\n")
	write("f.go", "line1\n")
	run("add", ".")
	run("commit", "-q", "-m", "C0")
	staleSHA = run("rev-parse", "HEAD")

	// M: the already-merged change to shared.go the PR must NOT re-display.
	write("shared.go", "package p\n\nimport \"fmt\"\n\nfunc V() { fmt.Println(1) }\n")
	run("add", ".")
	run("commit", "-q", "-m", "M intervening merge")
	baseSHA = run("rev-parse", "HEAD")

	// feature branches from M and changes only f.go (the PR's own change).
	run("checkout", "-q", "-b", "feature")
	write("f.go", "line1\nline2\n")
	run("add", ".")
	run("commit", "-q", "-m", "PR change")
	headSHA = run("rev-parse", "HEAD")

	// Pin origin/main to C0 — the stale, pre-M tracking ref a reused bare carries.
	run("update-ref", "refs/remotes/origin/main", staleSHA)
	return dir, headSHA, baseSHA, staleSHA
}

// TestResolveDiffBase_RecordedBaseAvoidsPhantomHunks is the TFAC-505 regression:
// framing the PR diff against the recorded base.sha (M) yields only the PR's own
// change, while the clone-time-frozen origin/main (C0, predating M) would replay
// M's edit to shared.go as a phantom hunk.
func TestResolveDiffBase_RecordedBaseAvoidsPhantomHunks(t *testing.T) {
	dir, _, baseSHA, staleSHA := gitInitInterveningMerge(t)

	// Recorded base.sha (M) is in the local store → use it.
	base, ok := resolveDiffBase(dir, baseSHA, "main")
	if !ok || base != baseSHA {
		t.Fatalf("resolveDiffBase(baseSHA) = (%q, %v), want (%q, true)", base, ok, baseSHA)
	}
	diff, err := localUnifiedDiff(dir, base, "")
	if err != nil {
		t.Fatalf("localUnifiedDiff(recorded base): %v", err)
	}
	if strings.Contains(diff, "shared.go") {
		t.Errorf("recorded-base diff leaked the already-merged file shared.go:\n%s", diff)
	}
	if !strings.Contains(diff, "f.go") {
		t.Errorf("recorded-base diff dropped the PR's own change to f.go:\n%s", diff)
	}

	// Pin the bug: the stale origin/main (C0) WOULD drag shared.go in. This is
	// exactly what resolving the base by branch name produced before TFAC-505.
	staleDiff, err := localUnifiedDiff(dir, staleSHA, "")
	if err != nil {
		t.Fatalf("localUnifiedDiff(stale base): %v", err)
	}
	if !strings.Contains(staleDiff, "shared.go") {
		t.Fatalf("test topology is wrong: the stale base should reproduce the phantom hunk, got:\n%s", staleDiff)
	}

	// Recorded base.sha absent from the local store → ok=false so the caller uses
	// the API diff, NEVER the (possibly stale) branch ref.
	if base, ok := resolveDiffBase(dir, strings.Repeat("dead", 10), "main"); ok {
		t.Errorf("absent recorded base should yield ok=false (API fallback), got base=%q", base)
	}

	// No recorded base.sha at all (host didn't populate base.sha) → fall back to
	// the base branch tracking ref.
	if base, ok := resolveDiffBase(dir, "", "main"); !ok || base != staleSHA {
		t.Errorf("empty baseSHA should fall back to origin/main (%q), got (%q, %v)", staleSHA, base, ok)
	}
}
