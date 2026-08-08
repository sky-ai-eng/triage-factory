package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EventQueueStoreFactory is what a per-backend test file hands to
// RunEventQueueStoreConformance. Returns the wired EventQueueStore, the
// orgID to pass to org-scoped calls, and a seeder for the entity rows
// Enqueue's event-row FK needs.
type EventQueueStoreFactory func(t *testing.T) (
	store db.EventQueueStore,
	orgID string,
	seed EventQueueSeeder,
)

// EventQueueSeeder bags the raw-SQL fixtures a backend test provides.
type EventQueueSeeder struct {
	// Entity inserts a fresh entity row and returns its id. Enqueue's
	// events-audit row FKs to it (and the queue row denormalizes it).
	Entity func(t *testing.T) string

	// BackdateClaim rewinds a claimed row's claimed_at by age, standing in
	// for a claim made that long ago. Raw SQL because the store interface
	// deliberately offers no way to write the column — every other caller
	// gets it stamped by ClaimNext — and the staleness sweep can only be
	// exercised against a claim old enough to be stale.
	BackdateClaim func(t *testing.T, queueID int64, age time.Duration)

	// ClearEntityRef NULLs a queue row's entity_id, standing in for a row
	// the display join finds no entity for. Raw SQL because Enqueue is the
	// only writer of the column and it never writes NULL for a
	// router-bound event. This is the nullable column's reachable state at
	// read time: an actually-deleted entity cascades the queue row away
	// with it, so what a list can encounter is the empty reference, not a
	// dangling one.
	ClearEntityRef func(t *testing.T, queueID int64)
}

// conformanceExecutorID/conformanceBootEpoch are the fixed ownership
// identity every claim in this suite stamps a row with, except where a test
// is specifically exercising the ownership-scoping predicate — those pass
// their own values inline.
const (
	conformanceExecutorID = "conformance-executor"
	conformanceBootEpoch  = int64(1)
)

// enqueueOn is a local helper: Enqueue a ci_check_failed event against
// entityID and return the event id. ci_check_failed is a seeded catalog
// entry, so the events.event_type FK is satisfied. No traceparent — the
// untraced producer is the ordinary case, and the round-trip of a real one
// has its own case below.
func enqueueOn(t *testing.T, ctx context.Context, s db.EventQueueStore, orgID, entityID string) string {
	t.Helper()
	return enqueueTracedOn(t, ctx, s, orgID, entityID, "")
}

// enqueueTracedOn is enqueueOn with the producer's trace context attached.
func enqueueTracedOn(t *testing.T, ctx context.Context, s db.EventQueueStore, orgID, entityID, traceparent string) string {
	t.Helper()
	id, err := s.Enqueue(ctx, orgID, domain.Event{
		EntityID:     &entityID,
		EventType:    domain.EventGitHubPRCICheckFailed,
		DedupKey:     "",
		MetadataJSON: "",
	}, traceparent)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatalf("Enqueue returned empty event id")
	}
	return id
}

// parkOn drives one event all the way to the 'failed' terminal — enqueue,
// claim, MarkFailed — and returns the claimed row so the caller has its
// queue id. The park is the only way a row enters the operator surface, so
// every case below that needs a parked row builds it this way rather than
// writing the status directly.
func parkOn(t *testing.T, ctx context.Context, s db.EventQueueStore, orgID, entityID, reason string) *domain.QueuedEvent {
	t.Helper()
	enqueueOn(t, ctx, s, orgID, entityID)
	claimed, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext while parking a row: got=%v err=%v", claimed, err)
	}
	if err := s.MarkFailed(ctx, orgID, claimed.ID, reason); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	return claimed
}

// RunEventQueueStoreConformance covers the durable-queue contract every
// backend impl must hold:
//
//   - Enqueue writes the events audit row + a pending queue row
//     atomically and returns the event id (the queue row's event_id FK
//     proves the audit row landed).
//   - Each Enqueue is a distinct event → a distinct queue row (no
//     dedup; every emitted event is unique).
//   - ClaimNext returns the globally-oldest pending row (FIFO by id),
//     flips it to processing, stamps claimed_at, and bumps attempts.
//   - ClaimNext returns nil on an empty queue.
//   - MarkDone / MarkFailed flip a processing row to the terminal
//     state, guarded against non-processing rows.
//   - Requeue returns a processing row to pending (attempts retained,
//     claimed_at cleared) for retry.
//   - ResetProcessing flips every processing row back to pending (boot
//     crash recovery).
//   - PruneDone deletes done rows older than the cutoff; failed rows
//     are retained.
//   - ListFailedEvents surfaces parked rows (with their entity, attempts
//     and last_error) newest-first, and nothing else.
//   - RequeueFailedEvents flips only 'failed' rows, grants a fresh
//     attempt budget, keeps last_error until the next attempt, and
//     counts out every id it did not move.
//   - ListForEntity orders by id.
//   - traceparent round-trips through the claim: what the producer stamped
//     is what the consumer reads back, and an untraced producer reads back
//     empty rather than as anything a link could be built from.
func RunEventQueueStoreConformance(t *testing.T, mk EventQueueStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Enqueue_persists_pending_row", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		eventID := enqueueOn(t, ctx, s, orgID, entityID)

		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 queue row, got %d", len(rows))
		}
		r := rows[0]
		if r.Status != domain.QueuedEventStatusPending {
			t.Errorf("status = %q, want pending", r.Status)
		}
		if r.EventID != eventID {
			t.Errorf("event_id = %q, want %q (the enqueued event)", r.EventID, eventID)
		}
		if r.EntityID != entityID {
			t.Errorf("entity_id = %q, want %q", r.EntityID, entityID)
		}
		if r.EventType != domain.EventGitHubPRCICheckFailed {
			t.Errorf("event_type = %q, want %q", r.EventType, domain.EventGitHubPRCICheckFailed)
		}
		if r.Attempts != 0 {
			t.Errorf("attempts = %d, want 0 before any claim", r.Attempts)
		}
		if r.ProcessedAt != nil || r.ClaimedAt != nil {
			t.Errorf("claimed_at/processed_at should be nil for a fresh row: %+v", r)
		}
	})

	t.Run("Enqueue_distinct_rows_per_event", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		id1 := enqueueOn(t, ctx, s, orgID, entityID)
		id2 := enqueueOn(t, ctx, s, orgID, entityID)
		if id1 == id2 {
			t.Fatalf("two Enqueues should mint distinct event ids, got %q twice", id1)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if len(rows) != 2 {
			t.Errorf("expected 2 distinct queue rows, got %d", len(rows))
		}
	})

	t.Run("ClaimNext_fifo_and_marks_processing", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		first := enqueueOn(t, ctx, s, orgID, entityID)
		second := enqueueOn(t, ctx, s, orgID, entityID)

		got, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil || got == nil {
			t.Fatalf("ClaimNext #1: got=%v err=%v", got, err)
		}
		if got.EventID != first {
			t.Errorf("ClaimNext returned %q, want oldest %q", got.EventID, first)
		}
		if got.Status != domain.QueuedEventStatusProcessing {
			t.Errorf("claimed row status = %q, want processing", got.Status)
		}
		if got.Attempts != 1 {
			t.Errorf("attempts = %d, want 1 after first claim", got.Attempts)
		}
		if got.ClaimedAt == nil {
			t.Errorf("claimed_at should be set after ClaimNext")
		}

		got2, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil || got2 == nil {
			t.Fatalf("ClaimNext #2: got=%v err=%v", got2, err)
		}
		if got2.EventID != second {
			t.Errorf("ClaimNext #2 returned %q, want %q", got2.EventID, second)
		}

		empty, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil {
			t.Fatalf("ClaimNext #3: %v", err)
		}
		if empty != nil {
			t.Errorf("ClaimNext on drained queue should be nil, got %+v", empty)
		}
	})

	t.Run("ClaimNext_nil_on_empty", func(t *testing.T) {
		s, _, _ := mk(t)
		got, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil {
			t.Fatalf("ClaimNext: %v", err)
		}
		if got != nil {
			t.Errorf("ClaimNext on empty queue should be nil, got %+v", got)
		}
	})

	t.Run("MarkDone_transitions_and_guards", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)
		claimed, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)

		if err := s.MarkDone(ctx, orgID, claimed.ID); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if len(rows) != 1 || rows[0].Status != domain.QueuedEventStatusDone {
			t.Fatalf("expected one done row, got %+v", rows)
		}
		if rows[0].ProcessedAt == nil {
			t.Errorf("processed_at should be set after MarkDone")
		}

		// Guard: MarkDone on an already-terminal row is a silent no-op.
		if err := s.MarkDone(ctx, orgID, claimed.ID); err != nil {
			t.Fatalf("MarkDone on terminal: %v", err)
		}
		rows, _ = s.ListForEntity(ctx, orgID, entityID)
		if rows[0].Status != domain.QueuedEventStatusDone {
			t.Errorf("terminal row should stay done, got %q", rows[0].Status)
		}
	})

	t.Run("MarkFailed_records_error", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)
		claimed, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)

		if err := s.MarkFailed(ctx, orgID, claimed.ID, "boom"); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if rows[0].Status != domain.QueuedEventStatusFailed {
			t.Errorf("status = %q, want failed", rows[0].Status)
		}
		if rows[0].LastError != "boom" {
			t.Errorf("last_error = %q, want %q", rows[0].LastError, "boom")
		}
	})

	t.Run("Requeue_returns_to_pending", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)
		claimed, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)

		if err := s.Requeue(ctx, orgID, claimed.ID, "transient"); err != nil {
			t.Fatalf("Requeue: %v", err)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if rows[0].Status != domain.QueuedEventStatusPending {
			t.Errorf("status = %q, want pending after Requeue", rows[0].Status)
		}
		if rows[0].LastError != "transient" {
			t.Errorf("last_error = %q, want %q", rows[0].LastError, "transient")
		}
		if rows[0].ClaimedAt != nil {
			t.Errorf("claimed_at should be cleared on Requeue, got %v", rows[0].ClaimedAt)
		}
		// attempts retained (the claim counted this try).
		if rows[0].Attempts != 1 {
			t.Errorf("attempts = %d, want 1 retained after Requeue", rows[0].Attempts)
		}
		// Re-claimable.
		again, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil || again == nil {
			t.Fatalf("re-claim after Requeue: got=%v err=%v", again, err)
		}
		if again.Attempts != 2 {
			t.Errorf("attempts = %d, want 2 on second claim", again.Attempts)
		}
	})

	t.Run("ResetProcessing_resets_in_flight", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)
		enqueueOn(t, ctx, s, orgID, entityID)
		// Claim both → both processing.
		if _, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch); err != nil {
			t.Fatalf("claim 1: %v", err)
		}
		if _, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch); err != nil {
			t.Fatalf("claim 2: %v", err)
		}

		// A later boot epoch of the SAME executor sweeps both — this models
		// a restart of the same persistent instance identity.
		n, err := s.ResetProcessing(ctx, conformanceExecutorID, conformanceBootEpoch+1)
		if err != nil {
			t.Fatalf("ResetProcessing: %v", err)
		}
		if n != 2 {
			t.Errorf("ResetProcessing returned %d, want 2", n)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		for _, r := range rows {
			if r.Status != domain.QueuedEventStatusPending {
				t.Errorf("row %d status = %q, want pending after reset", r.ID, r.Status)
			}
			if r.ClaimedAt != nil {
				t.Errorf("row %d claimed_at should be cleared after reset", r.ID)
			}
		}
	})

	// ResetProcessing must self-scope to (executor_id, boot_epoch) so a
	// booting instance never resets a live sibling's claimed row.
	t.Run("ResetProcessing_scoped_to_owner", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID) // claimed by "exec-a"
		enqueueOn(t, ctx, s, orgID, entityID) // claimed by "exec-b"

		rowA, err := s.ClaimNext(ctx, "exec-a", 1)
		if err != nil || rowA == nil {
			t.Fatalf("claim as exec-a: got=%v err=%v", rowA, err)
		}
		rowB, err := s.ClaimNext(ctx, "exec-b", 1)
		if err != nil || rowB == nil {
			t.Fatalf("claim as exec-b: got=%v err=%v", rowB, err)
		}

		// exec-a booting (a later epoch of itself) must reset only its own
		// row — exec-b's still-processing row is a live sibling's claimed
		// work and must be untouched.
		n, err := s.ResetProcessing(ctx, "exec-a", 2)
		if err != nil {
			t.Fatalf("ResetProcessing as exec-a: %v", err)
		}
		if n != 1 {
			t.Errorf("ResetProcessing(exec-a) reset %d rows, want 1 (only its own)", n)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		var gotA, gotB *domain.QueuedEvent
		for i := range rows {
			switch rows[i].ID {
			case rowA.ID:
				gotA = &rows[i]
			case rowB.ID:
				gotB = &rows[i]
			}
		}
		if gotA == nil || gotA.Status != domain.QueuedEventStatusPending {
			t.Errorf("exec-a's row = %+v, want status pending", gotA)
		}
		if gotB == nil || gotB.Status != domain.QueuedEventStatusProcessing {
			t.Errorf("exec-b's row = %+v, want status processing (untouched — a live sibling's claim)", gotB)
		}
	})

	// A boot must never reset rows claimed under its OWN current
	// epoch — only strictly earlier boots of itself are orphans. Guards
	// against a self-sweep treating its own in-flight claims as stale.
	t.Run("ResetProcessing_never_resets_current_epoch", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)

		claimed, err := s.ClaimNext(ctx, "exec-self", 5)
		if err != nil || claimed == nil {
			t.Fatalf("claim at epoch 5: got=%v err=%v", claimed, err)
		}

		// Same executor, same epoch: not an orphan of an earlier boot — must
		// not reset.
		if n, err := s.ResetProcessing(ctx, "exec-self", 5); err != nil {
			t.Fatalf("ResetProcessing at epoch 5: %v", err)
		} else if n != 0 {
			t.Errorf("ResetProcessing at the SAME epoch reset %d rows, want 0", n)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if rows[0].Status != domain.QueuedEventStatusProcessing {
			t.Errorf("row status = %q, want processing (untouched by a same-epoch reset)", rows[0].Status)
		}

		// A later epoch of the same executor (a restart) DOES sweep it.
		if n, err := s.ResetProcessing(ctx, "exec-self", 6); err != nil {
			t.Fatalf("ResetProcessing at epoch 6: %v", err)
		} else if n != 1 {
			t.Errorf("ResetProcessing at a later epoch reset %d rows, want 1", n)
		}
	})

	// The staleness backstop under the ownership-scoped boot reset: a row
	// whose owner was replaced rather than rebooted is never swept by
	// ResetProcessing, so without this it stays 'processing' — and its event
	// unrouted — forever.
	t.Run("RequeueStaleProcessing_reclaims_regardless_of_owner", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)

		// Claimed by an instance id that never comes back, eleven minutes
		// ago — past the ten-minute threshold the drain worker sweeps at.
		claimed, err := s.ClaimNext(ctx, "vanished-executor", 1)
		if err != nil || claimed == nil {
			t.Fatalf("ClaimNext: got=%v err=%v", claimed, err)
		}
		seed.BackdateClaim(t, claimed.ID, 11*time.Minute)

		n, err := s.RequeueStaleProcessing(ctx, 10*time.Minute)
		if err != nil {
			t.Fatalf("RequeueStaleProcessing: %v", err)
		}
		if n != 1 {
			t.Fatalf("RequeueStaleProcessing reclaimed %d rows, want 1", n)
		}

		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Status != domain.QueuedEventStatusPending {
			t.Errorf("status = %q, want pending after reclaim", rows[0].Status)
		}
		if rows[0].ClaimedAt != nil {
			t.Errorf("claimed_at should be cleared on reclaim, got %v", rows[0].ClaimedAt)
		}
		if rows[0].LastError != db.StaleProcessingReclaimReason {
			t.Errorf("last_error = %q, want %q", rows[0].LastError, db.StaleProcessingReclaimReason)
		}
		// attempts untouched: the claim already counted this try, so the
		// retry budget still bounds a row that keeps reaching this sweep.
		if rows[0].Attempts != 1 {
			t.Errorf("attempts = %d, want 1 (the reclaim must not reset the budget)", rows[0].Attempts)
		}
		// Re-claimable by whoever drains next, budget still counting up.
		again, err := s.ClaimNext(ctx, "successor-executor", 1)
		if err != nil || again == nil {
			t.Fatalf("re-claim after reclaim: got=%v err=%v", again, err)
		}
		if again.Attempts != 2 {
			t.Errorf("attempts = %d on the re-claim, want 2", again.Attempts)
		}
	})

	// The negative space is the whole safety argument: the sweep must be
	// incapable of stealing a claim that is merely in progress.
	t.Run("RequeueStaleProcessing_leaves_fresh_and_terminal_rows", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)

		// An in-flight claim a minute old — well inside the threshold.
		enqueueOn(t, ctx, s, orgID, entityID)
		fresh, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil || fresh == nil {
			t.Fatalf("ClaimNext (fresh): got=%v err=%v", fresh, err)
		}
		seed.BackdateClaim(t, fresh.ID, time.Minute)
		// A done row.
		enqueueOn(t, ctx, s, orgID, entityID)
		doneRow, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err := s.MarkDone(ctx, orgID, doneRow.ID); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
		// A failed row.
		enqueueOn(t, ctx, s, orgID, entityID)
		failRow, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err := s.MarkFailed(ctx, orgID, failRow.ID, "boom"); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		// A never-claimed pending row.
		enqueueOn(t, ctx, s, orgID, entityID)

		n, err := s.RequeueStaleProcessing(ctx, 10*time.Minute)
		if err != nil {
			t.Fatalf("RequeueStaleProcessing: %v", err)
		}
		if n != 0 {
			t.Fatalf("RequeueStaleProcessing reclaimed %d rows, want 0 (nothing is ten minutes stale)", n)
		}

		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		want := map[int64]string{
			fresh.ID:   domain.QueuedEventStatusProcessing,
			doneRow.ID: domain.QueuedEventStatusDone,
			failRow.ID: domain.QueuedEventStatusFailed,
		}
		for _, r := range rows {
			if w, ok := want[r.ID]; ok && r.Status != w {
				t.Errorf("row %d status = %q, want %q (untouched by the sweep)", r.ID, r.Status, w)
			}
			if r.LastError == db.StaleProcessingReclaimReason {
				t.Errorf("row %d was reclaimed by a sweep that should have matched nothing", r.ID)
			}
		}
		// The fresh claim is still owned — the sweep left it claimable by
		// nobody else.
		if next, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch); err != nil {
			t.Fatalf("ClaimNext after sweep: %v", err)
		} else if next == nil || next.ID == fresh.ID {
			t.Errorf("ClaimNext returned %v, want the never-claimed pending row, not the fresh in-flight one", next)
		}
	})

	t.Run("PruneDone_deletes_old_done_keeps_failed", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)

		// One done row.
		enqueueOn(t, ctx, s, orgID, entityID)
		doneRow, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err := s.MarkDone(ctx, orgID, doneRow.ID); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
		// One failed row.
		enqueueOn(t, ctx, s, orgID, entityID)
		failRow, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err := s.MarkFailed(ctx, orgID, failRow.ID, "boom"); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}

		// Cutoff in the future → the done row is older and gets pruned;
		// the failed row is retained regardless of age.
		n, err := s.PruneDone(ctx, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("PruneDone: %v", err)
		}
		if n != 1 {
			t.Errorf("PruneDone removed %d, want 1 (only the done row)", n)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if len(rows) != 1 || rows[0].Status != domain.QueuedEventStatusFailed {
			t.Errorf("after prune, only the failed row should remain, got %+v", rows)
		}
	})

	// ── The parked-row operator surface ──
	//
	// A 'failed' row is routing work that was dropped and will not re-emit,
	// so these two methods are the only way it ever moves again. parkOn below
	// is the shared fixture: enqueue, claim, MarkFailed.

	t.Run("ListFailedEvents_lists_parked_rows_with_entity", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		parked := parkOn(t, ctx, s, orgID, entityID, "route: store unavailable")

		got, err := s.ListFailedEvents(ctx, orgID, 0)
		if err != nil {
			t.Fatalf("ListFailedEvents: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 parked row, got %d (%+v)", len(got), got)
		}
		fe := got[0]
		if fe.ID != parked.ID {
			t.Errorf("id = %d, want %d", fe.ID, parked.ID)
		}
		if fe.EventType != domain.EventGitHubPRCICheckFailed {
			t.Errorf("event_type = %q, want %q", fe.EventType, domain.EventGitHubPRCICheckFailed)
		}
		if fe.EntityID != entityID {
			t.Errorf("entity_id = %q, want %q", fe.EntityID, entityID)
		}
		// The entity fields are what makes the row identifiable to a human;
		// a list of bare uuids is not an operator surface.
		if fe.EntitySource != "github" || fe.EntitySourceID == "" || fe.EntityTitle == "" {
			t.Errorf("entity display fields = {%q %q %q}, want all populated from the joined entity",
				fe.EntitySource, fe.EntitySourceID, fe.EntityTitle)
		}
		if fe.Attempts != 1 {
			t.Errorf("attempts = %d, want 1 (the claim that parked it)", fe.Attempts)
		}
		if fe.LastError != "route: store unavailable" {
			t.Errorf("last_error = %q, want the park reason", fe.LastError)
		}
		if fe.EnqueuedAt.IsZero() {
			t.Errorf("enqueued_at is zero; the list has no age to show")
		}
	})

	// Only parked rows, and newest first — an operator scanning the table is
	// looking for what just broke.
	t.Run("ListFailedEvents_excludes_live_rows_and_orders_newest_first", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)

		first := parkOn(t, ctx, s, orgID, entityID, "first")
		second := parkOn(t, ctx, s, orgID, entityID, "second")
		// A done row, an in-flight claim, and a never-claimed pending row —
		// none of them is dropped work.
		enqueueOn(t, ctx, s, orgID, entityID)
		doneRow, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err := s.MarkDone(ctx, orgID, doneRow.ID); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
		enqueueOn(t, ctx, s, orgID, entityID)
		if _, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch); err != nil {
			t.Fatalf("claim the in-flight row: %v", err)
		}
		enqueueOn(t, ctx, s, orgID, entityID)

		got, err := s.ListFailedEvents(ctx, orgID, 0)
		if err != nil {
			t.Fatalf("ListFailedEvents: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected exactly the 2 parked rows, got %d (%+v)", len(got), got)
		}
		if got[0].ID != second.ID || got[1].ID != first.ID {
			t.Errorf("order = [%d %d], want newest-first [%d %d]", got[0].ID, got[1].ID, second.ID, first.ID)
		}

		// limit truncates from the newest end.
		capped, err := s.ListFailedEvents(ctx, orgID, 1)
		if err != nil {
			t.Fatalf("ListFailedEvents(limit=1): %v", err)
		}
		if len(capped) != 1 || capped[0].ID != second.ID {
			t.Errorf("limit=1 returned %+v, want just the newest row %d", capped, second.ID)
		}
	})

	// entity_id is nullable, so the join can miss. Listing must still show the
	// row — it is the only record that the event was dropped, and a list that
	// hid it would hide the odd case specifically.
	t.Run("ListFailedEvents_survives_a_missing_entity", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		parked := parkOn(t, ctx, s, orgID, entityID, "boom")
		seed.ClearEntityRef(t, parked.ID)

		got, err := s.ListFailedEvents(ctx, orgID, 0)
		if err != nil {
			t.Fatalf("ListFailedEvents: %v", err)
		}
		if len(got) != 1 || got[0].ID != parked.ID {
			t.Fatalf("expected the entity-less parked row, got %+v", got)
		}
		if got[0].EntitySource != "" || got[0].EntityTitle != "" {
			t.Errorf("entity display fields = {%q %q}, want empty for a row with no entity",
				got[0].EntitySource, got[0].EntityTitle)
		}
	})

	// The retention contract the operator surface depends on: a parked row
	// has to still be there when someone comes looking, however long that
	// takes. PruneDone is the only sweep that deletes from this table.
	t.Run("ListFailedEvents_survives_a_prune_pass", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		parked := parkOn(t, ctx, s, orgID, entityID, "parked before the sweep")

		// A cutoff in the future makes every processed row older than it, so
		// this is the most aggressive prune the retention sweep can run.
		if _, err := s.PruneDone(ctx, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("PruneDone: %v", err)
		}

		got, err := s.ListFailedEvents(ctx, orgID, 0)
		if err != nil {
			t.Fatalf("ListFailedEvents after prune: %v", err)
		}
		if len(got) != 1 || got[0].ID != parked.ID {
			t.Fatalf("parked row did not survive the prune: %+v", got)
		}
		if got[0].LastError != "parked before the sweep" {
			t.Errorf("last_error = %q, want the park reason intact", got[0].LastError)
		}
	})

	t.Run("RequeueFailedEvents_grants_a_fresh_budget", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		parked := parkOn(t, ctx, s, orgID, entityID, "route: store unavailable")

		n, err := s.RequeueFailedEvents(ctx, orgID, []int64{parked.ID})
		if err != nil {
			t.Fatalf("RequeueFailedEvents: %v", err)
		}
		if n != 1 {
			t.Fatalf("RequeueFailedEvents moved %d rows, want 1", n)
		}
		if left, _ := s.ListFailedEvents(ctx, orgID, 0); len(left) != 0 {
			t.Errorf("requeued row still lists as parked: %+v", left)
		}

		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Status != domain.QueuedEventStatusPending {
			t.Errorf("status = %q, want pending", rows[0].Status)
		}
		// The whole point of the operator requeue: the row parked because its
		// budget ran out, so it needs the budget back or it re-parks on the
		// first attempt.
		if rows[0].Attempts != 0 {
			t.Errorf("attempts = %d, want 0 — an operator requeue grants a fresh budget", rows[0].Attempts)
		}
		if rows[0].ProcessedAt != nil || rows[0].ClaimedAt != nil {
			t.Errorf("claimed_at/processed_at should be cleared on requeue: %+v", rows[0])
		}
		// last_error survives until the next attempt writes over it, so the
		// reason it parked is still correlatable with whatever happens next.
		if rows[0].LastError != "route: store unavailable" {
			t.Errorf("last_error = %q, want the park reason retained until the next attempt", rows[0].LastError)
		}

		// Claimable again, counting from one.
		again, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil || again == nil {
			t.Fatalf("re-claim after requeue: got=%v err=%v", again, err)
		}
		if again.ID != parked.ID || again.Attempts != 1 {
			t.Errorf("re-claim = id %d attempts %d, want id %d attempts 1", again.ID, again.Attempts, parked.ID)
		}

		// Parking it a second time keeps the NEW reason.
		if err := s.MarkFailed(ctx, orgID, again.ID, "route: still unavailable"); err != nil {
			t.Fatalf("second MarkFailed: %v", err)
		}
		reparked, _ := s.ListFailedEvents(ctx, orgID, 0)
		if len(reparked) != 1 || reparked[0].LastError != "route: still unavailable" {
			t.Errorf("re-parked row = %+v, want the second park's reason", reparked)
		}
	})

	// The mixed-batch contract: an operator selecting rows from a stale page
	// may name ids that have since moved on. Those are no-ops, counted out.
	t.Run("RequeueFailedEvents_moves_only_failed_rows", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)

		parkedA := parkOn(t, ctx, s, orgID, entityID, "a")
		parkedB := parkOn(t, ctx, s, orgID, entityID, "b")
		enqueueOn(t, ctx, s, orgID, entityID)
		doneRow, _ := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err := s.MarkDone(ctx, orgID, doneRow.ID); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}

		n, err := s.RequeueFailedEvents(ctx, orgID, []int64{parkedA.ID, doneRow.ID, parkedB.ID})
		if err != nil {
			t.Fatalf("RequeueFailedEvents: %v", err)
		}
		if n != 2 {
			t.Fatalf("RequeueFailedEvents returned %d, want 2 (the done id is counted out)", n)
		}

		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		for _, r := range rows {
			switch r.ID {
			case parkedA.ID, parkedB.ID:
				if r.Status != domain.QueuedEventStatusPending {
					t.Errorf("row %d status = %q, want pending", r.ID, r.Status)
				}
			case doneRow.ID:
				if r.Status != domain.QueuedEventStatusDone {
					t.Errorf("done row %d status = %q, want done (untouched)", r.ID, r.Status)
				}
				if r.ProcessedAt == nil {
					t.Errorf("done row %d lost its processed_at to a requeue that should not have matched it", r.ID)
				}
			}
		}
	})

	// Idempotence: the second call finds nothing left to move. This is what
	// makes a double-click on the panel's Requeue button harmless.
	t.Run("RequeueFailedEvents_double_requeue_is_a_noop", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		parked := parkOn(t, ctx, s, orgID, entityID, "boom")

		if n, err := s.RequeueFailedEvents(ctx, orgID, []int64{parked.ID}); err != nil || n != 1 {
			t.Fatalf("first requeue: n=%d err=%v", n, err)
		}
		n, err := s.RequeueFailedEvents(ctx, orgID, []int64{parked.ID})
		if err != nil {
			t.Fatalf("second requeue: %v", err)
		}
		if n != 0 {
			t.Errorf("second requeue moved %d rows, want 0", n)
		}
		rows, _ := s.ListForEntity(ctx, orgID, entityID)
		if rows[0].Status != domain.QueuedEventStatusPending || rows[0].Attempts != 0 {
			t.Errorf("row after the double requeue = %+v, want pending with a fresh budget", rows[0])
		}
	})

	t.Run("RequeueFailedEvents_empty_and_unknown_ids", func(t *testing.T) {
		s, orgID, _ := mk(t)
		if n, err := s.RequeueFailedEvents(ctx, orgID, nil); err != nil || n != 0 {
			t.Errorf("requeue of no ids = (%d, %v), want (0, nil)", n, err)
		}
		if n, err := s.RequeueFailedEvents(ctx, orgID, []int64{987654}); err != nil || n != 0 {
			t.Errorf("requeue of an unknown id = (%d, %v), want (0, nil)", n, err)
		}
	})

	// The producer's trace context has to survive the hop intact: the
	// consumer builds a span link out of exactly these bytes, and a
	// truncated or re-encoded value yields an invalid SpanContext, which
	// degrades silently to "no link" — the failure mode nobody notices.
	t.Run("Traceparent_round_trips_through_claim", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		enqueueTracedOn(t, ctx, s, orgID, entityID, traceparent)

		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 1 || rows[0].Traceparent != traceparent {
			t.Fatalf("stored traceparent = %+v, want %q", rows, traceparent)
		}

		claimed, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil || claimed == nil {
			t.Fatalf("ClaimNext: got=%v err=%v", claimed, err)
		}
		if claimed.Traceparent != traceparent {
			t.Errorf("claimed traceparent = %q, want %q — the claim is where the consumer picks the link up", claimed.Traceparent, traceparent)
		}
	})

	// An untraced producer is the common case (tracing disabled, or a path
	// nobody instrumented), and it must be indistinguishable from any other
	// row apart from carrying no context.
	t.Run("Traceparent_absent_reads_empty", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		enqueueOn(t, ctx, s, orgID, entityID)

		claimed, err := s.ClaimNext(ctx, conformanceExecutorID, conformanceBootEpoch)
		if err != nil || claimed == nil {
			t.Fatalf("ClaimNext: got=%v err=%v", claimed, err)
		}
		if claimed.Traceparent != "" {
			t.Errorf("traceparent = %q, want empty for an untraced producer", claimed.Traceparent)
		}
		if claimed.Status != domain.QueuedEventStatusProcessing {
			t.Errorf("status = %q, want processing — an untraced row claims like any other", claimed.Status)
		}
	})

	t.Run("ListForEntity_id_order", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t)
		first := enqueueOn(t, ctx, s, orgID, entityID)
		second := enqueueOn(t, ctx, s, orgID, entityID)
		rows, err := s.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("expected 2 rows, got %d", len(rows))
		}
		if rows[0].EventID != first || rows[1].EventID != second {
			t.Errorf("id order broken: got [%q, %q], want [%q, %q]",
				rows[0].EventID, rows[1].EventID, first, second)
		}
	})
}
