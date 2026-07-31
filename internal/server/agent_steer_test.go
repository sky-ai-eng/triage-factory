package server

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
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

// seedSteerRun installs the entity → event → prompt → task → blueprint_run → run
// chain a steering endpoint needs, on the local-default org/team, and returns
// the run id. status sets the run's lifecycle state.
func seedSteerRun(t *testing.T, database *sql.DB, suffix, status string) string {
	t.Helper()
	const eventType = "github:pr:ci_check_failed"
	e, ev, p, tk, rn := "e_"+suffix, "ev_"+suffix, "p_"+suffix, "t_"+suffix, "r_"+suffix
	execSQL(t, database, `INSERT INTO entities (id, source, source_id, kind, state) VALUES (?, 'github', ?, 'pr', 'active')`, e, "owner/repo#"+suffix)
	execSQL(t, database, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')`, ev, e, eventType)
	execSQL(t, database, `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'P', 'b', ?, ?)`, p, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, database, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id) VALUES (?, ?, ?, ?)`, tk, e, eventType, ev)
	brID := seedBlueprintRunSQLite(t, database, tk)
	execSQL(t, database, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id) VALUES (?, ?, ?, ?, 'manual', ?)`, rn, tk, p, status, brID)
	return rn
}

// TestHandleMessage_RecordsThenConflictsOnTerminal drives the message
// endpoint against a terminal run: the run is visible (so it's recorded, not a
// 404), the user message lands in the transcript, then SendMessage reports it
// not steerable → 409. Asserts the 409 and the recorded row (role/subtype/body).
func TestHandleMessage_RecordsThenConflictsOnTerminal(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	runID := seedSteerRun(t, s.db, "msg", "completed")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+runID+"/message", map[string]string{"text": "pick this back up"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (a terminal run is not steerable)", rec.Code)
	}

	var role, subtype, content string
	if err := s.db.QueryRow(`SELECT role, subtype, content FROM messages WHERE conversation_id=?`, runID).Scan(&role, &subtype, &content); err != nil {
		t.Fatalf("read recorded message: %v", err)
	}
	if role != "user" || subtype != "" || content != "pick this back up" {
		t.Errorf("recorded message = {role:%q subtype:%q content:%q}, want {user, text, pick this back up}", role, subtype, content)
	}
}

// TestHandleMessage_UnknownRunNotFound: a message to a run not visible to
// the caller's org is 404 (the authz gate), before anything is recorded.
func TestHandleMessage_UnknownRunNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/r_absent/message", map[string]string{"text": "hello"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unknown run)", rec.Code)
	}
}

// TestHandleMessage_EmptyTextRejected: a blank message is 400 before the
// run is even looked up.
func TestHandleMessage_EmptyTextRejected(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/whatever/message", map[string]string{"text": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty text", rec.Code)
	}
}

// TestHandleAgentInterrupt_NoLiveProcessConflict: interrupting an existing run
// that has no live process is 409 (nothing to interrupt).
func TestHandleAgentInterrupt_NoLiveProcessConflict(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	runID := seedSteerRun(t, s.db, "int", "running")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+runID+"/interrupt", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (run exists but has no live process)", rec.Code)
	}
}

// TestHandleAgentInterrupt_UnknownRunNotFound: interrupting a run not visible to
// the caller's org is 404 — the authz gate keeps a known run id from reaching
// another tenant's process.
func TestHandleAgentInterrupt_UnknownRunNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/r_absent/interrupt", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unknown run)", rec.Code)
	}
}

// TestHandleAgentPermission_NotPendingNotFound: with a visible run but no
// pending prompt for the tool call id, the broker miss is a 404. The run is seeded
// so the request clears the run-authz gate and actually reaches the broker
// (otherwise this would test the run-not-found 404 instead).
func TestHandleAgentPermission_NotPendingNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	runID := seedSteerRun(t, s.db, "noperm", "running")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+runID+"/permissions/req-ghost", map[string]string{"behavior": "allow"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no pending permission request)", rec.Code)
	}
}

// TestHandleAgentPermission_UnknownRunNotFound: resolving a permission on a run
// not visible to the caller's org is 404 before the broker is consulted —
// mirrors the message/interrupt authz gate.
func TestHandleAgentPermission_UnknownRunNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/r_absent/permissions/req-1", map[string]string{"behavior": "allow"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unknown run)", rec.Code)
	}
}

// TestHandleAgentPermission_InvalidBehaviorRejected: behavior must be allow or
// deny — anything else is 400 before the broker is consulted.
func TestHandleAgentPermission_InvalidBehaviorRejected(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/r1/permissions/req-x", map[string]string{"behavior": "maybe"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid behavior)", rec.Code)
	}
}

// TestHandleAgentPermission_ResolvesLiveRequest exercises the endpoint → broker
// wiring on the success path: a parked permission handler is resolved by a POST,
// the parked goroutine receives the decision, and the endpoint returns 200.
func TestHandleAgentPermission_ResolvesLiveRequest(t *testing.T) {
	s := newTestServer(t)
	spawner := delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6")
	s.SetSpawner(spawner)
	runID := seedSteerRun(t, s.db, "perm", "running")

	// Park a permission prompt for the run: the broker registers synchronously,
	// then the handler blocks until resolved (or it times out).
	got := make(chan agentproc.PermissionDecision, 1)
	h := spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, runID, "", delegate.AbsentAutoDeny{})
	go func() { got <- h(agentproc.PermissionRequest{ToolCallID: "req-1", ToolName: "Bash"}) }()

	// Registration races the POST, so retry until the broker has the entry (404
	// until then), bounded.
	var code int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		code = doJSON(t, s, "POST", "/api/agent/conversations/"+runID+"/permissions/req-1", map[string]string{"behavior": "allow"}).Code
		if code != http.StatusNotFound {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", code)
	}
	select {
	case d := <-got:
		if d.Behavior != "allow" {
			t.Errorf("handler decision = %q, want allow", d.Behavior)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not receive the decision after resolve")
	}
}
