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

	t.Run("Upsert_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard: what the write handed back is what a point
		// read finds. ov carries no updated_at, and the empty pin it is handed
		// is stored as NULL — the row and the argument disagree by design.
		store, repos, orgID := mk(t)
		repo := register(t, repos, orgID, TestGitHubHost, "acme", "ret")
		read := func() (*domain.PlacementOverride, error) { return store.Get(ctx, orgID, repo) }

		first, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, RepositoryID: repo, Replicas: 2})
		if err != nil {
			t.Fatalf("Upsert (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert (insert)", first, read)
		if first.UpdatedAt.IsZero() || first.PinnedInstanceID != "" || first.RepositoryID != repo {
			t.Errorf("Upsert returned %+v, want repository %s, a stamped updated_at and no pin", first, repo)
		}

		replaced, err := store.Upsert(ctx, domain.PlacementOverride{
			OrgID: orgID, RepositoryID: repo, PinnedInstanceID: "exec-9", Replicas: 5,
		})
		if err != nil {
			t.Fatalf("Upsert (replace): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Upsert (replace)", replaced, read)
		if replaced.PinnedInstanceID != "exec-9" || replaced.Replicas != 5 {
			t.Errorf("Upsert (replace) returned %+v, want the replaced pin and replica count", replaced)
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

	t.Run("upsert_pin_roundtrips", func(t *testing.T) {
		store, repos, orgID := mk(t)
		repo := register(t, repos, orgID, TestGitHubHost, "acme", "web")
		if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, RepositoryID: repo, PinnedInstanceID: "exec-7"}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, err := store.Get(ctx, orgID, repo)
		if err != nil || got == nil {
			t.Fatalf("get: %v (got %+v)", err, got)
		}
		if got.PinnedInstanceID != "exec-7" || got.Replicas != 0 || got.OrgID != orgID || got.RepositoryID != repo {
			t.Fatalf("roundtrip mismatch: %+v", got)
		}
		if got.UpdatedAt.IsZero() {
			t.Fatalf("updated_at not stamped")
		}
	})

	t.Run("upsert_replaces_wholesale", func(t *testing.T) {
		store, repos, orgID := mk(t)
		repo := register(t, repos, orgID, TestGitHubHost, "acme", "mono")
		if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, RepositoryID: repo, PinnedInstanceID: "exec-1"}); err != nil {
			t.Fatalf("upsert pin: %v", err)
		}
		// Re-upsert the same repository with a replica count and no pin — must
		// replace, not merge (pin cleared, replicas set).
		if _, err := store.Upsert(ctx, domain.PlacementOverride{OrgID: orgID, RepositoryID: repo, Replicas: 3}); err != nil {
			t.Fatalf("upsert replicas: %v", err)
		}
		got, err := store.Get(ctx, orgID, repo)
		if err != nil || got == nil {
			t.Fatalf("get: %v (%+v)", err, got)
		}
		if got.PinnedInstanceID != "" {
			t.Fatalf("pin should have been replaced away, got %q", got.PinnedInstanceID)
		}
		if got.Replicas != 3 {
			t.Fatalf("replicas = %d, want 3", got.Replicas)
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

		matched, err := store.Delete(ctx, orgID, repo)
		if err != nil || !matched {
			t.Fatalf("first delete: matched=%v err=%v", matched, err)
		}
		matched, err = store.Delete(ctx, orgID, repo)
		if err != nil {
			t.Fatalf("second delete err: %v", err)
		}
		if matched {
			t.Fatalf("second delete should report matched=false")
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
