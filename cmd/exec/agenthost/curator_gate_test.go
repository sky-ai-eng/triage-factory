package agenthost

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// gateRuntime is a minimal Runtime for exercising authorizeRepo in isolation:
// it answers the team-tracks + run_worktrees reads the gate makes and records
// git-denied audit rows, panicking on any other method (none is reached).
type gateRuntime struct {
	Runtime
	tracks    bool
	worktrees []domain.RunWorktree
	denied    []string // audit targets recorded via RecordGitDenied
}

func (r *gateRuntime) TeamTracksRepo(context.Context, string, string) (bool, error) {
	return r.tracks, nil
}

func (r *gateRuntime) ListRunWorktrees(context.Context) ([]domain.RunWorktree, error) {
	return r.worktrees, nil
}

func (r *gateRuntime) Record(_ context.Context, _ *domain.Artifact, act *domain.ExternalAction) {
	r.denied = append(r.denied, act.Target)
}

func newGateClient(pinnedRepos []string, rt *gateRuntime) *LocalClient {
	return &LocalClient{
		info:      RunInfo{OrgID: "org-1", RunID: "run-1", TeamID: "team-1", PinnedRepos: pinnedRepos},
		rt:        rt,
		gateWired: true,
	}
}

// TestAuthorizeRepo_CuratorPinnedSet pins the fix: a curator turn
// creates no run_worktrees rows, so exec-gh verbs authorize against its pinned
// set instead of the ledger. Without the pinned arm (the pre-640 behavior) an
// empty ledger denied every curator gh verb even for a tracked repo — that is
// the failing case the pinned set closes.
func TestAuthorizeRepo_CuratorPinnedSet(t *testing.T) {
	ctx := context.Background()

	t.Run("tracked_and_pinned_is_authorized", func(t *testing.T) {
		rt := &gateRuntime{tracks: true} // no run_worktrees rows — a curator turn
		c := newGateClient([]string{"acme/widgets"}, rt)
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil (tracked + pinned)", err)
		}
		if len(rt.denied) != 0 {
			t.Errorf("recorded denials %v, want none for an authorized repo", rt.denied)
		}
	})

	t.Run("tracked_but_not_pinned_is_denied", func(t *testing.T) {
		rt := &gateRuntime{tracks: true} // tracked, but no ledger row and not pinned
		c := newGateClient([]string{"acme/other"}, rt)
		err := c.authorizeRepo(ctx, "acme", "widgets")
		// Tracked but not materialized: the message must tell the agent to
		// self-serve with `workspace add`, since that is the recovery.
		if err == nil || !strings.Contains(err.Error(), "workspace add acme/widgets") {
			t.Fatalf("authorizeRepo = %v, want a 'workspace add' recovery hint (tracked, not in pinned set, no ledger row)", err)
		}
		if len(rt.denied) != 1 || rt.denied[0] != "acme/widgets" {
			t.Errorf("recorded denials %v, want [acme/widgets]", rt.denied)
		}
	})

	t.Run("pinned_but_untracked_is_denied", func(t *testing.T) {
		rt := &gateRuntime{tracks: false} // pinned but the team does not track it
		c := newGateClient([]string{"acme/widgets"}, rt)
		err := c.authorizeRepo(ctx, "acme", "widgets")
		// Untracked: the agent can't self-serve, so the message must point at a
		// team admin adding a tracked repo, NOT at `workspace add`.
		if err == nil || !strings.Contains(err.Error(), "team admin") {
			t.Fatalf("authorizeRepo = %v, want an admin/tracked-repo hint (pinned but untracked — tracks gate still applies)", err)
		}
		if strings.Contains(err.Error(), "workspace add") {
			t.Errorf("untracked message %q should not suggest 'workspace add' (agent cannot self-serve an untracked repo)", err)
		}
	})
}

// TestAuthorizeRepo_DelegatedRunUnchanged pins that a delegated run (empty
// PinnedRepos) authorizes purely off the run_worktrees ledger, exactly as
// before — the pinned arm is inert with an empty list.
func TestAuthorizeRepo_DelegatedRunUnchanged(t *testing.T) {
	ctx := context.Background()

	t.Run("tracked_and_in_ledger_is_authorized", func(t *testing.T) {
		rt := &gateRuntime{tracks: true, worktrees: []domain.RunWorktree{{RepoID: "acme/widgets"}}}
		c := newGateClient(nil, rt) // delegated run: empty pinned set
		if err := c.authorizeRepo(ctx, "acme", "widgets"); err != nil {
			t.Fatalf("authorizeRepo = %v, want nil (tracked + in ledger)", err)
		}
	})

	t.Run("tracked_but_not_in_ledger_is_denied", func(t *testing.T) {
		rt := &gateRuntime{tracks: true} // tracked, empty ledger, empty pinned set
		c := newGateClient(nil, rt)
		err := c.authorizeRepo(ctx, "acme", "widgets")
		// A delegated run that hasn't materialized the repo gets the same
		// self-serve `workspace add` recovery hint as the curator arm.
		if err == nil || !strings.Contains(err.Error(), "workspace add acme/widgets") {
			t.Fatalf("authorizeRepo = %v, want a 'workspace add' recovery hint (empty ledger, empty pinned set)", err)
		}
	})
}
