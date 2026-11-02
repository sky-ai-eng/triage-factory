package dbtest

import (
	"cmp"
	"context"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TeamListSeeder stages the orgs and teams the suite lists. Org and team
// creation have no store methods, so each backend implements them against its
// own schema; archiving goes through the store.
type TeamListSeeder struct {
	// Org inserts an org row and returns its ID.
	Org func(t *testing.T) string
	// Team inserts a team row in orgID and returns its ID.
	Team func(t *testing.T, orgID string) string
}

// TeamListFactory is what a per-backend test file hands to
// RunTeamListForOrgConformance: a fresh, isolated store plus the seeder.
type TeamListFactory func(t *testing.T) (db.TeamsStore, TeamListSeeder)

// RunTeamListForOrgConformance is the shared assertion suite for
// TeamsStore.ListForOrg. It pins:
//
//   - the set: every active team in the org, archived teams and another
//     org's teams left out.
//   - the ordering: creation time, then id, so offset paging neither drops
//     nor repeats a row.
//   - paging: (items, total, err), where total is the unpaged count on every
//     page, the pages partition the list, an offset past the end is an empty
//     page, and a count-only read returns the total with no items.
//
// Role is not pinned: the backends answer it from different identities (the
// sole local user owns every team; Postgres reads the caller's membership),
// and each backend's own tests pin it.
func RunTeamListForOrgConformance(t *testing.T, mk TeamListFactory) {
	t.Helper()
	ctx := context.Background()

	teams, seed := mk(t)
	org := seed.Org(t)
	var active []string
	for range 4 {
		active = append(active, seed.Team(t, org))
	}
	archived := seed.Team(t, org)
	if _, err := teams.Archive(ctx, archived); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	other := seed.Team(t, seed.Org(t))

	all, total, err := teams.ListForOrg(ctx, org, db.ListOpts{Limit: 50})
	if err != nil {
		t.Fatalf("ListForOrg: %v", err)
	}
	ids := teamIDs(all)
	if total != len(active) || len(ids) != len(active) {
		t.Fatalf("ListForOrg = %v (total %d); want the %d active teams", ids, total, len(active))
	}
	for _, id := range active {
		if !slices.Contains(ids, id) {
			t.Errorf("active team %s not listed in %v", id, ids)
		}
	}
	if slices.Contains(ids, archived) {
		t.Errorf("archived team %s listed", archived)
	}
	if slices.Contains(ids, other) {
		t.Errorf("another org's team %s listed", other)
	}
	for _, tm := range all {
		if tm.OrgID != org {
			t.Errorf("team %s carries org %s; want %s", tm.ID, tm.OrgID, org)
		}
	}
	if !slices.IsSortedFunc(all, func(a, b domain.Team) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	}) {
		t.Errorf("ListForOrg = %v; want creation order, then id", ids)
	}

	var walked []string
	for offset := 0; offset < len(active); offset += 3 {
		page, pageTotal, err := teams.ListForOrg(ctx, org, db.ListOpts{Limit: 3, Offset: offset})
		if err != nil {
			t.Fatalf("ListForOrg page at %d: %v", offset, err)
		}
		if pageTotal != len(active) {
			t.Errorf("page at %d total = %d; want %d", offset, pageTotal, len(active))
		}
		walked = append(walked, teamIDs(page)...)
	}
	if !slices.Equal(walked, ids) {
		t.Errorf("paged walk = %v; want %v", walked, ids)
	}
	past, pastTotal, err := teams.ListForOrg(ctx, org, db.ListOpts{Limit: 3, Offset: 10})
	if err != nil || len(past) != 0 || pastTotal != len(active) {
		t.Errorf("page past the end = %v (total %d), %v; want an empty page with total %d", teamIDs(past), pastTotal, err, len(active))
	}
	counted, countTotal, err := teams.ListForOrg(ctx, org, db.ListOpts{CountOnly: true})
	if err != nil || len(counted) != 0 || countTotal != len(active) {
		t.Errorf("count-only = %v (total %d), %v; want no items with total %d", teamIDs(counted), countTotal, err, len(active))
	}
}

func teamIDs(ts []domain.Team) []string {
	out := make([]string, len(ts))
	for i, tm := range ts {
		out[i] = tm.ID
	}
	return out
}
