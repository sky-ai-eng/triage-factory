package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// POST /api/tasks chains an existing helper trio — Entities.Get →
// Events.LatestForEntityTypeAndDedupKey → Tasks.FindOrCreate — all covered at
// the db level. What the handler adds is request validation, the three refusals
// and the create-vs-found status, which is what these pin. The Factory's drop
// gesture is this route plus POST /api/tasks/{id}/delegate; the two-call flow
// gets its own test below.

// seedStation stages an active entity with one event of eventType — the state a
// station chip represents, and the minimum POST /api/tasks needs to resolve.
func seedStation(t *testing.T, s *Server, sourceID, eventType string) *domain.Entity {
	t.Helper()
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", sourceID, "pr", "", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eid := entity.ID
	if _, err := s.events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EntityID:  &eid,
		EventType: eventType,
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	return entity
}

func TestTaskCreate_FindOrCreate(t *testing.T) {
	s := newTestServer(t)
	entity := seedStation(t, s, "owner/repo#201", domain.EventGitHubPRCICheckPassed)

	body := map[string]string{
		"entity_id":  entity.ID,
		"event_type": domain.EventGitHubPRCICheckPassed,
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first POST /api/tasks = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created taskJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" || created.EntityID != entity.ID {
		t.Fatalf("created task = %+v; want the station's task", created)
	}

	// The gesture is idempotent by construction (the partial unique index on
	// (entity_id, event_type, dedup_key)): a second drop finds the same row and
	// says so with 200 rather than minting a second task.
	rec = doJSON(t, s, http.MethodPost, "/api/tasks", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("second POST /api/tasks = %d, want 200 (found); body=%s", rec.Code, rec.Body.String())
	}
	var found taskJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &found); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("second call resolved task %s, want the first call's %s", found.ID, created.ID)
	}
}

func TestTaskCreate_404OnMissingEntity(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/tasks", map[string]string{
		"entity_id":  "no-such-entity",
		"event_type": domain.EventGitHubPRCICheckPassed,
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestTaskCreate_422OnNoMatchingEvent confirms the "no matching event"
// rejection fires for an active entity that hasn't produced an event of the
// requested type — a malformed snapshot reference or a stale frontend retry. A
// 422, not a 400: the body is well-formed, the station it references doesn't
// exist for this entity.
func TestTaskCreate_422OnNoMatchingEvent(t *testing.T) {
	s := newTestServer(t)
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#400e", "pr", "", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	// No event recorded — the request asks for a station the entity has never
	// been at.
	rec := doJSON(t, s, http.MethodPost, "/api/tasks", map[string]string{
		"entity_id":  entity.ID,
		"event_type": domain.EventGitHubPRCICheckPassed,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (no matching event)", rec.Code)
	}
}

// TestTaskCreate_409OnClosedEntity is the regression for the soft-close grace
// race: factory snapshots include entities for ~60s after they close so the
// chip can ride into the terminal station, but a drop during that window must
// not synthesize a task on a merged/closed entity. The router's task-creation
// path enforces the same "active only" contract.
func TestTaskCreate_409OnClosedEntity(t *testing.T) {
	s := newTestServer(t)
	entity := seedStation(t, s, "owner/repo#409", domain.EventGitHubPRMerged)
	if _, err := sqlitestore.New(s.db).Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, entity.ID); err != nil {
		t.Fatalf("close entity: %v", err)
	}
	rec := doJSON(t, s, http.MethodPost, "/api/tasks", map[string]string{
		"entity_id":  entity.ID,
		"event_type": domain.EventGitHubPRMerged,
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (closed entity)", rec.Code)
	}
}

func TestTaskCreate_400OnBadBody(t *testing.T) {
	s := newTestServer(t)

	t.Run("missing_event_type", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, "/api/tasks", map[string]string{"entity_id": "x"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	// blueprint_id belongs to the delegate call. A caller sending it here is
	// told by name rather than having it silently dropped — which is what
	// separates "this route doesn't fire runs" from "it ignored your run".
	t.Run("blueprint_id_rejected_by_name", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, "/api/tasks", map[string]string{
			"entity_id":    "x",
			"event_type":   domain.EventGitHubPRCICheckPassed,
			"blueprint_id": "p1",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, "UNKNOWN_FIELD", "blueprint_id")
	})

	t.Run("malformed_json", func(t *testing.T) {
		// Bypass doJSON's json.Marshal so the handler gets raw bytes that won't
		// decode — the decoder branch, separate from the empty-fields one.
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewReader([]byte("{not valid json")))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}

// TestFactoryDropGesture_CreateThenDelegate walks the drop end to end as the
// frontend does it — POST /api/tasks, then POST /api/tasks/{id}/delegate on the
// row it resolved — and pins the partial state the two calls make addressable:
// when the spawn fails (here a blueprint id that doesn't resolve), the claim and
// the swipe audit still land, and the client holds a task id it can retry
// /delegate against without re-deriving the task.
func TestFactoryDropGesture_CreateThenDelegate(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))
	entity := seedStation(t, s, "owner/repo#400p", domain.EventGitHubPRCICheckPassed)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks", map[string]string{
		"entity_id":  entity.ID,
		"event_type": domain.EventGitHubPRCICheckPassed,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/tasks = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var task taskJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}

	rec = doJSON(t, s, http.MethodPost, "/api/tasks/"+task.ID+"/delegate", map[string]any{
		"hesitation_ms": 0,
		"blueprint_id":  "no-such-prompt",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("delegate = %d, want 422 (bad blueprint reference; claim stamped, run didn't fire); body=%s", rec.Code, rec.Body.String())
	}
	// SPAWN_FAILED is the claim-survived marker the FE keys the retry
	// affordance on — a plain INTERNAL here would read as "nothing landed".
	assertFirstError(t, rec, "SPAWN_FAILED", "blueprint_id")

	stored, err := s.tasks.Get(t.Context(), runmode.LocalDefaultOrgID, task.ID)
	if err != nil || stored == nil {
		t.Fatalf("read task back: task=%v err=%v", stored, err)
	}
	if stored.ClaimedByAgentID == "" {
		t.Errorf("task.ClaimedByAgentID empty; the claim should survive a failed spawn")
	}
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = ? AND action = 'delegate'`,
		task.ID,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 1 {
		t.Errorf("swipe_events count = %d, want 1 (the gesture is audited even when the run doesn't fire)", swipeCount)
	}
}

// TestFactoryDropGesture_BotDisabledLeavesTaskUnclaimed pins where the
// bot-disabled refusal lands across the two calls: the task is created (the
// entity does need attention at this station, bot or no bot), and the delegate
// call refuses with 409, leaving no claim and no audit row.
func TestFactoryDropGesture_BotDisabledLeavesTaskUnclaimed(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))

	// Flip the bot OFF on the local team. The production path is
	// team_agents.SetEnabled via a team-admin gesture; the direct UPDATE
	// mirrors the same end state without the (unrelated) admin handler.
	if _, err := s.db.Exec(
		`UPDATE team_agents SET enabled = 0 WHERE team_id = ? AND agent_id = ?`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("disable bot: %v", err)
	}
	entity := seedStation(t, s, "owner/repo#fdrop-off", domain.EventGitHubPRCICheckPassed)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks", map[string]string{
		"entity_id":  entity.ID,
		"event_type": domain.EventGitHubPRCICheckPassed,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/tasks = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var task taskJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}

	rec = doJSON(t, s, http.MethodPost, "/api/tasks/"+task.ID+"/delegate", map[string]any{
		"hesitation_ms": 0,
		"blueprint_id":  "any-prompt",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("delegate = %d, want 409 (bot disabled); body=%s", rec.Code, rec.Body.String())
	}

	var claimAgent sql.NullString
	if err := s.db.QueryRow(
		`SELECT COALESCE(claimed_by_agent_id, '') FROM tasks WHERE id = ?`, task.ID,
	).Scan(&claimAgent); err != nil {
		t.Fatalf("scan task claim: %v", err)
	}
	if claimAgent.Valid && claimAgent.String != "" {
		t.Errorf("bot claim landed despite the disabled flag: %q", claimAgent.String)
	}
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = ? AND action = 'delegate'`, task.ID,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events count = %d, want 0 (a refused gesture leaves no trace)", swipeCount)
	}
}

// TestFactorySnapshot_PendingTasksRoundtrip pins the snapshot → create request
// shape: a queued entity that already has an active task at this station
// appears in /api/factory/snapshot under pending_tasks[event_type], and that
// task's dedup_key is what the frontend forwards (with entity_id + event_type)
// to POST /api/tasks.
func TestFactorySnapshot_PendingTasksRoundtrip(t *testing.T) {
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
