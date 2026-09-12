package delegate

import (
	"context"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// seedTaskRepoRegistry makes the fixture's task repo (owner/repo, from
// seedStepFixture's "owner/repo#7" entity) one the team tracks and the registry
// knows: the ledger insert resolves a repository row rather than minting one,
// and the push gate reads the repo's protected refs off the same row.
func seedTaskRepoRegistry(t *testing.T, f stepFixture) {
	t.Helper()
	ctx := context.Background()
	stores := sqlitestore.New(f.database)
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{
		{Owner: "owner", Repo: "repo"},
	}); err != nil {
		t.Fatalf("track the task repo: %v", err)
	}
	if _, err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Repository{
		Owner: "owner", Repo: "repo", DefaultBranch: "main", CloneURL: "https://x", ProfileText: "t",
	}); err != nil {
		t.Fatalf("seed the task repository: %v", err)
	}
}

// ledgerRows reads one conversation's conversation_worktrees rows.
func ledgerRows(t *testing.T, f stepFixture, conversationID string) []domain.ConversationWorktree {
	t.Helper()
	rows, err := sqlitestore.New(f.database).ConversationWorktrees.ListSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("list conversation_worktrees for %s: %v", conversationID, err)
	}
	return rows
}

// TestBuildStepConfig_LaterStepRecordsTheSharedTree is the push gate's
// precondition, per conversation. Push authority is earned through a
// conversation's OWN conversation_worktrees rows, and the first claim writes
// one only for the conversation whose setup clone materialized the tree. Every
// later conversation in that shared tree — a later blueprint step, and a later
// run on the same task — arrived without one, so the gate's bootstrap arm gave
// it read on the very repo its pull request lives in and refused its push.
func TestBuildStepConfig_LaterStepRecordsTheSharedTree(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	wt := t.TempDir()
	f := seedStepFixture(t, "github", "ledger", 2, wt)
	seedTaskRepoRegistry(t, f)

	f.claimStep(t, f.blueprintRun(t), 1)

	rows := ledgerRows(t, f, f.conversationIDs[1])
	if len(rows) != 1 {
		t.Fatalf("later step has %d ledger rows, want 1 — without one its pushes are refused", len(rows))
	}
	got := rows[0]
	if got.RepoID != "owner/repo" || got.Path != wt || got.Ref != worktree.PRRefSlug(7) {
		t.Errorf("ledger row = {repo:%q path:%q ref:%q}, want {repo:%q path:%q ref:%q}",
			got.RepoID, got.Path, got.Ref, "owner/repo", wt, worktree.PRRefSlug(7))
	}
}

// TestBuildStepConfig_LaterStepLedgerIsIdempotent pins the row against a
// re-claim: the dispatcher re-claims one queue episode after a crash, and the
// step's config is rebuilt from scratch each time.
func TestBuildStepConfig_LaterStepLedgerIsIdempotent(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	wt := t.TempDir()
	f := seedStepFixture(t, "github", "ledger-reclaim", 2, wt)
	seedTaskRepoRegistry(t, f)

	br := f.blueprintRun(t)
	f.claimStep(t, br, 1)
	f.claimStep(t, br, 1)

	if rows := ledgerRows(t, f, f.conversationIDs[1]); len(rows) != 1 {
		t.Fatalf("after a re-claim the later step has %d ledger rows, want 1", len(rows))
	}
}

// TestGitAuthorizeDecision_LaterStepMayPushTheSharedTreesBranch is the whole
// point of the row, read from the gate's side: the second conversation in the
// shared tree may push the branch that tree is on, and only that branch. A ref
// outside AllowedRefs is what the receive-pack gate rejects per-ref, so the set
// being exactly the tree's branch is both halves — the allow and the refusal.
func TestGitAuthorizeDecision_LaterStepMayPushTheSharedTreesBranch(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	wt := t.TempDir()
	f := seedStepFixture(t, "github", "ledger-push", 2, wt)
	seedTaskRepoRegistry(t, f)

	// The tree carries the task's branch: step 0 pushed it, and step 1 opens in
	// the same checkout.
	stubLiveBranch(t, map[string]string{wt: "tfac/task-branch"})
	stores := sqlitestore.New(f.database)
	info := agenthost.ConversationInfo{
		OrgID:          runmode.LocalDefaultOrgID,
		TeamID:         runmode.LocalDefaultTeamID,
		ConversationID: f.conversationIDs[1],
	}

	// Before the claim the later step has no row of its own, so the gate's
	// bootstrap arm reads the task's repo and nothing more.
	before, err := gitAuthorizeDecision(ctx, stores, info, "owner", "repo")
	if err != nil {
		t.Fatalf("gitAuthorizeDecision before the claim: %v", err)
	}
	if !before.Allowed || len(before.AllowedRefs) != 0 {
		t.Fatalf("pre-claim decision = %+v, want read-only bootstrap (allowed, no pushable refs)", before)
	}

	f.claimStep(t, f.blueprintRun(t), 1)

	after, err := gitAuthorizeDecision(ctx, stores, info, "owner", "repo")
	if err != nil {
		t.Fatalf("gitAuthorizeDecision after the claim: %v", err)
	}
	if !after.Allowed || !equalRefs(after.AllowedRefs, []string{"refs/heads/tfac/task-branch"}) {
		t.Fatalf("post-claim decision = %+v, want a push allowance for refs/heads/tfac/task-branch", after)
	}
	if slices.Contains(after.AllowedRefs, "refs/heads/somewhere-else") {
		t.Errorf("AllowedRefs = %v, want a ref the tree is not on to stay out of it", after.AllowedRefs)
	}
}
