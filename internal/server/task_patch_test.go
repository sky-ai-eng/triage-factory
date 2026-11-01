package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// TestTaskPatchStage_RejectsUnclaimedTask: a stage is the claimant's own
// progress marker, so writing one on a task the caller does not hold is a 403
// that changes nothing.
func TestTaskPatchStage_RejectsUnclaimedTask(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "unclaimed", lifecycleTaskOpts{})

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "in_progress"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (unclaimed task); body=%s",
			rec.Code, rec.Body.String())
	}
	if got := readTaskStatus(t, s.db, taskID); got != "queued" {
		t.Errorf("task.status = %q, want %q", got, "queued")
	}
}

// TestTaskPatch_RejectsEveryMutationOnATerminalTask: a closed task takes no
// PATCH at all, a second close included; re-opening is requeue's and undo's job.
func TestTaskPatch_RejectsEveryMutationOnATerminalTask(t *testing.T) {
	s := newTestServer(t)

	for _, seeded := range []string{"done", "dismissed"} {
		for _, body := range []map[string]any{
			{"status": "in_progress"},
			{"status": "done"},
			{"status": "dismissed"},
			{"snooze_until": "2999-01-01T00:00:00Z"},
			{"snooze_until": nil},
		} {
			name := seeded + "/" + firstKeyValue(body)
			t.Run(name, func(t *testing.T) {
				taskID := seedLifecycleTask(t, s.db, "terminal-"+name, lifecycleTaskOpts{
					claimedByUserID: runmode.LocalDefaultUserID,
					status:          seeded,
				})
				rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, body)
				if rec.Code != http.StatusConflict {
					t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), "ALREADY_TERMINAL") {
					t.Errorf("body=%s; want reason ALREADY_TERMINAL", rec.Body.String())
				}
				if got := readTaskStatus(t, s.db, taskID); got != seeded {
					t.Errorf("task.status = %q, want %q (a refusal must not change a closed task)", got, seeded)
				}
			})
		}
	}
}

// TestTaskPatchClose_SecondCloseRewritesNothing pins the store's own status
// predicate, the guard for a close that lands between the handler's pre-read and
// its write: driving the store directly stands in for that concurrent request.
func TestTaskPatchClose_SecondCloseRewritesNothing(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "double-close", lifecycleTaskOpts{})

	if rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "dismissed"}); rec.Code != http.StatusOK {
		t.Fatalf("first close = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	closedAt, closeReason := readTaskClose(t, s.db, taskID)
	if closedAt == "" || closeReason != "user_dismissed" {
		t.Fatalf("after the first close: closed_at=%q close_reason=%q; want both stamped", closedAt, closeReason)
	}

	for _, action := range []string{"complete", "dismiss"} {
		_, ok, err := s.swipes.RecordSwipe(t.Context(), runmode.LocalDefaultOrgID, taskID, action, 0, nil)
		if err != nil {
			t.Fatalf("RecordSwipe(%s) on a closed task: %v", action, err)
		}
		if ok {
			t.Fatalf("RecordSwipe(%s) returned ok=true against a closed task", action)
		}
		gotAt, gotReason := readTaskClose(t, s.db, taskID)
		if gotAt != closedAt || gotReason != closeReason {
			t.Fatalf("close metadata rewritten by a second %s: (%q, %q) want (%q, %q)",
				action, gotAt, gotReason, closedAt, closeReason)
		}
		if got := readTaskStatus(t, s.db, taskID); got != "dismissed" {
			t.Fatalf("status = %q after a refused %s, want the original dismissed", got, action)
		}
		if got := readSwipeActions(t, s.db, taskID); !equalStrings(got, []string{"dismiss"}) {
			t.Fatalf("swipe_events actions = %v, want [dismiss] (a refused close leaves no trace)", got)
		}
	}
}

// readTaskClose returns a task's close metadata as raw strings, empty when
// NULL — the columns a second close must not touch.
func readTaskClose(t *testing.T, database *sql.DB, taskID string) (closedAt, closeReason string) {
	t.Helper()
	var at, reason sql.NullString
	if err := database.QueryRow(
		`SELECT closed_at, close_reason FROM tasks WHERE id = ?`, taskID,
	).Scan(&at, &reason); err != nil {
		t.Fatalf("scan close metadata: %v", err)
	}
	return at.String, reason.String
}

// TestTaskPatch_RejectsUnwritableStatuses pins the accepted set: 'queued' is
// requeue's and undo's, 'snoozed' is spelled by snooze_until, and any word outside
// the vocabulary is a 400.
func TestTaskPatch_RejectsUnwritableStatuses(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "bad-target", lifecycleTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
	})

	for _, badStatus := range []any{"queued", "snoozed", "claimed", "in_review", "wat", "", 7, nil} {
		t.Run(statusName(badStatus), func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
				map[string]any{"status": badStatus})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status=%v: got %d, want 400; body=%s",
					badStatus, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTaskPatch_ValidatesTheBodyBeforeTheTaskState: a malformed body against a
// closed task answers for the body, not the task. A body naming no field wrote
// nothing, and one naming both axes contradicts itself.
func TestTaskPatch_ValidatesTheBodyBeforeTheTaskState(t *testing.T) {
	s := newTestServer(t)

	for name, tc := range map[string]struct {
		body map[string]any
		want int
	}{
		"empty_body":          {map[string]any{}, http.StatusBadRequest},
		"hesitation_only":     {map[string]any{"hesitation_ms": 400}, http.StatusBadRequest},
		"unwritable_status":   {map[string]any{"status": "queued"}, http.StatusBadRequest},
		"both_axes":           {map[string]any{"status": "done", "snooze_until": nil}, http.StatusBadRequest},
		"both_axes_with_wake": {map[string]any{"status": "done", "snooze_until": "2999-01-01T00:00:00Z"}, http.StatusBadRequest},
		"past_wake_time":      {map[string]any{"snooze_until": "2020-01-02T03:04:05Z"}, http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			taskID := seedLifecycleTask(t, s.db, "order-"+name, lifecycleTaskOpts{status: "dismissed"})
			rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (the body's own fault, not the task's state); body=%s",
					rec.Code, tc.want, rec.Body.String())
			}
			if got := readTaskStatus(t, s.db, taskID); got != "dismissed" {
				t.Errorf("task.status = %q, want the seeded dismissed", got)
			}
		})
	}
}

// TestTaskPatch_RejectsHesitationWhereNoGestureLands: hesitation_ms is refused
// rather than dropped on an arm with no swipe_events row to carry it, and a
// negative one is refused everywhere.
func TestTaskPatch_RejectsHesitationWhereNoGestureLands(t *testing.T) {
	s := newTestServer(t)

	t.Run("stage", func(t *testing.T) {
		taskID := seedLifecycleTask(t, s.db, "hesitation-stage", lifecycleTaskOpts{
			claimedByUserID: runmode.LocalDefaultUserID,
		})
		rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
			map[string]any{"status": "in_progress", "hesitation_ms": 250})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("negative_on_a_gesture_arm", func(t *testing.T) {
		taskID := seedLifecycleTask(t, s.db, "hesitation-negative", lifecycleTaskOpts{})
		rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
			map[string]any{"status": "dismissed", "hesitation_ms": -1})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestTaskPatchStage_HappyPath_ReassertsInProgress: the holder re-asserts
// in_progress, which the claim already set, and gets the task resource back.
func TestTaskPatchStage_HappyPath_ReassertsInProgress(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "happy-ip", lifecycleTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
	})

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "in_progress"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body taskJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ID != taskID {
		t.Errorf("response id = %q, want the patched task %q", body.ID, taskID)
	}
	if body.Status != "in_progress" {
		t.Errorf("response status = %q, want %q", body.Status, "in_progress")
	}
	if got := readTaskStatus(t, s.db, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want %q", got, "in_progress")
	}
	// A stage move is a field write, not a card gesture: nothing lands in the
	// swipe analytics.
	if got := readSwipeActions(t, s.db, taskID); len(got) != 0 {
		t.Errorf("swipe_events actions = %v, want none for a stage move", got)
	}
}

// TestTaskPatchStage_RefusesInReview: in_review is outside the vocabulary, so
// even on a task the caller holds it is a 400 naming status, and nothing lands.
func TestTaskPatchStage_RefusesInReview(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "refuse-ir", lifecycleTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
		status:          "in_progress",
	})

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "in_review"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidField, "status")
	if got := readTaskStatus(t, s.db, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want the seeded in_progress (a refusal writes nothing)", got)
	}
}

// TestTaskPatch_404OnUnknownTask: a well-formed id naming no task is a 404,
// distinct from the 403 and 409 guards.
func TestTaskPatch_404OnUnknownTask(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPatch,
		"/api/tasks/00000000-0000-0000-0000-000000000bad",
		map[string]any{"status": "in_progress"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (task not found); body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestTaskPatch_404OnMalformedID: a non-UUID path id is a 404 like a missing
// one, not the 22P02 a Postgres uuid column would raise.
func TestTaskPatch_404OnMalformedID(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPatch,
		"/api/tasks/not-a-uuid",
		map[string]any{"status": "in_progress"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (malformed id treated as not found); body=%s",
			rec.Code, rec.Body.String())
	}
}

// --- Fixture helpers ---

type lifecycleTaskOpts struct {
	status           string // defaults to "queued"
	claimedByUserID  string
	claimedByAgentID string
}

// seedLifecycleTask inserts a task row with its entity and event, and nothing
// else, and returns its UUID id. Each call needs a unique suffix to stay clear
// of the dedup index.
func seedLifecycleTask(t *testing.T, database *sql.DB, suffix string, opts lifecycleTaskOpts) string {
	t.Helper()
	ctx := context.Background()

	const eventType = "github:pr:ci_check_passed"
	entityID := "e_lc_" + suffix
	eventID := "ev_lc_" + suffix
	taskID := uuid.New().String()

	repoID := dbseed.GitHubRepository(t, database, runmode.LocalDefaultOrgID, "https://github.com", "owner", "repo")
	if _, err := database.ExecContext(ctx,
		`INSERT INTO entities (id, source, source_id, kind, state, scope, repository_id)
		 VALUES (?, 'github', ?, 'pr', 'active', 'https://github.com', ?)`,
		entityID, "owner/repo#lc-"+suffix, repoID,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES (?, ?, ?, '')`,
		eventID, entityID, eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	// A held task is in progress and an unheld one is queued: the claim is
	// the stage marker, and tasks_queue_unclaimed refuses any other pairing.
	// An explicit opts.status still wins, for the rows that are about a
	// terminal or a snooze.
	status := opts.status
	if status == "" {
		status = "queued"
		if opts.claimedByAgentID != "" || opts.claimedByUserID != "" {
			status = "in_progress"
		}
	}
	var agentClaim, userClaim any
	if opts.claimedByAgentID != "" {
		agentClaim = opts.claimedByAgentID
	}
	if opts.claimedByUserID != "" {
		userClaim = opts.claimedByUserID
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id,
		                    status, claimed_by_agent_id, claimed_by_user_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		taskID, entityID, eventType, eventID, status, agentClaim, userClaim,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

func readTaskStatus(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		t.Fatalf("scan task status: %v", err)
	}
	return status
}

// firstKeyValue names a one-key PATCH body for a subtest name.
func firstKeyValue(body map[string]any) string {
	for k, v := range body {
		return k + "=" + statusName(v)
	}
	return "empty"
}

// statusName renders a status value (including the non-string ones the type
// checks reject) as a subtest-safe name.
func statusName(v any) string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return "blank"
		}
		return t
	case nil:
		return "null"
	default:
		return "wrong_type"
	}
}
