package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepositoryDependabotAccessFixture is one subtest's world for the
// Dependabot-access store.
type RepositoryDependabotAccessFixture struct {
	Stores db.Stores
	OrgID  string
	TeamID string
	// OtherOrgID is a second org of the same database, or "" where the
	// dialect has only one (SQLite), which skips the cross-org case.
	OtherOrgID string
}

// RepositoryDependabotAccessFactory builds a fresh fixture per subtest.
type RepositoryDependabotAccessFactory func(t *testing.T) RepositoryDependabotAccessFixture

// RunRepositoryDependabotAccessConformance covers
// db.RepositoryDependabotAccessStore: a record returns the row it stored and
// a later one replaces it, a value outside the three answers is refused, the
// list is confined to the host's live rows, and the count covers the tracked
// repositories only, an unprobed one counting as unchecked.
func RunRepositoryDependabotAccessConformance(t *testing.T, mk RepositoryDependabotAccessFactory) {
	t.Helper()
	ctx := context.Background()

	repo := func(t *testing.T, f RepositoryDependabotAccessFixture, host, name string) string {
		t.Helper()
		row, err := f.Stores.Repos.FindOrCreateByRefSystem(ctx, f.OrgID, domain.RepoRef{Host: host, Owner: "octo", Repo: name})
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		return row.ID
	}
	record := func(t *testing.T, f RepositoryDependabotAccessFixture, repositoryID string, access domain.DependabotAlertsAccess) domain.RepositoryDependabotAccess {
		t.Helper()
		a, err := f.Stores.DependabotAccess.RecordSystem(ctx, f.OrgID, repositoryID, access)
		if err != nil {
			t.Fatalf("RecordSystem(%s): %v", access, err)
		}
		return a
	}

	t.Run("Record_returns_the_stored_row", func(t *testing.T) {
		f := mk(t)
		repoID := repo(t, f, TestGitHubHost, "api")
		a := record(t, f, repoID, domain.DependabotAlertsReadable)
		if a.RepositoryID != repoID || a.OrgID != f.OrgID || a.Access != domain.DependabotAlertsReadable || a.CheckedAt.IsZero() {
			t.Errorf("record = %+v, want a readable answer for %s", a, repoID)
		}
		AssertWriteReturnedStoredRow(t, "RecordSystem (insert)", a, func() (*domain.RepositoryDependabotAccess, error) {
			return f.Stores.DependabotAccess.GetSystem(ctx, f.OrgID, repoID)
		})
	})

	t.Run("A_later_answer_replaces_the_earlier_one", func(t *testing.T) {
		f := mk(t)
		repoID := repo(t, f, TestGitHubHost, "api")
		first := record(t, f, repoID, domain.DependabotAlertsNoPermission)
		second := record(t, f, repoID, domain.DependabotAlertsDisabled)
		if second.Access != domain.DependabotAlertsDisabled {
			t.Errorf("second record = %+v, want disabled", second)
		}
		if second.CheckedAt.Before(first.CheckedAt) {
			t.Errorf("CheckedAt went back from %v to %v", first.CheckedAt, second.CheckedAt)
		}
		AssertWriteReturnedStoredRow(t, "RecordSystem (replace)", second, func() (*domain.RepositoryDependabotAccess, error) {
			return f.Stores.DependabotAccess.GetSystem(ctx, f.OrgID, repoID)
		})
		got, err := f.Stores.DependabotAccess.ListSystem(ctx, f.OrgID, TestGitHubHost)
		if err != nil {
			t.Fatalf("ListSystem: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("answers = %+v, want the one row", got)
		}
	})

	t.Run("An_unknown_answer_is_refused", func(t *testing.T) {
		f := mk(t)
		repoID := repo(t, f, TestGitHubHost, "api")
		if _, err := f.Stores.DependabotAccess.RecordSystem(ctx, f.OrgID, repoID, "maybe"); err == nil {
			t.Error("RecordSystem accepted an answer outside the three")
		}
		if got, err := f.Stores.DependabotAccess.GetSystem(ctx, f.OrgID, repoID); err != nil || got != nil {
			t.Errorf("GetSystem = %+v, %v; want no row", got, err)
		}
	})

	t.Run("List_is_confined_to_the_host", func(t *testing.T) {
		f := mk(t)
		here := repo(t, f, TestGitHubHost, "api")
		there := repo(t, f, TestOtherGitHubHost, "api")
		record(t, f, here, domain.DependabotAlertsReadable)
		record(t, f, there, domain.DependabotAlertsReadable)
		got, err := f.Stores.DependabotAccess.ListSystem(ctx, f.OrgID, TestGitHubHost)
		if err != nil {
			t.Fatalf("ListSystem: %v", err)
		}
		if len(got) != 1 || got[0].RepositoryID != here {
			t.Errorf("answers on %s = %+v, want only %s", TestGitHubHost, got, here)
		}
		if _, err := f.Stores.DependabotAccess.ListSystem(ctx, f.OrgID, ""); err == nil {
			t.Error("ListSystem accepted an empty host")
		}
	})

	t.Run("Count_covers_the_tracked_repositories", func(t *testing.T) {
		f := mk(t)
		readable := repo(t, f, TestGitHubHost, "readable")
		noPermission := repo(t, f, TestGitHubHost, "no-permission")
		disabled := repo(t, f, TestGitHubHost, "disabled")
		unchecked := repo(t, f, TestGitHubHost, "unchecked")
		untracked := repo(t, f, TestGitHubHost, "untracked")
		if err := f.Stores.TeamGitHubRepos.ReplaceForTeam(ctx, f.OrgID, f.TeamID, TestGitHubHost,
			[]string{readable, noPermission, disabled, unchecked}); err != nil {
			t.Fatalf("track: %v", err)
		}
		record(t, f, readable, domain.DependabotAlertsReadable)
		record(t, f, noPermission, domain.DependabotAlertsNoPermission)
		record(t, f, disabled, domain.DependabotAlertsDisabled)
		// An answer for a repository no team tracks is not counted.
		record(t, f, untracked, domain.DependabotAlertsNoPermission)

		got, err := f.Stores.DependabotAccess.CountTrackedSystem(ctx, f.OrgID, TestGitHubHost)
		if err != nil {
			t.Fatalf("CountTrackedSystem: %v", err)
		}
		want := domain.DependabotAccessCounts{Readable: 1, NoPermission: 1, Disabled: 1, Unchecked: 1}
		if got != want {
			t.Errorf("counts = %+v, want %+v", got, want)
		}

		other, err := f.Stores.DependabotAccess.CountTrackedSystem(ctx, f.OrgID, TestOtherGitHubHost)
		if err != nil {
			t.Fatalf("CountTrackedSystem (other host): %v", err)
		}
		if other != (domain.DependabotAccessCounts{}) {
			t.Errorf("counts on %s = %+v, want none", TestOtherGitHubHost, other)
		}
	})

	t.Run("Another_orgs_answer_is_not_read", func(t *testing.T) {
		f := mk(t)
		if f.OtherOrgID == "" {
			t.Skip("one org in this dialect")
		}
		repoID := repo(t, f, TestGitHubHost, "api")
		record(t, f, repoID, domain.DependabotAlertsReadable)
		if got, err := f.Stores.DependabotAccess.GetSystem(ctx, f.OtherOrgID, repoID); err != nil || got != nil {
			t.Errorf("GetSystem under another org = %+v, %v; want no row", got, err)
		}
		if _, err := f.Stores.DependabotAccess.RecordSystem(ctx, f.OtherOrgID, repoID, domain.DependabotAlertsDisabled); err == nil {
			t.Error("RecordSystem wrote an answer for another org's repository")
		}
		if got, err := f.Stores.DependabotAccess.GetSystem(ctx, f.OrgID, repoID); err != nil || got == nil || got.Access != domain.DependabotAlertsReadable {
			t.Errorf("the org's own answer after another org's write = %+v, %v; want readable, untouched", got, err)
		}
	})
}
