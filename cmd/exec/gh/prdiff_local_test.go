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
// out, returning the worktree dir and the feature HEAD SHA. The feature branch
// adds one line to f.go so base...HEAD is a real one-file diff.
func gitInit(t *testing.T) (dir, headSHA string) {
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
	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "feature change")
	headSHA = run("rev-parse", "HEAD")
	return dir, headSHA
}

// TestPersistPRDiff_LocalCheckout pins the primary path: with a worktree the
// diff is framed against the local HEAD (not the live PR head), the manifest
// records source=local_checkout + head_sha=local HEAD, and the file rows come
// from the local diff.
func TestPersistPRDiff_LocalCheckout(t *testing.T) {
	dir, headSHA := gitInit(t)

	// The live PR head equals the local HEAD here (fresh checkout) → no staleness.
	srv := newPRDiffServer(t, prDiffBackend{prJSON: prJSON(t, headSHA, "main", 1, 0, 1)})
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
	dir, headSHA := gitInit(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/compare/"):
			_, _ = w.Write([]byte(`{"ahead_by":3}`))
		default:
			// live head is a different (newer) commit than the local checkout
			_, _ = w.Write(prJSON(t, "live_newer_head", "main", 1, 0, 1))
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
