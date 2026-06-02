package server

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// pendingApprovalFixture installs the full FK chain for a task whose
// delegated run is sitting in pending_approval with a saved review +
// comments + agent-side memory row. Returns (taskID, runID,
// reviewID). Centralized here so each requeue test exercises the
// exact shape SKY-206 is meant to clean up: agent finished, wrote
// memory, prepared a review, awaiting human submit.
func pendingApprovalFixture(t *testing.T, database *sql.DB) (taskID, runID, reviewID string) {
	t.Helper()

	const eventType = "github:pr:ci_check_passed"
	// SKY-261 B+: pre-B+ this fixture used status='delegated' to mean
	// "the bot owns this task and has a pending review." Post-B+ that
	// shape is status='queued' + claimed_by_agent_id stamped — status
	// is lifecycle-only, claim is responsibility. Statements split
	// (modernc.org/sqlite multi-statement Exec is unreliable on FK
	// chains).
	if _, err := database.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_pa', 'github', 'owner/repo#pa', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_pa', 'e_pa', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_pa', 'Review', 'body', ?, ?)`,
		runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_agent_id)
		 VALUES ('t_pa', 'e_pa', ?, 'ev_pa', 'queued', ?)`,
		eventType, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO runs (id, task_id, prompt_id, status, trigger_type)
		 VALUES ('r_pa', 't_pa', 'p_pa', 'pending_approval', 'manual')`,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// run_memory: agent finished and wrote its self-report (the
	// SKY-204 termination upsert). We assert below that
	// human_content lands without trampling agent_content.
	if err := sqlitestore.New(database).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrg, "r_pa", "e_pa", "", "agent self-report"); err != nil {
		t.Fatalf("UpsertAgentMemory: %v", err)
	}

	// Pending review with one comment, populated via the same
	// helpers production uses so the original_* columns get the
	// real write-once snapshots.
	if err := sqlitestore.New(database).Reviews.Create(context.Background(), runmode.LocalDefaultOrgID, domain.PendingReview{
		ID: "rev_pa", PRNumber: 7, Owner: "owner", Repo: "repo", CommitSHA: "sha", DiffLines: "", RunID: "r_pa",
	}); err != nil {
		t.Fatalf("CreatePendingReview: %v", err)
	}
	if err := sqlitestore.New(database).Reviews.AddComment(context.Background(), runmode.LocalDefaultOrgID, domain.PendingReviewComment{
		ID: "c_pa", ReviewID: "rev_pa", Path: "x.go", Line: 1, Body: "agent comment",
	}); err != nil {
		t.Fatalf("AddPendingReviewComment: %v", err)
	}
	if err := sqlitestore.New(database).Reviews.SetSubmission(context.Background(), runmode.LocalDefaultOrgID, "rev_pa", "agent draft body", "APPROVE"); err != nil {
		t.Fatalf("SetPendingReviewSubmission: %v", err)
	}
	return "t_pa", "r_pa", "rev_pa"
}

// assertPendingApprovalCleanedUp checks every post-condition the
// SKY-206 cleanup is meant to deliver: task at the expected
// post-state, run cancelled with the discriminator stop_reason,
// pending_reviews + comments gone, human_content recording the
// discard with a marker phrase that distinguishes the requeue
// from the dismiss flavor, agent_content preserved (the whole
// point of SKY-204 was keeping both halves). wantTaskStatus and
// wantHumanContentMarker let callers vary the assertion across
// the requeue (`queued` + "returned to the triage queue") and
// dismiss (`dismissed` + "dismissed the task entirely") paths.
func assertPendingApprovalCleanedUp(
	t *testing.T,
	database *sql.DB,
	taskID, runID, reviewID string,
	wantTaskStatus, wantHumanContentMarker string,
) {
	t.Helper()

	var taskStatus string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if taskStatus != wantTaskStatus {
		t.Errorf("task.status = %q, want %q", taskStatus, wantTaskStatus)
	}

	var runStatus, stopReason string
	var completedAt sql.NullTime
	if err := database.QueryRow(
		`SELECT status, COALESCE(stop_reason, ''), completed_at FROM runs WHERE id = ?`, runID,
	).Scan(&runStatus, &stopReason, &completedAt); err != nil {
		t.Fatalf("scan run: %v", err)
	}
	if runStatus != "cancelled" {
		t.Errorf("run.status = %q, want %q", runStatus, "cancelled")
	}
	if stopReason != "review_discarded_by_user" {
		t.Errorf("run.stop_reason = %q, want %q", stopReason, "review_discarded_by_user")
	}
	if !completedAt.Valid {
		t.Errorf("run.completed_at not populated")
	}

	var revCount, commentCount int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pending_reviews WHERE id = ?`, reviewID,
	).Scan(&revCount); err != nil {
		t.Fatalf("scan pending_reviews count: %v", err)
	}
	if revCount != 0 {
		t.Errorf("pending_reviews count = %d, want 0", revCount)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pending_review_comments WHERE review_id = ?`, reviewID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("scan pending_review_comments count: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("pending_review_comments count = %d, want 0", commentCount)
	}

	var agentContent, humanContent sql.NullString
	if err := database.QueryRow(
		`SELECT agent_content, human_content FROM run_memory WHERE run_id = ?`, runID,
	).Scan(&agentContent, &humanContent); err != nil {
		t.Fatalf("scan run_memory: %v", err)
	}
	if !agentContent.Valid || agentContent.String != "agent self-report" {
		t.Errorf("agent_content = %v, want preserved %q", agentContent, "agent self-report")
	}
	if !humanContent.Valid || !strings.Contains(humanContent.String, wantHumanContentMarker) {
		t.Errorf("human_content missing %q marker; got %q", wantHumanContentMarker, humanContent.String)
	}
	// Stored value MUST NOT carry the "## Human feedback (post-run)"
	// heading — materialization owns that, and a writer that bakes it
	// into the stored body double-heads the agent-readable file. The
	// canonical heading should appear only after materializeMemory
	// joins agent_content + human_content via humanFeedbackSeparator.
	if humanContent.Valid && strings.Contains(humanContent.String, "## Human feedback (post-run)") {
		t.Errorf("stored human_content includes the canonical heading; materialization layer should own it: %q", humanContent.String)
	}

	// Read-side check: GetRunMemory's materialization must produce
	// the heading exactly once, anchoring the boundary the next
	// agent's prompt parser scans for.
	mem, err := sqlitestore.New(database).TaskMemory.GetRunMemory(context.Background(), runmode.LocalDefaultOrg, runID)
	if err != nil {
		t.Fatalf("GetRunMemory: %v", err)
	}
	if mem == nil {
		t.Fatalf("GetRunMemory returned nil after cleanup")
	}
	headingCount := strings.Count(mem.Content, "## Human feedback (post-run)")
	if headingCount != 1 {
		t.Errorf("materialized content has %d occurrences of canonical heading; want exactly 1\n--- materialized ---\n%s",
			headingCount, mem.Content)
	}
}

// TestHandleUndo_CleansUpPendingApprovalRun is the SKY-206 regression
// for the swipe-toast UX path: Cards user dismissed/claimed the
// task, agent ran and reached pending_approval, user hits Cmd-Z (or
// the toast's Undo button). The full cleanup must run AND a swipe
// audit row should be recorded since this is a swipe undo.
func TestHandleUndo_CleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"queued", "returned to the triage queue")

	// /undo must record an 'undo' swipe_events row — that's the
	// audit signal for swipe-card analytics that distinguishes it
	// from /requeue.
	var undoCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = ? AND action = 'undo'`, taskID,
	).Scan(&undoCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if undoCount != 1 {
		t.Errorf("undo swipe_events count = %d, want 1", undoCount)
	}
}

// TestHandleRequeue_CleansUpPendingApprovalRun is the parallel for
// the state-driven path: Board's drag-to-Queue, SKY-207's "Return
// to queue" button. Same cleanup, but NO swipe row — drag/click
// gestures aren't swipes and shouldn't muddy the swipe analytics.
func TestHandleRequeue_CleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"queued", "returned to the triage queue")

	// /requeue must NOT record a swipe_events row — this is a
	// deliberate state change, not a swipe undo. Recording it
	// would inflate the swipe-undo rate analytics every time the
	// user drags a card to the Queue column.
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = ?`, taskID,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("/requeue should not record swipe_events; got %d rows", swipeCount)
	}
}

// TestHandleSwipe_DismissCleansUpPendingApprovalRun is the third
// entry point: user swipes left to dismiss a delegated card whose
// agent already produced a pending_approval review. Today this
// orphans the review and leaves the run as a phantom
// pending_approval against a dismissed task — SKY-206's other half.
//
// The dismiss-flavored human_content note carries a different
// implication marker ("dismissed the task entirely") than the
// requeue paths so a future agent reading prior memory can
// distinguish "the human shelved this verdict but kept the entity
// on the docket" from "the human walked away from this entity".
func TestHandleSwipe_DismissCleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe",
		map[string]any{"action": "dismiss", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"dismissed", "dismissed the task entirely")
}

// TestHandleSwipe_CompleteCleansUpPendingApprovalRun is the fourth
// entry point: the Board's drag-AgentCard-to-Done gesture for a
// pending_approval run. The complete swipe action flips the task to
// 'done' (so the card lands in the Done column rather than
// disappearing from the board, the way dismiss makes it) but reuses
// the same SKY-206 cleanup — pending_reviews row gone, run flipped
// to cancelled, agent_content preserved, human_content recording
// the user's verdict with a complete-flavored marker that's distinct
// from both the requeue and dismiss shapes. Future agents reading
// memory should be able to tell "the human resolved this themselves
// without applying my prepared review" from "the human walked away
// from the entity entirely."
func TestHandleSwipe_CompleteCleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe",
		map[string]any{"action": "complete", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"done", "marked the task complete without submitting")
}

// TestHandleSwipe_ClaimCleansUpPendingApprovalRun guards the SKY-206
// race the PR #77 review flagged: Board's drag-Agent-to-You issues
// /swipe claim, but the frontend's agentRuns map can be transiently
// empty during a fetchTasks refresh — so any frontend gating on
// agentRuns[taskId]?.Status === 'pending_approval' would silently
// skip the cleanup, stranding the prepared review row and leaving a
// phantom pending_approval run.
//
// Backend-authoritative cleanup closes that hole: the swipe handler
// runs cleanupPendingApprovalRun for every claim, and the
// pending_approval-row lookup makes it a no-op for tasks without a
// review. The claim-flavored marker carries its own
// recalibration signal — "human took over manually" — distinct from
// requeue/dismiss/complete.
func TestHandleSwipe_ClaimCleansUpPendingApprovalRun(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// SKY-261 B+: claim no longer transitions status; the task stays
	// 'queued' and claimed_by_user_id is set instead. The
	// pending-approval cleanup invariants (run cancelled, review row
	// removed, human_content marker) are unchanged.
	assertPendingApprovalCleanedUp(t, s.db, taskID, runID, reviewID,
		"queued", "claimed the task to handle it themselves")
	// Pin the claim col too — it's the actual responsibility signal
	// post-B+.
	var claimedByUserID sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id FROM tasks WHERE id = ?`, taskID,
	).Scan(&claimedByUserID); err != nil {
		t.Fatalf("scan claim: %v", err)
	}
	if !claimedByUserID.Valid || claimedByUserID.String == "" {
		t.Errorf("task.claimed_by_user_id empty after claim swipe; want stamped")
	}
}

// TestHandleSwipe_ClaimWithoutPendingApprovalIsNoOp pins the
// idempotency contract: cleanupPendingApprovalRun must be a no-op
// when the task has no pending_approval run, so adding the cleanup
// to the claim path doesn't disturb the queue → claim flow used by
// Cards.tsx and the existing Board queue → you drag.
func TestHandleSwipe_ClaimWithoutPendingApprovalIsNoOp(t *testing.T) {
	s := newTestServer(t)

	// Plain queued task with no agent run. Mirrors what claim from
	// the queue looks like — the event/task FK chain mirrors
	// pendingApprovalFixture but stops short of any runs or reviews.
	const eventType = "github:pr:opened"
	if _, err := s.db.Exec(`
		INSERT INTO entities (id, source, source_id, kind, state)
		VALUES ('e1', 'github', 'sky/repo#1', 'pr', 'active');
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES ('ev1', 'e1', ?, '');
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES ('task-noruns', 'e1', ?, 'ev1', 'queued');
	`, eventType, eventType); err != nil {
		t.Fatalf("seed FK chain: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/task-noruns/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// SKY-261 B+: claim is a responsibility-axis action; status stays
	// 'queued', claim col gets stamped.
	var status string
	var claimedByUserID sql.NullString
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_user_id FROM tasks WHERE id = 'task-noruns'`,
	).Scan(&status, &claimedByUserID); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if status != "queued" {
		t.Errorf("task.status = %q, want %q (claim no longer changes status)", status, "queued")
	}
	if !claimedByUserID.Valid || claimedByUserID.String == "" {
		t.Errorf("task.claimed_by_user_id empty after claim swipe; want stamped")
	}
}

// TestHandleSwipe_ClaimRejectsStealingFromBot pins the swipe-claim
// race-safe handler: when the task is bot-claimed, the handler must
// route through TakeoverClaimFromAgent's optimistic guard and
// produce a clean takeover (bot claim → user claim, atomic). This
// pins the legitimate takeover branch — the steal-from-bot is
// allowed; what's not allowed is stealing from another user.
func TestHandleSwipe_ClaimAgainstBotClaimedIsTakeover(t *testing.T) {
	s := newTestServer(t)
	const eventType = "github:pr:opened"
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_bot', 'github', 'sky/repo#bot', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_bot', 'e_bot', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_agent_id)
		 VALUES ('task-bot', 'e_bot', ?, 'ev_bot', 'queued', ?)`,
		eventType, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed task with bot claim: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/task-bot/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var claimedAgent, claimedUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = 'task-bot'`,
	).Scan(&claimedAgent, &claimedUser); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if claimedAgent.Valid {
		t.Errorf("claimed_by_agent_id = %q after takeover; want NULL", claimedAgent.String)
	}
	if !claimedUser.Valid || claimedUser.String != runmode.LocalDefaultUserID {
		t.Errorf("claimed_by_user_id = %v; want sentinel user", claimedUser)
	}
}

// TestHandleSwipe_ClaimRefusedLeavesNoAuditRow pins the SKY-261 v0.7
// audit contract: swipe_events records state CHANGES, not gesture
// ATTEMPTS. A claim swipe that's refused (different user owns the
// task) returns 409 with no audit row, no status flip, no snooze
// clear. The reviewer flagged this earlier as "RecordSwipe at the
// top mutates state for refused gestures" — the post-restructure
// handler runs claim mutation first and only records the audit
// after accept.
func TestHandleSwipe_ClaimRefusedLeavesNoAuditRow(t *testing.T) {
	s := newTestServer(t)
	const eventType = "github:pr:opened"
	const otherUserID = "00000000-0000-0000-0000-0000000004cc"

	if _, err := s.db.Exec(
		`INSERT INTO users (id, display_name) VALUES (?, 'Other User')`,
		otherUserID,
	); err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_refuse', 'github', 'sky/repo#refuse', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_refuse', 'e_refuse', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('t_refuse', 'e_refuse', ?, 'ev_refuse', 'queued', ?)`,
		eventType, otherUserID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_refuse/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// No swipe_events row written — refused gesture leaves no trace.
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = 't_refuse'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0 (refused claim must not audit)", swipeCount)
	}

	// State unchanged — other user still owns it, status unchanged.
	var status string
	var claim sql.NullString
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_user_id FROM tasks WHERE id = 't_refuse'`,
	).Scan(&status, &claim); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q; refused gesture changed lifecycle", status)
	}
	if !claim.Valid || claim.String != otherUserID {
		t.Errorf("claim = %v; refused gesture disturbed claim", claim)
	}
}

// TestHandleSwipe_DelegateRefusedLeavesNoAuditRow is the delegate
// half of the audit-contract guarantee. Pre-condition: task is
// user-claimed by ANOTHER user (the only delegate refuse path).
// Post: 409, no swipe_events, no state change.
func TestHandleSwipe_DelegateRefusedLeavesNoAuditRow(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku", ""))
	const eventType = "github:pr:opened"
	const otherUserID = "00000000-0000-0000-0000-0000000004dd"

	if _, err := s.db.Exec(
		`INSERT INTO users (id, display_name) VALUES (?, 'Other User')`,
		otherUserID,
	); err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_drefuse', 'github', 'sky/repo#drefuse', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_drefuse', 'e_drefuse', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('t_drefuse', 'e_drefuse', ?, 'ev_drefuse', 'queued', ?)`,
		eventType, otherUserID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_drefuse/swipe",
		map[string]any{"action": "delegate", "hesitation_ms": 0, "blueprint_id": "any"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = 't_drefuse'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0 (refused delegate must not audit)", swipeCount)
	}
	var claimUser sql.NullString
	var claimAgent sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id, claimed_by_agent_id FROM tasks WHERE id = 't_drefuse'`,
	).Scan(&claimUser, &claimAgent); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if !claimUser.Valid || claimUser.String != otherUserID {
		t.Errorf("claim_by_user = %v; refused delegate disturbed claim", claimUser)
	}
	if claimAgent.Valid {
		t.Errorf("claim_by_agent = %v; refused delegate stamped bot anyway", claimAgent)
	}
}

// TestHandleSwipe_ClaimRefusedOnTerminalTask pins the handler-level
// guard for the same-user-idempotent fall-through path. The data-
// layer helpers refuse claim transitions on done/dismissed rows,
// but the handler's same-user check is a no-op early-return that
// doesn't call any helper — so without an explicit status check
// in the handler, RecordSwipe's vestigial status='queued' write
// would reopen a closed task as a side effect of recording the
// audit row.
//
// Seeds a done task already claimed by the local user (the sticky-
// past-close audit state), fires /swipe claim, asserts 409 +
// status preserved + no swipe_events row written.
func TestHandleSwipe_ClaimRefusedOnTerminalTask(t *testing.T) {
	for _, terminalStatus := range []string{"done", "dismissed"} {
		t.Run(terminalStatus, func(t *testing.T) {
			s := newTestServer(t)
			const eventType = "github:pr:opened"
			if _, err := s.db.Exec(
				`INSERT INTO entities (id, source, source_id, kind, state)
				 VALUES ('e_term', 'github', 'sky/repo#term', 'pr', 'active')`,
			); err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			if _, err := s.db.Exec(
				`INSERT INTO events (id, entity_id, event_type, dedup_key)
				 VALUES ('ev_term', 'e_term', ?, '')`,
				eventType,
			); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			// Sticky past close: terminal status with the user's
			// claim retained as audit. This is the exact shape that
			// would have triggered the reopen bug pre-guards.
			if _, err := s.db.Exec(
				`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
				 VALUES ('t_term', 'e_term', ?, 'ev_term', ?, ?)`,
				eventType, terminalStatus, runmode.LocalDefaultUserID,
			); err != nil {
				t.Fatalf("seed terminal task: %v", err)
			}

			rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_term/swipe",
				map[string]any{"action": "claim", "hesitation_ms": 0})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}

			// Status must be preserved — the reopen bug would have
			// flipped it to 'queued' via RecordSwipe's lifecycle write.
			var status string
			if err := s.db.QueryRow(
				`SELECT status FROM tasks WHERE id = 't_term'`,
			).Scan(&status); err != nil {
				t.Fatalf("scan task: %v", err)
			}
			if status != terminalStatus {
				t.Errorf("status = %q, want %q (refused claim reopened terminal task)", status, terminalStatus)
			}

			// Audit must be silent on the refusal.
			var swipeCount int
			if err := s.db.QueryRow(
				`SELECT COUNT(*) FROM swipe_events WHERE task_id = 't_term'`,
			).Scan(&swipeCount); err != nil {
				t.Fatalf("scan swipe_events: %v", err)
			}
			if swipeCount != 0 {
				t.Errorf("swipe_events count = %d, want 0 (refused gesture must leave no trace)", swipeCount)
			}
		})
	}
}

// TestHandleSwipe_DelegateDifferentiatesRefusalReasons pins the
// post-fix error-mapping on the swipe-delegate path. HandoffRefused
// collapses three reasons (missing task / terminal task / different-
// user claim); the handler pre-loads to disambiguate so the
// response carries the right status code and message for each.
//
//   - missing task → 404 "task not found"
//   - terminal task → 409 "task is closed; delegate transitions
//     aren't allowed past close"
//   - different-user claim → 409 "task is claimed by another user"
func TestHandleSwipe_DelegateDifferentiatesRefusalReasons(t *testing.T) {
	t.Run("missing_task_404", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/swipe",
			map[string]any{"action": "delegate", "hesitation_ms": 0, "blueprint_id": "any"})
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("terminal_task_409_with_closed_message", func(t *testing.T) {
		s := newTestServer(t)
		const eventType = "github:pr:opened"
		if _, err := s.db.Exec(
			`INSERT INTO entities (id, source, source_id, kind, state)
			 VALUES ('e_term_del', 'github', 'sky/repo#td', 'pr', 'active')`,
		); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO events (id, entity_id, event_type, dedup_key)
			 VALUES ('ev_term_del', 'e_term_del', ?, '')`,
			eventType,
		); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
			 VALUES ('t_term_del', 'e_term_del', ?, 'ev_term_del', 'done')`,
			eventType,
		); err != nil {
			t.Fatalf("seed terminal task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_term_del/swipe",
			map[string]any{"action": "delegate", "hesitation_ms": 0, "blueprint_id": "any"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "closed") {
			t.Errorf("body=%s; want closed-task message (not theft message)", rec.Body.String())
		}
	})

	t.Run("different_user_409_with_theft_message", func(t *testing.T) {
		s := newTestServer(t)
		const eventType = "github:pr:opened"
		const otherUserID = "00000000-0000-0000-0000-0000000003ee"
		if _, err := s.db.Exec(
			`INSERT INTO users (id, display_name) VALUES (?, 'Other User')`,
			otherUserID,
		); err != nil {
			t.Fatalf("seed other user: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO entities (id, source, source_id, kind, state)
			 VALUES ('e_diff_del', 'github', 'sky/repo#dd', 'pr', 'active')`,
		); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO events (id, entity_id, event_type, dedup_key)
			 VALUES ('ev_diff_del', 'e_diff_del', ?, '')`,
			eventType,
		); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
			 VALUES ('t_diff_del', 'e_diff_del', ?, 'ev_diff_del', 'queued', ?)`,
			eventType, otherUserID,
		); err != nil {
			t.Fatalf("seed other-user-claimed task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_diff_del/swipe",
			map[string]any{"action": "delegate", "hesitation_ms": 0, "blueprint_id": "any"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "another user") {
			t.Errorf("body=%s; want theft message (not closed-task message)", rec.Body.String())
		}
	})
}

// TestHandleSwipe_DelegateRefusedWhenBotDisabled pins the SKY-261
// acceptance criterion "swipe-to-delegate re-checks team_agents.enabled
// at swipe time." A team admin can toggle the bot off via SetEnabled
// — subsequent /swipe delegate gestures must 409, with no claim
// stamp, no spawn, no audit row. Local-mode N=1 doesn't normally
// flip this off but the data-layer enforcement is what multi-tenant
// will need.
func TestHandleSwipe_DelegateRefusedWhenBotDisabled(t *testing.T) {
	s := newTestServer(t)
	// Flip the bot OFF on the local team.
	if _, err := s.db.Exec(
		`UPDATE team_agents SET enabled = 0 WHERE team_id = ? AND agent_id = ?`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("disable bot: %v", err)
	}
	const eventType = "github:pr:opened"
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_bot_off', 'github', 'sky/repo#off', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_bot_off', 'e_bot_off', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		 VALUES ('t_bot_off', 'e_bot_off', ?, 'ev_bot_off', 'queued')`,
		eventType,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_bot_off/swipe",
		map[string]any{"action": "delegate", "hesitation_ms": 0, "blueprint_id": "any"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (bot disabled); body=%s", rec.Code, rec.Body.String())
	}

	// No state changes — claim cols untouched, no swipe_events row.
	var claimAgent, claimUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = 't_bot_off'`,
	).Scan(&claimAgent, &claimUser); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if claimAgent.Valid {
		t.Errorf("bot claim landed despite disabled flag: %q", claimAgent.String)
	}
	if claimUser.Valid {
		t.Errorf("user claim disturbed: %q", claimUser.String)
	}
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = 't_bot_off'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0", swipeCount)
	}
}

// TestHandleSnooze_404OnMissingTask pins missing-task parity with
// /undo and /requeue. Pre-fix, hitting /snooze on a bogus id would
// trip the swipe_events→tasks FK constraint inside SnoozeTask and
// surface the SQLite error string as 500. The GetTask pre-check
// catches the common case so legitimate 404 callers don't have to
// parse FK error strings to tell "doesn't exist" from "real server
// error."
func TestHandleSnooze_404OnMissingTask(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/snooze",
		map[string]any{"until": "1h", "hesitation_ms": 0})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleSnooze_RefusesOnClaimedTask pins the SKY-261 B+
// "snoozed ↔ unclaimed" invariant from the snooze side: the
// SnoozeTask store-level atomic UPDATE refuses on a claimed task,
// the handler maps the refusal to 409, and no state mutates (status
// stays the same, snooze_until stays NULL, audit row was rolled
// back as part of the atomic tx).
//
// This is the deliberate trade we made to avoid the snoozed+claimed
// incoherent state: users wanting to defer work on a claimed task
// must explicitly requeue first.
func TestHandleSnooze_RefusesOnClaimedTask(t *testing.T) {
	s := newTestServer(t)
	const eventType = "github:pr:opened"
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_snz_claim', 'github', 'sky/repo#sz', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_snz_claim', 'e_snz_claim', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('t_snz_claim', 'e_snz_claim', ?, 'ev_snz_claim', 'queued', ?)`,
		eventType, runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed claimed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_snz_claim/snooze",
		map[string]any{"until": "1h", "hesitation_ms": 0})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (snooze refused on claimed task); body=%s", rec.Code, rec.Body.String())
	}

	// State must be unchanged: status='queued', snooze_until NULL,
	// claim still on the user. The atomic tx rollback means no
	// swipe_events row either.
	var status string
	var snoozeUntil sql.NullTime
	var claim sql.NullString
	if err := s.db.QueryRow(
		`SELECT status, snooze_until, claimed_by_user_id FROM tasks WHERE id = 't_snz_claim'`,
	).Scan(&status, &snoozeUntil, &claim); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want 'queued' (refusal must not transition lifecycle)", status)
	}
	if snoozeUntil.Valid {
		t.Errorf("snooze_until = %v, want NULL (refusal must not set deferral)", snoozeUntil.Time)
	}
	if !claim.Valid || claim.String != runmode.LocalDefaultUserID {
		t.Errorf("claim was disturbed by refused snooze: got %v", claim)
	}
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = 't_snz_claim'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0 (refused gesture should leave no audit)", swipeCount)
	}
}

// TestHandleSwipe_DelegateTransfersOwnUserClaim pins the SKY-133
// flow: when the user drags their own claimed task from the You
// lane to the Agent lane, the FE fires a delegate swipe. The
// handler must accept the gesture as a legitimate user → bot
// transfer, not refuse it as a stolen-claim race.
//
// This was broken in an earlier iteration where the delegate path
// used a stamp helper that refused ANY non-NULL claimed_by_user_id
// — HandoffAgentClaim is the post-fix helper that allows same-user
// transfer while still refusing different-user theft.
func TestHandleSwipe_DelegateTransfersOwnUserClaim(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku", ""))

	// Seed a queued task already claimed by the local user — the
	// pre-condition right before a You → Agent drag fires the
	// delegate swipe.
	const eventType = "github:pr:opened"
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_y2a', 'github', 'sky/repo#y2a', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_y2a', 'e_y2a', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('task-y2a', 'e_y2a', ?, 'ev_y2a', 'queued', ?)`,
		eventType, runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed user-claimed task: %v", err)
	}

	// Use a prompt id that won't resolve — the spawner will fail
	// before producing a run, but the claim stamping is the part
	// under test and that runs before the spawn. delegate_error in
	// the response is expected; what we care about is that the
	// transfer landed (claim flipped to bot).
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/task-y2a/swipe", map[string]any{
		"action":        "delegate",
		"hesitation_ms": 0,
		"blueprint_id":  "no-such-prompt",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (delegate accepted as user→bot transfer); body=%s", rec.Code, rec.Body.String())
	}

	var claimedAgent, claimedUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = 'task-y2a'`,
	).Scan(&claimedAgent, &claimedUser); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if !claimedAgent.Valid || claimedAgent.String != runmode.LocalDefaultAgentID {
		t.Errorf("claimed_by_agent_id = %v; want bot to have taken the claim", claimedAgent)
	}
	if claimedUser.Valid {
		t.Errorf("claimed_by_user_id = %q; want NULL after transfer", claimedUser.String)
	}
}

// TestHandleSwipe_ClaimAgainstOtherUserClaimReturns409 pins the
// anti-steal guarantee: if a different user already owns the task,
// the swipe-claim handler must refuse with 409 rather than
// overwriting the other user's claim. The previous unconditional
// SetTaskClaimedByUser would have silently stolen the row.
//
// At N=1 local mode this can't happen via real user gestures, but
// the helper-level race-safety is load-bearing for multi-mode and
// the test pins the contract.
func TestHandleSwipe_ClaimAgainstOtherUserClaimReturns409(t *testing.T) {
	s := newTestServer(t)
	const eventType = "github:pr:opened"
	// Synthetic "other user" — distinct from LocalDefaultUserID so
	// the swipe-claim's "is it me?" branch routes to the refuse-
	// to-steal path. In local-mode SQLite the users table has no
	// FK to auth.users (that's the Postgres path); we can seed any
	// UUID with the local-shape columns. Statements split so a
	// stray FK violation points at the offending row rather than
	// the whole multi-statement Exec.
	const otherUserID = "00000000-0000-0000-0000-000000000999"
	if _, err := s.db.Exec(
		`INSERT INTO users (id, display_name) VALUES (?, 'Other User')`,
		otherUserID,
	); err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_oth', 'github', 'sky/repo#oth', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_oth', 'e_oth', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('task-oth', 'e_oth', ?, 'ev_oth', 'queued', ?)`,
		eventType, otherUserID,
	); err != nil {
		t.Fatalf("seed task with other-user claim: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/task-oth/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Claim must be unchanged — the swipe refused to overwrite.
	var claimedUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id FROM tasks WHERE id = 'task-oth'`,
	).Scan(&claimedUser); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if !claimedUser.Valid || claimedUser.String != otherUserID {
		t.Errorf("claimed_by_user_id = %v; want preserved as %q (handler must not steal)",
			claimedUser, otherUserID)
	}
}

// TestHandleUndo_404OnMissingTask pins the missing-id behavior:
// /undo against a bogus task ID must return 404 with a clean error
// body, not the SQLite FK violation surfaced as a 500. The
// GetTask-first check in the handler fails fast before
// UndoLastSwipe's INSERT into swipe_events trips the FK constraint
// — so legitimate 404 callers don't have to parse SQLite error
// strings to tell "doesn't exist" from "real server error."
func TestHandleUndo_404OnMissingTask(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/undo", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleRequeue_404OnMissingTask is the regression for the
// silent-success bug: /requeue used to return 200 against a bogus
// id because the underlying UPDATE just affected 0 rows. Both the
// handler-level GetTask check and RequeueTask's ok-bool now catch
// it — the test goes through the handler so both layers are
// exercised together.
func TestHandleRequeue_404OnMissingTask(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/requeue", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestRequeueTask_OkFalseOnMissingID directly exercises the DB
// helper's ok-bool. The handler check above catches the common
// case, but the bool is the defense against a race between
// GetTask and the UPDATE (task deleted in the gap). Without this
// signal, that race would silently 200 even with the handler
// check in place.
func TestRequeueTask_OkFalseOnMissingID(t *testing.T) {
	s := newTestServer(t)

	ok, err := s.swipes.RequeueTask(t.Context(), runmode.LocalDefaultOrg, "no-such-task")
	if err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}
	if ok {
		t.Errorf("RequeueTask returned ok=true for missing id; want false")
	}
}

// TestHandleUndo_NoPendingApprovalIsNoOp guards the common case:
// the task has no delegated run (or its delegated run is still
// active, not pending_approval). The cleanup should silently
// no-op rather than touching unrelated runs/reviews.
func TestHandleUndo_NoPendingApprovalIsNoOp(t *testing.T) {
	s := newTestServer(t)

	// Seed a plain user-claimed task with no run at all — the simplest
	// shape that exercises handleUndo's other half (claim clear +
	// Jira reversal skipped because EntitySource isn't 'jira'). Post-
	// SKY-261 B+ this is status='queued' + claimed_by_user_id; pre-B+
	// it was status='claimed'.
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_plain', 'github', 'owner/repo#plain', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_plain', 'e_plain', 'github:pr:opened', '')`,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('t_plain', 'e_plain', 'github:pr:opened', 'ev_plain', 'queued', ?)`,
		runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_plain/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var taskStatus string
	if err := s.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, "t_plain").Scan(&taskStatus); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if taskStatus != "queued" {
		t.Errorf("task.status = %q, want %q", taskStatus, "queued")
	}
}

// TestHandleUndo_ClearsClaimColumns pins the SKY-261 B+ semantic:
// /undo returns the task to the team's unclaimed queue, which means
// both claim_by_* cols are cleared — not just status reset. Without
// this, a claim/delegate swipe followed by Undo would leave the task
// status='queued' but still in the owner's lane (queue-view filter
// requires both claim cols NULL), so the user would think they
// undid the action while the Board kept rendering the task as
// claimed.
func TestHandleUndo_ClearsClaimColumns(t *testing.T) {
	s := newTestServer(t)

	// Seed a user-claimed queued task — the post-swipe state for
	// action='claim'. (Pre-invariant this test also seeded
	// snooze_until on the same row to cover "claim during a snoozed
	// window"; that combo is now forbidden by the "snoozed ↔
	// unclaimed" invariant, so the snooze_until pre-stage is dropped.
	// Snooze-clearing on undo is covered by /requeue's existing test
	// against unclaimed-snoozed rows.)
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_undo_claim', 'github', 'owner/repo#u1', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_undo_claim', 'e_undo_claim', 'github:pr:opened', '')`,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('t_undo_claim', 'e_undo_claim', 'github:pr:opened', 'ev_undo_claim', 'queued', ?)`,
		runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_undo_claim/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status, claimedAgent, claimedUser sql.NullString
	var snoozeUntil sql.NullTime
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_agent_id, claimed_by_user_id, snooze_until
		 FROM tasks WHERE id = ?`, "t_undo_claim",
	).Scan(&status, &claimedAgent, &claimedUser, &snoozeUntil); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if status.String != "queued" {
		t.Errorf("status = %q, want %q", status.String, "queued")
	}
	if claimedAgent.Valid {
		t.Errorf("claimed_by_agent_id = %q; want NULL (undo must clear claim)", claimedAgent.String)
	}
	if claimedUser.Valid {
		t.Errorf("claimed_by_user_id = %q; want NULL (undo must clear claim)", claimedUser.String)
	}
	if snoozeUntil.Valid {
		t.Errorf("snooze_until = %v; want NULL", snoozeUntil.Time)
	}
}

// TestCleanupPendingApprovalRun_Idempotent calls the cleanup twice
// against the same task with a different outcome the second time.
// The second call must find the run already cancelled (the
// PendingApprovalRunIDForTask filter returns "" once status flips
// off pending_approval) and exit silently — otherwise:
//
//   - human_content would be overwritten with the second outcome's
//     text, erasing the first verdict from memory
//   - the websocket would double-fire agent_run_update
//   - the audit row's stop_reason / completed_at would shift
//
// We pick discardOutcomeDismissed for the second call so that if
// the early-out broke, the human_content marker would visibly
// flip from "returned to the triage queue" to "dismissed the task
// entirely" — making the test failure mode loud rather than silent.
func TestCleanupPendingApprovalRun_Idempotent(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, _ := pendingApprovalFixture(t, s.db)

	s.cleanupPendingApprovalRun(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, taskID, discardOutcomeRequeued)

	var humanContentBefore sql.NullString
	var completedAtBefore sql.NullTime
	if err := s.db.QueryRow(
		`SELECT rm.human_content, r.completed_at
		 FROM run_memory rm JOIN runs r ON r.id = rm.run_id
		 WHERE rm.run_id = ?`, runID,
	).Scan(&humanContentBefore, &completedAtBefore); err != nil {
		t.Fatalf("scan after first call: %v", err)
	}
	if !strings.Contains(humanContentBefore.String, "returned to the triage queue") {
		t.Fatalf("first call didn't write requeue marker; got %q", humanContentBefore.String)
	}

	// Second call: different outcome, must not take effect.
	s.cleanupPendingApprovalRun(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, taskID, discardOutcomeDismissed)

	var humanContentAfter sql.NullString
	var completedAtAfter sql.NullTime
	var runStatusAfter string
	if err := s.db.QueryRow(
		`SELECT rm.human_content, r.completed_at, r.status
		 FROM run_memory rm JOIN runs r ON r.id = rm.run_id
		 WHERE rm.run_id = ?`, runID,
	).Scan(&humanContentAfter, &completedAtAfter, &runStatusAfter); err != nil {
		t.Fatalf("scan after second call: %v", err)
	}
	if runStatusAfter != "cancelled" {
		t.Errorf("run.status drifted after second call: %q", runStatusAfter)
	}
	if humanContentAfter.String != humanContentBefore.String {
		t.Errorf("human_content overwritten by second call:\n  before: %q\n  after:  %q",
			humanContentBefore.String, humanContentAfter.String)
	}
	if !completedAtBefore.Valid || !completedAtAfter.Valid ||
		!completedAtAfter.Time.Equal(completedAtBefore.Time) {
		t.Errorf("completed_at shifted on idempotent re-call: before=%v after=%v",
			completedAtBefore, completedAtAfter)
	}
}

// TestCleanupPendingApprovalRun_DeleteFailureHoldsRunForRetry is
// the regression for the ordering bug: if DeletePendingReviewByRunID
// fails transiently, the cleanup must NOT flip the run off
// status='pending_approval'. PendingApprovalRunIDForTask filters on
// that status, so cancelling the run too eagerly would strand the
// review with no path back to retry.
//
// We force a delete failure by temporarily renaming the
// pending_review_comments table — the DELETE inside the
// transactional helper sees a missing dependency and rolls back. After
// restoring the table, a second cleanup call must find the run
// still in pending_approval, succeed, and leave the expected
// terminal state.
func TestCleanupPendingApprovalRun_DeleteFailureHoldsRunForRetry(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, _ := pendingApprovalFixture(t, s.db)

	// Sabotage: rename the comments table so the DELETE inside
	// DeletePendingReviewByRunID fails. SQLite's foreign-key
	// validation isn't what trips here — the transactional helper's
	// first DELETE references pending_review_comments by name and
	// fails with "no such table".
	if _, err := s.db.Exec(`ALTER TABLE pending_review_comments RENAME TO pending_review_comments_temp`); err != nil {
		t.Fatalf("rename comments table: %v", err)
	}

	s.cleanupPendingApprovalRun(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, taskID, discardOutcomeRequeued)

	// Run must still be pending_approval — the delete failed and
	// MarkAgentRunDiscarded must have been skipped.
	var runStatus string
	if err := s.db.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("scan run after sabotaged cleanup: %v", err)
	}
	if runStatus != "pending_approval" {
		t.Fatalf("run.status = %q after delete failure; want %q (cleanup must hold for retry)",
			runStatus, "pending_approval")
	}

	// Heal the table; the next cleanup call must rediscover the
	// run via PendingApprovalRunIDForTask and complete the work.
	if _, err := s.db.Exec(`ALTER TABLE pending_review_comments_temp RENAME TO pending_review_comments`); err != nil {
		t.Fatalf("restore comments table: %v", err)
	}

	s.cleanupPendingApprovalRun(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, taskID, discardOutcomeRequeued)

	if err := s.db.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("scan run after retry: %v", err)
	}
	if runStatus != "cancelled" {
		t.Errorf("run.status = %q after successful retry; want %q", runStatus, "cancelled")
	}
	var revCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pending_reviews WHERE run_id = ?`, runID).Scan(&revCount); err != nil {
		t.Fatalf("scan pending_reviews after retry: %v", err)
	}
	if revCount != 0 {
		t.Errorf("pending_reviews count = %d after retry; want 0 (review should be torn down)", revCount)
	}
}

// TestCleanupPendingApprovalRun_AgentContentNullSurvives is the
// SKY-204 synthetic-row case: agent skipped the memory file, so
// run_memory exists with agent_content NULL. The discard cleanup
// still lands human_content cleanly on the existing row (the spec's
// guarantee that the unconditional termination-time upsert means
// no INSERT-or-UPDATE branching is needed downstream).
func TestCleanupPendingApprovalRun_AgentContentNullSurvives(t *testing.T) {
	s := newTestServer(t)
	taskID, runID, _ := pendingApprovalFixture(t, s.db)

	// Force agent_content NULL to simulate a noncompliant gate
	// (SKY-204's UpsertAgentMemory("") would have done this in
	// production; we set it directly to skip the dependency).
	if _, err := s.db.Exec(
		`UPDATE run_memory SET agent_content = NULL WHERE run_id = ?`, runID,
	); err != nil {
		t.Fatalf("force null agent_content: %v", err)
	}

	s.cleanupPendingApprovalRun(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultUserID, taskID, discardOutcomeRequeued)

	var agentContent, humanContent sql.NullString
	if err := s.db.QueryRow(
		`SELECT agent_content, human_content FROM run_memory WHERE run_id = ?`, runID,
	).Scan(&agentContent, &humanContent); err != nil {
		t.Fatalf("scan run_memory: %v", err)
	}
	if agentContent.Valid {
		t.Errorf("agent_content was NULL pre-cleanup; should still be NULL post-cleanup, got %q", agentContent.String)
	}
	if !humanContent.Valid || !strings.Contains(humanContent.String, "Human discarded") {
		t.Errorf("human_content not landed against NULL agent_content row: %v", humanContent)
	}
}
