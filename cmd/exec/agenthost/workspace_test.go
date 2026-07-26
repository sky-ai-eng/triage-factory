package agenthost

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// tokenResolver wraps fakeGitHubResolver with a working TokenFor, for the
// multi-mode clone-auth tests (the base fake panics there because the gh
// surface never mints raw tokens).
type tokenResolver struct {
	fakeGitHubResolver
	cloneToken string
}

func (r tokenResolver) TokenFor(_ context.Context, _, _ string) (githubapp.Token, error) {
	return githubapp.Token{Value: r.cloneToken}, nil
}

// seedWorkspaceRepo seeds an org-configured, team-tracked repo profile — the
// state CreateWorkspaceCheckout's gates require.
func seedWorkspaceRepo(t *testing.T, stores db.Stores, owner, repo, cloneURL string) {
	t.Helper()
	ctx := context.Background()
	if err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: owner + "/" + repo, Owner: owner, Repo: repo,
		CloneURL: cloneURL, DefaultBranch: "main", ProfileText: "test profile",
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	tracked, err := stores.TeamGitHubRepos.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("list team repos: %v", err)
	}
	tracked = append(tracked, domain.TeamGitHubRepo{Owner: owner, Repo: repo})
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, tracked); err != nil {
		t.Fatalf("track team repo: %v", err)
	}
}

func workspaceInfo(runID string) RunInfo {
	return RunInfo{
		OrgID:  runmode.LocalDefaultOrgID,
		UserID: runmode.LocalDefaultUserID,
		TeamID: runmode.LocalDefaultTeamID,
		RunID:  runID,
	}
}

// stubWorkspaceCreates swaps both worktree-create seams for the duration of
// the test. Each stub records its call and returns path (or err).
type createRecorder struct {
	checkoutCalls int
	prCalls       int

	// checkout args
	coOwner, coRepo, coCloneURL, coRef, coRunID, coRunRoot string
	coAuth                                                 worktree.CloneAuth

	// PR args
	prOwner, prRepo, prUpstream, prHead, prHeadBranch, prRunID, prRunRoot string
	prNumber                                                              int
	prBase                                                                string
	prAuth                                                                worktree.CloneAuth

	path string
	err  error
}

func stubWorkspaceCreates(t *testing.T, rec *createRecorder) {
	t.Helper()
	origCheckout, origPR := workspaceCreateCheckout, workspaceCreatePR
	workspaceCreateCheckout = func(_ context.Context, owner, repo, cloneURL, ref, runID, runRoot string, opts ...worktree.CloneOption) (string, error) {
		rec.checkoutCalls++
		rec.coOwner, rec.coRepo, rec.coCloneURL, rec.coRef, rec.coRunID, rec.coRunRoot = owner, repo, cloneURL, ref, runID, runRoot
		rec.coAuth = worktree.CloneAuthFromOptions(opts...)
		return rec.path, rec.err
	}
	workspaceCreatePR = func(_ context.Context, owner, repo, upstream, head, headBranch string, prNumber int, runID, runRoot string, opts ...worktree.CloneOption) (string, error) {
		rec.prCalls++
		rec.prOwner, rec.prRepo, rec.prUpstream, rec.prHead, rec.prHeadBranch = owner, repo, upstream, head, headBranch
		rec.prNumber, rec.prRunID, rec.prRunRoot = prNumber, runID, runRoot
		rec.prBase = worktree.BaseBranchFromOptions(opts...)
		rec.prAuth = worktree.CloneAuthFromOptions(opts...)
		return rec.path, rec.err
	}
	t.Cleanup(func() {
		workspaceCreateCheckout, workspaceCreatePR = origCheckout, origPR
	})
}

// TestLocalClient_WorkspaceRoots pins the host-root derivation: the run's
// recorded worktree_path when present (the value resume maintains — after a
// cold rehydrate the runID-derived path diverges from the real cwd), the
// worktree.RunRoot(runID) fallback otherwise; and both views equal for the
// in-process client (no sandbox boundary on this transport).
func TestLocalClient_WorkspaceRoots(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-roots", runmode.LocalDefaultUserID, "manual")
	client := NewLocal(stores, workspaceInfo("run-roots"))

	host, agent, err := client.WorkspaceRoots(context.Background())
	if err != nil {
		t.Fatalf("WorkspaceRoots: %v", err)
	}
	if want := worktree.RunRoot("run-roots"); host != want {
		t.Errorf("host root (no worktree_path) = %q, want RunRoot fallback %q", host, want)
	}
	if host != agent {
		t.Errorf("LocalClient views differ: host %q, agent %q — no sandbox boundary on this transport", host, agent)
	}

	if err := stores.Conversations.SetWorktreePathSystem(context.Background(), runmode.LocalDefaultOrgID, "run-roots", "/data/runs/rehydrated-root"); err != nil {
		t.Fatalf("SetWorktreePathSystem: %v", err)
	}
	host, agent, err = client.WorkspaceRoots(context.Background())
	if err != nil {
		t.Fatalf("WorkspaceRoots after set: %v", err)
	}
	if host != "/data/runs/rehydrated-root" || agent != host {
		t.Errorf("roots = (%q, %q), want the recorded worktree_path for both", host, agent)
	}
}

// TestServer_WorkspaceRoots_AgentViewIsWorkMount pins the daemon's namespace
// substitution: over IPC — the transport that only ever serves sandboxed
// callers — the agent view comes back as the /work bind mount while the host
// view stays the run's real on-host root. This pair is what lets `workspace
// add` record host paths (for the push gate / snapshot) yet hand the agent a
// cd-able path (TFAC-546).
func TestServer_WorkspaceRoots_AgentViewIsWorkMount(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-ipc-roots", runmode.LocalDefaultUserID, "manual")
	if err := stores.Conversations.SetWorktreePathSystem(context.Background(), runmode.LocalDefaultOrgID, "run-ipc-roots", "/tmp/triagefactory-runs/run-ipc-roots"); err != nil {
		t.Fatalf("SetWorktreePathSystem: %v", err)
	}

	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, workspaceInfo("run-ipc-roots"), nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})

	client := Dial(sockPath)
	defer client.Close()

	host, agent, err := client.WorkspaceRoots(context.Background())
	if err != nil {
		t.Fatalf("WorkspaceRoots over IPC: %v", err)
	}
	if host != "/tmp/triagefactory-runs/run-ipc-roots" {
		t.Errorf("host root = %q, want the run's recorded worktree_path", host)
	}
	if agent != "/work" {
		t.Errorf("agent root = %q, want /work (the sandbox mount)", agent)
	}
}

// TestLocalClient_CreateWorkspaceCheckout_Gates pins the host-side re-checks:
// the RPC carries only (owner, repo, ref, pr), so an in-jail caller that
// skips the workspace CLI must still be refused for an unconfigured repo, an
// untracked repo, a profile with no clone URL, an injection-shaped ref, and a
// ref+pr conflict — all before any git runs.
func TestLocalClient_CreateWorkspaceCheckout_Gates(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-gates", runmode.LocalDefaultUserID, "manual")
	rec := &createRecorder{}
	stubWorkspaceCreates(t, rec)
	client := NewLocal(stores, workspaceInfo("run-gates"))
	ctx := context.Background()

	if _, err := client.CreateWorkspaceCheckout(ctx, "sky", "nowhere", "", 0); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("unconfigured repo: err = %v, want 'not configured'", err)
	}

	// Org-configured but team-untracked: profile only, no team row.
	if err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: "sky/untracked", Owner: "sky", Repo: "untracked",
		CloneURL: "https://x", DefaultBranch: "main", ProfileText: "t",
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	if _, err := client.CreateWorkspaceCheckout(ctx, "sky", "untracked", "", 0); err == nil || !strings.Contains(err.Error(), "not tracked") {
		t.Errorf("untracked repo: err = %v, want 'not tracked'", err)
	}

	seedWorkspaceRepo(t, stores, "sky", "nourl", "")
	if _, err := client.CreateWorkspaceCheckout(ctx, "sky", "nourl", "", 0); err == nil || !strings.Contains(err.Error(), "clone URL") {
		t.Errorf("missing clone URL: err = %v, want 'clone URL'", err)
	}

	seedWorkspaceRepo(t, stores, "sky", "core", "https://github.com/sky/core.git")
	if _, err := client.CreateWorkspaceCheckout(ctx, "sky", "core", "--upload-pack=evil", 0); err == nil || !strings.Contains(err.Error(), "invalid git ref") {
		t.Errorf("injection ref: err = %v, want 'invalid git ref'", err)
	}
	if _, err := client.CreateWorkspaceCheckout(ctx, "sky", "core", "feature-x", 7); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("ref+pr: err = %v, want 'mutually exclusive'", err)
	}

	if rec.checkoutCalls != 0 || rec.prCalls != 0 {
		t.Errorf("create seams invoked %d/%d times across gate rejections; want 0/0", rec.checkoutCalls, rec.prCalls)
	}
}

// TestLocalClient_CreateWorkspaceCheckout_DefaultPath pins the branch/default
// route: profile-derived clone URL and canonical owner/repo, the raw ref, the
// run's host root, and — in local mode — NO clone credential (byte-for-byte
// the operator's own git path).
func TestLocalClient_CreateWorkspaceCheckout_DefaultPath(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-co", runmode.LocalDefaultUserID, "manual")
	seedWorkspaceRepo(t, stores, "sky", "core", "https://github.com/sky/core.git")
	rec := &createRecorder{path: "/wt/path"}
	stubWorkspaceCreates(t, rec)
	client := NewLocal(stores, workspaceInfo("run-co"))

	path, err := client.CreateWorkspaceCheckout(context.Background(), "sky", "core", "feature-x", 0)
	if err != nil {
		t.Fatalf("CreateWorkspaceCheckout: %v", err)
	}
	if path != "/wt/path" {
		t.Errorf("path = %q, want the create's return", path)
	}
	if rec.checkoutCalls != 1 || rec.prCalls != 0 {
		t.Fatalf("calls = %d/%d, want 1 checkout / 0 pr", rec.checkoutCalls, rec.prCalls)
	}
	if rec.coOwner != "sky" || rec.coRepo != "core" {
		t.Errorf("owner/repo = %s/%s, want sky/core (canonical, from the profile)", rec.coOwner, rec.coRepo)
	}
	if rec.coCloneURL != "https://github.com/sky/core.git" {
		t.Errorf("cloneURL = %q, want the stored profile's — never the wire's", rec.coCloneURL)
	}
	if rec.coRef != "feature-x" {
		t.Errorf("ref = %q, want the raw branch", rec.coRef)
	}
	if rec.coRunID != "run-co" {
		t.Errorf("runID = %q, want run-co", rec.coRunID)
	}
	if want := worktree.RunRoot("run-co"); rec.coRunRoot != want {
		t.Errorf("runRoot = %q, want the host root %q", rec.coRunRoot, want)
	}
	if rec.coAuth != (worktree.CloneAuth{}) {
		t.Errorf("local mode threaded a clone credential %+v; want none (operator's own git path)", rec.coAuth)
	}
}

// TestLocalClient_CreateWorkspaceCheckout_PRPath pins the --pr route: the PR
// fetch happens host-side through the run's resolver, the fork head URL is
// derived in the upstream's protocol, WithBaseBranch carries the PR's base
// (TFAC-505), and in multi mode the clone credential is minted from the same
// resolver and scoped to the upstream (TFAC-546).
func TestLocalClient_CreateWorkspaceCheckout_PRPath(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"number": 42,
			"head": {"ref": "contrib-branch", "repo": {"clone_url": "https://github.com/fork/core.git", "ssh_url": "git@github.com:fork/core.git"}},
			"base": {"ref": "main", "repo": {"clone_url": "https://github.com/sky/core.git", "ssh_url": "git@github.com:sky/core.git"}}
		}`)
	}))
	defer gh.Close()

	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-pr", runmode.LocalDefaultUserID, "manual")
	seedWorkspaceRepo(t, stores, "sky", "core", "https://github.com/sky/core.git")
	// The workspace CLI reserves the conversation_worktrees row BEFORE the create — and
	// the host-side PR fetch rides the exec-gh channel, whose least-privilege
	// gate requires that row. Mirror the production ordering.
	if _, _, err := stores.RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "run-pr", RepoID: "sky/core", Path: "/wt/pr-path", Ref: "pr-42",
	}); err != nil {
		t.Fatalf("reserve conversation_worktrees row: %v", err)
	}
	rec := &createRecorder{path: "/wt/pr-path"}
	stubWorkspaceCreates(t, rec)
	client := NewLocal(stores, workspaceInfo("run-pr"))
	client.SetGitHubResolver(tokenResolver{
		fakeGitHubResolver: fakeGitHubResolver{baseURL: gh.URL, token: "api-token"},
		cloneToken:         "ghs_clone_token",
	})

	path, err := client.CreateWorkspaceCheckout(context.Background(), "sky", "core", "", 42)
	if err != nil {
		t.Fatalf("CreateWorkspaceCheckout (PR): %v", err)
	}
	if path != "/wt/pr-path" {
		t.Errorf("path = %q", path)
	}
	if rec.prCalls != 1 || rec.checkoutCalls != 0 {
		t.Fatalf("calls = %d/%d, want 0 checkout / 1 pr", rec.checkoutCalls, rec.prCalls)
	}
	if rec.prNumber != 42 || rec.prHeadBranch != "contrib-branch" {
		t.Errorf("pr/head = %d/%q, want 42/contrib-branch", rec.prNumber, rec.prHeadBranch)
	}
	if rec.prUpstream != "https://github.com/sky/core.git" {
		t.Errorf("upstream = %q, want the profile clone URL", rec.prUpstream)
	}
	if rec.prHead != "https://github.com/fork/core.git" {
		t.Errorf("head = %q, want the fork URL in the upstream's (HTTPS) protocol", rec.prHead)
	}
	if rec.prBase != "main" {
		t.Errorf("baseBranch = %q, want main (WithBaseBranch(pr.BaseRef), TFAC-505)", rec.prBase)
	}
	if want := worktree.CloneAuthFor("https://github.com/sky/core.git", "ghs_clone_token"); rec.prAuth != want {
		t.Errorf("multi-mode clone auth = %+v, want the resolver token scoped to the upstream", rec.prAuth)
	}
}

// TestStrictlyWithin pins the guard on the daemon's create-cleanup RemoveAll:
// destructive recovery after a chown failure must be provably scoped to a
// strict descendant of the run root. One chown failure mode is exactly "this
// path is outside the run root" (a regressed create contract) — an unguarded
// RemoveAll there would delete an arbitrary host path.
func TestStrictlyWithin(t *testing.T) {
	cases := []struct {
		root, path string
		want       bool
	}{
		{"/tmp/runs/r1", "/tmp/runs/r1/o/r/@default", true},
		{"/tmp/runs/r1", "/tmp/runs/r1/x", true},

		{"/tmp/runs/r1", "/tmp/runs/r1", false},         // the root itself is never removable
		{"/tmp/runs/r1", "/tmp/runs/r2/o", false},       // sibling
		{"/tmp/runs/r1", "/etc/passwd", false},          // arbitrary host path
		{"/tmp/runs/r1", "/tmp/runs/r1/../r2/o", false}, // lexical escape
		{"", "/tmp/runs/r1/o", false},                   // unknown root → never remove
		{"/tmp/runs/r1", "", false},
	}
	for _, c := range cases {
		if got := strictlyWithin(c.root, c.path); got != c.want {
			t.Errorf("strictlyWithin(%q, %q) = %v, want %v", c.root, c.path, got, c.want)
		}
	}
}

// TestLocalClient_CreateWorkspaceCheckout_CreateErrorPropagates pins that a
// create failure surfaces to the caller (which releases its reservation).
func TestLocalClient_CreateWorkspaceCheckout_CreateErrorPropagates(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-fail", runmode.LocalDefaultUserID, "manual")
	seedWorkspaceRepo(t, stores, "sky", "core", "https://github.com/sky/core.git")
	rec := &createRecorder{err: errors.New("simulated git failure")}
	stubWorkspaceCreates(t, rec)
	client := NewLocal(stores, workspaceInfo("run-fail"))

	if _, err := client.CreateWorkspaceCheckout(context.Background(), "sky", "core", "", 0); err == nil || !strings.Contains(err.Error(), "simulated git failure") {
		t.Errorf("err = %v, want the create failure to propagate", err)
	}
}
