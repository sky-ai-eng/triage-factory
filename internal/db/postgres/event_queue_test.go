package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
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
// filter on the org-scoped reads and controls. Claim is cross-org by design
// (one system worker drains every tenant), so it is excluded — the claimed
// row carries its org_id, which scopes everything downstream, and the
// receipt carries it into every holder write.
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

	// Claim is global — it claims orgA's row and the receipt names orgA.
	batch, err := stores.EventQueue.Claim(ctx, workitem.Owner{ID: "cross-org-executor", Epoch: 1}, 1)
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("Claim: got=%+v err=%v", batch, err)
	}
	claimed := batch.Events[0]
	if claimed.Event.OrgID != orgA || claimed.Receipt.OrgID != orgA {
		t.Errorf("claimed row org_id = %q/%q, want %q", claimed.Event.OrgID, claimed.Receipt.OrgID, orgA)
	}

	// A receipt re-addressed to orgB matches nothing: the guard binds the
	// org beside the generation.
	forged := claimed.Receipt
	forged.OrgID = orgB
	if err := stores.EventQueue.MarkDone(ctx, forged); !errors.Is(err, workitem.ErrLeaseLost) {
		t.Errorf("MarkDone with a cross-org receipt = %v, want ErrLeaseLost", err)
	}
	rowsA, _ := stores.EventQueue.ListForEntity(ctx, orgA, entityA)
	if len(rowsA) != 1 || rowsA[0].Status != domain.QueuedEventStatusLeased {
		t.Errorf("orgA row mutated by a cross-org receipt: %+v", rowsA)
	}

	// ListForEntity scoped to orgB must not see orgA's entity rows.
	if rowsB, _ := stores.EventQueue.ListForEntity(ctx, orgB, entityA); len(rowsB) != 0 {
		t.Errorf("orgB ListForEntity returned %d rows for orgA's entity", len(rowsB))
	}

	// The operator surface is org-scoped on the same admin pool, so its
	// org_id bind is the only thing between one tenant's admin and another
	// tenant's parked work. Park orgA's row and prove orgB can neither see
	// nor move it.
	if parked, err := stores.EventQueue.Requeue(ctx, claimed.Receipt, workitem.OutcomePermanent, errors.New("boom")); err != nil || !parked {
		t.Fatalf("Requeue orgA: parked=%v err=%v", parked, err)
	}
	handle := stores.EventQueue.(db.WorkKindHandle)
	if parkedB, _, err := workitem.List(ctx, handle.Conn(), handle.Kind(), orgB, workitem.StatusParked, 50, 0); err != nil {
		t.Fatalf("List orgB: %v", err)
	} else if len(parkedB) != 0 {
		t.Errorf("orgB List returned %d of orgA's parked rows", len(parkedB))
	}
	if row, err := workitem.Get(ctx, handle.Conn(), handle.Kind(), orgB, claimed.Event.ID); err != nil || row != nil {
		t.Errorf("orgB Get = %+v err=%v, want (nil, nil)", row, err)
	}
	if subjects, err := handle.Describe(ctx, orgB, []int64{claimed.Event.ID}); err != nil || len(subjects) != 0 {
		t.Errorf("orgB Describe = %+v err=%v, want nothing", subjects, err)
	}
	if err := workitem.Redrive(ctx, handle.Conn(), handle.Kind(), orgB, claimed.Event.ID, "operator"); !errors.Is(err, workitem.ErrNotParked) {
		t.Errorf("orgB Redrive of orgA's parked row = %v, want ErrNotParked", err)
	}
	parkedA, _, _ := workitem.List(ctx, handle.Conn(), handle.Kind(), orgA, workitem.StatusParked, 50, 0)
	if len(parkedA) != 1 || parkedA[0].ID != claimed.Event.ID {
		t.Fatalf("orgA's parked row = %+v, want the row it parked, untouched", parkedA)
	}
	if unsettledB, err := stores.EventQueue.UnsettledCloseExistsSystem(ctx, orgB, entityA); err != nil || unsettledB {
		t.Errorf("orgB UnsettledCloseExistsSystem = %v err=%v, want false", unsettledB, err)
	}

	// The correctly-scoped redrive puts it back, and the correctly-scoped
	// receipt still drives it to a terminal.
	if err := workitem.Redrive(ctx, handle.Conn(), handle.Kind(), orgA, claimed.Event.ID, "operator"); err != nil {
		t.Fatalf("Redrive orgA: %v", err)
	}
	again, err := stores.EventQueue.Claim(ctx, workitem.Owner{ID: "cross-org-executor", Epoch: 1}, 1)
	if err != nil || len(again.Events) != 1 {
		t.Fatalf("re-claim after redrive: got=%+v err=%v", again, err)
	}
	if err := stores.EventQueue.MarkDone(ctx, again.Events[0].Receipt); err != nil {
		t.Fatalf("MarkDone orgA: %v", err)
	}
	rowsA, _ = stores.EventQueue.ListForEntity(ctx, orgA, entityA)
	if rowsA[0].Status != domain.QueuedEventStatusDone {
		t.Errorf("orgA row status = %q, want done after the correctly-scoped MarkDone", rowsA[0].Status)
	}
}

// TestEventQueueStore_Postgres_LateWriterAfterReclaim pins the loser
// contract of lease expiry under a real takeover. Once a row has been
// reclaimed, the original owner may still be alive and may still finish the
// unit it claimed — and when it does, its terminal write presents a receipt
// whose generation the row has moved past. Every one of those writes must
// be a no-op, not a false 'done' that consumes an event nobody has routed
// yet.
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

	slow, err := stores.EventQueue.Claim(ctx, workitem.Owner{ID: "slow-executor", Epoch: 1}, 1)
	if err != nil || len(slow.Events) != 1 {
		t.Fatalf("Claim: got=%+v err=%v", slow, err)
	}
	stale := slow.Events[0]
	seeder.ExpireLease(t, stale.Event.ID)

	successor, err := stores.EventQueue.Claim(ctx, workitem.Owner{ID: "successor-executor", Epoch: 1}, 1)
	if err != nil || len(successor.Events) != 1 || successor.Reclaimed != 1 {
		t.Fatalf("successor Claim: got=%+v err=%v, want one reclaim", successor, err)
	}
	if r := successor.Events[0].Receipt; !r.Reclaimed || r.PreviousOwner != "slow-executor" {
		t.Errorf("successor receipt = %+v, want a reclaim from slow-executor", r)
	}

	// The original pass finishes late and writes its terminal. Each of
	// these is the write it would have made had nothing intervened.
	if err := stores.EventQueue.MarkDone(ctx, stale.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
		t.Errorf("late MarkDone = %v, want ErrLeaseLost", err)
	}
	if _, err := stores.EventQueue.Requeue(ctx, stale.Receipt, workitem.OutcomeTransient, errors.New("late requeue")); !errors.Is(err, workitem.ErrLeaseLost) {
		t.Errorf("late Requeue = %v, want ErrLeaseLost", err)
	}
	if _, err := stores.EventQueue.RenewLease(ctx, stale.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
		t.Errorf("late RenewLease = %v, want ErrLeaseLost", err)
	}

	rows, err := stores.EventQueue.ListForEntity(ctx, orgID, entityID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListForEntity: rows=%d err=%v", len(rows), err)
	}
	if rows[0].Status != domain.QueuedEventStatusLeased || rows[0].LeaseOwner != "successor-executor" {
		t.Errorf("row = %+v, want still leased by the successor — a late writer must not move a reclaimed row", rows[0])
	}
	if rows[0].LeaseGeneration != successor.Events[0].Receipt.LeaseGeneration {
		t.Errorf("lease_generation = %d, want the successor's %d", rows[0].LeaseGeneration, successor.Events[0].Receipt.LeaseGeneration)
	}

	// The successor drives it to a real terminal.
	if err := stores.EventQueue.MarkDone(ctx, successor.Events[0].Receipt); err != nil {
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

// pgExecOne runs a statement that must touch exactly one row.
func pgExecOne(t *testing.T, h *pgtest.Harness, what, query string, args ...any) {
	t.Helper()
	res, err := h.AdminDB.Exec(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%s touched %d rows, want 1", what, n)
	}
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
	return dbtest.EventQueueSeeder{
		Entity: entity,
		ExpireLease: func(t *testing.T, queueID int64) {
			t.Helper()
			// Rewound against the server clock — the one the claim stamped
			// the lease from and the guard compares against.
			pgExecOne(t, h, "expire lease",
				`UPDATE event_queue SET lease_expires_at = clock_timestamp() - interval '1 hour' WHERE id = $1 AND status = 'leased'`, queueID)
		},
		Ripen: func(t *testing.T, queueID int64) {
			t.Helper()
			pgExecOne(t, h, "ripen", `UPDATE event_queue SET next_attempt_at = NULL WHERE id = $1 AND status = 'ready'`, queueID)
		},
		RequestCancel: func(t *testing.T, queueID int64) {
			t.Helper()
			pgExecOne(t, h, "request cancel",
				`UPDATE event_queue SET cancel_requested_at = clock_timestamp(), cancel_requested_by = 'operator', cancel_reason = 'test' WHERE id = $1`, queueID)
		},
		ClearEntityRef: func(t *testing.T, queueID int64) {
			t.Helper()
			pgExecOne(t, h, "clear entity_id", `UPDATE event_queue SET entity_id = NULL WHERE id = $1`, queueID)
		},
		KeyedRow: func(t *testing.T, entityID string) {
			t.Helper()
			eventID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
			`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
				t.Fatalf("seed keyed event: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO event_queue (org_id, event_id, entity_id, event_type, status, max_attempts, unique_key, first_enqueued_at)
				VALUES ($1, $2, $3, $4, 'ready', 5, $5, now())
			`, orgID, eventID, entityID, domain.EventGitHubPRCICheckFailed, workkinds.EventQueueCloseOwedKey(entityID)); err != nil {
				t.Fatalf("seed keyed row: %v", err)
			}
		},
		EntitySnapshot: func(t *testing.T, entityID string) (string, int64) {
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
		},
		CountEventRows: func(t *testing.T, entityID string) int {
			t.Helper()
			var n int
			if err := conn.QueryRow(
				`SELECT COUNT(*) FROM events WHERE entity_id = $1 AND org_id = $2`, entityID, orgID,
			).Scan(&n); err != nil {
				t.Fatalf("count events: %v", err)
			}
			return n
		},
	}
}

// TestEventQueueStore_Postgres_ClaimInterleavesOrgs pins the kind's fairness
// on the production table: with one org holding the queue's head and every
// live lease, a batch claim prefers the org with fewer leased rows rather
// than draining the busy org's backlog first.
func TestEventQueueStore_Postgres_ClaimInterleavesOrgs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	busy, _ := seedPgEventQueueOrg(t, h)
	quiet, _ := seedPgEventQueueOrg(t, h)
	busyEntity := newPgEventQueueSeeder(h, busy).Entity(t)
	quietEntity := newPgEventQueueSeeder(h, quiet).Entity(t)
	enqueue := func(orgID, entityID string) {
		t.Helper()
		if _, err := stores.EventQueue.Enqueue(ctx, orgID, domain.Event{
			EntityID: &entityID, EventType: domain.EventGitHubPRCICheckFailed,
		}, ""); err != nil {
			t.Fatalf("Enqueue in %s: %v", orgID, err)
		}
	}

	// The busy org holds three live leases; then it enqueues three more
	// ahead of the quiet org's one, so id order and fairness order disagree.
	for i := 0; i < 3; i++ {
		enqueue(busy, busyEntity)
	}
	if batch, err := stores.EventQueue.Claim(ctx, workitem.Owner{ID: "pre", Epoch: 1}, 3); err != nil || len(batch.Events) != 3 {
		t.Fatalf("seed leases: got=%+v err=%v", batch, err)
	}
	for i := 0; i < 3; i++ {
		enqueue(busy, busyEntity)
	}
	enqueue(quiet, quietEntity)

	batch, err := stores.EventQueue.Claim(ctx, workitem.Owner{ID: "worker", Epoch: 1}, 4)
	if err != nil || len(batch.Events) != 4 {
		t.Fatalf("Claim: got=%+v err=%v", batch, err)
	}
	if got := batch.Events[0].Event.OrgID; got != quiet {
		t.Errorf("first claimed row belongs to %s, want the quiet org %s — fairness must interleave ahead of id order", got, quiet)
	}
	for _, ce := range batch.Events[1:] {
		if ce.Event.OrgID != busy {
			t.Errorf("claimed row %d belongs to %s, want the busy org's backlog after the quiet org's row", ce.Event.ID, ce.Event.OrgID)
		}
	}
}
