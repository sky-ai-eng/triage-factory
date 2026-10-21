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
// converted site — the tracked set, the worktree ledger and GitHub entities —
// plus the lifetime rule that governs them.
//
// The rule is asymmetric, and the asymmetry is the whole design:
//
//   - A repository is brought into the registry by registering it by
//     GitHub id (RepositoryStore.FindOrCreateByExternalID) or by TF
//     recording a GitHub entity in it (RepositoryStore.FindOrCreateByRefSystem),
//     and tracked by the registry id that returns. A row is not tracking: one
//     an entity brought in is in no tracked set. A worktree
//     reservation resolves the row and refuses if it is absent, because a
//     checkout of a repository TF holds nothing for is a caller's mistake.
//   - Untracking deletes the tracking row and NOTHING else. A registry row
//     outlives the tracking decision that created it, because a
//     conversation's worktree ledger or an entity may still reference the
//     repository — tracking is forward-only in both directions.
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
		// orgID scopes the registry rows the ids must name; teamID goes
		// straight onto the tracking row. If the two name different tenants
		// the row is invisible — a team in one org pointing at a repository in
		// another, which no org-scoped read returns — so the save would look
		// like it worked and nothing would be tracked or polled. Refused
		// instead.
		s, orgID, _, _ := mk(t)
		const foreignTeam = "00000000-0000-0000-0000-0000000000ff"
		kept, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, repoRef(keptSlug))
		if err != nil {
			t.Fatalf("register %s: %v", keptSlug, err)
		}
		err = s.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, foreignTeam, TestGitHubHost, []string{kept.ID})
		if !errors.Is(err, db.ErrTeamNotInOrg) {
			t.Fatalf("ReplaceForTeam with a team outside the org = %v, want db.ErrTeamNotInOrg", err)
		}
		if names, err := s.Repos.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost); err != nil || len(names) != 0 {
			t.Errorf("tracked names after the refusal = %v, %v; want none", names, err)
		}
	})

	t.Run("Tracking_puts_the_registry_row_in_the_tracked_set", func(t *testing.T) {
		s, orgID, teamID, _ := mk(t)
		kept, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, repoRef(keptSlug))
		if err != nil {
			t.Fatalf("register %s: %v", keptSlug, err)
		}
		if names, err := s.Repos.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost); err != nil || len(names) != 0 {
			t.Fatalf("tracked names before tracking = %v, %v; want none — a registry row is not tracking", names, err)
		}
		if err := s.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, teamID, TestGitHubHost, []string{kept.ID}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		names, err := s.Repos.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost)
		if err != nil {
			t.Fatalf("ListTrackedNamesSystem: %v", err)
		}
		if len(names) != 1 || names[0] != keptSlug {
			t.Errorf("tracked names = %v, want [%s]", names, keptSlug)
		}
	})

	t.Run("Untracking_keeps_the_registry_row_and_everything_referencing_it", func(t *testing.T) {
		s, orgID, teamID, conversation := mk(t)
		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, teamID, TestGitHubHost, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "kept"},
			{Owner: "octo", Repo: "dropped"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		// Give the repository about to be untracked the durable reference a
		// real one accumulates: a worktree a run checked out.
		dropped, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(untrackedSlug))
		if err != nil || dropped == nil {
			t.Fatalf("resolve %s: got=%v err=%v", untrackedSlug, dropped, err)
		}
		convID := conversation(t, "untrack")
		if _, _, err := s.ConversationWorktrees.InsertSystem(ctx, orgID, domain.ConversationWorktree{
			ConversationID: convID, RepositoryID: dropped.ID, Ref: "pr-7",
			Path: "/tmp/wt/" + convID + "/octo/dropped/pr-7",
		}); err != nil {
			t.Fatalf("reserve worktree: %v", err)
		}

		// The untrack.
		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, teamID, TestGitHubHost, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "kept"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam (untrack): %v", err)
		}

		// It leaves the tracked set — this is the part that must change, and
		// it is what stops the poller and the profiler working on it.
		names, err := s.Repos.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost)
		if err != nil {
			t.Fatalf("ListTrackedNamesSystem: %v", err)
		}
		if len(names) != 1 || names[0] != keptSlug {
			t.Errorf("tracked names = %v, want [%s] — untracking must remove it from the polled set", names, keptSlug)
		}

		// And nothing else moves.
		row, err := s.Repos.GetByRef(ctx, orgID, repoRef(untrackedSlug))
		if err != nil || row == nil {
			t.Fatalf("registry row after untracking = %v, %v; want it standing — a reference may still name it", row, err)
		}
		if tracked, err := s.TeamGitHubRepos.TracksRepositorySystem(ctx, teamID, row.ID); err != nil || tracked {
			t.Errorf("TracksRepositorySystem(dropped) = %v, %v; want false", tracked, err)
		}
		worktrees, err := s.ConversationWorktrees.ListSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("worktrees ListSystem: %v", err)
		}
		if len(worktrees) != 1 || worktrees[0].RepoID != untrackedSlug {
			t.Errorf("worktree ledger = %+v, want the entry for %s intact — a ledger records what happened", worktrees, untrackedSlug)
		}
	})

	t.Run("Tracking_is_answered_by_the_registry_row_id", func(t *testing.T) {
		s, orgID, teamID, _ := mk(t)
		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, teamID, TestGitHubHost, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "kept"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		kept, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(keptSlug))
		if err != nil || kept == nil {
			t.Fatalf("resolve %s: got=%v err=%v", keptSlug, kept, err)
		}
		untracked, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, repoRef(untrackedSlug))
		if err != nil {
			t.Fatalf("FindOrCreateByRefSystem(%s): %v", untrackedSlug, err)
		}
		// The same name on another host is another repository.
		otherHost, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRef{Host: TestOtherGitHubHost, Owner: "octo", Repo: "kept"})
		if err != nil {
			t.Fatalf("FindOrCreateByRefSystem(%s on another host): %v", keptSlug, err)
		}
		const otherTeam = "00000000-0000-0000-0000-0000000000ff"
		for _, c := range []struct {
			name, team, repositoryID string
			want                     bool
		}{
			{"the tracked row", teamID, kept.ID, true},
			{"an untracked row", teamID, untracked.ID, false},
			{"the tracked name's row on another host", teamID, otherHost.ID, false},
			{"the tracked row, for another team", otherTeam, kept.ID, false},
		} {
			got, err := s.TeamGitHubRepos.TracksRepositorySystem(ctx, c.team, c.repositoryID)
			if err != nil || got != c.want {
				t.Errorf("TracksRepositorySystem(%s) = %v, %v; want %v", c.name, got, err, c.want)
			}
		}
	})

	t.Run("A_worktree_cannot_be_reserved_for_a_repository_with_no_row", func(t *testing.T) {
		// The reference is checked rather than assumed: a reservation naming a
		// repository id no row answers to fails at the point the caller is
		// wrong instead of recording a reference to nothing. The store checks
		// rather than creates on purpose: a reservation holds an id, and an id
		// that resolves to nothing is a broken handle, not a name to register.
		s, orgID, _, conversation := mk(t)
		convID := conversation(t, "unknown")
		if _, _, err := s.ConversationWorktrees.InsertSystem(ctx, orgID, domain.ConversationWorktree{
			ConversationID: convID, RepositoryID: unknownRepoID, RepoID: "ghost/repo", Ref: "default", Path: "/tmp/wt/ghost",
		}); !errors.Is(err, db.ErrNoSuchRepository) {
			t.Errorf("reserving a worktree for a repository with no registry row = %v; want db.ErrNoSuchRepository", err)
		}
		if got, _ := s.Repos.GetByRef(ctx, orgID, repoRef("ghost/repo")); got != nil {
			t.Errorf("the refused reservation minted a repository row: %+v", got)
		}
	})

	t.Run("The_door_creates_a_bare_untracked_row_once", func(t *testing.T) {
		s, orgID, _, _ := mk(t)
		created, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "Octo", Repo: "Door"})
		if err != nil {
			t.Fatalf("FindOrCreateByRefSystem: %v", err)
		}
		if created.ID == "" || created.Slug() != "Octo/Door" || created.Host != TestGitHubHost || created.Source != domain.RepoSourceGitHub {
			t.Fatalf("created row = %+v, want Octo/Door on %s with an id", created, TestGitHubHost)
		}
		if created.ExternalID != "" || created.ProfileText != "" || created.CloneURL != "" || created.CloneStatus != "pending" {
			t.Errorf("created row = %+v, want a bare row: no provider id, no profile, no clone url, clone pending", created)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreateByRefSystem", created, func() (*domain.Repository, error) {
			return s.Repos.GetSystem(ctx, orgID, created.ID)
		})

		// Another spelling of the same repository is the same row, keeping
		// the stored casing.
		again, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "door"})
		if err != nil {
			t.Fatalf("FindOrCreateByRefSystem (again): %v", err)
		}
		if again.ID != created.ID || again.Slug() != "Octo/Door" {
			t.Errorf("second call = %s %s, want the existing row %s under its stored casing", again.ID, again.Slug(), created.ID)
		}

		// It is in the registry and in no tracked set.
		all, err := s.Repos.ListSystem(ctx, orgID, TestGitHubHost)
		if err != nil || len(all) != 1 {
			t.Fatalf("ListSystem = %d rows, err=%v; want the one row", len(all), err)
		}
		tracked, _, err := s.Repos.ListTrackedSystem(ctx, orgID, TestGitHubHost, db.Unwindowed)
		if err != nil || len(tracked) != 0 {
			t.Errorf("ListTrackedSystem = %+v, err=%v; want nothing — the door tracks nothing", tracked, err)
		}

		// The same name on another host is another repository.
		other, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRef{Host: TestOtherGitHubHost, Owner: "Octo", Repo: "Door"})
		if err != nil || other.ID == created.ID || other.Host != TestOtherGitHubHost {
			t.Errorf("FindOrCreateByRefSystem on %s = %+v, err=%v; want a row of its own", TestOtherGitHubHost, other, err)
		}

		if _, err := s.Repos.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRef{Owner: "octo", Repo: "hostless"}); !errors.Is(err, db.ErrRepoHostRequired) {
			t.Errorf("FindOrCreateByRefSystem without a host = %v, want db.ErrRepoHostRequired", err)
		}
	})

	t.Run("A_github_entity_references_its_repository_row", func(t *testing.T) {
		s, orgID, teamID, _ := mk(t)
		if err := TrackRepos(ctx, s.Repos, s.TeamGitHubRepos, orgID, teamID, TestGitHubHost, []domain.TeamGitHubRepo{
			{Owner: "octo", Repo: "kept"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		kept, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef(keptSlug))
		if err != nil || kept == nil {
			t.Fatalf("resolve %s: got=%v err=%v", keptSlug, kept, err)
		}

		// A pull request in a tracked repository references the row tracking
		// minted, whatever casing its key spells.
		pr, created, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, "Octo/Kept#7", "", "pr", "a pr", "")
		if err != nil || !created {
			t.Fatalf("FindOrCreateSystem: created=%v err=%v", created, err)
		}
		if pr.RepositoryID != kept.ID {
			t.Errorf("created entity's repository = %q, want the tracked row %s", pr.RepositoryID, kept.ID)
		}
		if got, err := s.Entities.GetSystem(ctx, orgID, pr.ID); err != nil || got == nil || got.RepositoryID != kept.ID {
			t.Errorf("GetSystem = %+v, err=%v; want repository %s stored", got, err, kept.ID)
		}

		// A pull request in a repository nothing tracks gets a registry row of
		// its own, untracked, and a second one in that repository reuses it.
		// The request-path create resolves through the same door.
		first, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, "elsewhere/lib#1", "", "pr", "", "")
		if err != nil {
			t.Fatalf("FindOrCreateSystem (untracked): %v", err)
		}
		lib, err := s.Repos.GetByRefSystem(ctx, orgID, repoRef("elsewhere/lib"))
		if err != nil || lib == nil {
			t.Fatalf("registry row for elsewhere/lib = %v, err=%v; want the one the create minted", lib, err)
		}
		if first.RepositoryID != lib.ID {
			t.Errorf("untracked pr's repository = %q, want the minted row %s", first.RepositoryID, lib.ID)
		}
		second, _, err := s.Entities.FindOrCreate(ctx, orgID, "github", TestGitHubHost, "Elsewhere/Lib#2", "", "pr", "", "")
		if err != nil || second.RepositoryID != lib.ID {
			t.Errorf("FindOrCreate (second pr) repository = %q, err=%v; want the same row %s", second.RepositoryID, err, lib.ID)
		}
		names, err := s.Repos.ListTrackedNamesSystem(ctx, orgID, TestGitHubHost)
		if err != nil || len(names) != 1 || names[0] != keptSlug {
			t.Errorf("tracked names = %v, err=%v; want [%s] — an entity tracks nothing", names, err, keptSlug)
		}

		// A row found is returned as stored, and creates nothing.
		found, created, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, "elsewhere/lib#1", "", "pr", "", "")
		if err != nil || created || found.ID != first.ID || found.RepositoryID != lib.ID {
			t.Errorf("FindOrCreateSystem on a known key = (%+v, %v, %v), want the stored row", found, created, err)
		}
	})

	t.Run("A_github_entity_key_naming_no_repository_is_refused", func(t *testing.T) {
		s, orgID, _, _ := mk(t)
		for _, key := range []string{"no-slash#4", "/repo#4", "owner/#4", ""} {
			got, created, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost, key, "", "pr", "", "")
			if !errors.Is(err, db.ErrEntityRepositoryRequired) || got != nil || created {
				t.Errorf("FindOrCreateSystem(%q) = (%+v, %v, %v), want db.ErrEntityRepositoryRequired", key, got, created, err)
			}
		}
		if all, err := s.Repos.ListSystem(ctx, orgID, TestGitHubHost); err != nil || len(all) != 0 {
			t.Errorf("registry after the refusals = %+v, err=%v; want nothing minted", all, err)
		}
	})

	t.Run("Other_sources_reference_no_repository", func(t *testing.T) {
		s, orgID, _, _ := mk(t)
		issue, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, "https://jira.example.com", "OCTO-1", "10001", TestJiraProject, "", "")
		if err != nil {
			t.Fatalf("FindOrCreateSystem (jira): %v", err)
		}
		if issue.RepositoryID != "" {
			t.Errorf("jira entity's repository = %q, want none", issue.RepositoryID)
		}
		if got, err := s.Entities.GetSystem(ctx, orgID, issue.ID); err != nil || got == nil || got.RepositoryID != "" {
			t.Errorf("GetSystem (jira) = %+v, err=%v; want no repository stored", got, err)
		}
		if all, err := s.Repos.ListSystem(ctx, orgID, TestGitHubHost); err != nil || len(all) != 0 {
			t.Errorf("registry after a jira create = %+v, err=%v; want nothing minted", all, err)
		}
	})
}
