package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskPatchStage_RejectsBotClaimedTask pins the guard rule for the manual
// user transition: only a task the CALLER holds may move through Claimed →
// In Progress → In Review. Bot-claimed tasks transition automatically via the
// spawner (see internal/delegate/spawner.go recomputeTaskBoardColumn);
// allowing the user to flip them by hand would race the conversation
// lifecycle.
func TestTaskPatchStage_RejectsBotClaimedTask(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "bot-claimed", lifecycleTaskOpts{
		claimedByAgentID: runmode.LocalDefaultAgentID,
	})

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "in_progress"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (bot-claimed task isn't the caller's to advance); body=%s",
			rec.Code, rec.Body.String())
	}
	if got := readTaskStatus(t, s.db, taskID); got != "queued" {
		t.Errorf("task.status = %q, want %q", got, "queued")
	}
}

// TestTaskPatchStage_RejectsUnclaimedTask covers the second guard: a stage is
// the claimant's own progress marker, so an unclaimed (Queued column) task
// must be claimed first. The AdvanceStatusForUser store method enforces the
// same rule with a WHERE clause on claimed_by_user_id; the handler's 403 is
// what distinguishes "not yours" from a lost race.
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

// TestTaskPatch_RejectsEveryMutationOnATerminalTask is the structural close of
// the terminal-rewrite hole: a closed task takes no PATCH at all, dismiss and
// complete included — those used to rewrite closed_at / close_reason on an
// already-closed row and answer 200. Re-opening is requeue's and undo's job.
func TestTaskPatch_RejectsEveryMutationOnATerminalTask(t *testing.T) {
	s := newTestServer(t)

	for _, seeded := range []string{"done", "dismissed"} {
		for _, body := range []map[string]any{
			{"status": "in_progress"},
			{"status": "in_review"},
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

// TestTaskPatchClose_SecondCloseRewritesNothing pins the guard that the
// handler's pre-read cannot provide. The pre-read and the write are separate
// statements, so a close can land between them; the store's own status
// predicate is what makes the second write find no row. Driving the store
// directly is the point — it stands in for the write half of a concurrent
// request that passed its own pre-read a moment earlier.
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

// TestTaskPatch_RejectsUnwritableStatuses pins the accepted set. 'queued' is
// requeue/undo territory — a bare status write would leave the claim stamped
// and the agent's artifacts stranded — and 'snoozed' is what setting
// snooze_until means, so accepting it as a status would be a second spelling
// that can't carry the wake time the row needs.
func TestTaskPatch_RejectsUnwritableStatuses(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "bad-target", lifecycleTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
	})

	for _, badStatus := range []any{"queued", "snoozed", "claimed", "wat", "", 7, nil} {
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

// TestTaskPatch_ValidatesTheBodyBeforeTheTaskState pins the guard ordering as
// a decision rather than an accident: a malformed body against a closed task
// answers for the body, not for the task. The two rounds are independent — a
// request can be wrong in its own right regardless of what it is aimed at —
// and reversing them would make every shape error on a closed task report a
// state conflict the caller can do nothing about.
func TestTaskPatch_ValidatesTheBodyBeforeTheTaskState(t *testing.T) {
	s := newTestServer(t)

	for name, tc := range map[string]struct {
		body map[string]any
		want int
	}{
		"empty_body":        {map[string]any{}, http.StatusBadRequest},
		"unwritable_status": {map[string]any{"status": "queued"}, http.StatusBadRequest},
		"both_axes":         {map[string]any{"status": "done", "snooze_until": nil}, http.StatusBadRequest},
		"past_wake_time":    {map[string]any{"snooze_until": "2020-01-02T03:04:05Z"}, http.StatusUnprocessableEntity},
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

// TestTaskPatch_RejectsEmptyAndContradictoryBodies pins the two shapes that
// aren't a partial update at all: a body naming no field wrote nothing (and
// "updated" is the one answer a client can't tell from a real write), and a
// body naming both axes says two contradictory things about where the task
// lands.
func TestTaskPatch_RejectsEmptyAndContradictoryBodies(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "empty-body", lifecycleTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
	})

	for name, body := range map[string]map[string]any{
		"empty":              {},
		"hesitation_only":    {"hesitation_ms": 400},
		"both_axes":          {"status": "done", "snooze_until": "2999-01-01T00:00:00Z"},
		"both_axes_with_nil": {"status": "done", "snooze_until": nil},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if got := readTaskStatus(t, s.db, taskID); got != "queued" {
				t.Errorf("task.status = %q, want the seeded queued", got)
			}
		})
	}
}

// TestTaskPatch_RejectsHesitationWhereNoGestureLands pins that hesitation_ms
// is refused rather than silently dropped on the arms with no swipe_events row
// to carry it. A field the server accepts and discards is indistinguishable
// from one it recorded.
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

// TestTaskPatchStage_HappyPath_QueuedToInProgress is the canonical flow: a
// task the caller holds moves from 'queued' to 'in_progress', and the route
// answers with the task resource rather than a status stub.
func TestTaskPatchStage_HappyPath_QueuedToInProgress(t *testing.T) {
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
	// swipe analytics, matching what /advance wrote before this route existed.
	if got := readSwipeActions(t, s.db, taskID); len(got) != 0 {
		t.Errorf("swipe_events actions = %v, want none for a stage move", got)
	}
}

// TestTaskPatchStage_HappyPath_InProgressToInReview is the second forward
// step. Backward (in_review → in_progress) is allowed by the same store method
// — the caller picks a sane next state — and is implicitly covered by the seed
// using status='in_progress'.
func TestTaskPatchStage_HappyPath_InProgressToInReview(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "happy-ir", lifecycleTaskOpts{
		claimedByUserID: runmode.LocalDefaultUserID,
		status:          "in_progress",
	})

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "in_review"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := readTaskStatus(t, s.db, taskID); got != "in_review" {
		t.Errorf("task.status = %q, want %q", got, "in_review")
	}
}

// TestTaskPatchClose_WritesHistoricalActionStrings pins analytics continuity:
// dismissed and done keep writing the 'dismiss' and 'complete' rows the swipe
// deck has written since it shipped, so the historical aggregates stay
// comparable across the route change.
func TestTaskPatchClose_WritesHistoricalActionStrings(t *testing.T) {
	s := newTestServer(t)

	for status, wantAction := range map[string]string{"dismissed": "dismiss", "done": "complete"} {
		t.Run(status, func(t *testing.T) {
			taskID := seedLifecycleTask(t, s.db, "close-"+status, lifecycleTaskOpts{})
			rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
				map[string]any{"status": status, "hesitation_ms": 900})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if got := readTaskStatus(t, s.db, taskID); got != status {
				t.Errorf("task.status = %q, want %q", got, status)
			}
			if got := readSwipeActions(t, s.db, taskID); !equalStrings(got, []string{wantAction}) {
				t.Errorf("swipe_events actions = %v, want [%s]", got, wantAction)
			}
			var hesitation sql.NullInt64
			if err := s.db.QueryRow(
				`SELECT hesitation_ms FROM swipe_events WHERE task_id = ?`, taskID,
			).Scan(&hesitation); err != nil {
				t.Fatalf("scan hesitation_ms: %v", err)
			}
			if !hesitation.Valid || hesitation.Int64 != 900 {
				t.Errorf("hesitation_ms = %v, want 900", hesitation)
			}
		})
	}
}

// TestTaskPatch_404OnUnknownTask covers the not-found case. With the pre-load
// Get in the handler, "task not found" cleanly produces 404 — distinct from
// the 403 / 409 guard paths above. The 00000000-…-0bad id is a valid UUID
// format so it passes the uuid.Parse gate and reaches the Get call.
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

// TestTaskPatch_404OnMalformedID covers the Postgres-portability case: a
// non-UUID path id would surface as SQLSTATE 22P02 from the store layer
// without the handler-side uuid.Parse guard. We treat it as 404 (same as a
// missing UUID-shaped id) rather than 400 so the API doesn't expose
// backend-specific id-format quirks.
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

// seedLifecycleTask inserts a task row directly. Mirrors the
// pendingApprovalFixture pattern in requeue_test.go but without the
// conversation / review / memory machinery — the lifecycle tests only touch
// the task row. Each call uses a unique suffix to dodge the dedup index.
//
// taskID uses a real UUID since every task route validates the path id as a
// UUID up front (defense against Postgres' 22P02 on malformed input). Entity +
// event ids stay suffix-based strings since no handler validates those.
func seedLifecycleTask(t *testing.T, database *sql.DB, suffix string, opts lifecycleTaskOpts) string {
	t.Helper()
	ctx := context.Background()

	const eventType = "github:pr:ci_check_passed"
	entityID := "e_lc_" + suffix
	eventID := "ev_lc_" + suffix
	taskID := uuid.New().String()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES (?, 'github', ?, 'pr', 'active')`,
		entityID, "owner/repo#lc-"+suffix,
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

	status := opts.status
	if status == "" {
		status = "queued"
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
