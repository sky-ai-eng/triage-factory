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

// TestTaskPatchSnooze_RejectsUnusableWakeTimes pins the negative space around
// snooze_until: no route may persist status='snoozed' without a wake time the
// sweep will actually fire on. An empty or unparseable value is a 400 (the
// shape is wrong), a past instant is a 422 (the shape is right and the value
// can't be one), and the presets the picker offers are the picker's own
// vocabulary — the API takes timestamps.
func TestTaskPatchSnooze_RejectsUnusableWakeTimes(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"empty", map[string]any{"snooze_until": ""}, http.StatusBadRequest},
		{"unparseable", map[string]any{"snooze_until": "next tuesday"}, http.StatusBadRequest},
		{"preset_grammar_is_the_frontend's", map[string]any{"snooze_until": "2h"}, http.StatusBadRequest},
		{"wrong_type", map[string]any{"snooze_until": 7}, http.StatusBadRequest},
		{
			"past",
			map[string]any{"snooze_until": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
			http.StatusUnprocessableEntity,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID := seedLifecycleTask(t, s.db, "snooze-"+tc.name, lifecycleTaskOpts{})
			rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("patch = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			status, until := readSnoozeState(t, s.db, taskID)
			if status != "queued" || !until.IsZero() {
				t.Fatalf("task = (%q, %v) after a refused snooze; want it untouched at (queued, NULL)", status, until)
			}
		})
	}
}

// TestTaskPatchSnooze_WritesWakeTime pins the accepted shape: an RFC3339
// instant lands on the row as a real snooze_until, because the queue's wake
// sweep requeues on that column and nothing else.
func TestTaskPatchSnooze_WritesWakeTime(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	want := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	taskID := seedLifecycleTask(t, s.db, "snoozed", lifecycleTaskOpts{})
	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{
		"snooze_until": want.Format(time.RFC3339), "hesitation_ms": 120,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	status, until := readSnoozeState(t, s.db, taskID)
	if status != "snoozed" {
		t.Fatalf("status = %q, want snoozed", status)
	}
	if until.IsZero() {
		t.Fatal("snooze_until is NULL — the wake sweep would never requeue this task")
	}
	if delta := until.Sub(want); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("snooze_until = %v, want ~%v (delta=%v)", until, want, delta)
	}
	// The gesture keeps its historical action string so the swipe analytics
	// stay comparable across the route change.
	if got := readSwipeActions(t, s.db, taskID); !equalStrings(got, []string{"snooze"}) {
		t.Fatalf("swipe_events actions = %v, want [snooze]", got)
	}
}

// TestTaskPatchSnooze_ClearWakesTheTask pins the null arm: clearing the wake
// time returns the task to the queue, and clearing one a task doesn't have is
// a 409 rather than a silent drag out of whatever lane it was in.
func TestTaskPatchSnooze_ClearWakesTheTask(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	taskID := seedLifecycleTask(t, s.db, "wake", lifecycleTaskOpts{})
	until := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	if rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"snooze_until": until}); rec.Code != http.StatusOK {
		t.Fatalf("seed snooze = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"snooze_until": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	status, wake := readSnoozeState(t, s.db, taskID)
	if status != "queued" || !wake.IsZero() {
		t.Fatalf("task = (%q, %v) after clearing the snooze; want (queued, NULL)", status, wake)
	}
	// Waking is a field write, not a card gesture — the audit log still shows
	// only the snooze.
	if got := readSwipeActions(t, s.db, taskID); !equalStrings(got, []string{"snooze"}) {
		t.Fatalf("swipe_events actions = %v, want [snooze] (waking writes no gesture row)", got)
	}

	rec = doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"snooze_until": nil})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second clear = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTaskPatchClose_ClearsSnooze pins that closing a snoozed task clears its
// wake time: a dismissed or completed row that kept snooze_until would stay
// hidden from the queue's own filters on a later re-open.
func TestTaskPatchClose_ClearsSnooze(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for status, wantAction := range map[string]string{"dismissed": "dismiss", "done": "complete"} {
		t.Run(status, func(t *testing.T) {
			taskID := seedLifecycleTask(t, s.db, "clear-"+status, lifecycleTaskOpts{})
			until := time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)
			if rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"snooze_until": until}); rec.Code != http.StatusOK {
				t.Fatalf("seed snooze = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"status": status})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200; body=%s", status, rec.Code, rec.Body.String())
			}
			if _, until := readSnoozeState(t, s.db, taskID); !until.IsZero() {
				t.Fatalf("snooze_until = %v after %s, want NULL", until, status)
			}
			// Analytics continuity: the close arms keep the action strings
			// the swipe deck has written since it shipped.
			if got := readSwipeActions(t, s.db, taskID); !equalStrings(got, []string{"snooze", wantAction}) {
				t.Fatalf("swipe_events actions = %v, want [snooze %s]", got, wantAction)
			}
		})
	}
}

// readSwipeActions returns a task's swipe_events action strings, oldest first.
// Audit rows are append-only and observed in insertion order, so the callers'
// equalStrings comparison against an expected slice is order-sensitive on
// purpose.
func readSwipeActions(t *testing.T, database *sql.DB, taskID string) []string {
	t.Helper()
	rows, err := database.Query(`SELECT action FROM swipe_events WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("read swipe_events for %s: %v", taskID, err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scan swipe_events action: %v", err)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate swipe_events: %v", err)
	}
	return actions
}
