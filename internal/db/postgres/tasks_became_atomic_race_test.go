package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTaskStore_Postgres_FindOrCreateAtUnlessEntityActiveSystem is basic
// correctness coverage for the TFAC-579 became_atomic guard: it suppresses
// when the entity already has an active task (of a DIFFERENT event type —
// the case the standard (entity_id, event_type, dedup_key) dedup index
// can't catch) and proceeds normally on a clean entity.
func TestTaskStore_Postgres_FindOrCreateAtUnlessEntityActiveSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, _, _ := seedPgOrgUserAgent(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	t.Run("clean_entity_proceeds", func(t *testing.T) {
		entityID, eventID := seedPgEntityEvent(t, h.AdminDB, orgID, "clean")
		task, created, suppressed, err := stores.Tasks.FindOrCreateAtUnlessEntityActiveSystem(
			ctx, orgID, teamID, entityID, domain.EventJiraIssueBecameAtomic, "", eventID, 0.5, time.Now())
		if err != nil {
			t.Fatalf("FindOrCreateAtUnlessEntityActiveSystem: %v", err)
		}
		if suppressed {
			t.Fatal("clean entity should not suppress")
		}
		if !created || task == nil {
			t.Fatalf("expected a new task, created=%v task=%v", created, task)
		}
	})

	t.Run("active_other_type_suppresses", func(t *testing.T) {
		entityID, assignedEventID := seedPgEntityEvent(t, h.AdminDB, orgID, "active")
		// Pre-existing active task of a DIFFERENT event type — the
		// standard dedup index can't see this from became_atomic's side.
		if _, _, err := stores.Tasks.FindOrCreateAtSystem(
			ctx, orgID, teamID, entityID, domain.EventJiraIssueAssigned, "", assignedEventID, 0.5, time.Now(),
		); err != nil {
			t.Fatalf("seed active assigned task: %v", err)
		}

		atomicEventID := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
		`, atomicEventID, orgID, entityID, domain.EventJiraIssueBecameAtomic); err != nil {
			t.Fatalf("seed became_atomic event: %v", err)
		}

		task, created, suppressed, err := stores.Tasks.FindOrCreateAtUnlessEntityActiveSystem(
			ctx, orgID, teamID, entityID, domain.EventJiraIssueBecameAtomic, "", atomicEventID, 0.5, time.Now())
		if err != nil {
			t.Fatalf("FindOrCreateAtUnlessEntityActiveSystem: %v", err)
		}
		if !suppressed {
			t.Fatal("entity with an active (different-type) task should suppress became_atomic")
		}
		if created || task != nil {
			t.Errorf("suppressed call should return created=false, task=nil; got created=%v task=%v", created, task)
		}

		var count int
		if err := h.AdminDB.QueryRow(`
			SELECT COUNT(*) FROM tasks WHERE org_id = $1 AND entity_id = $2 AND status NOT IN ('done','dismissed')
		`, orgID, entityID).Scan(&count); err != nil {
			t.Fatalf("count active tasks: %v", err)
		}
		if count != 1 {
			t.Errorf("expected exactly 1 active task (the assigned one), got %d — became_atomic double-minted", count)
		}
	})
}

// TestTaskStore_Postgres_BecameAtomic_ConcurrentIdenticalEventsCollapse is
// the TFAC-579 concurrency proof: several concurrent
// FindOrCreateAtUnlessEntityActiveSystem calls for the SAME entity and the
// SAME (event_type, dedup_key) — e.g. a leader-failover overlap re-emitting
// the same became_atomic transition more than once — must collapse to
// exactly one task row, never two, and none may error.
//
// Note this suppression check is "any active task on the entity", not
// "any active task of a DIFFERENT type" — that's the existing (pre-
// TFAC-579) became_atomic semantics, preserved as-is here: once the first
// racer's insert lands, every other racer's active-check sees that very
// row and reports suppressed=true rather than falling through to the
// standard find-or-create's "return the existing match" behavior. The
// property under test is what changed: exactly one insert ever happens,
// with no double-mint and no error, regardless of how many racers there
// are or what order the lock admits them in.
func TestTaskStore_Postgres_BecameAtomic_ConcurrentIdenticalEventsCollapse(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, _, _ := seedPgOrgUserAgent(t, h)
	teamID := firstTeamForOrg(t, h, orgID)
	entityID, _ := seedPgEntityEvent(t, h.AdminDB, orgID, "concurrent-atomic")

	const writers = 6
	eventIDs := make([]string, writers)
	for i := range eventIDs {
		eventIDs[i] = uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
		`, eventIDs[i], orgID, entityID, domain.EventJiraIssueBecameAtomic); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	taskIDs := make([]string, writers)
	createdFlags := make([]bool, writers)
	suppressedFlags := make([]bool, writers)
	errs := make([]error, writers)
	var ready sync.WaitGroup
	ready.Add(writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			task, created, suppressed, err := stores.Tasks.FindOrCreateAtUnlessEntityActiveSystem(
				ctx, orgID, teamID, entityID, domain.EventJiraIssueBecameAtomic, "", eventIDs[i], 0.5, time.Now())
			errs[i], createdFlags[i], suppressedFlags[i] = err, created, suppressed
			if task != nil {
				taskIDs[i] = task.ID
			}
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	creators, suppressedCount := 0, 0
	seen := map[string]struct{}{}
	for i := range taskIDs {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		switch {
		case createdFlags[i]:
			creators++
			seen[taskIDs[i]] = struct{}{}
		case suppressedFlags[i]:
			suppressedCount++
		default:
			t.Fatalf("writer %d: neither created nor suppressed (created=%v suppressed=%v task=%q)",
				i, createdFlags[i], suppressedFlags[i], taskIDs[i])
		}
	}
	if creators != 1 {
		t.Errorf("expected exactly 1 writer to create the task, got %d creators (double-mint under concurrency)", creators)
	}
	if suppressedCount != writers-1 {
		t.Errorf("expected the remaining %d writers to be suppressed, got %d suppressed", writers-1, suppressedCount)
	}
	if len(seen) != 1 {
		t.Errorf("expected exactly 1 distinct created task id, got %d: %v", len(seen), taskIDs)
	}

	var count int
	if err := h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM tasks WHERE org_id = $1 AND entity_id = $2 AND event_type = $3
	`, orgID, entityID, domain.EventJiraIssueBecameAtomic).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 became_atomic task row, got %d", count)
	}
}
