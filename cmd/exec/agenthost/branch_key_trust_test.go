package agenthost

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// pushRowBySHA is the branch_pushed row of the push that landed sha, or nil.
func pushRowBySHA(t *testing.T, acts []domain.ExternalAction, sha string) *domain.ExternalAction {
	t.Helper()
	for i := range acts {
		var d struct {
			SHA string `json:"sha"`
		}
		if acts[i].Action == domain.ActionBranchPushed && json.Unmarshal([]byte(acts[i].DetailJSON), &d) == nil && d.SHA == sha {
			return &acts[i]
		}
	}
	return nil
}

// TestRecordBranchPush_NamesTheRepositoryItsIDNames covers the row the runtime
// builds for a push: it names the registry row GitHub's repository id names,
// under that row's name, whatever name the push spelled, keys it on that row
// (domain.BranchPushDedupKey), and links the branch on the org's GitHub host. A
// push whose identity read failed records the row naming no repository, keyed
// and named by the repository the push named. No push writes a deliverable.
func TestRecordBranchPush_NamesTheRepositoryItsIDNames(t *testing.T) {
	const ref = "refs/heads/feat"
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			stores, info := newCaptureStores(t, eventTriggered)
			rt := newDirectRuntime(stores, info)
			ctx := context.Background()
			repoID := registerRepo(t, stores, "octo", "repo", "1")
			registerRepo(t, stores, "octo", "other", "2")
			id := domain.GitHubIdentity{RepositoryID: "1", Repository: "octo/repo"}

			// The second push spells the repository by a name it has had: the
			// id, not the name, decides the row.
			rt.RecordBranchPush(ctx, BranchPush{Repo: "octo/repo", Ref: ref, SHA: "abc123", Created: true}, id)
			rt.RecordBranchPush(ctx, BranchPush{Repo: "octo/old-repo", Ref: ref, SHA: "def456"}, id)
			rt.RecordBranchPush(ctx, BranchPush{Repo: "octo/ghost", Ref: ref, SHA: "0ff1ce"}, domain.GitHubIdentity{})

			acts := listExternalActions(t, stores)
			if len(acts) != 3 {
				t.Fatalf("push rows = %d, want 3: %+v", len(acts), acts)
			}
			wantURL, _ := domain.BranchWebURL(testScope("github"), "octo/repo", ref)
			for _, sha := range []string{"abc123", "def456"} {
				row := pushRowBySHA(t, acts, sha)
				if row == nil {
					t.Fatalf("no push row for %s", sha)
				}
				if row.RepositoryID != repoID || row.Target != "octo/repo" {
					t.Errorf("push %s names %q under %q, want octo/repo's %s", sha, row.RepositoryID, row.Target, repoID)
				}
				if want := domain.BranchPushDedupKey(info.ConversationID, repoID, ref, sha); row.DedupKey != want {
					t.Errorf("push %s key = %q, want %q", sha, row.DedupKey, want)
				}
				if row.URL != wantURL || row.ObjectKind != domain.ObjectKindBranch || row.ObjectKey != ref {
					t.Errorf("push %s = url %q object %q %q, want %q and the branch %s", sha, row.URL, row.ObjectKind, row.ObjectKey, wantURL, ref)
				}
				// No signature of the org's produced the tip, so the row says so.
				if !strings.Contains(row.DetailJSON, `"signer":"unsigned"`) || row.Credential != domain.CredentialGitHubApp {
					t.Errorf("push %s = detail %q credential %q, want it stamped unsigned under the App", sha, row.DetailJSON, row.Credential)
				}
			}
			ghost := pushRowBySHA(t, acts, "0ff1ce")
			if ghost == nil || ghost.RepositoryID != "" || ghost.Target != "octo/ghost" ||
				ghost.DedupKey != domain.BranchPushDedupKey(info.ConversationID, "octo/ghost", ref, "0ff1ce") {
				t.Errorf("a push whose identity read failed: row = %+v, want one naming no repository, keyed on octo/ghost", ghost)
			}
			if got := listConversationDeliverables(t, stores, info.ConversationID); len(got) != 0 {
				t.Errorf("pushes wrote deliverables %+v, want none", got)
			}
		})
	}
}

// TestRecordBranchPush_SkipsARefThatIsNotABranch: a tag, or a push to a path
// that is not owner/repo, records nothing.
func TestRecordBranchPush_SkipsARefThatIsNotABranch(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	rt := newDirectRuntime(stores, info)
	ctx := context.Background()
	rt.RecordBranchPush(ctx, BranchPush{Repo: "octo/repo", Ref: "refs/tags/v1", SHA: "abc123"}, domain.GitHubIdentity{})
	rt.RecordBranchPush(ctx, BranchPush{Repo: "octo", Ref: "refs/heads/feat", SHA: "abc123"}, domain.GitHubIdentity{})
	if acts := listExternalActions(t, stores); len(acts) != 0 {
		t.Errorf("rows = %+v, want none", acts)
	}
}

// TestRecordBranchPush_RelayedRepositoryOutsideTheRunIsUnlinked: in multi mode
// the repository id a push arrives with was read by the run's sidecar, so it
// names the push's repository only when it names a repository the run works in
// — a worktree of the run's, or its task's own repository. Any other id records
// the push unlinked, so a sidecar cannot file a push under a repository of its
// choosing.
func TestRecordBranchPush_RelayedRepositoryOutsideTheRunIsUnlinked(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	runmode.SetForTest(t, runmode.ModeMulti)
	rt := newDirectRuntime(stores, info)
	ctx := context.Background()
	ownID := registerRepo(t, stores, "octo", "repo", "1")
	registerRepo(t, stores, "octo", "other", "2")
	if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, info.OrgID, domain.ConversationWorktree{
		ConversationID: info.ConversationID, RepositoryID: ownID, Ref: "refs/heads/main", Path: "/runs/octo/repo/main",
	}); err != nil {
		t.Fatalf("seed the run's worktree: %v", err)
	}

	rt.RecordBranchPush(ctx, BranchPush{Repo: "octo/repo", Ref: "refs/heads/feat", SHA: "abc123", Created: true},
		domain.GitHubIdentity{RepositoryID: "1", Repository: "octo/repo"})
	rt.RecordBranchPush(ctx, BranchPush{Repo: "octo/repo", Ref: "refs/heads/feat", SHA: "def456"},
		domain.GitHubIdentity{RepositoryID: "2", Repository: "octo/other"})

	acts := listExternalActions(t, stores)
	if own := pushRowBySHA(t, acts, "abc123"); own == nil || own.RepositoryID != ownID ||
		own.DedupKey != domain.BranchPushDedupKey(info.ConversationID, ownID, "refs/heads/feat", "abc123") {
		t.Errorf("the run's own repository: row = %+v, want it on %s", own, ownID)
	}
	if other := pushRowBySHA(t, acts, "def456"); other == nil || other.RepositoryID != "" ||
		other.DedupKey != domain.BranchPushDedupKey(info.ConversationID, "octo/repo", "refs/heads/feat", "def456") {
		t.Errorf("a repository outside the run: row = %+v, want it naming no repository", other)
	}
}

// TestServer_SandboxRecordBranchPush_WritesThePushRow pins the sandbox's push
// door: over the socket the pre-push hook names a push and nothing else, and
// the daemon records its one branch_pushed row — about the registry row the id
// it read for the repository names, naming the branch — and no deliverable. A
// ref that is not a branch records nothing.
func TestServer_SandboxRecordBranchPush_WritesThePushRow(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	repoID := registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))
	ctx := context.Background()

	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// The daemon reads the pushed repository's id with its own credential.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !answerPRRead(w, r) {
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(gh.Close)
	srv := NewServer(stores, info, nil)
	srv.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"})
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})
	sandbox := Dial(sockPath)
	t.Cleanup(func() { _ = sandbox.Close() })

	if err := sandbox.RecordBranchPush(ctx, "octo/repo", "refs/heads/feature/x", "abc123", true); err != nil {
		t.Fatalf("RecordBranchPush: %v", err)
	}
	if err := sandbox.RecordBranchPush(ctx, "octo/repo", "refs/tags/v1", "abc123", true); err != nil {
		t.Fatalf("RecordBranchPush (tag): %v", err)
	}
	// The jail names the push and nothing else: an identity it sends beside
	// the push is not read, so it cannot file the push under another
	// repository's registry row.
	registerRepo(t, stores, "octo", "other", testRepoID("octo/other"))
	forged := recordBranchPushArgs{
		BranchPush: BranchPush{Repo: "octo/repo", Ref: "refs/heads/forged", SHA: "def456", Created: true},
		GitHub:     domain.GitHubIdentity{RepositoryID: testRepoID("octo/other"), Repository: "octo/other"},
	}
	if err := sandbox.call(ctx, methodRecordBranchPush, forged, nil); err != nil {
		t.Fatalf("RecordBranchPush (forged identity): %v", err)
	}

	acts := listExternalActions(t, stores)
	if len(acts) != 2 {
		t.Fatalf("ledger rows = %+v, want the two branch pushes", acts)
	}
	for _, a := range acts {
		sha := "abc123"
		if a.ExternalID == "refs/heads/forged" {
			sha = "def456"
		}
		if a.Action != domain.ActionBranchPushed || a.RepositoryID != repoID || a.Target != "octo/repo" ||
			a.ObjectKind != domain.ObjectKindBranch || a.ObjectKey != a.ExternalID ||
			a.DedupKey != domain.BranchPushDedupKey(info.ConversationID, repoID, a.ExternalID, sha) {
			t.Errorf("push row = %+v, want branch_pushed about %s naming the branch", a, repoID)
		}
	}
	if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
		t.Errorf("deliverables = %+v, want none", arts)
	}
}
