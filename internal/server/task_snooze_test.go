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

// TestTaskPatchSnooze_RejectsUnusableWakeTimes: no write persists a snooze the
// wake sweep cannot fire on. An empty, unparseable, preset ("2h") or non-string
// value is a 400, and a past instant is a 422.
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

// TestTaskPatchSnooze_ClearWakesTheTask: an RFC3339 snooze_until lands on the
// row as the wake time the sweep requeues on; clearing it returns the task to
// the queue, and clearing one the task does not have is a 409.
func TestTaskPatchSnooze_ClearWakesTheTask(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	taskID := seedLifecycleTask(t, s.db, "wake", lifecycleTaskOpts{})
	want := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	if rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"snooze_until": want.Format(time.RFC3339)}); rec.Code != http.StatusOK {
		t.Fatalf("snooze = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	status, until := readSnoozeState(t, s.db, taskID)
	if delta := until.Sub(want); status != "snoozed" || until.IsZero() || delta < -time.Minute || delta > time.Minute {
		t.Fatalf("task = (%q, %v) after the snooze, want (snoozed, ~%v)", status, until, want)
	}
	// The snooze is recorded as the 'snooze' gesture.
	if got := readSwipeActions(t, s.db, taskID); !equalStrings(got, []string{"snooze"}) {
		t.Fatalf("swipe_events actions = %v, want [snooze]", got)
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

// TestTaskPatchClose_ClearsSnooze: closing a snoozed task clears its wake time,
// which would otherwise hide it from the queue on a re-open, and records the
// close as the 'dismiss' or 'complete' gesture with its hesitation.
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
			rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"status": status, "hesitation_ms": 900})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200; body=%s", status, rec.Code, rec.Body.String())
			}
			if got, until := readSnoozeState(t, s.db, taskID); got != status || !until.IsZero() {
				t.Fatalf("task = (%q, %v) after %s, want (%s, NULL)", got, until, status, status)
			}
			if got := readSwipeActions(t, s.db, taskID); !equalStrings(got, []string{"snooze", wantAction}) {
				t.Fatalf("swipe_events actions = %v, want [snooze %s]", got, wantAction)
			}
			var hesitation sql.NullInt64
			if err := s.db.QueryRow(
				`SELECT hesitation_ms FROM swipe_events WHERE task_id = ? AND action = ?`, taskID, wantAction,
			).Scan(&hesitation); err != nil {
				t.Fatalf("scan hesitation_ms: %v", err)
			}
			if !hesitation.Valid || hesitation.Int64 != 900 {
				t.Errorf("hesitation_ms = %v, want 900", hesitation)
			}
		})
	}
}

// readSwipeActions returns a task's swipe_events action strings, oldest first,
// so a comparison against an expected slice is order-sensitive.
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
