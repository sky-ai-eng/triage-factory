package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// SwipeStoreFactory is what a per-backend test file hands to
// RunSwipeStoreConformance. The factory returns:
//   - the wired SwipeStore impl
//   - the orgID to pass to every method
//   - a seedTask hook that creates a task row the harness can swipe
//     against. The harness is schema-blind; backend tests own task
//     creation against their dialect's columns.
//   - a readTask hook that returns the task's current status +
//     snooze_until so the harness can assert state transitions
//     without touching the schema directly.
//   - a readSwipeAudit hook that returns the ordered list of
//     swipe_events.action values for a given task, so the harness
//     can pin audit-row invariants (RecordSwipe writes one,
//     RequeueTask writes none, UndoLastSwipe writes 'undo')
//     without coupling to the events table's schema.
type SwipeStoreFactory func(t *testing.T) (store db.SwipeStore, orgID string, seedTask TaskSeederForSwipes, readTask TaskReaderForSwipes, readSwipeAudit SwipeAuditReader)

// TaskSeederForSwipes creates one task row in 'queued' state and
// returns its ID. The harness calls it once per subtest.
type TaskSeederForSwipes func(t *testing.T) string

// TaskReaderForSwipes returns the task's status + snooze_until so
// the harness can assert post-swipe state without coupling to
// schema. snoozeUntil is the zero Time when NULL.
type TaskReaderForSwipes func(t *testing.T, taskID string) (status string, snoozeUntil time.Time)

// SwipeAuditReader returns the ordered list of action strings from
// swipe_events for the given task. The harness uses it to pin the
// "did the audit row get written / not written" contract that's
// not observable through the SwipeStore interface itself.
type SwipeAuditReader func(t *testing.T, taskID string) []string

// RunSwipeStoreConformance runs the shared assertion suite. The
// invariants are the contract every SwipeStore impl must hold:
//
//   - RecordSwipe maps action → status correctly + writes an audit
//     row + leaves a fresh task in the right state.
//   - SnoozeTask sets snooze_until + status='snoozed'.
//   - RequeueTask flips status back without writing an audit row,
//     and returns ok=false on a missing id.
//   - UndoLastSwipe writes an 'undo' audit row + resets the task to
//     queued + clears snooze_until.
//   - Context cancellation fails fast.
func RunSwipeStoreConformance(t *testing.T, factory SwipeStoreFactory) {
	t.Helper()

	t.Run("RecordSwipe_ClaimLeavesStatusQueued", func(t *testing.T) {
		// Claim is a responsibility-axis action, not a
		// lifecycle one. RecordSwipe records the audit row but leaves
		// status at 'queued' — the swipe handler stamps the claim
		// column separately, and the Board's derived filter (claim
		// cols + status) is what reflects "user has this." See spec
		// §2 three-axes framing.
		store, orgID, seedTask, readTask, readAudit := factory(t)
		ctx := context.Background()
		taskID := seedTask(t)
		newStatus, err := store.RecordSwipe(ctx, orgID, taskID, "claim", 200, nil)
		if err != nil {
			t.Fatalf("RecordSwipe: %v", err)
		}
		if newStatus != "queued" {
			t.Fatalf("newStatus=%q want queued (claim no longer changes status)", newStatus)
		}
		status, _ := readTask(t, taskID)
		if status != "queued" {
			t.Fatalf("task.status=%q want queued", status)
		}
		// Audit row invariant: RecordSwipe writes exactly one
		// swipe_events row whose action matches the call. Claim col
		// stamping happens at the handler layer; conformance tests
		// here only pin the SwipeStore contract.
		if got := readAudit(t, taskID); !equalActions(got, []string{"claim"}) {
			t.Fatalf("swipe_events actions=%v want [claim]", got)
		}
	})

	t.Run("RecordSwipe_DismissMapsToDismissed", func(t *testing.T) {
		store, orgID, seedTask, readTask, readAudit := factory(t)
		taskID := seedTask(t)
		if _, err := store.RecordSwipe(context.Background(), orgID, taskID, "dismiss", 0, nil); err != nil {
			t.Fatalf("RecordSwipe: %v", err)
		}
		status, _ := readTask(t, taskID)
		if status != "dismissed" {
			t.Fatalf("task.status=%q want dismissed", status)
		}
		if got := readAudit(t, taskID); !equalActions(got, []string{"dismiss"}) {
			t.Fatalf("swipe_events actions=%v want [dismiss]", got)
		}
	})

	t.Run("RecordSwipe_UnknownActionFallsBackToQueued", func(t *testing.T) {
		// Misuse path: action="garbage" must NOT silently strand the
		// task in an invalid status. The fallback keeps the task
		// reachable; the audit row preserves the ORIGINAL (wrong)
		// action so a future debug pass can find it — the test
		// pins both halves of that contract.
		store, orgID, seedTask, _, readAudit := factory(t)
		taskID := seedTask(t)
		newStatus, err := store.RecordSwipe(context.Background(), orgID, taskID, "garbage", 0, nil)
		if err != nil {
			t.Fatalf("RecordSwipe: %v", err)
		}
		if newStatus != "queued" {
			t.Fatalf("newStatus=%q want queued for unknown action", newStatus)
		}
		if got := readAudit(t, taskID); !equalActions(got, []string{"garbage"}) {
			t.Fatalf("swipe_events actions=%v want [garbage] (original action preserved for audit)", got)
		}
	})

	t.Run("RecordSwipe_OnSnoozedTask_ClearsSnoozeUntil", func(t *testing.T) {
		// The queue listing hides 'queued' rows whose snooze_until is
		// in the future. A snoozed→claimed→requeue path that leaves
		// snooze_until stamped would silently drop the task off the
		// Board. RecordSwipe must clear snooze_until on every
		// transition; only SnoozeTask sets it.
		//
		// Claim no longer changes status (it's now a
		// responsibility-axis action), so post-claim status reads as
		// 'queued'. The snooze-cleared invariant still holds — that's
		// what this test pins.
		store, orgID, seedTask, readTask, _ := factory(t)
		taskID := seedTask(t)
		until := time.Now().Add(2 * time.Hour).UTC()
		if ok, err := store.SnoozeTask(context.Background(), orgID, taskID, until, 0); err != nil || !ok {
			t.Fatalf("snooze: ok=%v err=%v", ok, err)
		}
		if _, err := store.RecordSwipe(context.Background(), orgID, taskID, "claim", 0, nil); err != nil {
			t.Fatalf("RecordSwipe: %v", err)
		}
		status, snoozeUntil := readTask(t, taskID)
		if status != "queued" {
			t.Fatalf("status=%q want queued (snoozed→claim transitions to queued, not claimed)", status)
		}
		if !snoozeUntil.IsZero() {
			t.Fatalf("snooze_until=%v want zero after RecordSwipe transition (stale snooze hides queued tasks)", snoozeUntil)
		}
	})

	t.Run("RecordSwipe_SnoozeWritesWakeTime", func(t *testing.T) {
		// The swipe route's snooze arm lands here (the standalone snooze
		// route goes through SnoozeTask). Both must leave a real
		// snooze_until: the queue's wake sweep requeues on that column,
		// so a snoozed row without one never comes back.
		store, orgID, seedTask, readTask, readAudit := factory(t)
		taskID := seedTask(t)
		until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
		newStatus, err := store.RecordSwipe(context.Background(), orgID, taskID, "snooze", 0, &until)
		if err != nil {
			t.Fatalf("RecordSwipe: %v", err)
		}
		if newStatus != "snoozed" {
			t.Fatalf("newStatus=%q want snoozed", newStatus)
		}
		status, snoozeUntil := readTask(t, taskID)
		if status != "snoozed" {
			t.Fatalf("task.status=%q want snoozed", status)
		}
		if snoozeUntil.IsZero() {
			t.Fatalf("snooze_until is NULL after a snooze swipe — the wake sweep would never requeue this task")
		}
		// Same 5s slop as SnoozeTask's assertion below: SQLite roundtrips
		// DATETIME through a string at second resolution.
		if delta := snoozeUntil.Sub(until); delta < -5*time.Second || delta > 5*time.Second {
			t.Fatalf("snooze_until=%v want close to %v (delta=%v)", snoozeUntil, until, delta)
		}
		if got := readAudit(t, taskID); !equalActions(got, []string{"snooze"}) {
			t.Fatalf("swipe_events actions=%v want [snooze]", got)
		}
	})

	t.Run("RecordSwipe_SnoozeWithoutWakeTimeIsRefused", func(t *testing.T) {
		// The negative space of the case above: no dialect may persist
		// status='snoozed' with snooze_until NULL, so a nil wake time is
		// refused outright — audit row included, since the whole write is
		// one tx and a refused gesture leaves no trace.
		store, orgID, seedTask, readTask, readAudit := factory(t)
		taskID := seedTask(t)
		if _, err := store.RecordSwipe(context.Background(), orgID, taskID, "snooze", 0, nil); !errors.Is(err, db.ErrSnoozeUntilRequired) {
			t.Fatalf("RecordSwipe(snooze, nil) error = %v, want ErrSnoozeUntilRequired", err)
		}
		status, snoozeUntil := readTask(t, taskID)
		if status != "queued" || !snoozeUntil.IsZero() {
			t.Fatalf("task = (%q, %v) after a refused snooze; want the seeded (queued, zero)", status, snoozeUntil)
		}
		if got := readAudit(t, taskID); len(got) != 0 {
			t.Fatalf("swipe_events actions=%v want none (a refused snooze leaves no trace)", got)
		}
	})

	t.Run("SnoozeTask_SetsSnoozeUntilAndStatus", func(t *testing.T) {
		store, orgID, seedTask, readTask, readAudit := factory(t)
		taskID := seedTask(t)
		until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
		if ok, err := store.SnoozeTask(context.Background(), orgID, taskID, until, 150); err != nil || !ok {
			t.Fatalf("SnoozeTask: ok=%v err=%v", ok, err)
		}
		status, snoozeUntil := readTask(t, taskID)
		if status != "snoozed" {
			t.Fatalf("status=%q want snoozed", status)
		}
		// SQLite stores DATETIME as a string and roundtrips through
		// time.Time at second resolution; allow a 5s slop so the
		// assertion isn't brittle across timezone serialization.
		if delta := snoozeUntil.Sub(until); delta < -5*time.Second || delta > 5*time.Second {
			t.Fatalf("snooze_until=%v want close to %v (delta=%v)", snoozeUntil, until, delta)
		}
		if got := readAudit(t, taskID); !equalActions(got, []string{"snooze"}) {
			t.Fatalf("swipe_events actions=%v want [snooze]", got)
		}
	})

	t.Run("RequeueTask_FlipsToQueuedWithoutAuditRow", func(t *testing.T) {
		store, orgID, seedTask, readTask, readAudit := factory(t)
		taskID := seedTask(t)
		// First swipe → claimed so we can observe the transition back.
		if _, err := store.RecordSwipe(context.Background(), orgID, taskID, "claim", 0, nil); err != nil {
			t.Fatalf("seed swipe: %v", err)
		}
		ok, err := store.RequeueTask(context.Background(), orgID, taskID)
		if err != nil {
			t.Fatalf("RequeueTask: %v", err)
		}
		if !ok {
			t.Fatalf("RequeueTask returned ok=false for an existing task")
		}
		status, _ := readTask(t, taskID)
		if status != "queued" {
			t.Fatalf("status=%q want queued", status)
		}
		// Audit invariant: the seed claim wrote one row; RequeueTask
		// must NOT have appended a second row. The audit log is the
		// swipe-card decision history; drag-to-Queue gestures aren't
		// swipes and would muddy the analytics if logged.
		if got := readAudit(t, taskID); !equalActions(got, []string{"claim"}) {
			t.Fatalf("swipe_events actions=%v want [claim] (RequeueTask must NOT write an audit row)", got)
		}
	})

	t.Run("RequeueTask_OkFalseOnMissingTask", func(t *testing.T) {
		// Use a syntactically-valid UUID for the missing id —
		// Postgres rejects non-UUID strings at the column type
		// level before the WHERE filter ever runs, and we want to
		// assert the "no row matched" path, not the parse error.
		store, orgID, _, _, _ := factory(t)
		missing := "00000000-0000-0000-0000-000000000000"
		ok, err := store.RequeueTask(context.Background(), orgID, missing)
		if err != nil {
			t.Fatalf("RequeueTask returned error for missing id: %v", err)
		}
		if ok {
			t.Fatalf("ok=true for missing id; want false")
		}
	})

	t.Run("UndoLastSwipe_ResetsTaskAndClearsSnooze", func(t *testing.T) {
		store, orgID, seedTask, readTask, readAudit := factory(t)
		taskID := seedTask(t)
		// Snooze first so we can verify the undo clears snooze_until.
		until := time.Now().Add(time.Hour).UTC()
		if ok, err := store.SnoozeTask(context.Background(), orgID, taskID, until, 0); err != nil || !ok {
			t.Fatalf("snooze: ok=%v err=%v", ok, err)
		}
		if err := store.UndoLastSwipe(context.Background(), orgID, taskID); err != nil {
			t.Fatalf("UndoLastSwipe: %v", err)
		}
		status, snoozeUntil := readTask(t, taskID)
		if status != "queued" {
			t.Fatalf("status=%q want queued after undo", status)
		}
		if !snoozeUntil.IsZero() {
			t.Fatalf("snooze_until=%v want zero after undo", snoozeUntil)
		}
		// Audit invariant: the seed snooze wrote one row; UndoLastSwipe
		// appended a second row whose action is exactly 'undo' (the
		// swipe-history view filters on this).
		if got := readAudit(t, taskID); !equalActions(got, []string{"snooze", "undo"}) {
			t.Fatalf("swipe_events actions=%v want [snooze undo]", got)
		}
	})

	t.Run("CtxCancellation_FailsFast", func(t *testing.T) {
		store, orgID, seedTask, _, _ := factory(t)
		taskID := seedTask(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.RecordSwipe(ctx, orgID, taskID, "claim", 0, nil); err == nil {
			t.Fatalf("RecordSwipe with cancelled ctx returned nil error")
		}
	})
}

// equalActions does an order-sensitive comparison of two action
// slices. Audit rows are append-only and observed in insertion
// order, so slice equality is the right check.
func equalActions(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
