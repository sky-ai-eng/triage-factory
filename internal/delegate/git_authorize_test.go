package delegate

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitAuthorizeDecision is the proxy gate's brain: a run may touch a repo
// only if its team tracks it AND it appears in the run's run_worktrees ledger,
// and the allowed push refs are that repo's feature branch(es). It fails closed
// when the backing stores are absent.
func TestGitAuthorizeDecision(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	// run_worktrees FKs the run, so seed it first (LocalDefaultTeamID via the task).
	seedRun(t, database, "run-1", "sess", "/tmp/wt")
	info := agenthost.RunInfo{
		OrgID:  runmode.LocalDefaultOrgID,
		TeamID: runmode.LocalDefaultTeamID,
		RunID:  "run-1",
	}

	// Track two repos for the team; materialize two — one overlapping (tracked
	// AND materialized), one only-materialized (not tracked).
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "tracked-only"},
	}); err != nil {
		t.Fatalf("track repos: %v", err)
	}
	for _, w := range []domain.RunWorktree{
		{RunID: "run-1", RepoID: "acme/api", Path: "/tmp/a", FeatureBranch: "feature/SKY-1"},
		{RunID: "run-1", RepoID: "acme/materialized-only", Path: "/tmp/m", FeatureBranch: "feature/X"},
	} {
		if _, _, err := stores.RunWorktrees.InsertSystem(ctx, runmode.LocalDefaultOrgID, w); err != nil {
			t.Fatalf("materialize %s: %v", w.RepoID, err)
		}
	}

	cases := []struct {
		name        string
		owner, repo string
		wantAllowed bool
		wantRefs    []string
	}{
		{"tracked and materialized → allow with the feature branch", "acme", "api", true, []string{"refs/heads/feature/SKY-1"}},
		{"case-insensitive repo match", "Acme", "API", true, []string{"refs/heads/feature/SKY-1"}},
		{"tracked but not materialized → deny", "acme", "tracked-only", false, nil},
		{"materialized but untracked → deny", "acme", "materialized-only", false, nil},
		{"neither tracked nor materialized → deny", "ghost", "repo", false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := gitAuthorizeDecision(ctx, stores, info, c.owner, c.repo)
			if err != nil {
				t.Fatalf("gitAuthorizeDecision: %v", err)
			}
			if d.Allowed != c.wantAllowed {
				t.Errorf("Allowed = %v, want %v", d.Allowed, c.wantAllowed)
			}
			if !equalRefs(d.AllowedRefs, c.wantRefs) {
				t.Errorf("AllowedRefs = %v, want %v", d.AllowedRefs, c.wantRefs)
			}
		})
	}

	// Fail closed when the stores it needs are absent (a misconfigured gate
	// must never allow-all).
	if d, err := gitAuthorizeDecision(ctx, db.Stores{}, info, "acme", "api"); err != nil || d.Allowed {
		t.Errorf("nil-stores decision = %+v err=%v; want deny (fail closed)", d, err)
	}
}

func equalRefs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
