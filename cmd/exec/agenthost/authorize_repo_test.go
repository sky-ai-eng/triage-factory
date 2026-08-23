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
	worktrees []domain.ConversationWorktree
	denied    []string // audit targets recorded via Record
}

func (r *gateRuntime) TeamTracksRepo(context.Context, string, string) (bool, error) {
	return r.tracks, nil
}

func (r *gateRuntime) ListConversationWorktrees(context.Context) ([]domain.ConversationWorktree, error) {
	return r.worktrees, nil
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

	t.Run("gate_unwired_skips", func(t *testing.T) {
		c := &LocalClient{info: ConversationInfo{OrgID: "org-1"}, gateWired: false}
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil when the gate is unwired", err)
		}
	})
}
