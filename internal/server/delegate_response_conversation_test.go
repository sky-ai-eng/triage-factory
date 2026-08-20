package server

import (
	"encoding/json"
	"net/http"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestTaskDelegate_ConversationIDResolvesToAConversation pins what the
// response field promises. A delegation mints a blueprint_run and enqueues its
// first step; the id a client follows into /api/agent/conversations/{id} — and
// renders an optimistic run card from — has to be that step, because the
// blueprint_run id resolves against nothing there.
func TestTaskDelegate_ConversationIDResolvesToAConversation(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))

	const (
		eventType   = "github:pr:opened"
		promptID    = "p-delegate-resp"
		blueprintID = "bp-delegate-resp"
		taskID      = "00000000-0000-4000-8000-0000000008a1"
	)
	if _, err := s.db.Exec(
		`INSERT INTO prompts (id, name, body, source, creator_user_id) VALUES (?, 'Resp', 'do the thing', 'user', ?)`,
		promptID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if _, err := s.blueprints.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: blueprintID, Name: "Resp", Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id) VALUES (?, 0, ?)`,
		blueprintID, promptID); err != nil {
		t.Fatalf("seed blueprint step: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_dresp', 'github', 'sky/repo#dresp', 'pr', 'active')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_dresp', 'e_dresp', ?, '')`,
		eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		 VALUES (?, 'e_dresp', ?, 'ev_dresp', 'queued')`, taskID, eventType); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate", map[string]any{
		"hesitation_ms": 0,
		"blueprint_id":  blueprintID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("delegate = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ConversationID == "" {
		t.Fatal("conversation_id missing from a successful delegation")
	}

	var stepIndex int
	var blueprintRun string
	if err := s.db.QueryRow(
		`SELECT COALESCE(blueprint_step_index, -1), blueprint_run_id FROM conversations WHERE id = ?`,
		body.ConversationID).Scan(&stepIndex, &blueprintRun); err != nil {
		t.Fatalf("conversation_id %q does not resolve to a conversation: %v", body.ConversationID, err)
	}
	if stepIndex != 0 {
		t.Errorf("conversation_id names step %d, want the blueprint's first step", stepIndex)
	}
	var runCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE id = ? AND task_id = ?`,
		blueprintRun, taskID).Scan(&runCount); err != nil {
		t.Fatalf("count blueprint_runs: %v", err)
	}
	if runCount != 1 {
		t.Errorf("the named conversation belongs to no blueprint_run on this task (blueprint_run %q)", blueprintRun)
	}
}
