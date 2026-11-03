package pgtest

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The marketplace browse read is Postgres-only — local mode has no
// marketplace, and its SQLite store answers ErrNotApplicableInLocal — so its
// predicate and order are pinned here rather than in a dbtest suite run on
// both dialects.

// TestMarketplaceList_SearchIsALiteralSubstring: the needle matches the name or
// the description case-insensitively, and its LIKE metacharacters match
// themselves.
func TestMarketplaceList_SearchIsALiteralSubstring(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	orgID, alice, teamID := SeedOrgWithUser(t, h, "alice")

	publish := func(name, description string) string {
		t.Helper()
		var id string
		if err := h.WithUser(t, alice, orgID, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, SecretKey).Marketplace.Publish(t.Context(), orgID, domain.MarketplaceListing{
				Kind: domain.ListingKindPrompt, Name: name, Description: description, PublisherTeamID: teamID,
			}, marketplaceSnapshot(name))
			id = got
			return err
		}); err != nil {
			t.Fatalf("publish %q: %v", name, err)
		}
		return id
	}
	coverage := publish("Coverage", "Keeps coverage at 100%")
	thousand := publish("Thousand", "1000 runs a day")
	snake := publish("snake_case", "")
	publish("snakeXcase", "")

	for _, tc := range []struct {
		search string
		want   []string
	}{
		{"100%", []string{coverage}},
		{"snake_", []string{snake}},
		{"KEEPS", []string{coverage}},
		{"thou", []string{thousand}},
		{"", nil}, // every listing
	} {
		var got []string
		var total int
		if err := h.WithUser(t, alice, orgID, func(tx *sql.Tx) error {
			rows, n, err := pgstore.NewForTx(tx, SecretKey).Marketplace.List(t.Context(), orgID, alice,
				domain.ListingFilter{Search: tc.search}, db.ListOpts{Limit: 50})
			for _, r := range rows {
				got = append(got, r.ID)
			}
			total = n
			return err
		}); err != nil {
			t.Fatalf("list %q: %v", tc.search, err)
		}
		if tc.want == nil {
			if len(got) != 4 || total != 4 {
				t.Errorf("search %q = %d rows (total %d); want all 4", tc.search, len(got), total)
			}
			continue
		}
		slices.Sort(got)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if !slices.Equal(got, want) || total != len(want) {
			t.Errorf("search %q = %v (total %d); want %v", tc.search, got, total, want)
		}
	}
}

// TestMarketplaceList_SortDirection: each key orders in the direction asked,
// and a count with no rows behind it sorts as zero — last descending, first
// ascending — with equal counts ordered by the most recently updated.
func TestMarketplaceList_SortDirection(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	orgID, alice, teamID := SeedOrgWithUser(t, h, "alice")
	older := publishAsTeamWriter(t, h, orgID, alice, teamID, "Older", "")
	newer := publishAsTeamWriter(t, h, orgID, alice, teamID, "Newer", "")
	MustExec(t, h.AdminDB, `UPDATE marketplace_listings SET updated_at = now() - interval '1 hour' WHERE id = $1`, older)
	MustExec(t, h.AdminDB, `UPDATE marketplace_listings SET updated_at = now() WHERE id = $1`, newer)

	list := func(key, dir string) []string {
		t.Helper()
		var ids []string
		if err := h.WithUser(t, alice, orgID, func(tx *sql.Tx) error {
			rows, _, err := pgstore.NewForTx(tx, SecretKey).Marketplace.List(t.Context(), orgID, alice,
				domain.ListingFilter{SortKey: key, SortDir: dir}, db.ListOpts{Limit: 50})
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			return err
		}); err != nil {
			t.Fatalf("list %s %s: %v", key, dir, err)
		}
		return ids
	}

	// No installs, votes or stats yet: every count is zero and the tie orders
	// by recency in both directions.
	for _, key := range []string{domain.ListingSortInstalls, domain.ListingSortVotes, domain.ListingSortMostUsed} {
		for _, dir := range []string{db.SortDesc, db.SortAsc} {
			if got := list(key, dir); !slices.Equal(got, []string{newer, older}) {
				t.Errorf("%s %s with no counts = %v; want [newer older]", key, dir, got)
			}
		}
	}
	// One stats row: older has runs, newer has no row at all.
	MustExec(t, h.AdminDB, `
		INSERT INTO marketplace_listing_stats (listing_id, org_id, teams_using, total_runs, computed_at)
		VALUES ($1, $2, 1, 4, now())`, older, orgID)
	if got := list(domain.ListingSortMostUsed, db.SortDesc); !slices.Equal(got, []string{older, newer}) {
		t.Errorf("used desc = %v; want [older newer] — 4 runs, then no stats row", got)
	}
	if got := list(domain.ListingSortMostUsed, db.SortAsc); !slices.Equal(got, []string{newer, older}) {
		t.Errorf("used asc = %v; want [newer older] — no stats row is zero runs", got)
	}
	if got := list(domain.ListingSortRecent, db.SortAsc); !slices.Equal(got, []string{older, newer}) {
		t.Errorf("recent asc = %v; want [older newer]", got)
	}
	// An empty key is the store's own default: most recently updated first.
	if got := list("", ""); !slices.Equal(got, []string{newer, older}) {
		t.Errorf("default = %v; want [newer older]", got)
	}
}
