package agenthost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// gateRuntime is a minimal Runtime for exercising authorizeRepo in isolation;
// any method the gate does not call panics. repository is the row the name
// resolves to (repo-widgets when nil, none when noRow), asked the id each
// id-keyed read was asked about, and denied the git-denied audit targets.
type gateRuntime struct {
	Runtime
	tracks     bool
	taskRepo   bool
	worktrees  []domain.ConversationWorktree
	repository *domain.Repository
	noRow      bool
	asked      []string
	denied     []string
}

func (r *gateRuntime) GetRepo(context.Context, string) (*domain.Repository, error) {
	if r.noRow {
		return nil, nil
	}
	if r.repository != nil {
		return r.repository, nil
	}
	return &domain.Repository{ID: "repo-widgets", Owner: "acme", Repo: "widgets"}, nil
}

func (r *gateRuntime) TeamTracksRepository(_ context.Context, repositoryID string) (bool, error) {
	r.asked = append(r.asked, repositoryID)
	return r.tracks, nil
}

func (r *gateRuntime) ListConversationWorktrees(context.Context) ([]domain.ConversationWorktree, error) {
	return r.worktrees, nil
}

func (r *gateRuntime) TaskOwnRepository(_ context.Context, repositoryID string) (bool, error) {
	r.asked = append(r.asked, repositoryID)
	return r.taskRepo, nil
}

func (r *gateRuntime) Record(_ context.Context, _ *domain.Deliverable, act *domain.ExternalAction, _ domain.EntityContainer, _ domain.GitHubIdentity) {
	r.denied = append(r.denied, act.Target)
}

func newGateClient(rt *gateRuntime) *LocalClient {
	return &LocalClient{
		info:      ConversationInfo{OrgID: "org-1", ConversationID: "conv-1", TeamID: "team-1"},
		rt:        rt,
		gateWired: true,
	}
}

// TestAuthorizeRepo exercises the exec-gh repo gate's three outcomes: a repo
// the team doesn't track, a tracked repo the run hasn't materialized into
// conversation_worktrees, and a tracked + materialized repo.
func TestAuthorizeRepo(t *testing.T) {
	ctx := context.Background()

	t.Run("untracked_is_denied", func(t *testing.T) {
		rt := &gateRuntime{tracks: false}
		c := newGateClient(rt)
		err := c.authorizeRepo(ctx, "acme", "widgets")
		if err == nil || !strings.Contains(err.Error(), "not tracked by this team") {
			t.Fatalf("authorizeRepo = %v, want a not-tracked hint", err)
		}
		if strings.Contains(err.Error(), "workspace add") {
			t.Errorf("untracked message %q should not suggest 'workspace add' (agent cannot self-serve an untracked repo)", err)
		}
		if len(rt.denied) != 1 || rt.denied[0] != "acme/widgets" {
			t.Errorf("recorded denials %v, want [acme/widgets]", rt.denied)
		}
	})

	t.Run("tracked_but_not_materialized_is_denied", func(t *testing.T) {
		rt := &gateRuntime{tracks: true} // tracked, empty ledger
		c := newGateClient(rt)
		err := c.authorizeRepo(ctx, "acme", "widgets")
		if err == nil || !strings.Contains(err.Error(), "workspace add acme/widgets") {
			t.Fatalf("authorizeRepo = %v, want a 'workspace add' recovery hint", err)
		}
		if len(rt.denied) != 1 || rt.denied[0] != "acme/widgets" {
			t.Errorf("recorded denials %v, want [acme/widgets]", rt.denied)
		}
	})

	t.Run("tracked_and_materialized_is_authorized", func(t *testing.T) {
		rt := &gateRuntime{
			tracks:     true,
			worktrees:  []domain.ConversationWorktree{{RepositoryID: "repo-widgets", RepoID: "acme/widgets"}},
			repository: &domain.Repository{ID: "repo-widgets", Owner: "acme", Repo: "widgets"},
		}
		c := newGateClient(rt)
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil (tracked + materialized)", err)
		}
		if len(rt.denied) != 0 {
			t.Errorf("recorded denials %v, want none for an authorized repo", rt.denied)
		}
	})

	// A ledger row names its repository by row id. A checkout of acme/widgets
	// materialized while the org was on another GitHub host is that host's
	// repository; the name now resolves to a different row, so the checkout
	// authorizes nothing and the refusal is the one an unmaterialized repo gets.
	t.Run("materialized_on_another_host_is_not_this_repository", func(t *testing.T) {
		rt := &gateRuntime{
			tracks:     true,
			worktrees:  []domain.ConversationWorktree{{RepositoryID: "repo-widgets-old-host", RepoID: "acme/widgets"}},
			repository: &domain.Repository{ID: "repo-widgets-current-host", Owner: "acme", Repo: "widgets"},
		}
		c := newGateClient(rt)
		err := c.authorizeRepo(ctx, "acme", "widgets")
		if err == nil || !strings.Contains(err.Error(), "workspace add acme/widgets") {
			t.Fatalf("authorizeRepo = %v, want the not-materialized refusal", err)
		}
		if len(rt.denied) != 1 || rt.denied[0] != "acme/widgets" {
			t.Errorf("recorded denials %v, want [acme/widgets]", rt.denied)
		}
	})

	// The run's own task repo is authorized without a ledger row of its own.
	// A conversation that opens in a tree another conversation materialized —
	// a later blueprint step, a later run on the task — holds no row for the
	// one repo its mission is about, and refusing there told the agent to
	// `workspace add` a repo it was already standing in.
	t.Run("task_own_repo_is_authorized_without_a_ledger_row", func(t *testing.T) {
		rt := &gateRuntime{tracks: true, taskRepo: true} // tracked, empty ledger
		c := newGateClient(rt)
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil (the run's own task repo)", err)
		}
		if len(rt.denied) != 0 {
			t.Errorf("recorded denials %v, want none for the run's own task repo", rt.denied)
		}
	})

	// The exception sits behind the tracked-set gate, never in front of it:
	// an untracked repo is refused whatever the task says it is about.
	t.Run("task_own_repo_still_needs_tracking", func(t *testing.T) {
		rt := &gateRuntime{tracks: false, taskRepo: true}
		c := newGateClient(rt)
		err := c.authorizeRepo(ctx, "acme", "widgets")
		if err == nil || !strings.Contains(err.Error(), "not tracked by this team") {
			t.Fatalf("authorizeRepo = %v, want the not-tracked refusal to outrank the task-repo arm", err)
		}
	})

	// The recovery hint names both spellings. A bare add resolves the ref to
	// the repository's configured/default branch, so an agent that needs a
	// pull request head and follows the bare form lands on a checkout of the
	// base branch — which the push policy protects.
	t.Run("recovery_hint_names_both_checkout_spellings", func(t *testing.T) {
		rt := &gateRuntime{tracks: true}
		c := newGateClient(rt)
		err := c.authorizeRepo(ctx, "acme", "widgets")
		if err == nil {
			t.Fatal("authorizeRepo = nil, want a refusal")
		}
		for _, want := range []string{"workspace add acme/widgets", "--pr <N>"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not name %q", err, want)
			}
		}
	})

	// The name is resolved once, and every arm after it asks about the row it
	// resolved to. A name no row answers to is a repository no team tracks.
	t.Run("every_arm_asks_about_the_resolved_row", func(t *testing.T) {
		rt := &gateRuntime{tracks: true, taskRepo: true, repository: &domain.Repository{ID: "repo-renamed", Owner: "acme", Repo: "renamed"}}
		c := newGateClient(rt)
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil (the run's own task repo)", err)
		}
		if strings.Join(rt.asked, ",") != "repo-renamed,repo-renamed" {
			t.Errorf("id-keyed reads asked about %v, want the resolved row for tracking and the task repo", rt.asked)
		}

		unresolved := &gateRuntime{tracks: true, taskRepo: true, noRow: true}
		err := newGateClient(unresolved).authorizeRepo(ctx, "acme", "widgets")
		if err == nil || !strings.Contains(err.Error(), "not tracked by this team") {
			t.Fatalf("authorizeRepo for a name no row answers to = %v, want the not-tracked refusal", err)
		}
		if len(unresolved.asked) != 0 {
			t.Errorf("id-keyed reads asked about %v for a name with no row, want none", unresolved.asked)
		}
	})

	t.Run("gate_unwired_skips", func(t *testing.T) {
		c := &LocalClient{info: ConversationInfo{OrgID: "org-1"}, gateWired: false}
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil when the gate is unwired", err)
		}
	})
}

// TestReviewDraft_ReadsThePullRequestThroughTheGate pins that a review draft's
// pull request is read through the repository gate: the draft is keyed on the
// node id GitHub answers for owner/repo#number and linked to the entity
// carrying it, so start-review and its reset on a repository the run may not
// reach are refused before the draft is found or written.
func TestReviewDraft_ReadsThePullRequestThroughTheGate(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()

	c := newGateClient(&gateRuntime{tracks: false})
	if _, err := c.GithubCreatePendingReview(ctx, "acme", "widgets", 7, "head7", nil); err == nil || !strings.Contains(err.Error(), "not tracked by this team") {
		t.Errorf("GithubCreatePendingReview on an untracked repo = %v, want the not-tracked refusal", err)
	}
	if _, _, err := c.ResetReviewDraft(ctx, "acme", "widgets", 7); err == nil || !strings.Contains(err.Error(), "not tracked by this team") {
		t.Errorf("ResetReviewDraft on an untracked repo = %v, want the not-tracked refusal", err)
	}
}

// TestLocalClient_GithubVerbsAreGatedInLocalMode: in local mode the exec-gh
// verbs pass the repository gate they pass in multi mode, so an agent the
// real-gh channel refuses a repository finds it refused here too. Untracked
// and unchecked-out repositories are refused before any GitHub call, with a
// denial row each, and a repository the run checked out is read.
func TestLocalClient_GithubVerbsAreGatedInLocalMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	ctx := context.Background()
	var calls []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if !answerPRRead(w, r) {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)
	stores, info := newCaptureStores(t, true)
	client := NewLocal(stores, info)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"})

	if _, err := client.GithubGetPR(ctx, "acme", "payments", 12, false); err == nil || !strings.Contains(err.Error(), "not tracked by this team") {
		t.Fatalf("GithubGetPR on an untracked repo = %v, want the not-tracked refusal", err)
	}
	widgets := registerRepo(t, stores, "acme", "widgets", testRepoID("acme/widgets"))
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, info.OrgID, info.TeamID, testScope("github"), []string{widgets}); err != nil {
		t.Fatalf("track acme/widgets: %v", err)
	}
	if _, err := client.GithubGetPR(ctx, "acme", "widgets", 12, false); err == nil || !strings.Contains(err.Error(), "workspace add acme/widgets") {
		t.Fatalf("GithubGetPR on a tracked repo the run has not checked out = %v, want the not-materialized refusal", err)
	}
	if len(calls) != 0 {
		t.Errorf("GitHub calls %v, want none for a refused verb", calls)
	}
	var reasons []string
	for _, a := range listExternalActions(t, stores) {
		if a.Action == domain.ActionGitDenied {
			var detail map[string]string
			_ = json.Unmarshal([]byte(a.DetailJSON), &detail)
			reasons = append(reasons, a.Target+" "+detail["reason"])
		}
	}
	slices.Sort(reasons)
	if want := []string{"acme/payments repo-not-tracked", "acme/widgets repo-not-materialized"}; !slices.Equal(reasons, want) {
		t.Errorf("denials = %v, want %v", reasons, want)
	}

	checkOutRepoForTest(t, stores, info, widgets)
	if _, err := client.GithubGetPR(ctx, "acme", "widgets", 12, false); err != nil {
		t.Fatalf("GithubGetPR on a repo the run checked out = %v, want it read", err)
	}
}
