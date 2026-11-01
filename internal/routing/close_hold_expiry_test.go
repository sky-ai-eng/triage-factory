package routing

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
)

// A held close stops the work an event started once it has held for the
// sweep's limit, and the next pass closes the task unless a person's work or
// deliverable still holds it. A person's work is never stopped on the limit.

// startedByEvent makes a seeded run event-started, as a fired trigger's is:
// no person delegated it.
func startedByEvent(t *testing.T, database *sql.DB, runID, convID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE blueprint_runs SET trigger_type = 'event', creator_user_id = NULL WHERE id = ?`, runID); err != nil {
		t.Fatalf("make blueprint run %s event-started: %v", runID, err)
	}
	if _, err := database.Exec(`UPDATE conversations SET trigger_type = 'event', creator_user_id = NULL WHERE id = ?`, convID); err != nil {
		t.Fatalf("make conversation %s event-started: %v", convID, err)
	}
}

// ageHold moves back the time the task's close was first held by two hours,
// past DefaultCloseHoldTimeout.
func ageHold(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if _, err := database.Exec(`
		UPDATE tasks SET close_deferred_at = strftime('%Y-%m-%d %H:%M:%f', close_deferred_at, '-2 hours') WHERE id = ?
	`, taskID); err != nil {
		t.Fatalf("age task %s's hold: %v", taskID, err)
	}
}

// TestCloseHoldExpiry_LeavesAYoungHoldAlone: before the limit, the sweep stops
// nothing and the close stays held.
func TestCloseHoldExpiry_LeavesAYoungHoldAlone(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#expiry-young")
	startedByEvent(t, f.database, f.runID, f.convID)

	f.r.sweepCloseDeferrals(t.Context(), DefaultCloseHoldTimeout)

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task held for less than the limit")
	}
	if got := f.sp.stoppedIDs(); len(got) != 0 {
		t.Errorf("stopped = %v, want nothing", got)
	}
	if cancelRequested(t, f.database, f.runID) {
		t.Error("the run was called off")
	}
}

// TestCloseHoldExpiry_StopsTheWorkThenClosesTheTask: past the limit, the
// pass calls the run off and stops it on the limit's own cause, and leaves
// the close held. With nothing else holding the task, the next pass closes it
// the way any held close applies: the event's reason and type, its audit row,
// the teardown.
func TestCloseHoldExpiry_StopsTheWorkThenClosesTheTask(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#expiry-closes")
	startedByEvent(t, f.database, f.runID, f.convID)
	ageHold(t, f.database, f.taskID)

	f.r.sweepCloseDeferrals(t.Context(), DefaultCloseHoldTimeout)

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the expiry closed the task before the stopped run settled")
	}
	if !cancelRequested(t, f.database, f.runID) {
		t.Error("the expired run was not called off")
	}
	if got, causes := f.sp.stoppedIDs(), f.sp.stopCauses(); !slices.Equal(got, []string{f.convID}) || !slices.Equal(causes, []delegate.StopCause{delegate.StopCauseCloseHoldExpired}) {
		t.Errorf("stopped = %v for %v, want [%s] for the expired hold", got, causes, f.convID)
	}

	f.r.sweepCloseDeferrals(t.Context(), DefaultCloseHoldTimeout)

	status, reason, eventType := taskRow(t, f.database, f.taskID)
	if status != "done" || reason != "auto_closed_by_event" || eventType != "github:pr:ci_check_passed" {
		t.Errorf("task = (%q, %q, %q), want done by the pass", status, reason, eventType)
	}
	if n := closeAuditCount(t, f.database, f.taskID); n != 1 {
		t.Errorf("close audit rows = %d, want 1", n)
	}
	if !slices.Contains(f.sp.tornDownCopy(), f.taskID) {
		t.Errorf("teardowns = %v, want the task %s among them", f.sp.tornDownCopy(), f.taskID)
	}
	if ev := heldClose(t, f.database, f.taskID); ev != "" {
		t.Errorf("held close = %q after the close, want none", ev)
	}
}

// TestCloseHoldExpiry_StopsTheWorkAndKeepsTheCloseADeliverableHolds: a draft
// pull request awaiting a person does not run out. The run is stopped on the
// limit's own cause, and the close stays held across passes, with nothing
// torn down.
func TestCloseHoldExpiry_StopsTheWorkAndKeepsTheCloseADeliverableHolds(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#expiry-awaiting")
	startedByEvent(t, f.database, f.runID, f.convID)
	seedDraftPullRequest(t, f.database, f.convID, f.entityID)
	ageHold(t, f.database, f.taskID)

	f.r.sweepCloseDeferrals(t.Context(), DefaultCloseHoldTimeout)
	f.r.sweepCloseDeferrals(t.Context(), DefaultCloseHoldTimeout)

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task whose draft awaits a person")
	}
	if heldClose(t, f.database, f.taskID) != f.eventID {
		t.Error("the sweep dropped the close a draft awaiting a person holds")
	}
	if !cancelRequested(t, f.database, f.runID) {
		t.Error("the expired run was not called off")
	}
	if got, causes := f.sp.stoppedIDs(), f.sp.stopCauses(); !slices.Equal(got, []string{f.convID}) || !slices.Equal(causes, []delegate.StopCause{delegate.StopCauseCloseHoldExpired}) {
		t.Errorf("stopped = %v for %v, want [%s] once, for the expired hold", got, causes, f.convID)
	}
	if slices.Contains(f.sp.tornDownCopy(), f.taskID) {
		t.Error("the sweep tore down the draft awaiting a person")
	}
}

// TestCloseHoldExpiry_LeavesAPersonsWorkRunning: a person's delegation is
// told the task resolved without being told to stop, so the limit never
// stops it and the close stays held.
func TestCloseHoldExpiry_LeavesAPersonsWorkRunning(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#expiry-person")
	ageHold(t, f.database, f.taskID)

	f.r.sweepCloseDeferrals(t.Context(), DefaultCloseHoldTimeout)

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the expiry closed a task a person's delegation is working on")
	}
	if heldClose(t, f.database, f.taskID) != f.eventID {
		t.Error("the expiry dropped the close a person's work holds")
	}
	if got := f.sp.stoppedIDs(); len(got) != 0 {
		t.Errorf("stopped = %v, want nothing", got)
	}
	if cancelRequested(t, f.database, f.runID) {
		t.Error("the person's run was called off")
	}
}

// TestParseCloseHoldTimeout: unset is the default, and a value that is not a
// positive duration falls back to it with an error to log.
func TestParseCloseHoldTimeout(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: "", want: "20m0s"},
		{raw: " 45m ", want: "45m0s"},
		{raw: "0s", want: "20m0s", wantErr: true},
		{raw: "-5m", want: "20m0s", wantErr: true},
		{raw: "twenty", want: "20m0s", wantErr: true},
	} {
		got, err := ParseCloseHoldTimeout(tc.raw)
		if got.String() != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("ParseCloseHoldTimeout(%q) = %s, %v; want %s, error %v", tc.raw, got, err, tc.want, tc.wantErr)
		}
	}
}
