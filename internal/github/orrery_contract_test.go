package github_test

// Contract test: this package's real client, unmodified, against a served
// Orrery world (github.com/sky-ai-eng/orrery) — the mock's rule 2 ("real
// tools judge realness") pointed back at us. It exercises the poll-path
// reads the tracker depends on: open-PR discovery with the ETag fast path,
// stub enrichment, repo meta, reachability, contents, branches, status, and
// the owned-repo mirror walk.
//
// Skipped unless ORRERY_BIN names an orrery binary, so ordinary `go test`
// runs are unaffected:
//
//	cd ../orrery && cargo build
//	ORRERY_BIN=../orrery/target/debug/orrery go test ./internal/github -run TestOrreryContract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/github"
)

type orreryWorld struct {
	baseURL string
	t       *testing.T
}

func startOrrery(t *testing.T) *orreryWorld {
	t.Helper()
	bin := os.Getenv("ORRERY_BIN")
	if bin == "" {
		t.Skip("ORRERY_BIN not set; skipping Orrery contract test")
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	dir := t.TempDir()
	cfg := fmt.Sprintf("[instance]\ndir = %q\n\n[server]\nlisten = \"127.0.0.1:%d\"\n",
		filepath.Join(dir, "instance"), port)
	cfgPath := filepath.Join(dir, "orrery.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin, "serve", "--config", cfgPath)
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start orrery: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("orrery logs:\n%s", logs.String())
		}
	})

	w := &orreryWorld{baseURL: fmt.Sprintf("http://127.0.0.1:%d", port), t: t}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(w.baseURL + "/_orrery/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("orrery did not become healthy; logs:\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return w
}

// httpJSON drives the world's control plane and face directly — the seeding
// side of the test speaks plain HTTP so only the assertions go through the
// client under test.
func (w *orreryWorld) httpJSON(method, path, token string, body any) map[string]any {
	w.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, w.baseURL+path, reader)
	if err != nil {
		w.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		w.t.Fatalf("decode %s %s: %v", method, path, err)
	}
	if resp.StatusCode >= 300 {
		w.t.Fatalf("%s %s: status %d: %v", method, path, resp.StatusCode, decoded)
	}
	return decoded
}

func (w *orreryWorld) mintActor(login, kind string) string {
	created := w.httpJSON("POST", "/_orrery/actors", "", map[string]string{"login": login, "kind": kind})
	token, _ := created["token"].(string)
	if token == "" {
		w.t.Fatalf("no token for %s: %v", login, created)
	}
	return token
}

func (w *orreryWorld) clock() int64 {
	resp := w.httpJSON("GET", "/_orrery/clock", "", nil)
	return int64(resp["clock_unix"].(float64))
}

// git runs one git command the way Orrery's determinism rules require:
// hermetic config and commit dates from the world clock.
func (w *orreryWorld) git(dir string, clock int64, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	date := fmt.Sprintf("%d +0000", clock)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=alice", "GIT_AUTHOR_EMAIL=alice@orrery.invalid",
		"GIT_COMMITTER_NAME=alice", "GIT_COMMITTER_EMAIL=alice@orrery.invalid",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestOrreryContract(t *testing.T) {
	w := startOrrery(t)
	ctx := t.Context()

	alice := w.mintActor("alice", "user")
	bob := w.mintActor("bob", "user")
	ci := w.mintActor("ci", "bot")

	// Seed through the face + real git: a repo with main and a feature
	// branch, one open PR, an approval, and a completed check run.
	repo := w.httpJSON("POST", "/api/v3/user/repos", alice, map[string]any{"name": "webapp"})
	cloneURL, _ := repo["clone_url"].(string)
	authedURL := strings.Replace(cloneURL, "http://", "http://x-access-token:"+alice+"@", 1)
	clock := w.clock()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	w.git(src, clock, "init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# webapp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.git(src, clock, "add", ".")
	w.git(src, clock, "commit", "-m", "init")
	w.git(src, clock, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(src, "feature.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.git(src, clock, "add", ".")
	w.git(src, clock, "commit", "-m", "add feature")
	headSHA := strings.TrimSpace(w.git(src, clock, "rev-parse", "HEAD"))
	w.git(src, clock, "push", authedURL, "main", "feature")
	w.httpJSON("POST", "/api/v3/repos/alice/webapp/pulls", alice,
		map[string]any{"title": "Add feature", "body": "does the thing", "head": "feature", "base": "main"})
	w.httpJSON("POST", "/api/v3/repos/alice/webapp/pulls/1/reviews", bob,
		map[string]any{"event": "APPROVE", "body": "lgtm"})
	w.httpJSON("POST", "/api/v3/repos/alice/webapp/check-runs", ci,
		map[string]any{"name": "ci/test", "head_sha": headSHA, "conclusion": "success"})

	// The client under test, constructed exactly as a GHES org would be:
	// user-facing base URL in, /api/v3 derived by APIBase.
	client := github.NewClient(w.baseURL, alice)

	t.Run("discovery with the ETag fast path", func(t *testing.T) {
		prs, etag, notModified, err := client.ListOpenPRs(ctx, "alice", "webapp", "")
		if err != nil {
			t.Fatalf("ListOpenPRs: %v", err)
		}
		if notModified || etag == "" || len(prs) != 1 {
			t.Fatalf("want 1 PR + etag, got %d prs, etag %q, notModified %v", len(prs), etag, notModified)
		}
		snap := prs[0].Snapshot
		if snap.Number != 1 || snap.Title != "Add feature" || snap.Author != "alice" {
			t.Errorf("snapshot identity wrong: %+v", snap)
		}
		if snap.State != "OPEN" || snap.IsDraft {
			t.Errorf("snapshot state wrong: state=%q draft=%v", snap.State, snap.IsDraft)
		}
		if snap.HeadRef != "feature" || snap.BaseRef != "main" || snap.HeadSHA != headSHA {
			t.Errorf("snapshot refs wrong: %+v", snap)
		}
		if snap.Repo != "alice/webapp" || prs[0].NodeID == "" {
			t.Errorf("snapshot addressing wrong: repo=%q node=%q", snap.Repo, prs[0].NodeID)
		}

		// Quiet repo: the stored etag answers 304 and discovery is skipped.
		_, _, notModified, err = client.ListOpenPRs(ctx, "alice", "webapp", etag)
		if err != nil {
			t.Fatalf("conditional ListOpenPRs: %v", err)
		}
		if !notModified {
			t.Fatal("unchanged listing should be 304")
		}

		// A push to the PR's head branch (synchronize) invalidates the tag
		// and the fresh listing carries the new head.
		if err := os.WriteFile(filepath.Join(src, "feature.txt"), []byte("v2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		w.git(src, clock, "add", ".")
		w.git(src, clock, "commit", "-m", "v2")
		v2SHA := strings.TrimSpace(w.git(src, clock, "rev-parse", "HEAD"))
		w.git(src, clock, "push", authedURL, "feature")
		prs, etag2, notModified, err := client.ListOpenPRs(ctx, "alice", "webapp", etag)
		if err != nil {
			t.Fatalf("post-push ListOpenPRs: %v", err)
		}
		if notModified || etag2 == etag {
			t.Fatal("a synchronize must invalidate the etag")
		}
		if len(prs) != 1 || prs[0].Snapshot.HeadSHA != v2SHA {
			t.Fatalf("listing did not re-pin the head: %+v", prs)
		}
		headSHA = v2SHA
	})

	t.Run("stub enrichment", func(t *testing.T) {
		pr, err := client.GetPRBasic(ctx, "alice", "webapp", 1)
		if err != nil {
			t.Fatalf("GetPRBasic: %v", err)
		}
		if pr.NodeID == "" || pr.Merged || pr.State != "open" {
			t.Errorf("enrichment fields wrong: node=%q merged=%v state=%q", pr.NodeID, pr.Merged, pr.State)
		}
	})

	t.Run("repo meta and reachability", func(t *testing.T) {
		meta, err := client.GetRepoMeta(ctx, "alice", "webapp")
		if err != nil {
			t.Fatalf("GetRepoMeta: %v", err)
		}
		if meta.FullName != "alice/webapp" || meta.DefaultBranch != "main" {
			t.Errorf("meta wrong: %+v", meta)
		}
		if !strings.HasSuffix(meta.CloneURL, "/alice/webapp.git") || meta.ExternalID() == "" {
			t.Errorf("meta URLs/id wrong: %+v", meta)
		}
		if reachable, conclusive := client.CheckRepoAccess(ctx, "alice", "webapp"); !reachable || !conclusive {
			t.Error("existing repo must probe conclusive-reachable")
		}
		if reachable, conclusive := client.CheckRepoAccess(ctx, "alice", "ghost"); reachable || !conclusive {
			t.Error("missing repo must probe conclusive-unreachable")
		}
	})

	t.Run("profile contents", func(t *testing.T) {
		readme, err := client.GetFileContent(ctx, "alice", "webapp", "README.md")
		if err != nil {
			t.Fatalf("GetFileContent: %v", err)
		}
		if readme != "# webapp\n" {
			t.Errorf("content round-trip wrong: %q", readme)
		}
		absent, err := client.GetFileContent(ctx, "alice", "webapp", "CLAUDE.md")
		if err != nil || absent != "" {
			t.Errorf("404 must read as absent, got %q, %v", absent, err)
		}
	})

	t.Run("pr status", func(t *testing.T) {
		// Re-pin CI to the current head so the status read sees it.
		w.httpJSON("POST", "/api/v3/repos/alice/webapp/check-runs", ci,
			map[string]any{"name": "ci/test", "head_sha": headSHA, "conclusion": "success"})
		status, err := client.GetPRStatus(ctx, "alice", "webapp", 1)
		if err != nil {
			t.Fatalf("GetPRStatus: %v", err)
		}
		if len(status.Reviews) != 1 || status.Reviews[0].Author != "bob" || status.Reviews[0].State != "APPROVED" {
			t.Errorf("reviews wrong: %+v", status.Reviews)
		}
		if status.ReviewDecision != "approved" {
			t.Errorf("review decision wrong: %q", status.ReviewDecision)
		}
		if len(status.CheckRuns) != 1 || status.CheckRuns[0].Name != "ci/test" {
			t.Errorf("check runs wrong: %+v", status.CheckRuns)
		}
		if status.ChecksStatus.Total != 1 || status.ChecksStatus.Passing != 1 {
			t.Errorf("checks rollup wrong: %+v", status.ChecksStatus)
		}
	})

	t.Run("branches and owned repos", func(t *testing.T) {
		branches, rawCount, err := client.ListBranchesPage(ctx, "alice", "webapp", "", 100, 1)
		if err != nil {
			t.Fatalf("ListBranchesPage: %v", err)
		}
		names := make([]string, 0, len(branches))
		for _, b := range branches {
			names = append(names, b.Name)
		}
		if rawCount != 2 || strings.Join(names, ",") != "feature,main" {
			t.Errorf("branches wrong: raw=%d names=%v", rawCount, names)
		}
		repos, err := client.ListUserRepos(ctx)
		if err != nil {
			t.Fatalf("ListUserRepos: %v", err)
		}
		if len(repos) != 1 || repos[0].FullName != "alice/webapp" || repos[0].ExternalID() == "" {
			t.Errorf("owned mirror wrong: %+v", repos)
		}
	})

	t.Run("graphql refresh with check runs", func(t *testing.T) {
		pr, err := client.GetPRBasic(ctx, "alice", "webapp", 1)
		if err != nil {
			t.Fatalf("GetPRBasic: %v", err)
		}
		snaps, err := client.RefreshPRs(ctx, []string{pr.NodeID, "O_pull_999", "PR_kwDOjunk"}, true)
		if err != nil {
			t.Fatalf("RefreshPRs: %v", err)
		}
		if len(snaps) != 1 {
			t.Fatalf("unresolved ids must drop silently: got %d snapshots", len(snaps))
		}
		snap, ok := snaps[pr.NodeID]
		if !ok {
			t.Fatalf("no snapshot for %q: %v", pr.NodeID, snaps)
		}
		if snap.State != "OPEN" || snap.Merged || snap.HeadSHA != headSHA {
			t.Errorf("refresh identity wrong: state=%q merged=%v head=%q", snap.State, snap.Merged, snap.HeadSHA)
		}
		if snap.Mergeable != "MERGEABLE" {
			t.Errorf("open PR against unmoved base must be MERGEABLE, got %q", snap.Mergeable)
		}
		if snap.Additions != 1 || snap.ChangedFiles != 1 {
			t.Errorf("diff stats wrong: +%d/-%d over %d files", snap.Additions, snap.Deletions, snap.ChangedFiles)
		}
		if snap.CheckRuns == nil {
			t.Fatal("full refresh must carry known CI state (non-nil CheckRuns)")
		}
		if len(snap.CheckRuns) != 1 || snap.CheckRuns[0].Name != "ci/test" ||
			snap.CheckRuns[0].Status != "completed" || snap.CheckRuns[0].Conclusion != "success" {
			t.Errorf("check runs wrong: %+v", snap.CheckRuns)
		}
		if len(snap.Reviews) != 1 || snap.Reviews[0].Author != "bob" || snap.Reviews[0].State != "APPROVED" {
			t.Errorf("reviews wrong: %+v", snap.Reviews)
		}
		if len(snap.ReviewRequests) != 0 || len(snap.Labels) != 0 {
			t.Errorf("unmodeled surfaces must read empty: requests=%v labels=%v", snap.ReviewRequests, snap.Labels)
		}
	})

	t.Run("graphql review read by node id", func(t *testing.T) {
		// The review's node id travels on the refresh snapshot's
		// latestReviews — the same place the artifact reconciler reads it.
		pr, err := client.GetPRBasic(ctx, "alice", "webapp", 1)
		if err != nil {
			t.Fatalf("GetPRBasic: %v", err)
		}
		snaps, err := client.RefreshPRs(ctx, []string{pr.NodeID}, false)
		if err != nil || len(snaps[pr.NodeID].Reviews) != 1 {
			t.Fatalf("refresh for review id: %v %+v", err, snaps)
		}
		review, err := client.GetReview(ctx, snaps[pr.NodeID].Reviews[0].ID)
		if err != nil {
			t.Fatalf("GetReview: %v", err)
		}
		if review == nil || review.State != "APPROVED" || review.Body != "lgtm" || len(review.Comments) != 0 {
			t.Errorf("review read wrong: %+v", review)
		}
		notReview, err := client.GetReview(ctx, "O_pull_1")
		if err != nil || notReview != nil {
			t.Errorf("non-review node must read as absence, got %+v, %v", notReview, err)
		}
	})

	t.Run("graphql branch existence", func(t *testing.T) {
		refs := []github.BranchRef{
			{Owner: "alice", Repo: "webapp", Branch: "feature"},
			{Owner: "alice", Repo: "webapp", Branch: "gone"},
			{Owner: "alice", Repo: "ghost", Branch: "main"},
		}
		out, err := client.BranchesExist(ctx, refs)
		if err != nil {
			t.Fatalf("BranchesExist: %v", err)
		}
		if exists, ok := out[refs[0]]; !ok || !exists {
			t.Errorf("live branch must answer true: %v", out)
		}
		if exists, ok := out[refs[1]]; !ok || exists {
			t.Errorf("missing branch in a live repo must answer false: %v", out)
		}
		if _, ok := out[refs[2]]; ok {
			t.Errorf("missing repo must stay unknown (absent), got %v", out)
		}
	})

	t.Run("graphql draft mutations", func(t *testing.T) {
		// The PR is not a draft, so readying it errors — GitHub refuses the
		// no-op — while the convert/ready round trip lands both flips.
		if err := client.MarkPRReady(ctx, "alice", "webapp", 1); err == nil {
			t.Error("readying a non-draft PR must error")
		}
		if err := client.ConvertPRToDraft(ctx, "alice", "webapp", 1); err != nil {
			t.Fatalf("ConvertPRToDraft: %v", err)
		}
		if draft, _ := w.httpJSON("GET", "/api/v3/repos/alice/webapp/pulls/1", alice, nil)["draft"].(bool); !draft {
			t.Error("convert must persist draft=true")
		}
		if err := client.MarkPRReady(ctx, "alice", "webapp", 1); err != nil {
			t.Fatalf("MarkPRReady: %v", err)
		}
		if draft, _ := w.httpJSON("GET", "/api/v3/repos/alice/webapp/pulls/1", alice, nil)["draft"].(bool); draft {
			t.Error("ready must persist draft=false")
		}
	})

	t.Run("graphql org teams", func(t *testing.T) {
		w.mintActor("acme", "org")
		teams, err := client.ListUserTeamsInOrg(ctx, "acme", "alice")
		if err != nil {
			t.Fatalf("ListUserTeamsInOrg: %v", err)
		}
		if len(teams) != 0 {
			t.Errorf("teamless org must answer empty, got %+v", teams)
		}
		teams, err = client.ListUserTeamsInOrg(ctx, "alice", "alice")
		if err != nil || len(teams) != 0 {
			t.Errorf("user-account owner must answer empty without error, got %+v, %v", teams, err)
		}
	})

	t.Run("merge is visible to enrichment", func(t *testing.T) {
		w.httpJSON("PUT", "/api/v3/repos/alice/webapp/pulls/1/merge", alice, map[string]any{})
		pr, err := client.GetPRBasic(ctx, "alice", "webapp", 1)
		if err != nil {
			t.Fatalf("GetPRBasic after merge: %v", err)
		}
		if !pr.Merged || pr.State != "closed" {
			t.Errorf("merged PR must enrich as merged+closed: merged=%v state=%q", pr.Merged, pr.State)
		}
	})

	t.Run("graphql terminal refresh and discovery search", func(t *testing.T) {
		pr, err := client.GetPRBasic(ctx, "alice", "webapp", 1)
		if err != nil {
			t.Fatalf("GetPRBasic: %v", err)
		}
		snaps, err := client.RefreshPRs(ctx, []string{pr.NodeID}, false)
		if err != nil {
			t.Fatalf("terminal RefreshPRs: %v", err)
		}
		snap, ok := snaps[pr.NodeID]
		if !ok {
			t.Fatalf("no terminal snapshot: %v", snaps)
		}
		if snap.State != "MERGED" || !snap.Merged || snap.MergedAt == "" {
			t.Errorf("terminal snapshot wrong: state=%q merged=%v mergedAt=%q", snap.State, snap.Merged, snap.MergedAt)
		}
		if snap.CheckRuns != nil {
			t.Error("discovery fragment must leave CI state unknown (nil CheckRuns)")
		}

		// The dashboard-backfill templates (tracker's Phase 1b), verbatim.
		prs, err := client.DiscoverPRs(ctx, "is:pr is:merged author:alice merged:>=2020-01-01 repo:alice/webapp", 50)
		if err != nil {
			t.Fatalf("DiscoverPRs: %v", err)
		}
		if len(prs) != 1 || prs[0].NodeID != pr.NodeID || !prs[0].Snapshot.Merged {
			t.Errorf("merged-author search wrong: %+v", prs)
		}
		prs, err = client.DiscoverPRs(ctx, "is:pr is:merged reviewed-by:bob merged:>=2020-01-01", 50)
		if err != nil || len(prs) != 1 {
			t.Errorf("reviewed-by search wrong: %+v, %v", prs, err)
		}
		prs, err = client.DiscoverPRs(ctx, "is:pr is:closed is:unmerged author:alice closed:>=2020-01-01", 50)
		if err != nil || len(prs) != 0 {
			t.Errorf("closed-unmerged search must be empty here: %+v, %v", prs, err)
		}

		// A qualifier outside Orrery's search subset refuses loudly rather
		// than answering empty.
		if _, err := client.DiscoverPRs(ctx, "is:pr label:bug", 50); err == nil ||
			!strings.Contains(err.Error(), "not mocked") {
			t.Errorf("unmocked qualifier must surface the orrery refusal, got %v", err)
		}
	})
}
