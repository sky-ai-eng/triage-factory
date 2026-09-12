package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
	"github.com/zalando/go-keyring"
)

// pendingApprovalFixture installs the full FK chain for a task whose delegated
// conversation has COMPLETED (terminal) while leaving an unresolved review
// artifact behind — a finalized pending review plus the agent-side memory row. Returns (taskID, conversationID, reviewID). Centralized here
// so each teardown test exercises the shape the task-level resolve-all gesture
// is meant to clean up: agent finished, wrote memory, prepared a review, the
// human then dragged the card to Done / Queue / dismissed it instead of
// approving.
func pendingApprovalFixture(t *testing.T, database *sql.DB) (taskID, conversationID, reviewID string) {
	t.Helper()
	// The review-abandon path resolves a GitHub client (to delete the pending
	// review), which is the first secret-backend resolution in some test orderings.
	// Install the mock keychain so that resolution probes a mock (keychain backend)
	// rather than memoizing keychainOK=false (file backend) — which would otherwise
	// poison later seedApp-based tests that need TF_SECRET_ENCRYPTION_KEY.
	keyring.MockInit()

	const eventType = "github:pr:ci_check_passed"
	// Pre-B+ this fixture used status='delegated' to mean
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
		 VALUES ('00000000-0000-4000-8000-000000000024', 'e_pa', ?, 'ev_pa', 'queued', ?)`,
		eventType, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	blueprintRunID := seedBlueprintRunSQLite(t, database, "00000000-0000-4000-8000-000000000024")
	if _, err := database.Exec(
		`INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index)
		 VALUES ('r_pa', '00000000-0000-4000-8000-000000000024', 'p_pa', 'completed', 'manual', ?, 0)`,
		blueprintRunID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	// conversation_memory: agent finished and wrote its self-report (the
	// termination upsert). The teardown below must leave it alone.
	if _, err := sqlitestore.New(database).TaskMemory.UpsertAgentMemory(context.Background(), runmode.LocalDefaultOrgID, "r_pa", "", "agent self-report", domain.MemorySourceAgent); err != nil {
		t.Fatalf("UpsertAgentMemory: %v", err)
	}
	// The primary join row a conversation's completion carries beside its
	// memory — GetMemoriesForEntity's join-based read, exercised in
	// assertPendingApprovalCleanedUp below, needs it to find anything.
	if err := sqlitestore.New(database).TaskMemory.RecordEntityTouchSystem(context.Background(), runmode.LocalDefaultOrgID, "r_pa", "e_pa", domain.MemoryRolePrimary); err != nil {
		t.Fatalf("RecordEntityTouchSystem: %v", err)
	}

	// A finalized review draft parks the conversation: a review artifact in
	// state=pending whose ready sentinel (details.review_event) is set,
	// with the agent's draft snapshotted into details.proposed. Abandon
	// flips it to dismissed (no GitHub call — the draft is local). Returns
	// the artifact id.
	line := 1
	reviewArt := domain.NewReviewArtifact("owner/repo", 7, "headsha_pa", "r_pa")
	reviewArt.ConversationID = "r_pa"
	reviewArt.OrgID = runmode.LocalDefaultOrgID
	reviewArt.TeamID = runmode.LocalDefaultTeamID
	rd, _ := domain.ParseReviewArtifactDetails(reviewArt.DetailsJSON)
	rd.ReviewBody = "agent draft body"
	rd.ReviewEvent = "APPROVE"
	rd.Proposed = domain.ReviewArtifactProposed{
		Body:     "agent draft body",
		Event:    "APPROVE",
		Comments: []domain.ReviewArtifactComment{{ID: "c_pa", Path: "x.go", Line: &line, Body: "agent comment"}},
	}
	reviewArt.DetailsJSON = domain.MarshalReviewArtifactDetails(rd)
	stored, err := sqlitestore.New(database).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, reviewArt)
	if err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}
	return "00000000-0000-4000-8000-000000000024", "r_pa", stored.ID
}

// assertPendingApprovalCleanedUp checks every post-condition the task-level
// resolve-all teardown is meant to deliver: task at the expected post-state,
// the unresolved review artifact flipped to dismissed, and the conversation's
// own memory untouched — the teardown resolves artifacts, it does not write
// memory. wantTaskStatus lets callers vary the assertion across the requeue
// (`queued`), dismiss (`dismissed`) and complete (`done`) paths.
//
// Decoupled-lifecycle invariant (TFAC-379): teardown NEVER flips
// conversations.status — the completed conversation stays completed. The
// live-conversation cancellation that the old park model folded in here is now
// the spawner's job (only a still-running conversation is cancelled, by
// teardownTaskConversations), so a terminal conversation is left untouched.
func assertPendingApprovalCleanedUp(
	t *testing.T,
	database *sql.DB,
	taskID, conversationID, reviewID string,
	wantTaskStatus string,
) {
	t.Helper()

	var taskStatus string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if taskStatus != wantTaskStatus {
		t.Errorf("task.status = %q, want %q", taskStatus, wantTaskStatus)
	}

	// The conversation is untouched by the resolve — it stays terminal
	// (completed). A resolve must never flip conversations.status.
	var convStatus string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("scan conversation: %v", err)
	}
	if convStatus != "completed" {
		t.Errorf("conversation status = %q, want %q (teardown must not flip conversation lifecycle)", convStatus, "completed")
	}

	// The review artifact must be flipped to dismissed (its proposed snapshot is
	// preserved for the audit ledger — abandonment retires the GitHub review, not
	// the record). reviewID here is the artifact id.
	var artState string
	if err := database.QueryRow(
		`SELECT state FROM artifacts WHERE id = ?`, reviewID,
	).Scan(&artState); err != nil {
		t.Fatalf("scan review artifact state: %v", err)
	}
	if artState != domain.ArtifactStateReviewDismissed {
		t.Errorf("review artifact state = %q, want %q (dismissed on abandon)", artState, domain.ArtifactStateReviewDismissed)
	}

	// The agent's own memory is the conversation's account of what it tried;
	// the teardown resolves artifacts and must not touch it.
	mem, err := sqlitestore.New(database).TaskMemory.GetForConversationSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("GetForConversationSystem: %v", err)
	}
	if mem == nil {
		t.Fatalf("no conversation_memory row for conversation %s after cleanup", conversationID)
	}
	if mem.Content != "agent self-report" || mem.Source != domain.MemorySourceAgent {
		t.Errorf("memory = (Source=%q, Content=%q), want the agent's self-report preserved", mem.Source, mem.Content)
	}
}

// seedCallerGesture records a swipe_events row attributed to the acting user,
// which is what /undo reverses. Fixtures that insert a claimed task row
// directly skip the gesture that would have produced it in production, and
// undo refuses when the caller has nothing of their own to reverse.
func seedCallerGesture(t *testing.T, database *sql.DB, taskID, action string) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO swipe_events (task_id, action, hesitation_ms) VALUES (?, ?, 0)`,
		taskID, action,
	); err != nil {
		t.Fatalf("seed %s gesture on %s: %v", action, taskID, err)
	}
}

// TestHandleUndo_CleansUpPendingApprovalConversation is the regression
// for the swipe-toast UX path: Cards user dismissed/claimed the
// task, agent ran and left an artifact awaiting approval, user hits Cmd-Z (or
// the toast's Undo button). The full cleanup must run AND a swipe
// audit row should be recorded since this is a swipe undo.
func TestHandleUndo_CleansUpPendingApprovalConversation(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)
	seedCallerGesture(t, s.db, taskID, "claim")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, conversationID, reviewID, "queued")

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

// TestHandleRequeue_CleansUpPendingApprovalConversation is the parallel for
// the state-driven path: Board's drag-to-Queue, the "Return
// to queue" button. Same cleanup, but NO swipe row — drag/click
// gestures aren't swipes and shouldn't muddy the swipe analytics.
func TestHandleRequeue_CleansUpPendingApprovalConversation(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, conversationID, reviewID, "queued")

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

// TestTaskPatch_DismissCleansUpPendingApprovalConversation is the third
// entry point: user swipes left to dismiss a delegated card whose
// agent already produced a review awaiting approval. Today this
// orphans the review and leaves it hanging unresolved against a
// dismissed task — this is the other half.
func TestTaskPatch_DismissCleansUpPendingApprovalConversation(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "dismissed", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, conversationID, reviewID, "dismissed")
}

// TestTaskPatch_CompleteCleansUpPendingApprovalConversation is the fourth
// entry point: the Board's drag-AgentCard-to-Done gesture for a conversation
// awaiting approval. The complete swipe action flips the task to
// 'done' (so the card lands in the Done column rather than
// disappearing from the board, the way dismiss makes it) but reuses
// the same cleanup — review artifact flipped to dismissed, the completed
// conversation left untouched, the agent's own memory preserved.
func TestTaskPatch_CompleteCleansUpPendingApprovalConversation(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"status": "done", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertPendingApprovalCleanedUp(t, s.db, taskID, conversationID, reviewID, "done")
}

// TestTaskClaim_CleansUpPendingApprovalConversation guards the race the PR #77
// review flagged: Board's drag-Agent-to-You issues /claim, but the
// frontend's conversations map can be transiently empty during a fetchTasks
// refresh — so any frontend gating on a stale conversation-status snapshot
// would silently skip the cleanup, stranding the prepared review and leaving an
// unresolved artifact behind.
//
// Backend-authoritative teardown closes that hole: the swipe handler runs
// teardownTaskArtifacts for every claim, resolving every unresolved artifact (a
// no-op for tasks without one). The claim-flavored marker carries its own
// recalibration signal — "human took over manually" — distinct from
// requeue/dismiss/complete.
func TestTaskClaim_CleansUpPendingApprovalConversation(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim",
		map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Claim no longer transitions status; the task stays
	// 'queued' and claimed_by_user_id is set instead. The
	// pending-approval cleanup invariants (conversation cancelled, review row
	// removed) are unchanged.
	assertPendingApprovalCleanedUp(t, s.db, taskID, conversationID, reviewID, "queued")
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

// TestTaskClaim_WithoutPendingApprovalIsNoOp pins the
// idempotency contract: teardownTaskArtifacts must be a no-op
// when the task has no unresolved artifact, so wiring the teardown
// into the claim path doesn't disturb the queue → claim flow used by
// Cards.tsx and the existing Board queue → you drag.
func TestTaskClaim_WithoutPendingApprovalIsNoOp(t *testing.T) {
	s := newTestServer(t)

	// Plain queued task with no agent conversation. Mirrors what claim from
	// the queue looks like — the event/task FK chain mirrors
	// pendingApprovalFixture but stops short of any conversations or reviews.
	const eventType = "github:pr:opened"
	if _, err := s.db.Exec(`
		INSERT INTO entities (id, source, source_id, kind, state)
		VALUES ('e1', 'github', 'sky/repo#1', 'pr', 'active');
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES ('ev1', 'e1', ?, '');
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES ('00000000-0000-4000-8000-000000000002', 'e1', ?, 'ev1', 'queued');
	`, eventType, eventType); err != nil {
		t.Fatalf("seed FK chain: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000002/claim",
		map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Claim is a responsibility-axis action; status stays
	// 'queued', claim col gets stamped.
	var status string
	var claimedByUserID sql.NullString
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000002'`,
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

// TestTaskClaim_AgainstBotClaimedIsTakeover pins the swipe-claim
// race-safe handler: when the task is bot-claimed, the handler must
// route through TakeoverClaimFromAgent's optimistic guard and
// produce a clean takeover (bot claim → user claim, atomic). This
// pins the legitimate takeover branch — the steal-from-bot is
// allowed; what's not allowed is stealing from another user.
func TestTaskClaim_AgainstBotClaimedIsTakeover(t *testing.T) {
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
		 VALUES ('00000000-0000-4000-8000-000000000001', 'e_bot', ?, 'ev_bot', 'queued', ?)`,
		eventType, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed task with bot claim: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000001/claim",
		map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var claimedAgent, claimedUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000001'`,
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

// TestTaskClaim_RefusedLeavesNoAuditRow pins the
// audit contract: swipe_events records state CHANGES, not gesture
// ATTEMPTS. A claim swipe that's refused (different user owns the
// task) returns 409 with no audit row, no status flip, no snooze
// clear. The reviewer flagged this earlier as "RecordSwipe at the
// top mutates state for refused gestures" — the post-restructure
// handler runs claim mutation first and only records the audit
// after accept.
func TestTaskClaim_RefusedLeavesNoAuditRow(t *testing.T) {
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
		 VALUES ('00000000-0000-4000-8000-000000000009', 'e_refuse', ?, 'ev_refuse', 'queued', ?)`,
		eventType, otherUserID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000009/claim",
		map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// No swipe_events row written — refused gesture leaves no trace.
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = '00000000-0000-4000-8000-000000000009'`,
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
		`SELECT status, claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000009'`,
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

// TestTaskDelegate_RefusedLeavesNoAuditRow is the delegate
// half of the audit-contract guarantee. Pre-condition: task is
// user-claimed by ANOTHER user (the only delegate refuse path).
// Post: 409, no swipe_events, no state change.
func TestTaskDelegate_RefusedLeavesNoAuditRow(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))
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
		 VALUES ('00000000-0000-4000-8000-000000000007', 'e_drefuse', ?, 'ev_drefuse', 'queued', ?)`,
		eventType, otherUserID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000007/delegate",
		map[string]any{"hesitation_ms": 0, "blueprint_id": "any"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = '00000000-0000-4000-8000-000000000007'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0 (refused delegate must not audit)", swipeCount)
	}
	var claimUser sql.NullString
	var claimAgent sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id, claimed_by_agent_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000007'`,
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

// TestTaskClaim_RefusedOnTerminalTask pins the handler-level
// guard for the same-user-idempotent fall-through path. The data-
// layer helpers refuse claim transitions on done/dismissed rows,
// but the handler's same-user check is a no-op early-return that
// doesn't call any helper — so without an explicit status check
// in the handler, RecordSwipe's vestigial status='queued' write
// would reopen a closed task as a side effect of recording the
// audit row.
//
// Seeds a done task already claimed by the local user (the sticky-
// past-close audit state), fires /claim, asserts 409 +
// status preserved + no swipe_events row written.
func TestTaskClaim_RefusedOnTerminalTask(t *testing.T) {
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
				 VALUES ('00000000-0000-4000-8000-000000000012', 'e_term', ?, 'ev_term', ?, ?)`,
				eventType, terminalStatus, runmode.LocalDefaultUserID,
			); err != nil {
				t.Fatalf("seed terminal task: %v", err)
			}

			rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000012/claim",
				map[string]any{"hesitation_ms": 0})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}

			// Status must be preserved — the reopen bug would have
			// flipped it to 'queued' via RecordSwipe's lifecycle write.
			var status string
			if err := s.db.QueryRow(
				`SELECT status FROM tasks WHERE id = '00000000-0000-4000-8000-000000000012'`,
			).Scan(&status); err != nil {
				t.Fatalf("scan task: %v", err)
			}
			if status != terminalStatus {
				t.Errorf("status = %q, want %q (refused claim reopened terminal task)", status, terminalStatus)
			}

			// Audit must be silent on the refusal.
			var swipeCount int
			if err := s.db.QueryRow(
				`SELECT COUNT(*) FROM swipe_events WHERE task_id = '00000000-0000-4000-8000-000000000012'`,
			).Scan(&swipeCount); err != nil {
				t.Fatalf("scan swipe_events: %v", err)
			}
			if swipeCount != 0 {
				t.Errorf("swipe_events count = %d, want 0 (refused gesture must leave no trace)", swipeCount)
			}
		})
	}
}

// TestTaskDelegate_DifferentiatesRefusalReasons pins the
// post-fix error-mapping on the swipe-delegate path. HandoffRefused
// collapses three reasons (missing task / terminal task / different-
// user claim); the handler pre-loads to disambiguate so the
// response carries the right status code and message for each.
//
//   - missing task → 404 "task not found"
//   - terminal task → 409 "task is closed; delegate transitions
//     aren't allowed past close"
//   - different-user claim → 409 "task is claimed by another user"
func TestTaskDelegate_DifferentiatesRefusalReasons(t *testing.T) {
	t.Run("missing_task_404", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/delegate",
			map[string]any{"hesitation_ms": 0, "blueprint_id": "any"})
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
			 VALUES ('00000000-0000-4000-8000-000000000011', 'e_term_del', ?, 'ev_term_del', 'done')`,
			eventType,
		); err != nil {
			t.Fatalf("seed terminal task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000011/delegate",
			map[string]any{"hesitation_ms": 0, "blueprint_id": "any"})
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
			 VALUES ('00000000-0000-4000-8000-000000000006', 'e_diff_del', ?, 'ev_diff_del', 'queued', ?)`,
			eventType, otherUserID,
		); err != nil {
			t.Fatalf("seed other-user-claimed task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000006/delegate",
			map[string]any{"hesitation_ms": 0, "blueprint_id": "any"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "another user") {
			t.Errorf("body=%s; want theft message (not closed-task message)", rec.Body.String())
		}
	})
}

// TestTaskDelegate_RefusedWhenBotDisabled pins the
// acceptance criterion "swipe-to-delegate re-checks team_agents.enabled
// at swipe time." A team admin can toggle the bot off via SetEnabled
// — subsequent /delegate gestures must 409, with no claim
// stamp, no spawn, no audit row. Local-mode N=1 doesn't normally
// flip this off but the data-layer enforcement is what multi-tenant
// will need.
func TestTaskDelegate_RefusedWhenBotDisabled(t *testing.T) {
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
		 VALUES ('00000000-0000-4000-8000-000000000005', 'e_bot_off', ?, 'ev_bot_off', 'queued')`,
		eventType,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000005/delegate",
		map[string]any{"hesitation_ms": 0, "blueprint_id": "any"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (bot disabled); body=%s", rec.Code, rec.Body.String())
	}

	// No state changes — claim cols untouched, no swipe_events row.
	var claimAgent, claimUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000005'`,
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
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = '00000000-0000-4000-8000-000000000005'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0", swipeCount)
	}
}

// TestTaskPatchSnooze_RefusesOnClaimedTask pins the
// "snoozed ↔ unclaimed" invariant from the snooze side: the
// SnoozeTask store-level atomic UPDATE refuses on a claimed task,
// the handler maps the refusal to 409, and no state mutates (status
// stays the same, snooze_until stays NULL, audit row was rolled
// back as part of the atomic tx).
//
// This is the deliberate trade we made to avoid the snoozed+claimed
// incoherent state: users wanting to defer work on a claimed task
// must explicitly requeue first.
func TestTaskPatchSnooze_RefusesOnClaimedTask(t *testing.T) {
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
		 VALUES ('00000000-0000-4000-8000-000000000010', 'e_snz_claim', ?, 'ev_snz_claim', 'queued', ?)`,
		eventType, runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed claimed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/00000000-0000-4000-8000-000000000010",
		map[string]any{"snooze_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "hesitation_ms": 0})
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
		`SELECT status, snooze_until, claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000010'`,
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
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = '00000000-0000-4000-8000-000000000010'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0 (refused gesture should leave no audit)", swipeCount)
	}
}

// TestTaskDelegate_TransfersOwnUserClaim pins the
// flow: when the user drags their own claimed task from the You
// lane to the Agent lane, the FE fires a delegate swipe. The
// handler must accept the gesture as a legitimate user → bot
// transfer, not refuse it as a stolen-claim race.
//
// This was broken in an earlier iteration where the delegate path
// used a stamp helper that refused ANY non-NULL claimed_by_user_id
// — HandoffAgentClaim is the post-fix helper that allows same-user
// transfer while still refusing different-user theft.
func TestTaskDelegate_TransfersOwnUserClaim(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))

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
		 VALUES ('00000000-0000-4000-8000-000000000004', 'e_y2a', ?, 'ev_y2a', 'queued', ?)`,
		eventType, runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed user-claimed task: %v", err)
	}

	// Use a blueprint id that won't resolve — the spawner will fail
	// before producing a conversation, but the claim stamping is the part
	// under test and that runs before the spawn. The failed spawn is a
	// 422 (bad blueprint reference); what we care about is that the
	// transfer landed (claim flipped to bot) despite the error status.
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000004/delegate", map[string]any{
		"hesitation_ms": 0,
		"blueprint_id":  "no-such-prompt",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (spawn failed on a bad blueprint; transfer still landed); body=%s", rec.Code, rec.Body.String())
	}
	// The claim-survived marker: without SPAWN_FAILED the FE would read this
	// as a nothing-landed failure and skip the refetch + retry affordance.
	var errBody struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if len(errBody.Errors) != 1 || errBody.Errors[0].Reason != "SPAWN_FAILED" {
		t.Errorf("errors = %+v; want one SPAWN_FAILED item", errBody.Errors)
	}

	var claimedAgent, claimedUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000004'`,
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

// TestTaskClaim_AgainstOtherUserClaimReturns409 pins the
// anti-steal guarantee: if a different user already owns the task,
// the swipe-claim handler must refuse with 409 rather than
// overwriting the other user's claim. The previous unconditional
// SetClaimedByUser would have silently stolen the row.
//
// At N=1 local mode this can't happen via real user gestures, but
// the helper-level race-safety is load-bearing for multi-mode and
// the test pins the contract.
func TestTaskClaim_AgainstOtherUserClaimReturns409(t *testing.T) {
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
		 VALUES ('00000000-0000-4000-8000-000000000003', 'e_oth', ?, 'ev_oth', 'queued', ?)`,
		eventType, otherUserID,
	); err != nil {
		t.Fatalf("seed task with other-user claim: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000003/claim",
		map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Claim must be unchanged — the swipe refused to overwrite.
	var claimedUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000003'`,
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

// TestHandleUndo_RefusesWithNothingOfTheCallersToReverse closes the
// force-reset hole: /undo used to write an 'undo' row and full-reset ANY task
// the caller could address, whether or not they had ever touched it. It now
// reverses the caller's own last gesture and refuses when there isn't one —
// including a second undo, whose target was already reversed. The deliberate
// version of "put this back in the queue" is /requeue.
func TestHandleUndo_RefusesWithNothingOfTheCallersToReverse(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "undo-guard", lifecycleTaskOpts{status: "dismissed"})

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/undo", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("undo with no gesture = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	// The refusal changes nothing — in particular it does not re-open the
	// closed task, which is the whole point of the guard.
	if got := readTaskStatus(t, s.db, taskID); got != "dismissed" {
		t.Errorf("task.status = %q, want dismissed (a refused undo must not re-open a closed task)", got)
	}
	if got := readSwipeActions(t, s.db, taskID); len(got) != 0 {
		t.Errorf("swipe_events actions = %v, want none (a refused undo leaves no trace)", got)
	}

	// With a gesture of the caller's own to reverse, the same call succeeds —
	// and a second one is refused, because the first already reversed it.
	seedCallerGesture(t, s.db, taskID, "dismiss")
	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/undo", nil); rec.Code != http.StatusOK {
		t.Fatalf("undo with a gesture to reverse = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := readTaskStatus(t, s.db, taskID); got != "queued" {
		t.Errorf("task.status = %q, want queued after the undo", got)
	}
	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/undo", nil); rec.Code != http.StatusConflict {
		t.Fatalf("second undo = %d, want 409; body=%s", rec.Code, rec.Body.String())
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

	ok, err := s.swipes.RequeueTask(t.Context(), runmode.LocalDefaultOrgID, "no-such-task")
	if err != nil {
		t.Fatalf("RequeueTask: %v", err)
	}
	if ok {
		t.Errorf("RequeueTask returned ok=true for missing id; want false")
	}
}

// TestHandleUndo_NoPendingApprovalIsNoOp guards the common case:
// the task has no delegated conversation (or its delegated conversation is still
// active, with nothing awaiting approval). The cleanup should silently
// no-op rather than touching unrelated conversations/reviews.
func TestHandleUndo_NoPendingApprovalIsNoOp(t *testing.T) {
	s := newTestServer(t)

	// Seed a plain user-claimed task with no conversation at all — the simplest
	// shape that exercises handleUndo's other half (claim clear +
	// Jira reversal skipped because EntitySource isn't 'jira'). Post-B+
	// this is status='queued' + claimed_by_user_id; pre-B+
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
		 VALUES ('00000000-0000-4000-8000-000000000008', 'e_plain', 'github:pr:opened', 'ev_plain', 'queued', ?)`,
		runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	seedCallerGesture(t, s.db, "00000000-0000-4000-8000-000000000008", "claim")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000008/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var taskStatus string
	if err := s.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, "00000000-0000-4000-8000-000000000008").Scan(&taskStatus); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if taskStatus != "queued" {
		t.Errorf("task.status = %q, want %q", taskStatus, "queued")
	}
}

// TestHandleUndo_ClearsClaimColumns pins the B+ semantic:
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
		 VALUES ('00000000-0000-4000-8000-000000000013', 'e_undo_claim', 'github:pr:opened', 'ev_undo_claim', 'queued', ?)`,
		runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	seedCallerGesture(t, s.db, "00000000-0000-4000-8000-000000000013", "claim")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000013/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status, claimedAgent, claimedUser sql.NullString
	var snoozeUntil sql.NullTime
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_agent_id, claimed_by_user_id, snooze_until
		 FROM tasks WHERE id = ?`, "00000000-0000-4000-8000-000000000013",
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

// TestTeardownTaskArtifacts_Idempotent calls the teardown twice against the
// same task. The first call resolves the review artifact (→ dismissed); the
// second finds nothing unresolved left and is a no-op — the artifact stays
// dismissed, the agent's memory stays its own, and the completed conversation
// stays completed across both (a resolve never flips conversation lifecycle).
func TestTeardownTaskArtifacts_Idempotent(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, _ := pendingApprovalFixture(t, s.db)

	s.teardownTaskArtifacts(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, taskID)
	s.teardownTaskArtifacts(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, taskID)

	var convStatusAfter string
	if err := s.db.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&convStatusAfter); err != nil {
		t.Fatalf("scan after second call: %v", err)
	}
	if convStatusAfter != "completed" {
		t.Errorf("conversation status drifted after second call: %q (teardown must not flip conversation lifecycle)", convStatusAfter)
	}
	mem, err := sqlitestore.New(s.db).TaskMemory.GetForConversationSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil || mem == nil {
		t.Fatalf("GetForConversationSystem after second call: mem=%v err=%v", mem, err)
	}
	if mem.Content != "agent self-report" || mem.Source != domain.MemorySourceAgent {
		t.Errorf("memory = (Source=%q, Content=%q) after two teardowns, want the agent's row untouched", mem.Source, mem.Content)
	}
}

// TestTeardownTaskArtifacts_FailureHoldsArtifactForRetry is the regression for
// the all-or-nothing contract: if a DB op inside the teardown tx fails
// transiently, the whole batch rolls back, leaving the artifact unresolved
// (still pending) and the conversation untouched — a subsequent call retries
// cleanly.
//
// We force a failure by temporarily renaming the artifacts table — the teardown's
// ListByConversation (and the dismiss upsert) reference it by name and the whole tx rolls
// back. After restoring it, a second call resolves the review.
func TestTeardownTaskArtifacts_FailureHoldsArtifactForRetry(t *testing.T) {
	s := newTestServer(t)
	taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)

	// Sabotage: rename the artifacts table so the teardown's ListByConversation fails with
	// "no such table" and the tx rolls back before any flip.
	if _, err := s.db.Exec(`ALTER TABLE artifacts RENAME TO artifacts_temp`); err != nil {
		t.Fatalf("rename artifacts table: %v", err)
	}

	s.teardownTaskArtifacts(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, taskID)

	// The conversation is untouched (never flipped — teardown doesn't touch its status).
	var convStatus string
	if err := s.db.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("scan conversation after sabotaged teardown: %v", err)
	}
	if convStatus != "completed" {
		t.Fatalf("conversation status = %q after failure; want %q (conversation untouched)", convStatus, "completed")
	}

	// Heal the table; the next call must resolve the still-pending review.
	if _, err := s.db.Exec(`ALTER TABLE artifacts_temp RENAME TO artifacts`); err != nil {
		t.Fatalf("restore artifacts table: %v", err)
	}

	s.teardownTaskArtifacts(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, taskID)

	var artState string
	if err := s.db.QueryRow(`SELECT state FROM artifacts WHERE id = ?`, reviewID).Scan(&artState); err != nil {
		t.Fatalf("scan review artifact after retry: %v", err)
	}
	if artState != domain.ArtifactStateReviewDismissed {
		t.Errorf("review artifact state = %q after retry; want %q (review should be dismissed)", artState, domain.ArtifactStateReviewDismissed)
	}
}

// runningRunFixture installs the FK chain for a task an agent is actively
// working: a bot-claimed `in_progress` task whose conversation is `running`
// under a running blueprint run. Returns (taskID, conversationID,
// blueprintRunID).
//
// This is the shape the Board's old drag guard refused to move and the
// redesign makes draggable, so it is the shape the requeue stop pass exists
// for: without a stop, the agent keeps executing against a task that says
// nobody owns it.
func runningRunFixture(t *testing.T, database *sql.DB, suffix string) (taskID, conversationID, blueprintRunID string) {
	t.Helper()
	const eventType = "github:pr:ci_check_failed"
	entityID, eventID, promptID := fixtureUUID("e_"+suffix), fixtureUUID("ev_"+suffix), fixtureUUID("p_"+suffix)
	taskID, conversationID = fixtureUUID("t_"+suffix), fixtureUUID("r_"+suffix)
	execSQL(t, database,
		`INSERT INTO entities (id, source, source_id, kind, state) VALUES (?, 'github', ?, 'pr', 'active')`,
		entityID, "owner/repo#"+suffix)
	execSQL(t, database,
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')`,
		eventID, entityID, eventType)
	execSQL(t, database,
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'Fix CI', 'body', ?, ?)`,
		promptID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, database,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_agent_id)
		 VALUES (?, ?, ?, ?, 'in_progress', ?)`,
		taskID, entityID, eventType, eventID, runmode.LocalDefaultAgentID)
	blueprintRunID = seedBlueprintRunSQLite(t, database, taskID)
	execSQL(t, database,
		`INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index)
		 VALUES (?, ?, ?, 'running', 'manual', ?, 0)`,
		conversationID, taskID, promptID, blueprintRunID)
	return taskID, conversationID, blueprintRunID
}

// assertRequeueStoppedTheRun checks every post-condition a requeue owes a live
// run: the conversation parked `open` by a user-attributed stop, the last
// transcript row explaining that the task went back to the queue, the
// blueprint behind it cancelled rather than frozen 'running', and the task
// itself queued and unclaimed. The last two together are the point — a task
// nobody owns with an agent still executing against it is the state this
// exists to prevent.
func assertRequeueStoppedTheRun(t *testing.T, database *sql.DB, taskID, conversationID, blueprintRunID string) {
	t.Helper()

	var convStatus, parkReason string
	if err := database.QueryRow(
		`SELECT status, COALESCE(park_reason, '') FROM conversations WHERE id = ?`, conversationID,
	).Scan(&convStatus, &parkReason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if convStatus != "open" || parkReason != "user_cancelled" {
		t.Errorf("conversation = (%q, %q), want (open, user_cancelled) — a requeue stops the run that was working the task",
			convStatus, parkReason)
	}

	var role, subtype, content string
	if err := database.QueryRow(
		`SELECT role, subtype, COALESCE(content, '') FROM messages WHERE conversation_id = ? ORDER BY id DESC LIMIT 1`,
		conversationID,
	).Scan(&role, &subtype, &content); err != nil {
		t.Fatalf("read last transcript row: %v", err)
	}
	if role != "user" || subtype != domain.MessageSubtypeStopNote {
		t.Errorf("last transcript row = (role=%q, subtype=%q), want (user, %s) — the stop note is the record",
			role, subtype, domain.MessageSubtypeStopNote)
	}
	if !strings.Contains(content, "returned to the queue") {
		t.Errorf("stop note = %q; want the requeue cause's sentence — it is the whole explanation a reader of this transcript gets", content)
	}
	if strings.Contains(content, "dispositioned") {
		t.Errorf("stop note = %q; the task is still open, so the disposition wording is the wrong sentence", content)
	}

	var bpStatus string
	var cancelRequested bool
	if err := database.QueryRow(
		`SELECT status, cancel_requested FROM blueprint_runs WHERE id = ?`, blueprintRunID,
	).Scan(&bpStatus, &cancelRequested); err != nil {
		t.Fatalf("read blueprint run: %v", err)
	}
	if bpStatus != "cancelled" || !cancelRequested {
		t.Errorf("blueprint_run = (%q, cancel_requested=%v), want (cancelled, true) — nothing resumes a requeued task's run, so freezing the plan 'running' would hold its worktree forever",
			bpStatus, cancelRequested)
	}

	var taskStatus string
	var claimedAgent, claimedUser sql.NullString
	if err := database.QueryRow(
		`SELECT status, claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = ?`, taskID,
	).Scan(&taskStatus, &claimedAgent, &claimedUser); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if taskStatus != "queued" {
		t.Errorf("task status = %q, want queued", taskStatus)
	}
	if claimedAgent.Valid || claimedUser.Valid {
		t.Errorf("task claims = (agent=%v, user=%v), want both NULL", claimedAgent, claimedUser)
	}
}

// TestHandleRequeue_StopsTheRunWorkingTheTask is the route's whole contract
// against a live run. Requeue used to run artifact teardown, the Jira
// reversal and a broadcast, and nothing that touched the agent — so the card
// landed back in Queued while its agent kept writing messages, kept landing
// artifacts on a conversation whose task said nobody owned it, and the
// spawner's board-column recompute put the card straight back out of Queued
// on the claim it re-derived.
//
// The frontend used to mask it by refusing to drag a mid-flight card. The
// Board redesign drags every state, and this stop is what makes that safe —
// which is also why the route still takes every status rather than growing a
// guard.
func TestHandleRequeue_StopsTheRunWorkingTheTask(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))
	taskID, conversationID, blueprintRunID := runningRunFixture(t, s.db, "requeue_live")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertRequeueStoppedTheRun(t, s.db, taskID, conversationID, blueprintRunID)
}

// TestHandleUndo_StopsTheRunWorkingTheTask: /undo shares finalizeRequeue with
// /requeue, so it inherits the stop. Asserted rather than assumed — the two
// routes reaching the same finalizer is the reason undo is correct, and a
// future split that gave undo its own body would otherwise reintroduce the
// gap on the quieter of the two paths.
func TestHandleUndo_StopsTheRunWorkingTheTask(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))
	taskID, conversationID, blueprintRunID := runningRunFixture(t, s.db, "undo_live")
	seedCallerGesture(t, s.db, taskID, "delegate")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/undo", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertRequeueStoppedTheRun(t, s.db, taskID, conversationID, blueprintRunID)
}

// TestHandleRequeue_TerminalConversationIsNotStopped pins the other side: the
// stop pass enumerates ACTIVE conversations, so a task whose run already
// finished has nothing to stop and the finished conversation is left exactly
// as it was — no park, no stop note, no cancelled blueprint written over a
// conversation that concluded on its own. The artifact teardown still runs,
// which is the half that was already correct.
//
// A real spawner rather than a stub: what proves no stop happened is that the
// spawner had every means to do one and the completed row is untouched.
func TestHandleRequeue_TerminalConversationIsNotStopped(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))
	taskID, conversationID, reviewID := pendingApprovalFixture(t, s.db)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The teardown half still ran: artifact resolved, memory note written,
	// task back in the queue, the completed conversation left completed.
	assertPendingApprovalCleanedUp(t, s.db, taskID, conversationID, reviewID, "queued")

	var parkReason sql.NullString
	if err := s.db.QueryRow(`SELECT park_reason FROM conversations WHERE id = ?`, conversationID).Scan(&parkReason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if parkReason.Valid {
		t.Errorf("park_reason = %q; a conversation that already concluded has nothing to stop", parkReason.String)
	}
	var stopNotes int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND subtype = ?`,
		conversationID, domain.MessageSubtypeStopNote,
	).Scan(&stopNotes); err != nil {
		t.Fatalf("count stop notes: %v", err)
	}
	if stopNotes != 0 {
		t.Errorf("stop notes = %d, want 0 — nothing was stopped, so the transcript has nothing to say about one", stopNotes)
	}
	var bpStatus string
	if err := s.db.QueryRow(
		`SELECT br.status FROM blueprint_runs br JOIN conversations c ON c.blueprint_run_id = br.id WHERE c.id = ?`,
		conversationID,
	).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint run: %v", err)
	}
	if bpStatus == "cancelled" {
		t.Error("blueprint_run status = cancelled; the stop pass reached a conversation it should not have enumerated")
	}
}
