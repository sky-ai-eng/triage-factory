package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

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

	// The operator surface is org-scoped on the same admin pool, so its
	// org_id bind is the only thing between one tenant's admin and another
	// tenant's dropped work. Park orgA's row and prove orgB can neither see
	// nor move it.
	if err := stores.EventQueue.MarkFailed(ctx, orgA, claimed.ID, "boom"); err != nil {
		t.Fatalf("MarkFailed orgA: %v", err)
	}
	if parkedB, err := stores.EventQueue.ListFailedEvents(ctx, orgB, 0); err != nil {
		t.Fatalf("ListFailedEvents orgB: %v", err)
	} else if len(parkedB) != 0 {
		t.Errorf("orgB ListFailedEvents returned %d of orgA's parked rows", len(parkedB))
	}
	if n, err := stores.EventQueue.RequeueFailedEvents(ctx, orgB, []int64{claimed.ID}); err != nil {
		t.Fatalf("RequeueFailedEvents orgB: %v", err)
	} else if n != 0 {
		t.Errorf("orgB requeued %d of orgA's parked rows, want 0", n)
	}
	parkedA, _ := stores.EventQueue.ListFailedEvents(ctx, orgA, 0)
	if len(parkedA) != 1 || parkedA[0].ID != claimed.ID {
		t.Fatalf("orgA's parked row = %+v, want the row it parked, untouched", parkedA)
	}

	// The correctly-scoped requeue puts it back, and the correctly-scoped
	// MarkDone still drives it to a terminal.
	if n, err := stores.EventQueue.RequeueFailedEvents(ctx, orgA, []int64{claimed.ID}); err != nil || n != 1 {
		t.Fatalf("RequeueFailedEvents orgA: n=%d err=%v", n, err)
	}
	reclaimed, err := stores.EventQueue.ClaimNext(ctx, "cross-org-executor", 1)
	if err != nil || reclaimed == nil {
		t.Fatalf("re-claim after requeue: got=%v err=%v", reclaimed, err)
	}
	if err := stores.EventQueue.MarkDone(ctx, orgA, reclaimed.ID); err != nil {
		t.Fatalf("MarkDone orgA: %v", err)
	}
	rowsA, _ = stores.EventQueue.ListForEntity(ctx, orgA, entityA)
	if rowsA[0].Status != domain.QueuedEventStatusDone {
		t.Errorf("orgA row status = %q, want done after correctly-scoped MarkDone", rowsA[0].Status)
	}
}

// TestEventQueueStore_Postgres_LateWriterAfterReclaim pins the loser
// contract of the staleness backstop. Once a stale row has been reclaimed,
// the original owner may still be alive and may still finish the unit it
// claimed — and when it does, its terminal write lands on a row that now
// belongs to another pass. Every one of those writes must be a no-op, not a
// false 'done' that consumes an event nobody has routed yet. The
// status = 'processing' guard on the terminal mutators is what makes it so.
func TestEventQueueStore_Postgres_LateWriterAfterReclaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, _ := seedPgEventQueueOrg(t, h)
	seeder := newPgEventQueueSeeder(h, orgID)
	entityID := seeder.Entity(t)
	if _, err := stores.EventQueue.Enqueue(ctx, orgID, domain.Event{
		EntityID: &entityID, EventType: domain.EventGitHubPRCICheckFailed,
	}, ""); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claimed, err := stores.EventQueue.ClaimNext(ctx, "slow-executor", 1)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: got=%v err=%v", claimed, err)
	}
	seeder.BackdateClaim(t, claimed.ID, 11*time.Minute)

	if n, err := stores.EventQueue.RequeueStaleProcessing(ctx, 10*time.Minute); err != nil {
		t.Fatalf("RequeueStaleProcessing: %v", err)
	} else if n != 1 {
		t.Fatalf("RequeueStaleProcessing reclaimed %d rows, want 1", n)
	}

	// The original pass finishes late and writes its terminal. Each of
	// these is the write it would have made had nothing intervened.
	if err := stores.EventQueue.MarkDone(ctx, orgID, claimed.ID); err != nil {
		t.Fatalf("late MarkDone: %v", err)
	}
	if err := stores.EventQueue.MarkFailed(ctx, orgID, claimed.ID, "late failure"); err != nil {
		t.Fatalf("late MarkFailed: %v", err)
	}
	if err := stores.EventQueue.Requeue(ctx, orgID, claimed.ID, "late requeue"); err != nil {
		t.Fatalf("late Requeue: %v", err)
	}

	rows, err := stores.EventQueue.ListForEntity(ctx, orgID, entityID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListForEntity: rows=%d err=%v", len(rows), err)
	}
	if rows[0].Status != domain.QueuedEventStatusPending {
		t.Errorf("status = %q, want pending — a late writer must not move a reclaimed row", rows[0].Status)
	}
	if rows[0].ProcessedAt != nil {
		t.Errorf("processed_at = %v, want nil — the row has not been processed", rows[0].ProcessedAt)
	}
	if rows[0].LastError != db.StaleProcessingReclaimReason {
		t.Errorf("last_error = %q, want %q — the late writer's reason must not overwrite the reclaim's",
			rows[0].LastError, db.StaleProcessingReclaimReason)
	}

	// The successor claims the row and drives it to a real terminal.
	again, err := stores.EventQueue.ClaimNext(ctx, "successor-executor", 1)
	if err != nil || again == nil {
		t.Fatalf("successor ClaimNext: got=%v err=%v", again, err)
	}
	if err := stores.EventQueue.MarkDone(ctx, orgID, again.ID); err != nil {
		t.Fatalf("successor MarkDone: %v", err)
	}
	rows, _ = stores.EventQueue.ListForEntity(ctx, orgID, entityID)
	if rows[0].Status != domain.QueuedEventStatusDone {
		t.Errorf("status = %q, want done after the successor's pass", rows[0].Status)
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
	backdateClaim := func(t *testing.T, queueID int64, age time.Duration) {
		t.Helper()
		// Rewound against now() — the same server clock claimed_at was
		// stamped from and the sweep's predicate compares against.
		res, err := conn.Exec(`
			UPDATE event_queue SET claimed_at = now() - make_interval(secs => $1::double precision)
			WHERE id = $2
		`, age.Seconds(), queueID)
		if err != nil {
			t.Fatalf("backdate claimed_at: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("backdate claimed_at touched %d rows, want 1", n)
		}
	}
	clearEntityRef := func(t *testing.T, queueID int64) {
		t.Helper()
		res, err := conn.Exec(`UPDATE event_queue SET entity_id = NULL WHERE id = $1`, queueID)
		if err != nil {
			t.Fatalf("clear entity_id: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("clear entity_id touched %d rows, want 1", n)
		}
	}
	entitySnapshot := func(t *testing.T, entityID string) (string, int64) {
		t.Helper()
		var snap string
		var seq int64
		if err := conn.QueryRow(
			`SELECT COALESCE(snapshot_json::text, ''), poll_seq FROM entities WHERE id = $1 AND org_id = $2`,
			entityID, orgID,
		).Scan(&snap, &seq); err != nil {
			t.Fatalf("read entity snapshot: %v", err)
		}
		return snap, seq
	}
	countEventRows := func(t *testing.T, entityID string) int {
		t.Helper()
		var n int
		if err := conn.QueryRow(
			`SELECT COUNT(*) FROM events WHERE entity_id = $1 AND org_id = $2`, entityID, orgID,
		).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n
	}
	return dbtest.EventQueueSeeder{
		Entity:         entity,
		BackdateClaim:  backdateClaim,
		ClearEntityRef: clearEntityRef,
		EntitySnapshot: entitySnapshot,
		CountEventRows: countEventRows,
	}
}
