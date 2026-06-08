package server

import (
	"database/sql"
	"net/http"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// execSQL runs a statement against the test DB, failing the test on error.
func execSQL(t *testing.T, database *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// TestHandleAgentMessage_RecordsThenConflictsOnTerminal drives the message
// endpoint end-to-end against a terminal run: the handler records the user's
// message (recording precedes routing), then SendMessage reports the run not
// steerable, which maps to 409. Asserts both the 409 and that the message
// landed in the transcript.
func TestHandleAgentMessage_RecordsThenConflictsOnTerminal(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6", ""))

	const eventType = "github:pr:ci_check_failed"
	execSQL(t, s.db, `INSERT INTO entities (id, source, source_id, kind, state) VALUES ('e_msg','github','owner/repo#msg','pr','active')`)
	execSQL(t, s.db, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_msg','e_msg',?, '')`, eventType)
	execSQL(t, s.db, `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_msg','P','b',?,?)`, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, s.db, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id) VALUES ('t_msg','e_msg',?, 'ev_msg')`, eventType)
	brID := seedBlueprintRunSQLite(t, s.db, "t_msg")
	execSQL(t, s.db, `INSERT INTO runs (id, task_id, prompt_id, status, trigger_type, blueprint_run_id) VALUES ('r_msg','t_msg','p_msg','completed','manual',?)`, brID)

	rec := doJSON(t, s, "POST", "/api/agent/runs/r_msg/message", map[string]string{"text": "pick this back up"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (a terminal run is not steerable)", rec.Code)
	}

	var role, content string
	if err := s.db.QueryRow(`SELECT role, content FROM run_messages WHERE run_id='r_msg'`).Scan(&role, &content); err != nil {
		t.Fatalf("read recorded message: %v", err)
	}
	if role != "user" || content != "pick this back up" {
		t.Errorf("recorded message = {role:%q content:%q}, want {user, pick this back up}", role, content)
	}
}

// TestHandleAgentMessage_EmptyTextRejected: the endpoint rejects a blank
// message before touching the run.
func TestHandleAgentMessage_EmptyTextRejected(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6", ""))

	rec := doJSON(t, s, "POST", "/api/agent/runs/whatever/message", map[string]string{"text": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty text", rec.Code)
	}
}

// TestHandleAgentInterrupt_NoLiveProcessConflict: interrupting a run with no
// live process is a 409 (nothing to interrupt). No run row is needed — Interrupt
// resolves purely against the in-memory process registry.
func TestHandleAgentInterrupt_NoLiveProcessConflict(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6", ""))

	rec := doJSON(t, s, "POST", "/api/agent/runs/r_absent/interrupt", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (no live process to interrupt)", rec.Code)
	}
}

// TestHandleAgentPermission_NotPendingNotFound: answering a request that isn't
// pending (already resolved / timed out / never existed) is 404. The broker is
// in-memory, so no run row is needed.
func TestHandleAgentPermission_NotPendingNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6", ""))

	rec := doJSON(t, s, "POST", "/api/agent/runs/r1/permissions/req-ghost", map[string]string{"behavior": "allow"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no pending permission request)", rec.Code)
	}
}

// TestHandleAgentPermission_InvalidBehaviorRejected: behavior must be allow or
// deny — anything else is 400 before the broker is consulted.
func TestHandleAgentPermission_InvalidBehaviorRejected(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6", ""))

	rec := doJSON(t, s, "POST", "/api/agent/runs/r1/permissions/req-x", map[string]string{"behavior": "maybe"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid behavior)", rec.Code)
	}
}
