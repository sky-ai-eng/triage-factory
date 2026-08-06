package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestEventQueueStore_Postgres runs the shared conformance suite against
// the Postgres EventQueueStore impl. Wires the admin pool (BYPASSRLS) so
// behavior tests stay independent of the auth path; the cross-org test
// below exercises the org_id defense-in-depth filter directly.
func TestEventQueueStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunEventQueueStoreConformance(t, func(t *testing.T) (db.EventQueueStore, string, dbtest.EventQueueSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, _ := seedPgEventQueueOrg(t, h)
		return stores.EventQueue, orgID, newPgEventQueueSeeder(h, orgID)
	})
}

// TestEventQueueStore_Postgres_CrossOrg pins the org_id defense-in-depth
// filter on the org-scoped mutators + reads. ClaimNext is cross-org by
// design (one system worker drains every tenant in FIFO order), so it is
// excluded — the claimed row carries its org_id, which scopes everything
// downstream.
func TestEventQueueStore_Postgres_CrossOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, _ := seedPgEventQueueOrg(t, h)
	entityA := newPgEventQueueSeeder(h, orgA).Entity(t)
	orgB, _ := seedPgEventQueueOrg(t, h)

	if _, err := stores.EventQueue.Enqueue(ctx, orgA, domain.Event{
		EntityID: &entityA, EventType: domain.EventGitHubPRCICheckFailed,
	}, ""); err != nil {
		t.Fatalf("Enqueue orgA: %v", err)
	}

	// ClaimNext is global — it claims orgA's row and tags it with orgA.
	claimed, err := stores.EventQueue.ClaimNext(ctx, "cross-org-executor", 1)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: got=%v err=%v", claimed, err)
	}
	if claimed.OrgID != orgA {
		t.Errorf("claimed row org_id = %q, want %q", claimed.OrgID, orgA)
	}

	// MarkDone scoped to orgB must NOT flip orgA's (processing) row.
	if err := stores.EventQueue.MarkDone(ctx, orgB, claimed.ID); err != nil {
		t.Fatalf("MarkDone cross-org: %v", err)
	}
	rowsA, _ := stores.EventQueue.ListForEntity(ctx, orgA, entityA)
	if len(rowsA) != 1 || rowsA[0].Status != domain.QueuedEventStatusProcessing {
		t.Errorf("orgA row mutated by cross-org MarkDone: %+v", rowsA)
	}

	// ListForEntity scoped to orgB must not see orgA's entity rows.
	if rowsB, _ := stores.EventQueue.ListForEntity(ctx, orgB, entityA); len(rowsB) != 0 {
		t.Errorf("orgB ListForEntity returned %d rows for orgA's entity", len(rowsB))
	}

	// The correctly-scoped MarkDone still works.
	if err := stores.EventQueue.MarkDone(ctx, orgA, claimed.ID); err != nil {
		t.Fatalf("MarkDone orgA: %v", err)
	}
	rowsA, _ = stores.EventQueue.ListForEntity(ctx, orgA, entityA)
	if rowsA[0].Status != domain.QueuedEventStatusDone {
		t.Errorf("orgA row status = %q, want done after correctly-scoped MarkDone", rowsA[0].Status)
	}
}

func seedPgEventQueueOrg(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("event-queue-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "EventQueue Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "EventQueue Org "+orgID[:8], "eq-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}

// newPgEventQueueSeeder builds the seeder bag against AdminDB so raw
// inserts bypass RLS. Enqueue writes the events audit row itself, so the
// seeder only needs to stand up an entity for its FK.
func newPgEventQueueSeeder(h *pgtest.Harness, orgID string) dbtest.EventQueueSeeder {
	conn := h.AdminDB
	entity := func(t *testing.T) string {
		t.Helper()
		entityID := uuid.New().String()
		sourceID := fmt.Sprintf("owner/repo#%s", entityID[:8])
		if _, err := conn.Exec(`
			INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES ($1, $2, 'github', $3, 'pr', 'Test PR', '', '{}'::jsonb, now())
		`, entityID, orgID, sourceID); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		return entityID
	}
	return dbtest.EventQueueSeeder{Entity: entity}
}
