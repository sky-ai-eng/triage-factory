package dbtest

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubRefreshMarkFixture is one subtest's world for the refresh-mark store.
type GitHubRefreshMarkFixture struct {
	Stores db.Stores
	OrgID  string
	// OtherOrgID is a second org of the same database, or "" where the
	// dialect has only one (SQLite), which skips the cross-org cases.
	OtherOrgID string
}

// GitHubRefreshMarkFactory builds a fresh fixture per subtest.
type GitHubRefreshMarkFactory func(t *testing.T) GitHubRefreshMarkFixture

const (
	refreshSHA      = "0123456789abcdef0123456789abcdef01234567"
	refreshOtherSHA = "89abcdef0123456789abcdef0123456789abcdef"
)

// RunGitHubRefreshMarkConformance covers db.GitHubRefreshMarkStore: a mark
// returns the row it stored, deliveries naming one target collapse onto one
// row and bump its revision, distinct targets are distinct rows, a target in
// anything but its one spelling is refused, the list is oldest first and
// bounded, a clear deletes only the marks still at the revision it read, and
// a mark written in a system transaction commits and rolls back with the
// delivery recorded beside it.
func RunGitHubRefreshMarkConformance(t *testing.T, mk GitHubRefreshMarkFactory) {
	t.Helper()
	ctx := context.Background()

	repo := func(t *testing.T, f GitHubRefreshMarkFixture, name string) string {
		t.Helper()
		row, err := f.Stores.Repos.FindOrCreateByRefSystem(ctx, f.OrgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: name})
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		return row.ID
	}
	mark := func(t *testing.T, f GitHubRefreshMarkFixture, repositoryID string, target domain.GitHubRefreshTarget) domain.GitHubRefreshMark {
		t.Helper()
		m, err := f.Stores.GitHubRefreshMarks.MarkSystem(ctx, f.OrgID, repositoryID, target)
		if err != nil {
			t.Fatalf("MarkSystem(%+v): %v", target, err)
		}
		return m
	}
	list := func(t *testing.T, f GitHubRefreshMarkFixture) []domain.GitHubRefreshMark {
		t.Helper()
		got, err := f.Stores.GitHubRefreshMarks.ListSystem(ctx, f.OrgID, 100)
		if err != nil {
			t.Fatalf("ListSystem: %v", err)
		}
		return got
	}

	t.Run("Mark_returns_the_stored_row", func(t *testing.T) {
		f := mk(t)
		repoID := repo(t, f, "api")
		m := mark(t, f, repoID, domain.PullRequestRefreshTarget(7))
		if m.ID == "" || m.OrgID != f.OrgID || m.RepositoryID != repoID || m.Target != domain.PullRequestRefreshTarget(7) || m.Revision != 1 {
			t.Errorf("mark = %+v, want a revision-1 row for pull request 7 of %s", m, repoID)
		}
		if !m.FirstMarkedAt.Equal(m.MarkedAt) {
			t.Errorf("a new mark's FirstMarkedAt %v != MarkedAt %v", m.FirstMarkedAt, m.MarkedAt)
		}
		AssertWriteReturnedStoredRow(t, "MarkSystem (insert)", m, func() (*domain.GitHubRefreshMark, error) {
			return f.Stores.GitHubRefreshMarks.GetSystem(ctx, f.OrgID, m.ID)
		})
	})

	t.Run("Deliveries_naming_one_target_collapse_onto_one_row", func(t *testing.T) {
		f := mk(t)
		repoID := repo(t, f, "api")
		first := mark(t, f, repoID, domain.HeadSHARefreshTarget(refreshSHA))
		second := mark(t, f, repoID, domain.HeadSHARefreshTarget(refreshSHA))
		if second.ID != first.ID || second.Revision != 2 {
			t.Errorf("second mark = %+v, want row %s at revision 2", second, first.ID)
		}
		if !second.FirstMarkedAt.Equal(first.FirstMarkedAt) {
			t.Errorf("FirstMarkedAt moved from %v to %v; the first delivery's time is kept", first.FirstMarkedAt, second.FirstMarkedAt)
		}
		if second.MarkedAt.Before(first.MarkedAt) {
			t.Errorf("MarkedAt went back from %v to %v", first.MarkedAt, second.MarkedAt)
		}
		AssertWriteReturnedStoredRow(t, "MarkSystem (collapse)", second, func() (*domain.GitHubRefreshMark, error) {
			return f.Stores.GitHubRefreshMarks.GetSystem(ctx, f.OrgID, first.ID)
		})
		if got := list(t, f); len(got) != 1 {
			t.Errorf("marks = %+v, want the one row", got)
		}
	})

	t.Run("Distinct_targets_are_distinct_rows", func(t *testing.T) {
		f := mk(t)
		api, web := repo(t, f, "api"), repo(t, f, "web")
		targets := []struct {
			repositoryID string
			target       domain.GitHubRefreshTarget
		}{
			{api, domain.PullRequestRefreshTarget(7)},
			{api, domain.PullRequestRefreshTarget(8)},
			{api, domain.HeadSHARefreshTarget(refreshSHA)},
			{api, domain.HeadSHARefreshTarget(refreshOtherSHA)},
			{api, domain.BranchRefreshTarget("main")},
			// The same value under another kind is another target.
			{api, domain.BranchRefreshTarget("7")},
			// The same target in another repository is another mark.
			{web, domain.PullRequestRefreshTarget(7)},
		}
		ids := map[string]bool{}
		for _, c := range targets {
			ids[mark(t, f, c.repositoryID, c.target).ID] = true
		}
		if len(ids) != len(targets) {
			t.Errorf("%d targets landed on %d rows, want one row each", len(targets), len(ids))
		}
		if got := list(t, f); len(got) != len(targets) {
			t.Errorf("listed %d marks, want %d", len(got), len(targets))
		}
	})

	t.Run("Mark_refuses_a_target_outside_its_one_spelling", func(t *testing.T) {
		f := mk(t)
		repoID := repo(t, f, "api")
		for _, target := range []domain.GitHubRefreshTarget{
			{Kind: "issue", Value: "7"},
			{Kind: domain.GitHubRefreshPullRequest, Value: ""},
			{Kind: domain.GitHubRefreshPullRequest, Value: "07"},
			{Kind: domain.GitHubRefreshPullRequest, Value: "0"},
			{Kind: domain.GitHubRefreshPullRequest, Value: "-7"},
			{Kind: domain.GitHubRefreshPullRequest, Value: "+7"},
			{Kind: domain.GitHubRefreshHeadSHA, Value: strings.ToUpper(refreshSHA)},
			{Kind: domain.GitHubRefreshHeadSHA, Value: refreshSHA[:12]},
			{Kind: domain.GitHubRefreshBranch, Value: ""},
		} {
			if m, err := f.Stores.GitHubRefreshMarks.MarkSystem(ctx, f.OrgID, repoID, target); err == nil {
				t.Errorf("MarkSystem(%+v) = %+v, want a refusal", target, m)
			}
		}
		if got := list(t, f); len(got) != 0 {
			t.Errorf("refused marks wrote rows: %+v", got)
		}
	})

	t.Run("Mark_refuses_a_repository_with_no_row", func(t *testing.T) {
		f := mk(t)
		if m, err := f.Stores.GitHubRefreshMarks.MarkSystem(ctx, f.OrgID, unknownRepoID, domain.PullRequestRefreshTarget(7)); err == nil {
			t.Errorf("MarkSystem on a repository id no row carries = %+v, want a refusal", m)
		}
	})

	t.Run("List_is_oldest_first_and_bounded", func(t *testing.T) {
		f := mk(t)
		repoID := repo(t, f, "api")
		for n := 1; n <= 5; n++ {
			mark(t, f, repoID, domain.PullRequestRefreshTarget(n))
		}
		all := list(t, f)
		if len(all) != 5 {
			t.Fatalf("listed %d marks, want 5", len(all))
		}
		if !sort.SliceIsSorted(all, func(i, j int) bool {
			if !all[i].FirstMarkedAt.Equal(all[j].FirstMarkedAt) {
				return all[i].FirstMarkedAt.Before(all[j].FirstMarkedAt)
			}
			return all[i].ID < all[j].ID
		}) {
			t.Errorf("marks are not ordered by (FirstMarkedAt, ID): %+v", all)
		}
		page, err := f.Stores.GitHubRefreshMarks.ListSystem(ctx, f.OrgID, 2)
		if err != nil {
			t.Fatalf("ListSystem(limit 2): %v", err)
		}
		if len(page) != 2 || page[0].ID != all[0].ID || page[1].ID != all[1].ID {
			t.Errorf("ListSystem(limit 2) = %+v, want the two oldest marks", page)
		}
		for _, limit := range []int{0, -1} {
			if _, err := f.Stores.GitHubRefreshMarks.ListSystem(ctx, f.OrgID, limit); err == nil {
				t.Errorf("ListSystem(limit %d) succeeded, want a refusal", limit)
			}
		}
	})

	t.Run("Clear_deletes_only_marks_still_at_the_revision_read", func(t *testing.T) {
		// A delivery that lands while a pass runs collapses onto a mark the
		// pass already read. The pass may have refreshed before that change
		// happened, so clearing the row it read must not clear the new
		// delivery's mark with it.
		f := mk(t)
		repoID := repo(t, f, "api")
		mark(t, f, repoID, domain.PullRequestRefreshTarget(7))
		mark(t, f, repoID, domain.BranchRefreshTarget("main"))
		read := list(t, f)

		again := mark(t, f, repoID, domain.PullRequestRefreshTarget(7))
		late := mark(t, f, repoID, domain.HeadSHARefreshTarget(refreshSHA))

		n, err := f.Stores.GitHubRefreshMarks.ClearSystem(ctx, f.OrgID, read)
		if err != nil {
			t.Fatalf("ClearSystem: %v", err)
		}
		if n != 1 {
			t.Errorf("ClearSystem deleted %d marks, want 1 (the branch mark, untouched since the read)", n)
		}
		left := list(t, f)
		kept := map[string]int64{}
		for _, m := range left {
			kept[m.ID] = m.Revision
		}
		if len(left) != 2 || kept[again.ID] != 2 || kept[late.ID] != 1 {
			t.Errorf("marks left = %+v, want pull request 7 at revision 2 and the SHA mark written after the read", left)
		}

		// Clearing the same read again finds nothing at those revisions.
		if n, err := f.Stores.GitHubRefreshMarks.ClearSystem(ctx, f.OrgID, read); err != nil || n != 0 {
			t.Errorf("second ClearSystem of the same read = %d, %v; want 0, nil", n, err)
		}
		if n, err := f.Stores.GitHubRefreshMarks.ClearSystem(ctx, f.OrgID, nil); err != nil || n != 0 {
			t.Errorf("ClearSystem(nil) = %d, %v; want 0, nil", n, err)
		}
		// A mark cleared and then written again starts over.
		cleared := mark(t, f, repoID, domain.BranchRefreshTarget("main"))
		if cleared.Revision != 1 {
			t.Errorf("a target marked again after its clear = %+v, want a new row at revision 1", cleared)
		}
	})

	t.Run("Marked_orgs_are_the_orgs_holding_a_pending_mark", func(t *testing.T) {
		f := mk(t)
		marked := func() []string {
			t.Helper()
			got, err := f.Stores.GitHubRefreshMarks.ListMarkedOrgsSystem(ctx)
			if err != nil {
				t.Fatalf("ListMarkedOrgsSystem: %v", err)
			}
			return got
		}
		if got := marked(); len(got) != 0 {
			t.Fatalf("marked orgs before any mark = %v, want none", got)
		}
		repoID := repo(t, f, "api")
		mark(t, f, repoID, domain.PullRequestRefreshTarget(7))
		mark(t, f, repoID, domain.BranchRefreshTarget("main"))
		want := []string{f.OrgID}
		if f.OtherOrgID != "" {
			other, err := f.Stores.Repos.FindOrCreateByRefSystem(ctx, f.OtherOrgID, domain.RepoRef{Host: TestGitHubHost, Owner: "octo", Repo: "api"})
			if err != nil {
				t.Fatalf("register the other org's repository: %v", err)
			}
			if _, err := f.Stores.GitHubRefreshMarks.MarkSystem(ctx, f.OtherOrgID, other.ID, domain.PullRequestRefreshTarget(7)); err != nil {
				t.Fatalf("mark the other org's repository: %v", err)
			}
			want = append(want, f.OtherOrgID)
			sort.Strings(want)
		}
		if got := marked(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("marked orgs = %v, want %v, once each in id order", got, want)
		}
		if _, err := f.Stores.GitHubRefreshMarks.ClearSystem(ctx, f.OrgID, list(t, f)); err != nil {
			t.Fatalf("ClearSystem: %v", err)
		}
		want = want[:0]
		if f.OtherOrgID != "" {
			want = append(want, f.OtherOrgID)
		}
		if got := marked(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("marked orgs after clearing the org's marks = %v, want %v", got, want)
		}
	})

	t.Run("Reads_and_clears_are_scoped_to_the_org", func(t *testing.T) {
		f := mk(t)
		if f.OtherOrgID == "" {
			t.Skip("one org in this dialect")
		}
		repoID := repo(t, f, "api")
		m := mark(t, f, repoID, domain.PullRequestRefreshTarget(7))
		if got, err := f.Stores.GitHubRefreshMarks.GetSystem(ctx, f.OtherOrgID, m.ID); err != nil || got != nil {
			t.Errorf("GetSystem from another org = %+v, %v; want (nil, nil)", got, err)
		}
		if got, err := f.Stores.GitHubRefreshMarks.ListSystem(ctx, f.OtherOrgID, 100); err != nil || len(got) != 0 {
			t.Errorf("ListSystem of another org = %+v, %v; want none", got, err)
		}
		if n, err := f.Stores.GitHubRefreshMarks.ClearSystem(ctx, f.OtherOrgID, []domain.GitHubRefreshMark{m}); err != nil || n != 0 {
			t.Errorf("ClearSystem from another org = %d, %v; want 0, nil", n, err)
		}
		if m2, err := f.Stores.GitHubRefreshMarks.MarkSystem(ctx, f.OtherOrgID, repoID, domain.PullRequestRefreshTarget(8)); err == nil {
			t.Errorf("MarkSystem on another org's repository = %+v, want a refusal", m2)
		}
		if got := list(t, f); len(got) != 1 {
			t.Errorf("marks = %+v, want the one mark untouched", got)
		}
	})

	t.Run("A_mark_commits_and_rolls_back_with_its_delivery", func(t *testing.T) {
		// The receiver records a delivery and writes its marks in one system
		// transaction: a delivery is never recorded as handled without its
		// marks, and a failed mark leaves the delivery for a redelivery.
		f := mk(t)
		repoID := repo(t, f, "api")
		record := func(deliveryID string, target domain.GitHubRefreshTarget, fail error) error {
			return f.Stores.Tx.SystemWithTx(ctx, f.OrgID, func(tx db.SystemTxStores) error {
				fresh, err := tx.GitHubDeliveries.MarkDeliveredSystem(ctx, "4242", deliveryID)
				if err != nil {
					return err
				}
				if !fresh {
					return errors.New("delivery already recorded")
				}
				if _, err := tx.GitHubRefreshMarks.MarkSystem(ctx, f.OrgID, repoID, target); err != nil {
					return err
				}
				return fail
			})
		}

		if err := record("committed", domain.PullRequestRefreshTarget(7), nil); err != nil {
			t.Fatalf("committing transaction: %v", err)
		}
		boom := errors.New("boom")
		if err := record("rolled-back", domain.PullRequestRefreshTarget(8), boom); !errors.Is(err, boom) {
			t.Fatalf("failing transaction = %v, want its own error", err)
		}
		if err := record("refused-mark", domain.GitHubRefreshTarget{Kind: domain.GitHubRefreshPullRequest, Value: "07"}, nil); err == nil {
			t.Fatal("a transaction whose mark is refused committed")
		}

		got := list(t, f)
		if len(got) != 1 || got[0].Target != domain.PullRequestRefreshTarget(7) {
			t.Errorf("marks = %+v, want only the committed transaction's", got)
		}
		for deliveryID, wantFresh := range map[string]bool{"committed": false, "rolled-back": true, "refused-mark": true} {
			fresh, err := f.Stores.GitHubDeliveries.MarkDeliveredSystem(ctx, "4242", deliveryID)
			if err != nil {
				t.Fatalf("MarkDeliveredSystem(%s): %v", deliveryID, err)
			}
			if fresh != wantFresh {
				t.Errorf("delivery %s fresh = %v after its transaction, want %v", deliveryID, fresh, wantFresh)
			}
		}
	})
}
