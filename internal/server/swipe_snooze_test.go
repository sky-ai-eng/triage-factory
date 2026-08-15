package server

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// readSnoozeState returns a task's status + snooze_until. A NULL
// snooze_until reads back as the zero time.
func readSnoozeState(t *testing.T, database *sql.DB, taskID string) (string, time.Time) {
	t.Helper()
	var status string
	var until sql.NullTime
	if err := database.QueryRow(
		`SELECT status, snooze_until FROM tasks WHERE id = ?`, taskID,
	).Scan(&status, &until); err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	if until.Valid {
		return status, until.Time
	}
	return status, time.Time{}
}

// TestSwipeSnooze_RequiresWakeTime pins that the swipe route's snooze arm
// takes the same `until` the standalone /snooze route does. Without one the
// row used to land status='snoozed' with snooze_until NULL — an indefinite
// snooze the wake sweep never returns to the queue.
func TestSwipeSnooze_RequiresWakeTime(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"absent", map[string]any{"action": "snooze", "hesitation_ms": 0}},
		{"empty", map[string]any{"action": "snooze", "until": ""}},
		{"unparseable", map[string]any{"action": "snooze", "until": "next tuesday"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID := seedAdvanceTask(t, s.db, "snooze-"+tc.name, advanceTaskOpts{})
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("swipe = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			status, until := readSnoozeState(t, s.db, taskID)
			if status != "queued" || !until.IsZero() {
				t.Fatalf("task = (%q, %v) after a refused snooze; want it untouched at (queued, NULL)", status, until)
			}
		})
	}
}

// TestSwipeSnooze_WritesWakeTime pins the accepted shapes: the shorthand
// grammar and RFC3339, both landing a real snooze_until on the row.
func TestSwipeSnooze_WritesWakeTime(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	explicit := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name  string
		until string
		want  time.Time
	}{
		{"shorthand", "2h", time.Now().Add(2 * time.Hour)},
		{"rfc3339", explicit.Format(time.RFC3339), explicit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID := seedAdvanceTask(t, s.db, "snoozed-"+tc.name, advanceTaskOpts{})
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe", map[string]any{
				"action": "snooze", "until": tc.until, "hesitation_ms": 120,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("swipe = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			status, until := readSnoozeState(t, s.db, taskID)
			if status != "snoozed" {
				t.Fatalf("status = %q, want snoozed", status)
			}
			if until.IsZero() {
				t.Fatal("snooze_until is NULL — the wake sweep would never requeue this task")
			}
			if delta := until.Sub(tc.want); delta < -time.Minute || delta > time.Minute {
				t.Fatalf("snooze_until = %v, want ~%v (delta=%v)", until, tc.want, delta)
			}
		})
	}
}

// TestSwipeNonSnoozeActionsClearSnooze pins that threading a wake time
// through RecordSwipe didn't change the other actions: every non-snooze
// swipe still clears snooze_until, so a snoozed task doesn't stay hidden
// from the queue after being dismissed or completed.
func TestSwipeNonSnoozeActionsClearSnooze(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, action := range []string{"dismiss", "complete"} {
		t.Run(action, func(t *testing.T) {
			taskID := seedAdvanceTask(t, s.db, "clear-"+action, advanceTaskOpts{})
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe", map[string]any{
				"action": "snooze", "until": "4h",
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("seed snooze = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			rec = doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe", map[string]any{"action": action})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200; body=%s", action, rec.Code, rec.Body.String())
			}
			if _, until := readSnoozeState(t, s.db, taskID); !until.IsZero() {
				t.Fatalf("snooze_until = %v after %s, want NULL", until, action)
			}
		})
	}
}
