package agenthost

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// gateRuntime is a minimal Runtime for exercising authorizeRepo in isolation:
// it answers the team-tracks + conversation_worktrees reads the gate makes and records
// git-denied audit rows, panicking on any other method (none is reached).
type gateRuntime struct {
	Runtime
	tracks    bool
	taskRepo  bool
	worktrees []domain.ConversationWorktree
	denied    []string // audit targets recorded via Record
}

func (r *gateRuntime) TeamTracksRepo(context.Context, string, string) (bool, error) {
	return r.tracks, nil
}

func (r *gateRuntime) ListConversationWorktrees(context.Context) ([]domain.ConversationWorktree, error) {
	return r.worktrees, nil
}

func (r *gateRuntime) TaskOwnRepo(context.Context, string, string) (bool, error) {
	return r.taskRepo, nil
}

func (r *gateRuntime) Record(_ context.Context, _ *domain.Artifact, act *domain.ExternalAction) {
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
		rt := &gateRuntime{tracks: true, worktrees: []domain.ConversationWorktree{{RepoID: "acme/widgets"}}}
		c := newGateClient(rt)
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil (tracked + materialized)", err)
		}
		if len(rt.denied) != 0 {
			t.Errorf("recorded denials %v, want none for an authorized repo", rt.denied)
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

	t.Run("gate_unwired_skips", func(t *testing.T) {
		c := &LocalClient{info: ConversationInfo{OrgID: "org-1"}, gateWired: false}
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil when the gate is unwired", err)
		}
	})
}
