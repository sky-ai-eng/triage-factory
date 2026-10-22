package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PlacementOverrideStoreFactory hands the conformance suite a wired store,
// the repository store whose rows an override references, and the orgID to
// scope every call.
type PlacementOverrideStoreFactory func(t *testing.T) (store db.PlacementOverrideStore, repos db.RepositoryStore, orgID string)

// unregisteredRepositoryID is a well-formed registry id no row carries.
const unregisteredRepositoryID = "7d1c3a52-0b8e-4f39-9e1a-6c2f4d8b0a11"

// RunPlacementOverrideStoreConformance covers the contract every backend impl
// must hold: absence reads as (nil, nil) not an error; upsert then get
// round-trips a pin and a replica count; upsert is replace-wholesale keyed on
// (org, repository row); two repositories under one name on two hosts have
// overrides of their own; an override needs a registry row; list is
// org-scoped and ordered by the repositories' names; delete reports matched
// and is idempotent.
func RunPlacementOverrideStoreConformance(t *testing.T, mk PlacementOverrideStoreFactory) {
	t.Helper()
	ctx := context.Background()

	register := func(t *testing.T, repos db.RepositoryStore, orgID, host, owner, repo string) string {
		t.Helper()
		row, err := repos.UpsertSystem(ctx, orgID, domain.Repository{Host: host, Owner: owner, Repo: repo})
		if err != nil {
			t.Fatalf("register %s/%s on %s: %v", owner, repo, host, err)
		}
		return row.ID
	}

	t.Run("Upsert_replaces_wholesale_and_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard: what the write handed back is what a point
		// read finds. ov carries no updated_at, and an empty pin is stored as
		// NULL — the row and the argument disagree by design. Each write
		// replaces the override, so a pin the next one leaves out is cleared.
		store, repos, orgID := mk(t)
		repo := register(t, repos, orgID, TestGitHubHost, "acme", "ret")
		read := func() (*domain.PlacementOverride, error) { return store.Get(ctx, orgID, repo) }
		for _, ov := range []domain.PlacementOverride{
			{OrgID: orgID, RepositoryID: repo, PinnedInstanceID: "exec-7"},
			{OrgID: orgID, RepositoryID: repo, Replicas: 3},
			{OrgID: orgID, RepositoryID: repo, PinnedInstanceID: "exec-9", Replicas: 5},
		} {
			got, err := store.Upsert(ctx, ov)
			if err != nil {
				t.Fatalf("Upsert(%+v): %v", ov, err)
			}
			AssertWriteReturnedStoredRow(t, "Upsert", got, read)
			if got.OrgID != orgID || got.RepositoryID != repo || got.PinnedInstanceID != ov.PinnedInstanceID ||
				got.Replicas != ov.Replicas || got.UpdatedAt.IsZero() {
				t.Errorf("Upsert(%+v) returned %+v, want exactly its pin and replica count, stamped", ov, got)
			}
		}
	})

	t.Run("absent_get_is_nil_not_error", func(t *testing.T) {
		store, repos, orgID := mk(t)
		repo := register(t, repos, orgID, TestGitHubHost, "acme", "ghost")
		for _, id := range []string{repo, unregisteredRepositoryID, "acme/ghost", ""} {
			ov, err := store.Get(ctx, orgID, id)
			if err != nil {
				t.Fatalf("get %q: %v", id, err)
			}
			if ov != nil {
				t.Fatalf("absent override for %q must be nil, got %+v", id, ov)
			}
		}
	})

	t.Run("list_is_ordered_by_the_repositories_names", func(t *testing.T) {
		store, repos, orgID := mk(t)
		two := register(t, repos, orgID, TestGitHubHost, "b", "two")
		one := register(t, repos, orgID, TestGitHubHost, "A", "One")
		ghe := register(t, repos, orgID, TestOtherGitHubHost, "a", "zero")
		register(t, repos, orgID, TestGitHubHost, "a", "unpinned")
		for _, ov := range []domain.PlacementOverride{
			{OrgID: orgID, RepositoryID: two, Replicas: 2},
			{OrgID: orgID, RepositoryID: one, PinnedInstanceID: "x"},
			{OrgID: orgID, RepositoryID: ghe, Replicas: 4},
		} {
			if _, err := store.Upsert(ctx, ov); err != nil {
				t.Fatalf("upsert %s: %v", ov.RepositoryID, err)
			}
		}

		list, err := store.List(ctx, orgID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var got []string
		for _, ov := range list {
			got = append(got, ov.RepositoryID)
		}
		// Ordered by host, then the name case-folded: A/One before b/two on
		// one host, and the other host's a/zero on whichever side its host
		// sorts.
		want := []string{one, two, ghe}
		if TestOtherGitHubHost < TestGitHubHost {
			want = []string{ghe, one, two}
		}
		if !equalStringSlice(got, want) {
			t.Fatalf("list = %v, want %v (A/One then b/two on %s, a/zero on %s)", got, want, TestGitHubHost, TestOtherGitHubHost)
		}
	})

	t.Run("delete_reports_matched_and_is_idempotent", func(t *testing.T) {
		store, repos, orgID := mk(t)
		repo := register(t, repos, orgID, TestGitHubHost, "acme", "del")
		if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, RepositoryID: repo, PinnedInstanceID: "z"}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		if matched, err := store.Delete(ctx, orgID, repo); err != nil || !matched {
			t.Fatalf("first delete: matched=%v err=%v", matched, err)
		}
		if matched, err := store.Delete(ctx, orgID, repo); err != nil || matched {
			t.Fatalf("second delete: matched=%v err=%v, want no match", matched, err)
		}
		if got, _ := store.Get(ctx, orgID, repo); got != nil {
			t.Fatalf("row should be gone after delete, got %+v", got)
		}
		if matched, err := store.Delete(ctx, orgID, "acme/del"); err != nil || matched {
			t.Fatalf("delete by a name: matched=%v err=%v, want no match — an override is keyed by registry id", matched, err)
		}
	})

	t.Run("one_name_on_two_hosts_is_two_overrides", func(t *testing.T) {
		// An owner/repo names a repository only within one GitHub host, and
		// each host's repository is its own registry row, so a pin on one must
		// neither answer for nor be replaced or cleared by a write on the
		// other.
		store, repos, orgID := mk(t)
		dotcom := register(t, repos, orgID, TestGitHubHost, "acme", "api")
		ghe := register(t, repos, orgID, TestOtherGitHubHost, "acme", "api")
		if dotcom == ghe {
			t.Fatalf("one registry row %s answers for acme/api on both hosts", dotcom)
		}
		if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, RepositoryID: dotcom, PinnedInstanceID: "exec-dotcom"}); err != nil {
			t.Fatalf("upsert on %s: %v", TestGitHubHost, err)
		}
		if got, err := store.Get(ctx, orgID, ghe); err != nil || got != nil {
			t.Fatalf("Get(%s's row) = %+v, %v; want no override — the pin is %s's", TestOtherGitHubHost, got, err, TestGitHubHost)
		}
		if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, RepositoryID: ghe, Replicas: 4}); err != nil {
			t.Fatalf("upsert on %s: %v", TestOtherGitHubHost, err)
		}

		if got, err := store.Get(ctx, orgID, dotcom); err != nil || got == nil || got.PinnedInstanceID != "exec-dotcom" {
			t.Fatalf("Get(%s's row) = %+v, %v; want its own pin, untouched by the other host's write", TestGitHubHost, got, err)
		}
		if got, err := store.Get(ctx, orgID, ghe); err != nil || got == nil || got.Replicas != 4 || got.PinnedInstanceID != "" {
			t.Fatalf("Get(%s's row) = %+v, %v; want its own replica count", TestOtherGitHubHost, got, err)
		}
		if matched, err := store.Delete(ctx, orgID, ghe); err != nil || !matched {
			t.Fatalf("delete on %s: matched=%v err=%v", TestOtherGitHubHost, matched, err)
		}
		if got, _ := store.Get(ctx, orgID, dotcom); got == nil {
			t.Errorf("deleting %s's override removed %s's", TestOtherGitHubHost, TestGitHubHost)
		}
	})

	t.Run("an_override_needs_a_registry_row", func(t *testing.T) {
		// An override is a repository's, and nothing ranks a repository TF
		// holds no row for, so a write naming no row is refused and leaves
		// nothing behind — a name in place of the id included.
		store, _, orgID := mk(t)
		for _, id := range []string{unregisteredRepositoryID, "acme/nohost", ""} {
			if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, RepositoryID: id, Replicas: 2}); !errors.Is(err, db.ErrNoSuchRepository) {
				t.Errorf("Upsert(%q): err = %v, want ErrNoSuchRepository", id, err)
			}
		}
		list, err := store.List(ctx, orgID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("a refused write left a row: %+v", list)
		}
	})
}
