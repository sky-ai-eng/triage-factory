package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// redelegateFixture wires a real spawner and delegates a task once through the
// route, returning the task, the blueprint to delegate it with again, and the
// first run's step conversation.
func redelegateFixture(t *testing.T, s *Server, tag string) (taskID, blueprintID, firstStep string) {
	t.Helper()
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))

	const eventType = "github:pr:opened"
	promptID := "p-" + tag
	blueprintID = "bp-" + tag
	taskID = uuid.NewString()
	if _, err := s.db.Exec(
		`INSERT INTO prompts (id, name, body, source, creator_user_id) VALUES (?, ?, 'do the thing', 'user', ?)`,
		promptID, tag, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if _, err := s.blueprints.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: blueprintID, Name: tag, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id) VALUES (?, 0, ?)`,
		blueprintID, promptID); err != nil {
		t.Fatalf("seed blueprint step: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state) VALUES (?, 'github', ?, 'pr', 'active')`,
		"e_"+tag, "sky/repo#"+tag); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')`,
		"ev_"+tag, "e_"+tag, eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status) VALUES (?, ?, ?, ?, 'queued')`,
		taskID, "e_"+tag, eventType, "ev_"+tag); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID, blueprintID, delegateOnce(t, s, taskID, blueprintID)
}

func delegateOnce(t *testing.T, s *Server, taskID, blueprintID string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate",
		map[string]any{"hesitation_ms": 0, "blueprint_id": blueprintID})
	if rec.Code != http.StatusOK {
		t.Fatalf("delegate = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.ConversationID == "" {
		t.Fatalf("decode delegate response %s: %v", rec.Body.String(), err)
	}
	return body.ConversationID
}

func blueprintRunStatuses(t *testing.T, db *sql.DB, taskID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT status FROM blueprint_runs WHERE task_id = ? ORDER BY rowid`, taskID)
	if err != nil {
		t.Fatalf("read blueprint runs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			t.Fatalf("scan blueprint run: %v", err)
		}
		out = append(out, st)
	}
	return out
}

// Nothing holds the first run's step, so re-delegating settles its stop in
// the request and mints the next run on the first attempt.
func TestTaskDelegate_ReplacesAnUnheldRunInOneRequest(t *testing.T) {
	s := newTestServer(t)
	taskID, blueprintID, first := redelegateFixture(t, s, "redelegate-unheld")

	second := delegateOnce(t, s, taskID, blueprintID)
	if second == first {
		t.Fatal("the re-delegate answered with the first run's step")
	}
	if got := blueprintRunStatuses(t, s.db, taskID); len(got) != 2 ||
		got[0] != string(domain.BlueprintRunStatusCancelled) || got[1] != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint runs = %v, want [cancelled running]", got)
	}
	var status, reason string
	var intent sql.NullString
	if err := s.db.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, ''), stop_requested_at FROM conversations WHERE id = ?`, first,
	).Scan(&status, &reason, &intent); err != nil {
		t.Fatalf("read the first step: %v", err)
	}
	if status != string(domain.StatusOpen) || reason != string(domain.ParkReasonUserCancelled) || intent.Valid {
		t.Errorf("first step = (status %q, reason %q, intent %v), want (open, user_cancelled, settled)", status, reason, intent.String)
	}
}

// An executor holds the first run's step, so the re-delegate is refused
// before it writes anything: only the holder can stop what it is driving.
func TestTaskDelegate_RefusesAHeldRunWithoutTouchingIt(t *testing.T) {
	s := newTestServer(t)
	taskID, blueprintID, first := redelegateFixture(t, s, "redelegate-held")
	if _, err := s.db.Exec(`UPDATE conversations SET status = NULL WHERE id = ?`, first); err != nil {
		t.Fatalf("clear the stored status: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at, lease_expires_at)
		VALUES (?, ?, ?, 'test-engagement', 1, CURRENT_TIMESTAMP, strftime('%Y-%m-%d %H:%M:%f','now','+300.000 seconds'))
	`, uuid.NewString(), runmode.LocalDefaultOrgID, first); err != nil {
		t.Fatalf("mint a holder claim: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate",
		map[string]any{"hesitation_ms": 0, "blueprint_id": blueprintID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-delegate onto a held run = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Errors) != 1 || body.Errors[0].Reason != httpx.ReasonConflict {
		t.Errorf("error body = %s, want one CONFLICT (nothing landed, so not SPAWN_FAILED)", rec.Body.String())
	}

	var intent, ended sql.NullString
	if err := s.db.QueryRow(`SELECT stop_requested_at, ended_at FROM conversations WHERE id = ?`, first).Scan(&intent, &ended); err != nil {
		t.Fatalf("read the held step: %v", err)
	}
	if intent.Valid || ended.Valid {
		t.Errorf("held step after the refusal = (intent %v, ended %v), want neither written", intent.String, ended.String)
	}
	if got := blueprintRunStatuses(t, s.db, taskID); len(got) != 1 || got[0] != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint runs = %v, want the one run, still running", got)
	}
	var cancelRequested bool
	if err := s.db.QueryRow(`SELECT cancel_requested FROM blueprint_runs WHERE task_id = ?`, taskID).Scan(&cancelRequested); err != nil {
		t.Fatalf("read cancel_requested: %v", err)
	}
	if cancelRequested {
		t.Error("the refusal raised the run's cancel")
	}
}
