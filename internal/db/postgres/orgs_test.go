package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestOrgsStore_Postgres_ListActiveSystem_ExcludesSoftDeleted pins the
// admin-pool ListActiveSystem behavior:
//   - returns every orgs row with deleted_at IS NULL
//   - filters out soft-deleted rows (deleted_at IS NOT NULL)
//   - returns ids in ascending order so per-org iteration is stable
//     across poll cycles
//
// This is the contract the background-service callers (poller,
// tracker, projectclassify, repoprofile) iterate at the top of each
// cycle.
func TestOrgsStore_Postgres_ListActiveSystem_ExcludesSoftDeleted(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForAgents(t, h)
	orgB := seedPgOrgForAgents(t, h)
	orgDeleted := seedPgOrgForAgents(t, h)
	if _, err := h.AdminDB.Exec(
		`UPDATE orgs SET deleted_at = now() WHERE id = $1`, orgDeleted,
	); err != nil {
		t.Fatalf("soft-delete org: %v", err)
	}

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	got, err := stores.Orgs.ListActiveSystem(context.Background())
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	gotSet := make(map[string]bool, len(got))
	for _, id := range got {
		gotSet[id] = true
	}
	if !gotSet[orgA] || !gotSet[orgB] {
		t.Errorf("ListActiveSystem missing active orgs (got=%v, want both %s and %s)", got, orgA, orgB)
	}
	if gotSet[orgDeleted] {
		t.Errorf("ListActiveSystem leaked soft-deleted org %s; got=%v", orgDeleted, got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("ListActiveSystem ids not ascending at index %d: %s < %s", i, got[i], got[i-1])
			break
		}
	}
}
