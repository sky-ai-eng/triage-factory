package server

import (
	"database/sql"
	"net/http"
	"strings"
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

// seedSteerConversation installs the entity → event → prompt → task → blueprint_run → run
// chain a steering endpoint needs, on the local-default org/team, and returns
// the run id. status sets the run's lifecycle state.
//
// The run is step 0 of its blueprint, which is where the blueprint's
// current_step_index sits — the shape EnqueueConversation produces, and the one the
// drivability gate reads. A row that named no step would be undrivable, so a
// fixture that left it NULL would silently make every steering test a test of
// the refusal path.
func seedSteerConversation(t *testing.T, database *sql.DB, suffix, status string) string {
	t.Helper()
	const eventType = "github:pr:ci_check_failed"
	e, ev, p, tk, rn := fixtureUUID("e_"+suffix), fixtureUUID("ev_"+suffix), fixtureUUID("p_"+suffix), fixtureUUID("t_"+suffix), fixtureUUID("r_"+suffix)
	execSQL(t, database, `INSERT INTO entities (id, source, source_id, kind, state) VALUES (?, 'github', ?, 'pr', 'active')`, e, "owner/repo#"+suffix)
	execSQL(t, database, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')`, ev, e, eventType)
	execSQL(t, database, `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'P', 'b', ?, ?)`, p, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, database, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id) VALUES (?, ?, ?, ?)`, tk, e, eventType, ev)
	brID := seedBlueprintRunSQLite(t, database, tk)
	execSQL(t, database, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index) VALUES (?, ?, ?, ?, 'manual', ?, 0)`, rn, tk, p, status, brID)
	return rn
}

// messageRowCount counts the transcript rows a conversation holds.
func messageRowCount(t *testing.T, database *sql.DB, conversationID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id=?`, conversationID).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// TestHandleMessage_TerminalConflictRecordsNothing drives the message endpoint
// against a failed run: the run is visible (so the answer is a 409, not a 404)
// and the refused send leaves no transcript row — the previous record-first
// ordering durably recorded and broadcast the message before routing could
// refuse it, so the sender saw an error toast while their words scrolled onto
// every watcher's transcript as the run's latest activity.
//
// `failed` rather than `completed` because that is what "terminal" means to
// the steering gate now: a conversation that concluded takes a follow-up, and
// only one whose infrastructure died has nothing left to say a message to.
func TestHandleMessage_TerminalConflictRecordsNothing(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "msg", "failed")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/message", map[string]string{"text": "pick this back up"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (a terminal run is not steerable)", rec.Code)
	}
	if n := messageRowCount(t, s.db, conversationID); n != 0 {
		t.Errorf("messages recorded = %d, want 0 — a refused send must leave no transcript row", n)
	}
}

// TestHandleMessage_OrphanedRunningConflictRecordsNothing pins the window that
// made the old ordering user-visible: a run whose row still says `running` but
// with no live process registered anywhere (server restarted mid-run, or the
// process died before a status write). The composer keys off DB status, so it
// looks live and every send lands here. The send must 409 with nothing
// recorded — not paint the message onto the transcript it will never reach.
func TestHandleMessage_OrphanedRunningConflictRecordsNothing(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "orphan", "running")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/message", map[string]string{"text": "how is it going?"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (running with no live process is a conflict)", rec.Code)
	}
	if n := messageRowCount(t, s.db, conversationID); n != 0 {
		t.Errorf("messages recorded = %d, want 0 — a refused send must leave no transcript row", n)
	}
}

// TestHandleMessage_ParkedSDKRecordsExactlyOnce: a follow-up accepted onto a
// parked SDK run lands as exactly one transcript row — the undelivered queue
// row the resume dispatch will carry into the prompt. When the endpoint also
// wrote its own optimistic row, every accepted SDK follow-up appeared twice in
// the transcript (the endpoint's delivered row plus the queue's pending one).
func TestHandleMessage_ParkedSDKRecordsExactlyOnce(t *testing.T) {
	s := newTestServer(t)
	withEmptyBlobStore(t, s)
	conversationID := seedSteerConversation(t, s.db, "once", "open")
	execSQL(t, s.db, `UPDATE conversations SET sdk_session_id='sess-once', worktree_path=?, model='m' WHERE id=?`, t.TempDir(), conversationID)

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/message", map[string]string{"text": "pick this back up"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var role, content string
	var delivered bool
	if err := s.db.QueryRow(`SELECT role, delivered, content FROM messages WHERE conversation_id=?`, conversationID).Scan(&role, &delivered, &content); err != nil {
		t.Fatalf("read recorded message: %v", err)
	}
	if role != "user" || delivered || content != "pick this back up" {
		t.Errorf("recorded message = {role:%q delivered:%v content:%q}, want the one undelivered user row", role, delivered, content)
	}
	if n := messageRowCount(t, s.db, conversationID); n != 1 {
		t.Errorf("messages recorded = %d, want exactly 1 — the queue row is the transcript record", n)
	}
}

// TestHandleMessage_UnknownRunNotFound: a message to a run not visible to
// the caller's org is 404 (the authz gate), before anything is recorded.
func TestHandleMessage_UnknownRunNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+fixtureUUID("r_absent")+"/message", map[string]string{"text": "hello"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unknown run)", rec.Code)
	}
}

// TestHandleMessage_EmptyTextRejected: a blank message is 400 before the
// run is even looked up. The path id must still be well-formed — the uuid
// guard runs ahead of body validation, so a junk id would answer 404 first.
func TestHandleMessage_EmptyTextRejected(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+fixtureUUID("r_blank")+"/message", map[string]string{"text": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty text", rec.Code)
	}
}

// TestHandleAgentStop_ParksAndLeavesBlueprintRunning is the endpoint's whole
// contract: a run with no live process still stops — it parks — and the
// blueprint behind it is left running and un-cancelled, which is what keeps the
// parked conversation resumable.
//
// The former /cancel took the blueprint terminal with it and /interrupt did
// not, so `open` meant "resumable" or "dead forever" depending on which button
// the user pressed. Both paths are gone; this pins the one that replaced them.
func TestHandleAgentStop_ParksAndLeavesBlueprintRunning(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "stop", "running")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/stop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a run with no live process still stops — it parks)", rec.Code)
	}

	var runStatus, stopReason, bpStatus string
	var cancelRequested bool
	if err := s.db.QueryRow(`
		SELECT c.status, COALESCE(c.park_reason, ''), br.status, br.cancel_requested
		  FROM conversations c JOIN blueprint_runs br ON br.id = c.blueprint_run_id
		 WHERE c.id = ?`, conversationID).Scan(&runStatus, &stopReason, &bpStatus, &cancelRequested); err != nil {
		t.Fatalf("read post-stop state: %v", err)
	}
	if runStatus != "open" || stopReason != "user_cancelled" {
		t.Errorf("conversation = (%q, %q), want (open, user_cancelled)", runStatus, stopReason)
	}
	if bpStatus != "running" || cancelRequested {
		t.Errorf("blueprint = (%q, cancel_requested=%v), want (running, false) — a stop freezes the plan, it does not finalize it", bpStatus, cancelRequested)
	}
}

// TestHandleAgentStop_RetiredPathsAreGone: /cancel and /interrupt were removed
// outright rather than aliased. Two addresses for one gesture is how they
// drifted into two meanings of `open`, and nothing consumes the API outside
// this repo's own frontend.
func TestHandleAgentStop_RetiredPathsAreGone(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "retired", "running")

	for _, path := range []string{"cancel", "interrupt"} {
		rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/"+path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST /%s status = %d, want 404 (route retired)", path, rec.Code)
		}
	}
}

// TestHandleAgentStop_UnknownRunNotFound: stopping a run not visible to
// the caller's org is 404 — the authz gate keeps a known run id from reaching
// another tenant's process — and the body is the generic "run not found", not
// the spawner's error text, so the response can't be used to probe which ids
// exist.
func TestHandleAgentStop_UnknownRunNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+fixtureUUID("r_absent")+"/stop", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unknown run)", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, fixtureUUID("r_absent")) {
		t.Errorf("404 body %q echoes the requested id back", body)
	}
}

// TestHandleAgentStop_InternalFailureIsNot404 pins the classification: only
// "there is nothing to stop" is a 404. A read or park that fails on the way to
// stopping a run that really was active is a server fault — reporting it as a
// missing run tells the user their work is gone while the agent keeps running,
// and echoing the raw error leaks DB internals to the client.
//
// A closed handle is the cheapest real infrastructure failure to stage; the
// preflight read is the first thing to hit it.
func TestHandleAgentStop_InternalFailureIsNot404(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "dberr", "running")
	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/stop", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a failed stop is not a missing run)", rec.Code)
	}
}

// TestHandleAgentPermission_NotPendingNotFound: with a visible run but no
// pending prompt for the tool call id, the broker miss is a 404. The run is seeded
// so the request clears the run-authz gate and actually reaches the broker
// (otherwise this would test the run-not-found 404 instead).
func TestHandleAgentPermission_NotPendingNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "noperm", "running")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/permissions/req-ghost", map[string]string{"behavior": "allow"})
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

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+fixtureUUID("r_absent")+"/permissions/req-1", map[string]string{"behavior": "allow"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unknown run)", rec.Code)
	}
}

// TestHandleAgentPermission_InvalidBehaviorRejected: behavior must be allow or
// deny — anything else is 400 before the broker is consulted.
func TestHandleAgentPermission_InvalidBehaviorRejected(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+fixtureUUID("r1")+"/permissions/req-x", map[string]string{"behavior": "maybe"})
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
	conversationID := seedSteerConversation(t, s.db, "perm", "running")

	// Park a permission prompt for the run: the broker registers synchronously,
	// then the handler blocks until resolved (or it times out).
	got := make(chan agentproc.PermissionDecision, 1)
	h := spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, conversationID, "", delegate.AbsentAutoDeny{})
	go func() { got <- h(agentproc.PermissionRequest{ToolCallID: "req-1", ToolName: "Bash"}) }()

	// Registration races the POST, so retry until the broker has the entry (404
	// until then), bounded.
	var code int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		code = doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/permissions/req-1", map[string]string{"behavior": "allow"}).Code
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
