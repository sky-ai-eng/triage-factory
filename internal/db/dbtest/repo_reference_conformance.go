package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepoReferenceFactory is what a per-backend test file hands to
// RunRepoReferenceConformance. Returns the whole store bundle (the three
// reference sites belong to three different stores), the orgID and teamID
// every call passes, and a callback that mints a conversation row for the
// worktree ledger to hang off — the one fixture no store interface builds.
type RepoReferenceFactory func(t *testing.T) (stores db.Stores, orgID, teamID string, conversation func(t *testing.T, suffix string) string)

// RunRepoReferenceConformance covers what it means for a table to reference a
// repository by the registry row's id rather than by its slug. One case per
// converted site — the tracked set, the worktree ledger, a project's pinned
// repos — plus the lifetime rule that governs all three.
//
// The rule is asymmetric, and the asymmetry is the whole design:
//
//   - A repository is brought into the registry by tracking it. Nothing else
//     creates one; a worktree reservation resolves the row and refuses if it
//     is absent, because in multi mode that path runs on the executor, which
//     holds no INSERT on the table at all.
//   - Untracking deletes the tracking row and NOTHING else. A registry row
//     outlives the tracking decision that created it, because a run's worktree
//     ledger, a pinned project or a task may still name the repository —
//     tracking is forward-only in both directions.
//
// The other half of the conversion, that a rename rewrites none of these rows,
// is RunRepoRenameConformance's "Rename_rewrites_no_row_that_references_the_
// repository_by_id".
func RunRepoReferenceConformance(t *testing.T, mk RepoReferenceFactory) {
	t.Helper()
	ctx := context.Background()

	const (
		keptSlug      = "octo/kept"
		untrackedSlug = "octo/dropped"
	)

	t.Run("Tracking_refuses_a_team_from_another_org", func(t *testing.T) {
		// orgID scopes the registry rows the save mints; teamID goes straight
		// onto the tracking row. If the two name different tenants the row is
		// invisible — a team in one org pointing at a repository in another,
		// which no org-scoped read returns — so the save would look like it
		// worked and nothing would be tracked or polled. Refused instead.
		s, orgID, _, _ := mk(t)
		const foreignTeam = "00000000-0000-0000-0000-0000000000ff"
		err := s.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, foreignTeam, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "kept"},
		})
		if !errors.Is(err, db.ErrTeamNotInOrg) {
			t.Fatalf("ReplaceForTeam with a team outside the org = %v, want db.ErrTeamNotInOrg", err)
		}
		// And it refused before writing: no registry row was minted for a
		// save that could never have completed.
		if got, _ := s.Repos.Get(ctx, orgID, keptSlug); got != nil {
			t.Error("the refused save still created a registry row; the guard must run before the mint")
		}
	})

	t.Run("Tracking_brings_the_repository_into_the_registry", func(t *testing.T) {
		s, orgID, teamID, _ := mk(t)
		if err := s.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, teamID, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "kept"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		got, err := s.Repos.Get(ctx, orgID, keptSlug)
		if err != nil || got == nil {
			t.Fatalf("Get after tracking = %v, %v; want the row tracking minted", got, err)
		}
		names, err := s.Repos.ListTrackedNamesSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListTrackedNamesSystem: %v", err)
		}
		if len(names) != 1 || names[0] != keptSlug {
			t.Errorf("tracked names = %v, want [%s]", names, keptSlug)
		}
	})

	t.Run("Untracking_keeps_the_registry_row_and_everything_referencing_it", func(t *testing.T) {
		s, orgID, teamID, conversation := mk(t)
		if err := s.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, teamID, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "kept"},
			{Owner: "octo", Repo: "dropped"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		// Give the repository about to be untracked the two durable references
		// a real one accumulates: a worktree a run checked out, and a pin on a
		// project somebody configured.
		convID := conversation(t, "untrack")
		if _, _, err := s.RunWorktrees.InsertSystem(ctx, orgID, domain.RunWorktree{
			RunID: convID, RepoID: untrackedSlug, Ref: "pr-7",
			Path: "/tmp/wt/" + convID + "/octo/dropped/pr-7",
		}); err != nil {
			t.Fatalf("reserve worktree: %v", err)
		}
		projectID, err := s.Projects.Create(ctx, orgID, teamID, domain.Project{
			Name:        "pins the dropped repo",
			Visibility:  domain.ProjectVisibilityTeam,
			PinnedRepos: []string{untrackedSlug},
		})
		if err != nil {
			t.Fatalf("Projects.Create: %v", err)
		}

		// The untrack.
		if err := s.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, teamID, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "kept"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam (untrack): %v", err)
		}

		// It leaves the tracked set — this is the part that must change, and
		// it is what stops the poller and the profiler working on it.
		names, err := s.Repos.ListTrackedNamesSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListTrackedNamesSystem: %v", err)
		}
		if len(names) != 1 || names[0] != keptSlug {
			t.Errorf("tracked names = %v, want [%s] — untracking must remove it from the polled set", names, keptSlug)
		}
		if tracked, err := s.TeamGitHubRepos.TracksRepoSystem(ctx, teamID, "octo", "dropped"); err != nil || tracked {
			t.Errorf("TracksRepoSystem(dropped) = %v, %v; want false", tracked, err)
		}

		// And nothing else moves.
		row, err := s.Repos.Get(ctx, orgID, untrackedSlug)
		if err != nil || row == nil {
			t.Fatalf("registry row after untracking = %v, %v; want it standing — a reference may still name it", row, err)
		}
		worktrees, err := s.RunWorktrees.ListSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("worktrees ListSystem: %v", err)
		}
		if len(worktrees) != 1 || worktrees[0].RepoID != untrackedSlug {
			t.Errorf("worktree ledger = %+v, want the entry for %s intact — a ledger records what happened", worktrees, untrackedSlug)
		}
		proj, err := s.Projects.GetSystem(ctx, orgID, projectID)
		if err != nil || proj == nil {
			t.Fatalf("Projects.GetSystem: %v, %v", proj, err)
		}
		if len(proj.PinnedRepos) != 1 || proj.PinnedRepos[0] != untrackedSlug {
			t.Errorf("pinned repos = %v, want [%s] kept", proj.PinnedRepos, untrackedSlug)
		}
	})

	t.Run("Pinned_repos_round_trip_in_the_order_they_were_saved", func(t *testing.T) {
		// The pins live in their own table now, so their order is a stored
		// column rather than a property of a JSON array. A project round-trips
		// through bundle export/import and renders as an ordered list, so
		// re-deriving the order from the slug would reshuffle what somebody
		// arranged — this is the case that would catch it.
		s, orgID, teamID, _ := mk(t)
		if err := s.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, teamID, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "zeta"},
			{Owner: "octo", Repo: "alpha"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		pins := []string{"octo/zeta", "octo/alpha"}
		projectID, err := s.Projects.Create(ctx, orgID, teamID, domain.Project{
			Name: "ordered pins", Visibility: domain.ProjectVisibilityTeam, PinnedRepos: pins,
		})
		if err != nil {
			t.Fatalf("Projects.Create: %v", err)
		}
		proj, err := s.Projects.Get(ctx, orgID, projectID)
		if err != nil || proj == nil {
			t.Fatalf("Projects.Get: %v, %v", proj, err)
		}
		if !equalStringSlice(proj.PinnedRepos, pins) {
			t.Errorf("pinned repos = %v, want %v in the order they were saved", proj.PinnedRepos, pins)
		}

		// An update replaces the set wholesale, order and all.
		proj.PinnedRepos = []string{"octo/alpha"}
		if err := s.Projects.Update(ctx, orgID, *proj); err != nil {
			t.Fatalf("Projects.Update: %v", err)
		}
		updated, err := s.Projects.Get(ctx, orgID, projectID)
		if err != nil || updated == nil {
			t.Fatalf("Projects.Get after update: %v, %v", updated, err)
		}
		if !equalStringSlice(updated.PinnedRepos, []string{"octo/alpha"}) {
			t.Errorf("pinned repos after update = %v, want [octo/alpha]", updated.PinnedRepos)
		}
	})

	t.Run("A_worktree_cannot_be_reserved_for_a_repository_with_no_row", func(t *testing.T) {
		// The reference is checked rather than assumed. Before the conversion
		// this wrote a row naming a repository nothing knew about, and the
		// ledger rotted quietly; now it fails at the point the caller is
		// wrong. The store resolves rather than creates on purpose — the
		// executor's role holds no INSERT on repositories.
		s, orgID, _, conversation := mk(t)
		convID := conversation(t, "unknown")
		if _, _, err := s.RunWorktrees.InsertSystem(ctx, orgID, domain.RunWorktree{
			RunID: convID, RepoID: "ghost/repo", Ref: "@default", Path: "/tmp/wt/ghost",
		}); err == nil {
			t.Error("reserving a worktree for a repository with no registry row succeeded; want an error")
		}
		if got, _ := s.Repos.Get(ctx, orgID, "ghost/repo"); got != nil {
			t.Errorf("the refused reservation minted a repository row: %+v", got)
		}
	})
}
