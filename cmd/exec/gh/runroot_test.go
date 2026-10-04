package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// runHost is an agenthost.Client for driving a whole verb: its GitHub calls go
// to a real *ghclient.Client against an httptest backend and its checkout
// registry is canned. It embeds agenthost.Client (nil) so it satisfies the
// interface; a verb reaching any other method panics, failing the test loudly.
type runHost struct {
	agenthost.Client
	gh        *ghclient.Client
	checkouts fakeCheckouts
}

func (h runHost) ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error) {
	return h.checkouts.ListConversationWorktrees(ctx)
}

func (h runHost) WorkspaceRoots(ctx context.Context) (string, string, error) {
	return h.checkouts.WorkspaceRoots(ctx)
}

func (h runHost) GithubGetPR(ctx context.Context, owner, repo string, number int, verbose bool) (*ghclient.PRView, error) {
	return h.gh.GetPR(ctx, owner, repo, number, verbose)
}

func (h runHost) GithubGetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error) {
	return h.gh.GetPRDiff(ctx, owner, repo, number, file)
}

func (h runHost) GithubGetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error) {
	return h.gh.GetPRFiles(ctx, owner, repo, number)
}

func (h runHost) GithubAPIGet(ctx context.Context, _, _, path string) ([]byte, error) {
	return h.gh.Get(ctx, path)
}

func (h runHost) GithubDownloadArtifact(ctx context.Context, _, _, path string, dst io.Writer, maxBytes int64) (int64, error) {
	return h.gh.DownloadArtifact(ctx, path, dst, maxBytes)
}

// setOrigin points a checkout's origin at a GitHub repo, so repo resolution
// from inside it answers that repo.
func setOrigin(t *testing.T, dir, ownerRepo string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/"+ownerRepo+".git")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
}

// TestRunRoot pins the guard every _tfac-writing verb resolves its destination
// through: the variable must be set and absolute, and the error names it.
func TestRunRoot(t *testing.T) {
	t.Setenv(conversationRootEnv, "")
	if _, err := runRoot(); err == nil || !strings.Contains(err.Error(), conversationRootEnv) {
		t.Errorf("unset: want an error naming %s, got %v", conversationRootEnv, err)
	}

	t.Setenv(conversationRootEnv, "relative/root")
	if _, err := runRoot(); err == nil || !strings.Contains(err.Error(), conversationRootEnv) {
		t.Errorf("relative: want an error naming %s, got %v", conversationRootEnv, err)
	}

	root := t.TempDir()
	t.Setenv(conversationRootEnv, root+string(filepath.Separator))
	got, err := runRoot()
	if err != nil || got != root {
		t.Errorf("absolute: got (%q, %v), want (%q, nil)", got, err, root)
	}
}

// TestPRDiff_WritesUnderRunRootAndDiffsTheRegistryCheckout drives `pr diff`
// end to end in a run tree laid out the way a delegated run's is: the PR's
// checkout under the run root, plus a second repo's checkout beside it. From
// inside the PR's checkout (resolving the repo from its origin) and from the
// run root (with --repo), the capture lands under the run root's _tfac/, is
// framed against the registry's pr-<N> checkout HEAD, and no checkout gains a
// _tfac/ of its own.
func TestPRDiff_WritesUnderRunRootAndDiffsTheRegistryCheckout(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	t.Setenv(conversationRootEnv, root)

	prDir := filepath.Join(root, "owner", "repo", "pr-42")
	headSHA, baseSHA := gitInitAt(t, prDir)
	setOrigin(t, prDir, "owner/repo")
	otherDir := filepath.Join(root, "other", "lib", "default")
	gitInitAt(t, otherDir)
	setOrigin(t, otherDir, "other/lib")
	nested := filepath.Join(prDir, "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := newPRDiffServer(t, prDiffBackend{prJSON: prJSON(t, headSHA, "main", baseSHA, 1, 0, 1)})
	host := runHost{
		gh: ghclient.NewClient(srv.URL, "test-token"),
		checkouts: fakeCheckouts{
			hostRoot:  root,
			agentRoot: root,
			rows: []domain.ConversationWorktree{
				{RepoID: "owner/repo", Ref: "pr-42", Path: prDir},
				{RepoID: "other/lib", Ref: "default", Path: otherDir},
			},
		},
	}

	for _, tc := range []struct {
		name string
		wd   string
		args []string
	}{
		{"from a folder inside the PR checkout", nested, []string{"42"}},
		{"from the run root with --repo", root, []string{"--repo", "owner/repo", "42"}},
		{"from another repo's checkout with --repo", otherDir, []string{"--repo", "owner/repo", "42"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.wd)
			var m diffManifest
			out := captureStdout(t, func() { prDiff(context.Background(), host, tc.args) })
			if err := json.Unmarshal([]byte(out), &m); err != nil {
				t.Fatalf("decode manifest: %v\n%s", err, out)
			}
			if want := filepath.Join(root, "_tfac", "pr-diffs", "owner__repo__42"); m.Dir != want {
				t.Errorf("Dir = %q, want %q", m.Dir, want)
			}
			if m.Source != diffSourceLocal || m.HeadSHA != headSHA {
				t.Errorf("want the registry checkout's HEAD %s via %s, got head=%q source=%q warning=%q",
					headSHA, diffSourceLocal, m.HeadSHA, m.Source, m.Warning)
			}
			assertNoScratch(t, prDir)
			assertNoScratch(t, otherDir)
		})
	}
}

// TestPRDiff_NoCheckoutUsesAPIAndSaysSo covers a run that never materialized
// the PR: the diff comes from the API, still lands under the run root, and the
// manifest's warning says it is the live head rather than a local checkout.
func TestPRDiff_NoCheckoutUsesAPIAndSaysSo(t *testing.T) {
	root := t.TempDir()
	t.Setenv(conversationRootEnv, root)
	t.Chdir(root)

	const sha = "abcdef0123456789abcdef"
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, sha, "main", fakeBaseSHA, 1, 1, 1),
		diffBody:  "diff --git a/foo.go b/foo.go\n@@ -1,2 +1,2 @@\n context\n-old\n+new\n",
		filesBody: jsonPRFiles(t, []map[string]any{{"filename": "foo.go", "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1,2 +1,2 @@\n context\n-old\n+new\n"}}),
	})
	host := runHost{gh: ghclient.NewClient(srv.URL, "test-token")}

	var m diffManifest
	out := captureStdout(t, func() { prDiff(context.Background(), host, []string{"--repo", "owner/repo", "42"}) })
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, out)
	}
	if m.Source != diffSourceAPI || m.HeadSHA != sha {
		t.Errorf("want the API diff at the live head %s, got source=%q head=%q", sha, m.Source, m.HeadSHA)
	}
	if !strings.Contains(m.Warning, "live head") || !strings.Contains(m.Warning, "no checkout of the PR") {
		t.Errorf("warning should say the diff is the live head and why: %q", m.Warning)
	}
	if !strings.HasPrefix(m.Dir, filepath.Join(root, "_tfac")+string(filepath.Separator)) {
		t.Errorf("Dir = %q, want under %s/_tfac", m.Dir, root)
	}
}

// TestActionsDownloadLogs_WritesUnderRunRoot drives `actions download-logs`
// from a folder inside a checkout: the repo resolves from the checkout's
// origin, the logs land under the run root's _tfac/ci-logs/, and the checkout
// gains nothing.
func TestActionsDownloadLogs_WritesUnderRunRoot(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	t.Setenv(conversationRootEnv, root)
	checkout := filepath.Join(root, "owner", "repo", "pr-42")
	gitCheckoutWithOrigin(t, checkout, "https://github.com/owner/repo.git")
	t.Chdir(checkout)

	zipBytes := buildZip(t, map[string]string{"build/1_step.txt": "boom\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/repos/owner/repo/actions/runs/123/jobs":
			_, _ = w.Write([]byte(`{"total_count":1,"jobs":[{"id":1,"name":"build","status":"completed","conclusion":"failure"}]}`))
		case "/api/v3/repos/owner/repo/actions/runs/123/logs":
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	host := runHost{gh: ghclient.NewClient(srv.URL, "test-token")}

	var res downloadLogsResult
	out := captureStdout(t, func() { actionsDownloadLogs(context.Background(), host, []string{"123"}) })
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode result: %v\n%s", err, out)
	}
	want := filepath.Join(root, "_tfac", "ci-logs", "123")
	if res.DestDir != want || res.Owner != "owner" || res.Repo != "repo" {
		t.Errorf("got dest=%q repo=%s/%s, want dest=%q repo=owner/repo", res.DestDir, res.Owner, res.Repo, want)
	}
	if _, err := os.Stat(filepath.Join(want, "build", "1_step.txt")); err != nil {
		t.Errorf("extracted log missing under the run root: %v", err)
	}
	assertNoScratch(t, checkout)
}
