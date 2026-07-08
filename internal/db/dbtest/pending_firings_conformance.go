package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PendingFiringsStoreFactory is what a per-backend test file hands to
// RunPendingFiringsStoreConformance. Returns:
//   - the wired PendingFiringsStore impl,
//   - the orgID to pass to every call,
//   - a PendingFiringsSeeder for fixtures the store can't create itself
//     (entity → task → event_handler → event chains, plus optional
//     non-terminal runs for the HasActiveAutoRunForEntity gate).
type PendingFiringsStoreFactory func(t *testing.T) (
	store db.PendingFiringsStore,
	orgID string,
	seed PendingFiringsSeeder,
)

// PendingFiringsTuple is the minimum identifier set Enqueue needs.
// Returned by the per-backend Tuple seeder so each subtest can stand
// up an independent (entity, task, trigger, event) chain.
type PendingFiringsTuple struct {
	EntityID  string
	TaskID    string
	TriggerID string
	EventID   string
	// UserID is the value Enqueue should bind to creator_user_id when
	// the Postgres schema requires it. SQLite ignores it.
	UserID string
}

// PendingFiringsSeeder bags raw-SQL helpers backend tests provide.
type PendingFiringsSeeder struct {
	// Tuple inserts a fresh entity/task/trigger/event chain and
	// returns the IDs Enqueue needs.
	Tuple func(t *testing.T) PendingFiringsTuple

	// RunForTask inserts a run against the taskID and returns the
	// runID. Used by MarkFired tests to satisfy fired_run_id's FK to
	// runs(id). Status / trigger_type aren't load-bearing here —
	// the conformance suite only needs a real run row to point at;
	// the per-entity firing gate's runs-shaped half is owned by
	// AgentRunStore and tested there.
	RunForTask func(t *testing.T, taskID string) string
}

// RunPendingFiringsStoreConformance covers the pending-firings
// contract every backend impl must hold:
//
//   - Enqueue inserts a row in 'pending' status and returns inserted=true.
//   - Enqueue with the same (task_id, trigger_id) while one is pending
//     collapses via ON CONFLICT DO NOTHING and returns inserted=false.
//   - PopForEntity is a CLAIMING pop: it returns the oldest pending row
//     and atomically flips it to 'draining' in the same statement (no
//     window for a second concurrent pop to observe and claim the same
//     row).
//   - PopForEntity returns nil on empty queue and ignores non-pending
//     (including already-'draining') rows.
//   - Release reverts a 'draining' row back to 'pending'; no-op against
//     a row that's since reached a terminal state.
//   - MarkFired flips 'draining' → 'fired' with run_id; idempotent
//     against already-terminal rows (guarded by status='draining').
//   - MarkSkipped flips 'draining' → 'skipped_stale' with reason;
//     same idempotency guard.
//   - HasPendingForEntity tracks presence of 'pending' rows.
//   - ListEntitiesWithPending returns distinct entity ids that have
//     at least one 'pending' row, scoped to the org.
//   - ListForEntity orders by queued_at ASC then id ASC.
//
// The runs-shaped half of the per-entity firing gate
// (HasActiveAutoRunForEntity) is owned by AgentRunStore — its
// behavior is covered by that store's own tests, not here.
func RunPendingFiringsStoreConformance(t *testing.T, mk PendingFiringsStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Enqueue_inserts_pending_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		inserted, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if !inserted {
			t.Errorf("first Enqueue should report inserted=true")
		}
		rows, err := s.ListForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Status != domain.PendingFiringStatusPending {
			t.Errorf("status = %q, want pending", rows[0].Status)
		}
		if rows[0].EntityID != tup.EntityID || rows[0].TaskID != tup.TaskID || rows[0].TriggerID != tup.TriggerID {
			t.Errorf("row identity mismatch: %+v", rows[0])
		}
		if rows[0].TriggeringEventID != tup.EventID {
			t.Errorf("triggering_event_id = %q, want %q", rows[0].TriggeringEventID, tup.EventID)
		}
	})

	t.Run("Enqueue_collapses_duplicate", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if inserted, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil || !inserted {
			t.Fatalf("first Enqueue: inserted=%v err=%v", inserted, err)
		}
		inserted, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID)
		if err != nil {
			t.Fatalf("duplicate Enqueue: %v", err)
		}
		if inserted {
			t.Errorf("duplicate (task_id, trigger_id) while pending should report inserted=false")
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if len(rows) != 1 {
			t.Errorf("dedup should keep one row, got %d", len(rows))
		}
	})

	t.Run("PopForEntity_claims_oldest_pending", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup1 := seed.Tuple(t) // same entity, distinct task+trigger
		tup2 := seed.Tuple(t)
		// Re-point tup2 at tup1's entity by inserting against tup1's
		// entity with tup2's task/trigger. Backend seeders can't share
		// an entity across two Tuple calls, so we enqueue against
		// tup1.EntityID using tup1.TaskID/tup1.TriggerID first then
		// tup2.TaskID/tup2.TriggerID — both reference rows the seeder
		// already FK-validated under their own entities. The dedup
		// index is on (task_id, trigger_id) so collisions across
		// entities aren't a concern.
		if _, err := s.Enqueue(ctx, orgID, tup1.UserID, tup1.EntityID, tup1.TaskID, tup1.TriggerID, tup1.EventID); err != nil {
			t.Fatalf("first Enqueue: %v", err)
		}
		if _, err := s.Enqueue(ctx, orgID, tup2.UserID, tup1.EntityID, tup2.TaskID, tup2.TriggerID, tup2.EventID); err != nil {
			t.Fatalf("second Enqueue: %v", err)
		}
		got, err := s.PopForEntity(ctx, orgID, tup1.EntityID)
		if err != nil || got == nil {
			t.Fatalf("Pop: got=%v err=%v", got, err)
		}
		if got.TaskID != tup1.TaskID {
			t.Errorf("Pop returned task %q, want oldest %q", got.TaskID, tup1.TaskID)
		}
		// Claiming: the popped row is atomically reserved as 'draining' so
		// a concurrent drain can't also claim it.
		if got.Status != domain.PendingFiringStatusDraining {
			t.Errorf("popped row status = %q, want draining (Pop is a claiming pop)", got.Status)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup1.EntityID)
		pendingCount, drainingCount := 0, 0
		for _, r := range rows {
			switch r.Status {
			case domain.PendingFiringStatusPending:
				pendingCount++
			case domain.PendingFiringStatusDraining:
				drainingCount++
			}
		}
		if pendingCount != 1 {
			t.Errorf("queue should have 1 still-pending row after Pop, got %d", pendingCount)
		}
		if drainingCount != 1 {
			t.Errorf("queue should have 1 draining (claimed) row after Pop, got %d", drainingCount)
		}
		// A second Pop must NOT return the already-claimed row — it should
		// skip straight to the next pending one.
		got2, err := s.PopForEntity(ctx, orgID, tup1.EntityID)
		if err != nil || got2 == nil {
			t.Fatalf("second Pop: got=%v err=%v", got2, err)
		}
		if got2.TaskID != tup2.TaskID {
			t.Errorf("second Pop returned task %q, want %q (the still-pending one)", got2.TaskID, tup2.TaskID)
		}
		if got2.ID == got.ID {
			t.Errorf("second Pop returned the same row as the first claim — double-pop")
		}
	})

	t.Run("Release_reverts_draining_to_pending", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, err := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err != nil || row == nil {
			t.Fatalf("Pop: row=%v err=%v", row, err)
		}
		if err := s.Release(ctx, orgID, row.ID); err != nil {
			t.Fatalf("Release: %v", err)
		}
		has, err := s.HasPendingForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("HasPending: %v", err)
		}
		if !has {
			t.Errorf("released row should be pending again")
		}
		// Release against an already-terminal row is a no-op.
		row2, _ := s.PopForEntity(ctx, orgID, tup.EntityID)
		if row2 == nil {
			t.Fatalf("expected the released row to be poppable again")
		}
		if err := s.MarkSkipped(ctx, orgID, row2.ID, domain.PendingFiringSkipTaskClosed); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		if err := s.Release(ctx, orgID, row2.ID); err != nil {
			t.Fatalf("Release on terminal row should not error: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if len(rows) != 1 || rows[0].Status != domain.PendingFiringStatusSkippedStale {
			t.Errorf("Release must not resurrect a terminal row, got %+v", rows)
		}
	})

	t.Run("PopForEntity_nil_on_empty_queue", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		got, err := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("Pop: %v", err)
		}
		if got != nil {
			t.Errorf("Pop on empty queue should be nil, got %+v", got)
		}
	})

	t.Run("PopForEntity_ignores_non_pending", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, err := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err != nil || row == nil {
			t.Fatalf("Pop: row=%v err=%v", row, err)
		}
		if err := s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipTaskClosed); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		got, err := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("Pop after skip: %v", err)
		}
		if got != nil {
			t.Errorf("Pop should ignore skipped_stale rows, got %+v", got)
		}
	})

	t.Run("MarkFired_transitions_with_run_id", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)

		// MarkFired references a real run row in Postgres
		// (fired_run_id has FK with ON DELETE on (fired_run_id, org_id)
		// referencing runs(id, org_id)). The seeder's run-insert
		// helpers produce valid run ids.
		runID := seed.RunForTask(t, tup.TaskID)
		if err := s.MarkFired(ctx, orgID, row.ID, runID); err != nil {
			t.Fatalf("MarkFired: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if len(rows) != 1 || rows[0].Status != domain.PendingFiringStatusFired {
			t.Errorf("expected one fired row, got %+v", rows)
		}
		if rows[0].FiredRunID == nil || *rows[0].FiredRunID != runID {
			t.Errorf("fired_run_id = %v, want pointer to %q", rows[0].FiredRunID, runID)
		}
		if rows[0].DrainedAt == nil {
			t.Errorf("drained_at should be set after MarkFired")
		}
	})

	t.Run("MarkFired_no_op_on_terminal", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err := s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipTaskClosed); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		runID := seed.RunForTask(t, tup.TaskID)
		// Should silently no-op — guarded by WHERE status='pending'.
		if err := s.MarkFired(ctx, orgID, row.ID, runID); err != nil {
			t.Fatalf("MarkFired on terminal: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if len(rows) != 1 || rows[0].Status != domain.PendingFiringStatusSkippedStale {
			t.Errorf("terminal row should stay skipped_stale, got %+v", rows)
		}
	})

	t.Run("MarkSkipped_records_reason", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err := s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipBreakerTripped); err != nil {
			t.Fatalf("MarkSkipped: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if rows[0].SkipReason != domain.PendingFiringSkipBreakerTripped {
			t.Errorf("skip_reason = %q, want %q", rows[0].SkipReason, domain.PendingFiringSkipBreakerTripped)
		}
	})

	t.Run("HasPendingForEntity_tracks_pending_rows", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		has, err := s.HasPendingForEntity(ctx, orgID, tup.EntityID)
		if err != nil {
			t.Fatalf("HasPending: %v", err)
		}
		if has {
			t.Errorf("empty queue should not report pending")
		}
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		has, _ = s.HasPendingForEntity(ctx, orgID, tup.EntityID)
		if !has {
			t.Errorf("queue with pending row should report true")
		}
		row, _ := s.PopForEntity(ctx, orgID, tup.EntityID)
		// A popped ('draining') row is still queued intent — the gate must
		// stay closed while a drain is mid-flight, or a fresh event in
		// that window would fire immediately and jump the queue.
		has, _ = s.HasPendingForEntity(ctx, orgID, tup.EntityID)
		if !has {
			t.Errorf("a 'draining' row must keep HasPending true (gate stays closed mid-drain)")
		}
		_ = s.MarkSkipped(ctx, orgID, row.ID, domain.PendingFiringSkipTriggerDisabled)
		has, _ = s.HasPendingForEntity(ctx, orgID, tup.EntityID)
		if has {
			t.Errorf("after only terminal rows remain, HasPending should be false")
		}
	})

	t.Run("Enqueue_collapses_against_draining", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if row, err := s.PopForEntity(ctx, orgID, tup.EntityID); err != nil || row == nil {
			t.Fatalf("PopForEntity: row=%v err=%v", row, err)
		}
		// While the drain is mid-flight ('draining'), a duplicate
		// (task, trigger) enqueue must collapse exactly as it would
		// against a 'pending' row — the intent is already queued.
		inserted, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID)
		if err != nil {
			t.Fatalf("duplicate Enqueue during drain: %v", err)
		}
		if inserted {
			t.Errorf("duplicate (task_id, trigger_id) while draining should collapse (inserted=false)")
		}
		if rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID); len(rows) != 1 {
			t.Errorf("dedup should keep one row through the drain window, got %d", len(rows))
		}
	})

	t.Run("RequeueStaleDraining_recovers_orphaned_claims", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup := seed.Tuple(t)
		if _, err := s.Enqueue(ctx, orgID, tup.UserID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if row, err := s.PopForEntity(ctx, orgID, tup.EntityID); err != nil || row == nil {
			t.Fatalf("PopForEntity: row=%v err=%v", row, err)
		}

		// A cutoff in the past: the fresh claim is NOT stale — a live
		// drain must never have its row stolen mid-flight.
		n, err := s.RequeueStaleDraining(ctx, orgID, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("RequeueStaleDraining (fresh claim): %v", err)
		}
		if n != 0 {
			t.Errorf("a fresh 'draining' claim must not be requeued, got n=%d", n)
		}

		// A cutoff in the future makes the claim stale — the crashed-
		// drainer shape. The row must come back to 'pending' and be
		// poppable again.
		n, err = s.RequeueStaleDraining(ctx, orgID, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("RequeueStaleDraining (stale claim): %v", err)
		}
		if n != 1 {
			t.Fatalf("expected exactly the orphaned claim requeued, got n=%d", n)
		}
		rows, _ := s.ListForEntity(ctx, orgID, tup.EntityID)
		if len(rows) != 1 || rows[0].Status != domain.PendingFiringStatusPending {
			t.Fatalf("requeued row should be back in 'pending', got %+v", rows)
		}
		reclaimed, err := s.PopForEntity(ctx, orgID, tup.EntityID)
		if err != nil || reclaimed == nil {
			t.Fatalf("re-pop after requeue: row=%v err=%v", reclaimed, err)
		}
		// The dead drainer's late MarkFired-after-requeue-and-re-pop is
		// the reclaim race; the new claimant resolving it normally is the
		// expected outcome.
		if err := s.MarkSkipped(ctx, orgID, reclaimed.ID, domain.PendingFiringSkipTriggerDisabled); err != nil {
			t.Fatalf("resolve reclaimed row: %v", err)
		}
	})

	t.Run("ListEntitiesWithPending_distinct_per_entity", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tupA := seed.Tuple(t)
		tupB := seed.Tuple(t)

		// Two pending rows on tupA's entity; one on tupB's.
		if _, err := s.Enqueue(ctx, orgID, tupA.UserID, tupA.EntityID, tupA.TaskID, tupA.TriggerID, tupA.EventID); err != nil {
			t.Fatalf("Enqueue tupA: %v", err)
		}
		// Same entity, distinct task — Tuple gives us a fresh tuple
		// rooted on a different entity, so use tupB's task/trigger
		// against tupA's entity for the dedup-non-collision check.
		if _, err := s.Enqueue(ctx, orgID, tupB.UserID, tupA.EntityID, tupB.TaskID, tupB.TriggerID, tupB.EventID); err != nil {
			t.Fatalf("Enqueue second on tupA entity: %v", err)
		}

		ids, err := s.ListEntitiesWithPending(ctx, orgID)
		if err != nil {
			t.Fatalf("ListEntitiesWithPending: %v", err)
		}
		if len(ids) != 1 || ids[0] != tupA.EntityID {
			t.Errorf("expected distinct ids = [%q], got %v", tupA.EntityID, ids)
		}
	})

	t.Run("ListForEntity_fifo_order", func(t *testing.T) {
		s, orgID, seed := mk(t)
		tup1 := seed.Tuple(t)
		tup2 := seed.Tuple(t)
		// Two rows on the same entity in known order.
		if _, err := s.Enqueue(ctx, orgID, tup1.UserID, tup1.EntityID, tup1.TaskID, tup1.TriggerID, tup1.EventID); err != nil {
			t.Fatalf("Enqueue first: %v", err)
		}
		if _, err := s.Enqueue(ctx, orgID, tup2.UserID, tup1.EntityID, tup2.TaskID, tup2.TriggerID, tup2.EventID); err != nil {
			t.Fatalf("Enqueue second: %v", err)
		}
		rows, err := s.ListForEntity(ctx, orgID, tup1.EntityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("expected 2 rows, got %d", len(rows))
		}
		// queued_at ASC then id ASC — first enqueue must be index 0.
		if rows[0].TaskID != tup1.TaskID {
			t.Errorf("FIFO order broken: rows[0].TaskID=%q, want %q", rows[0].TaskID, tup1.TaskID)
		}
		if rows[1].TaskID != tup2.TaskID {
			t.Errorf("FIFO order broken: rows[1].TaskID=%q, want %q", rows[1].TaskID, tup2.TaskID)
		}
	})
}
