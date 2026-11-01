package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// execSQL runs a statement against the test DB, failing the test on error.
func execSQL(t *testing.T, database *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// seedSteerConversation installs the entity → event → prompt → task →
// blueprint_run → conversation chain on the local-default org/team and returns
// the conversation id; status sets its lifecycle state. The conversation is
// step 0, where the blueprint's current_step_index sits: a row naming no step
// is undrivable, which would turn every steering test into a refusal test.
func seedSteerConversation(t *testing.T, database *sql.DB, suffix, status string) string {
	t.Helper()
	const eventType = "github:pr:ci_check_failed"
	e, ev, p, tk, rn := fixtureUUID("e_"+suffix), fixtureUUID("ev_"+suffix), fixtureUUID("p_"+suffix), fixtureUUID("t_"+suffix), fixtureUUID("r_"+suffix)
	repoID := dbseed.GitHubEntityRepository(t, database, runmode.LocalDefaultOrgID, "https://github.com", "owner/repo#"+suffix)
	execSQL(t, database, `INSERT INTO entities (id, source, source_id, kind, state, scope, repository_id) VALUES (?, 'github', ?, 'pr', 'active', 'https://github.com', ?)`, e, "owner/repo#"+suffix, repoID)
	execSQL(t, database, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')`, ev, e, eventType)
	execSQL(t, database, `INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES (?, 'P', 'b', ?, ?)`, p, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	execSQL(t, database, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id) VALUES (?, ?, ?, ?)`, tk, e, eventType, ev)
	brID := seedBlueprintRunSQLite(t, database, tk)
	stored, concluded := dbtest.SeedStatus(status)
	var completedAt any
	if concluded {
		completedAt = time.Now().UTC()
	}
	execSQL(t, database, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index, completed_at) VALUES (?, ?, ?, ?, 'manual', ?, 0, ?)`, rn, tk, p, stored, brID, completedAt)
	return rn
}

// storedConcluded reports whether a conversation is parked with its step's
// verdict recorded: stored `open` with completed_at stamped.
func storedConcluded(t *testing.T, database *sql.DB, conversationID string) bool {
	t.Helper()
	var concluded bool
	if err := database.QueryRow(
		`SELECT status = 'open' AND completed_at IS NOT NULL FROM conversations WHERE id = ?`, conversationID,
	).Scan(&concluded); err != nil {
		t.Fatalf("read conclusion for %s: %v", conversationID, err)
	}
	return concluded
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

// TestHandleMessage_TerminalConflictRecordsNothing: a message to a failed
// conversation (the terminal state for steering; a concluded one takes a
// follow-up) is a 409 and leaves no transcript row.
func TestHandleMessage_TerminalConflictRecordsNothing(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "msg", "failed")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/message", map[string]string{"text": "pick this back up"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (a terminal conversation is not steerable)", rec.Code)
	}
	if n := messageRowCount(t, s.db, conversationID); n != 0 {
		t.Errorf("messages recorded = %d, want 0 — a refused send must leave no transcript row", n)
	}
}

// TestHandleMessage_OrphanedRunningConflictRecordsNothing: a conversation whose
// row says `running` with no live process anywhere (a restart mid-engagement)
// looks live to the composer, and a send to it is a 409 with nothing recorded.
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
// parked SDK conversation lands as exactly one transcript row, the undelivered
// queue row the resume dispatch carries into the prompt.
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

// TestSteerRoutes_UnknownConversationNotFound: a message, a stop or a permission
// answer aimed at a conversation the caller's org cannot see is a 404 before the
// spawner or broker is reached, with a body that does not echo the id.
func TestSteerRoutes_UnknownConversationNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	absent := fixtureUUID("r_absent")

	for name, tc := range map[string]struct {
		path string
		body any
	}{
		"message":    {"/message", map[string]string{"text": "hello"}},
		"stop":       {"/stop", nil},
		"permission": {"/permissions/req-1", map[string]string{"behavior": "allow"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, "POST", "/api/agent/conversations/"+absent+tc.path, tc.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (unknown conversation)", rec.Code)
			}
			if body := rec.Body.String(); strings.Contains(body, absent) {
				t.Errorf("404 body %q echoes the requested id back", body)
			}
		})
	}
}

// TestHandleMessage_EmptyTextRejected: a blank message is 400 before the
// conversation is even looked up. The path id must still be well-formed — the uuid
// guard runs ahead of body validation, so a junk id would answer 404 first.
func TestHandleMessage_EmptyTextRejected(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+fixtureUUID("r_blank")+"/message", map[string]string{"text": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty text", rec.Code)
	}
}

// TestHandleAgentStop_RecordsTheRequestAndLeavesBlueprintRunning: the POST
// records a stop intent and answers with it, status untouched; the settlement
// parks the unheld conversation; the blueprint stays running, so it can resume.
func TestHandleAgentStop_RecordsTheRequestAndLeavesBlueprintRunning(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "stop", "")
	execSQL(t, s.db, `UPDATE conversations SET status = NULL WHERE id = ?`, conversationID)

	stop := func() map[string]any {
		t.Helper()
		rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/stop", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return body
	}
	first := stop()
	if first["stop_requested_at"] == nil {
		t.Errorf("response carries no stop_requested_at: %v", first)
	}
	if got := first["Status"]; got != "queued" {
		t.Errorf("response Status = %v, want queued — the request writes no status", got)
	}
	// A second POST while the first is pending is the same answer.
	if second := stop(); second["stop_requested_at"] != first["stop_requested_at"] {
		t.Errorf("second stop_requested_at = %v, want the first's %v", second["stop_requested_at"], first["stop_requested_at"])
	}

	if _, err := sqlitestore.New(s.db).ConversationQueue.SettleUnclaimedStopsSystem(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	var convStatus, stopReason, bpStatus string
	var cancelRequested bool
	if err := s.db.QueryRow(`
		SELECT c.status, COALESCE(c.park_reason, ''), br.status, br.cancel_requested
		  FROM conversations c JOIN blueprint_runs br ON br.id = c.blueprint_run_id
		 WHERE c.id = ?`, conversationID).Scan(&convStatus, &stopReason, &bpStatus, &cancelRequested); err != nil {
		t.Fatalf("read post-stop state: %v", err)
	}
	if convStatus != "open" || stopReason != "user_cancelled" {
		t.Errorf("conversation = (%q, %q), want (open, user_cancelled)", convStatus, stopReason)
	}
	if bpStatus != "running" || cancelRequested {
		t.Errorf("blueprint = (%q, cancel_requested=%v), want (running, false) — a stop freezes the plan, it does not finalize it", bpStatus, cancelRequested)
	}
}

// TestHandleAgentStop_TerminalConflicts: a concluded conversation has nothing
// to stop, and the answer is the 409 its sibling /message gives.
func TestHandleAgentStop_TerminalConflicts(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "stop-done", dbtest.SeedConcluded)

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/stop", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleBlueprintRunCancel_StepCarriesTheIntent: cancelling a run records
// a stop intent on its step conversation and writes no status of its own; the
// settlement that follows is what cancels the run.
func TestHandleBlueprintRunCancel_StepCarriesTheIntent(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "bp-cancel", "")
	// The route addresses a run by uuid, so the fixture's run is re-seated
	// under one: a copy, the original settled, the step moved across.
	var oldID string
	if err := s.db.QueryRow(`SELECT blueprint_run_id FROM conversations WHERE id = ?`, conversationID).Scan(&oldID); err != nil {
		t.Fatalf("read blueprint run: %v", err)
	}
	brID := fixtureUUID("br_bp-cancel")
	execSQL(t, s.db, `UPDATE blueprint_runs SET status = 'completed' WHERE id = ?`, oldID)
	execSQL(t, s.db, `
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, step_plan, worktree_path, creator_user_id, started_at)
		SELECT ?, blueprint_id, task_id, trigger_type, 'running', step_plan, worktree_path, creator_user_id, started_at
		  FROM blueprint_runs WHERE id = ?`, brID, oldID)
	execSQL(t, s.db, `UPDATE conversations SET status = NULL, blueprint_run_id = ? WHERE id = ?`, brID, conversationID)

	rec := doJSON(t, s, "POST", "/api/blueprint-runs/"+brID+"/cancel", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var intent bool
	var convStatus sql.NullString
	var bpStatus string
	if err := s.db.QueryRow(`
		SELECT c.stop_requested_at IS NOT NULL, c.status, br.status
		  FROM conversations c JOIN blueprint_runs br ON br.id = c.blueprint_run_id
		 WHERE c.id = ?`, conversationID).Scan(&intent, &convStatus, &bpStatus); err != nil {
		t.Fatalf("read post-cancel state: %v", err)
	}
	if !intent || convStatus.Valid || bpStatus != "running" {
		t.Errorf("after the cancel = (intent %v, status %v, run %q), want (set, none, running)", intent, convStatus, bpStatus)
	}
}

// TestHandleAgentStop_RetiredPathsAreGone: /stop is the one address for the
// gesture, so /cancel and /interrupt are not routes.
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

// TestHandleAgentStop_InternalFailureIsNot404: only "nothing to stop" is a 404;
// a failed read on the way to a stop is a 500, not a report that the work is
// gone. A closed handle is the cheapest real failure to stage.
func TestHandleAgentStop_InternalFailureIsNot404(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "dberr", "running")
	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/stop", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a failed stop is not a missing conversation)", rec.Code)
	}
}

// TestHandleAgentPermission_NotPendingNotFound: a visible conversation with no
// pending prompt for the tool call id is a 404 from the broker, past the
// conversation gate the seeded row clears.
func TestHandleAgentPermission_NotPendingNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "noperm", "running")

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/permissions/req-ghost", map[string]string{"behavior": "allow"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no pending permission request)", rec.Code)
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

// TestHandleAgentPermission_ResolvesLiveRequest: a prompt parked with no claim
// is resolved by the POST and the agent receives the decision. No claim means
// no audit row, so the body is {"status":"resolved"} with `permission` absent.
func TestHandleAgentPermission_ResolvesLiveRequest(t *testing.T) {
	s := newTestServer(t)
	spawner := delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6")
	s.SetSpawner(spawner)
	conversationID := seedSteerConversation(t, s.db, "perm", "running")

	got := make(chan agentproc.PermissionDecision, 1)
	h := spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, conversationID, "", delegate.AbsentAutoDeny{})
	go func() { got <- h(agentproc.PermissionRequest{ToolCallID: "req-1", ToolName: "Bash"}) }()

	// Registration races the POST, so retry until the broker has the entry (404
	// until then), bounded.
	var rec *httptest.ResponseRecorder
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec = doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/permissions/req-1", map[string]string{"behavior": "allow"})
		if rec.Code != http.StatusNotFound || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", rec.Code)
	}
	var resolvedBody struct {
		Status     string                         `json:"status"`
		Permission *domain.ConversationPermission `json:"permission"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resolvedBody); err != nil {
		t.Fatalf("decode resolve response: %v (body %s)", err, rec.Body.String())
	}
	if resolvedBody.Status != "resolved" || resolvedBody.Permission != nil {
		t.Errorf("body = %+v, want status resolved and no permission (nothing was recorded)", resolvedBody)
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
