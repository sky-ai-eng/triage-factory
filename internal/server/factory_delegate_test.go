package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The drag-to-delegate handler chains an existing helper trio:
// db.GetEntity → LatestEventForEntityAndType → FindOrCreateTask →
// spawner.Delegate. The first three are covered at the db level; what
// the handler adds is request validation and HTTP status mapping.
// These tests pin the latter without depending on a real Spawner —
// the spawner-bound paths trust the already-tested Delegate behavior.

func TestHandleFactoryDelegate_ServiceUnavailableWithoutSpawner(t *testing.T) {
	s := newTestServer(t)
	// Seed a real entity + event so the request is otherwise valid
	// when it reaches the missing-spawner gate in the handler's
	// progressive validation flow.
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#503", "pr", "", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eid := entity.ID
	if _, err := s.events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EntityID:  &eid,
		EventType: domain.EventGitHubPRCICheckPassed,
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	// No SetSpawner call — simulate startup-order or test-config gap.
	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":    entity.ID,
		"event_type":   domain.EventGitHubPRCICheckPassed,
		"blueprint_id": "p1",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleFactoryDelegate_404OnMissingEntity(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":    "no-such-entity",
		"event_type":   domain.EventGitHubPRCICheckPassed,
		"blueprint_id": "p1",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestHandleFactoryDelegate_400OnNoMatchingEvent confirms the
// "no matching event" 400 fires for an active entity that hasn't
// produced an event of the requested type — a malformed snapshot
// reference or a stale frontend retry. Pinned without a spawner so
// the test also doubles as a regression for the gate ordering: any
// request validation error must precede the 503 infrastructure gate.
func TestHandleFactoryDelegate_400OnNoMatchingEvent(t *testing.T) {
	s := newTestServer(t)
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#400e", "pr", "", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	// No event recorded — request asks to delegate at a station the
	// entity has never produced.
	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":    entity.ID,
		"event_type":   domain.EventGitHubPRCICheckPassed,
		"blueprint_id": "p1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (no matching event)", rec.Code)
	}
}

// TestHandleFactoryDelegate_409OnClosedEntity is the regression for the
// soft-close grace race: factory snapshots include entities for ~60s
// after they close so the chip can ride into the terminal station, but
// drag-to-delegate during that window should not start a fresh run on
// a merged/closed entity. The router's task-creation path enforces the
// same "active only" contract; this test pins it for the drag path.
func TestHandleFactoryDelegate_409OnClosedEntity(t *testing.T) {
	s := newTestServer(t)
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#409", "pr", "", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := sqlitestore.New(s.db).Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, entity.ID); err != nil {
		t.Fatalf("close entity: %v", err)
	}
	eid := entity.ID
	if _, err := s.events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EntityID:  &eid,
		EventType: domain.EventGitHubPRMerged,
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":    entity.ID,
		"event_type":   domain.EventGitHubPRMerged,
		"blueprint_id": "p1",
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (closed entity)", rec.Code)
	}
}

func TestHandleFactoryDelegate_400OnMissingFields(t *testing.T) {
	s := newTestServer(t)
	// Missing prompt_id.
	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":  "x",
		"event_type": "github:pr:ci_check_passed",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleFactoryDelegate_400OnMalformedJSON(t *testing.T) {
	s := newTestServer(t)
	// Bypass doJSON's json.Marshal so we can hand the handler raw
	// bytes that won't decode. Hits the JSON-decoder error branch
	// (separate from the empty-fields branch covered above).
	req := httptest.NewRequest(http.MethodPost, "/api/factory/delegate",
		bytes.NewReader([]byte("{not valid json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestHandleFactoryDelegate_DelegateErrorPreservesClaim pins the
// semantic: when the user's drag-to-delegate gesture commits at the
// claim axis but the spawner.Delegate call fails (e.g. ErrPromptNotFound
// from a race-deleted prompt), the handler returns 200 OK with
// delegate_error populated and claim_stamped=true. Mirrors the swipe-
// delegate response shape exactly. The claim must survive in the DB so
// the Board renders the bot-claimed-but-no-run card with a Retry
// affordance.
//
// Replaces the previous "TestHandleFactoryDelegate_400OnMissingPrompt"
// which asserted a 400 — that contract changed when factory_delegate
// adopted the swipe convention of 200 + delegate_error for partial
// success (claim committed, run didn't fire).
func TestHandleFactoryDelegate_DelegateErrorPreservesClaim(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))

	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#400p", "pr", "", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eid := entity.ID
	if _, err := s.events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EntityID:  &eid,
		EventType: domain.EventGitHubPRCICheckPassed,
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":    entity.ID,
		"event_type":   domain.EventGitHubPRCICheckPassed,
		"blueprint_id": "no-such-prompt",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial-success convention: claim stamped, run didn't fire)", rec.Code)
	}
	var resp struct {
		TaskID         string `json:"task_id"`
		ConversationID string `json:"conversation_id"`
		DelegateError  string `json:"delegate_error"`
		ClaimStamped   bool   `json:"claim_stamped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.DelegateError == "" {
		t.Errorf("delegate_error empty; expected spawner failure message")
	}
	if resp.ConversationID != "" {
		t.Errorf("conversation_id = %q; want empty (spawn failed)", resp.ConversationID)
	}
	if !resp.ClaimStamped {
		t.Errorf("claim_stamped = false; want true (claim committed before spawn attempt)")
	}
	if resp.TaskID == "" {
		t.Fatal("task_id empty; can't verify claim persistence")
	}

	// Verify the claim survives in the DB — the FE relies on this to
	// surface the bot-claimed-with-failed-run state.
	task, err := s.tasks.Get(t.Context(), runmode.LocalDefaultOrgID, resp.TaskID)
	if err != nil || task == nil {
		t.Fatalf("read task back: task=%v err=%v", task, err)
	}
	if task.ClaimedByAgentID == "" {
		t.Errorf("task.ClaimedByAgentID empty; claim should be stamped despite spawn failure")
	}

	// Verify the swipe_events audit row was written even on the
	// partial-success path. Pre-fix RecordSwipe only fired on the
	// run-success path, leaving the user's drag-to-bot gesture
	// audit-less when the spawn errored — inconsistent with swipe-
	// delegate which audits at the top of the swipe handler.
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = ? AND action = 'delegate'`,
		resp.TaskID,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 1 {
		t.Errorf("swipe_events count = %d, want 1 (audit must survive partial-success)", swipeCount)
	}
}

// TestHandleFactoryDelegate_RefusedWhenBotDisabled pins the
// bot-disabled-team gate for the factory drag-to-Agent gesture.
// Symmetric with the swipe-delegate handler's gate: when an admin
// has flipped team_agents.enabled = false, the factory drop refuses
// with 409 and the same disabled-bot message — no claim stamp, no
// spawn attempt, no audit row, no task creation side effect on the
// entity.
//
// "Bot disabled" means the team has no bot at all (not "auto-
// delegation is paused" — that's the separate cfg.AI.AutoDelegateEnabled
// kill switch which doesn't block manual gestures). So manual
// delegate gestures must refuse alongside auto-fire, otherwise
// users could route around the team's explicit no-bot setting via
// the factory drop UI.
func TestHandleFactoryDelegate_RefusedWhenBotDisabled(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))

	// Flip the bot OFF on the local team. Production path is
	// team_agents.SetEnabled via a team-admin gesture; direct
	// UPDATE here mirrors the same end state without going through
	// the (unrelated) admin handler.
	if _, err := s.db.Exec(
		`UPDATE team_agents SET enabled = 0 WHERE team_id = ? AND agent_id = ?`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("disable bot: %v", err)
	}

	// Seed an entity + event that would otherwise let the factory
	// drop succeed (active entity, matching event for the station).
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#fdrop-off", "pr", "Disabled Factory", "https://example.com/fdo")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eid := entity.ID
	if _, err := s.events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EntityID:  &eid,
		EventType: domain.EventGitHubPRCICheckPassed,
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/factory/delegate", map[string]string{
		"entity_id":    entity.ID,
		"event_type":   domain.EventGitHubPRCICheckPassed,
		"blueprint_id": "any-prompt",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (bot disabled); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disabled") {
		t.Errorf("body=%s; want disabled-bot message", rec.Body.String())
	}

	// State invariants on refusal: no task created for the gesture,
	// no swipe_events audit row, no run spawned. The factory drop
	// returned before any of the find-or-create side effects could
	// land. A pre-existing task on this entity from event recording
	// would have status='queued' (router default), unclaimed; we
	// just assert no claim/run/audit traces of the refused gesture.
	var claimAgent sql.NullString
	if err := s.db.QueryRow(
		`SELECT COALESCE(claimed_by_agent_id, '') FROM tasks WHERE entity_id = ?`,
		entity.ID,
	).Scan(&claimAgent); err != nil && err != sql.ErrNoRows {
		t.Fatalf("scan task claim: %v", err)
	}
	if claimAgent.Valid && claimAgent.String != "" {
		t.Errorf("bot claim landed despite disabled flag: %q", claimAgent.String)
	}
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events se
		 JOIN tasks t ON t.id = se.task_id
		 WHERE t.entity_id = ? AND se.action = 'delegate'`,
		entity.ID,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0 (refused gesture must leave no trace)", swipeCount)
	}
}

// TestHandleFactoryDelegate_PendingTasksRoundtrip pins the snapshot →
// delegate request shape: a queued entity that already has an active
// task at this station should appear in /api/factory/snapshot under
// pending_tasks[event_type], and that task's id + dedup_key are what
// the frontend forwards to /api/factory/delegate. Walks the snapshot
// without exercising the delegate handler itself (the spawner needs
// integration setup).
func TestHandleFactoryDelegate_PendingTasksRoundtrip(t *testing.T) {
	s := newTestServer(t)

	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#7", "pr", "test PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eid := entity.ID
	evtID, err := s.events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EntityID:  &eid,
		EventType: domain.EventGitHubPRCICheckPassed,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := s.tasks.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckPassed, "", evtID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/factory/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d", rec.Code)
	}
	type snapshotShape struct {
		Entities []struct {
			ID           string                      `json:"id"`
			PendingTasks map[string][]pendingTaskRef `json:"pending_tasks"`
		} `json:"entities"`
	}
	var snap snapshotShape
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var got *pendingTaskRef
	for _, e := range snap.Entities {
		if e.ID != entity.ID {
			continue
		}
		if refs := e.PendingTasks[domain.EventGitHubPRCICheckPassed]; len(refs) > 0 {
			got = &refs[0]
		}
	}
	if got == nil {
		t.Fatalf("expected pending_tasks[%s] for entity %s in snapshot", domain.EventGitHubPRCICheckPassed, entity.ID)
	}
	if got.TaskID != task.ID {
		t.Errorf("task_id = %s, want %s", got.TaskID, task.ID)
	}
	if got.DedupKey != "" {
		t.Errorf("dedup_key = %q, want empty", got.DedupKey)
	}
}
