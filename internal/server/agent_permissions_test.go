package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// getPendingPermissions reads the endpoint under test.
func getPendingPermissions(t *testing.T, s *Server, conversationID string) (int, []domain.PendingPermissionDTO) {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/agent/conversations/"+conversationID+"/permissions", nil)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var out []domain.PendingPermissionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode pending permissions: %v (body %s)", err, rec.Body.String())
	}
	return rec.Code, out
}

// TestHandleAgentPermissions_ReconstructsAParkedPrompt is the endpoint doing
// the job the ticket exists for: a client that never saw the fire-once
// `permission_request` frame — a refreshed page, a second tab, a board loaded
// cold — asks and gets the live prompt back, with everything it needs to render
// and answer it.
func TestHandleAgentPermissions_ReconstructsAParkedPrompt(t *testing.T) {
	s := newTestServer(t)
	spawner := delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6")
	s.SetSpawner(spawner)
	conversationID := seedSteerConversation(t, s.db, "perms-read", "running")
	claimID := dbtest.SeedActiveClaim(t, s.db, conversationID, "exec-1", 0)

	got := make(chan agentproc.PermissionDecision, 1)
	h := spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, conversationID, claimID, delegate.AbsentAutoDeny{})
	go func() {
		got <- h(agentproc.PermissionRequest{
			ToolCallID: "toolu_1",
			ToolName:   "Bash",
			Input:      map[string]any{"command": "rm -rf ./build"},
			Title:      "Claude wants to run rm -rf ./build",
		})
	}()

	// The handler records then broadcasts, so the row lands slightly after the
	// goroutine starts — poll rather than assume.
	var pending []domain.PendingPermissionDTO
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var code int
		code, pending = getPendingPermissions(t, s, conversationID)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if len(pending) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending prompt, got %d: %+v", len(pending), pending)
	}
	p := pending[0]
	if p.ToolCallID != "toolu_1" || p.ToolName != "Bash" {
		t.Fatalf("prompt identity lost: %+v", p)
	}
	if cmd, _ := p.Input["command"].(string); cmd != "rm -rf ./build" {
		t.Fatalf("tool input lost — the user would be approving a category, not a call: %+v", p.Input)
	}
	if p.Title != "Claude wants to run rm -rf ./build" {
		t.Fatalf("title lost: %q", p.Title)
	}
	// The deadline is what's LEFT, so a prompt picked up mid-window doesn't get
	// a fresh full one on the client.
	if p.TimeoutMs <= 0 {
		t.Fatalf("timeout_ms = %d, want the remaining deadline", p.TimeoutMs)
	}

	// Answering it from this reconstructed view still unblocks the agent, and
	// clears it for every other surface.
	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/permissions/toolu_1",
		map[string]string{"behavior": "allow"})
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
	if resolvedBody.Status != "resolved" {
		t.Fatalf("status = %q, want resolved", resolvedBody.Status)
	}
	if resolvedBody.Permission == nil || resolvedBody.Permission.State != domain.PermissionStateAllowed {
		t.Fatalf("permission = %+v, want the allowed row this call settled", resolvedBody.Permission)
	}
	select {
	case d := <-got:
		if d.Behavior != "allow" {
			t.Fatalf("handler decision = %q, want allow", d.Behavior)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never received the decision")
	}
	if _, after := getPendingPermissions(t, s, conversationID); len(after) != 0 {
		t.Fatalf("an answered prompt must be gone for every other surface too: %+v", after)
	}
}

// TestHandleAgentPermissions_DeadlineSurvivesASuspend is the read after a
// system suspend. The prompt's wait runs on the monotonic clock, which stops
// while the machine sleeps; its stored expiry is a wall-clock projection,
// which a sleep puts in the past. That disagreement is the whole of what a
// suspend does to this path, so the test builds it directly: the row's
// expiry is moved behind now while the wait is untouched. A refreshed dock
// must get the wait's remaining time rather than 0, and the prompt must
// still be answerable.
func TestHandleAgentPermissions_DeadlineSurvivesASuspend(t *testing.T) {
	s := newTestServer(t)
	spawner := delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6")
	s.SetSpawner(spawner)
	conversationID := seedSteerConversation(t, s.db, "perms-suspend", "running")
	claimID := dbtest.SeedActiveClaim(t, s.db, conversationID, "exec-1", 0)

	got := make(chan agentproc.PermissionDecision, 1)
	h := spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, conversationID, claimID, delegate.AbsentAutoDeny{})
	go func() { got <- h(agentproc.PermissionRequest{ToolCallID: "toolu_slept", ToolName: "Bash"}) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, pending := getPendingPermissions(t, s, conversationID); len(pending) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The laptop slept for ten minutes: the wall clock moved on, the wait
	// did not.
	if _, err := s.db.Exec(`UPDATE conversation_permissions SET expires_at = ? WHERE tool_call_id = ?`,
		time.Now().UTC().Add(-10*time.Minute), "toolu_slept"); err != nil {
		t.Fatalf("age the stored expiry: %v", err)
	}

	code, pending := getPendingPermissions(t, s, conversationID)
	if code != http.StatusOK || len(pending) != 1 {
		t.Fatalf("status = %d, pending = %+v; want the one prompt", code, pending)
	}
	live := spawner.PermissionRemaining(runmode.LocalDefaultOrgID, conversationID)["toolu_slept"]
	if got := time.Duration(pending[0].TimeoutMs) * time.Millisecond; got <= 0 || got < live-5*time.Second || got > live+time.Second {
		t.Fatalf("timeout_ms = %v, want the wait's remaining time (~%v), not the lapsed stored expiry", got, live)
	}

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/permissions/toolu_slept",
		map[string]string{"behavior": "allow"})
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200 — the server is still waiting on this prompt", rec.Code)
	}
	select {
	case d := <-got:
		if d.Behavior != "allow" {
			t.Fatalf("handler decision = %q, want allow", d.Behavior)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never received the decision")
	}
}

// TestHandleAgentPermissions_StoredExpiryWhenNoWaitIsHeld: a pending row this
// process holds no wait for — every row a multi-mode control pod reads, were
// there any — is answered from its stored expiry, as before.
func TestHandleAgentPermissions_StoredExpiryWhenNoWaitIsHeld(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "perms-unheld", "running")
	claimID := dbtest.SeedActiveClaim(t, s.db, conversationID, "exec-1", 0)

	expires := time.Now().UTC().Add(time.Minute)
	if _, err := sqlitestore.New(s.db).Permissions.Create(context.Background(), runmode.LocalDefaultOrgID, domain.ConversationPermission{
		ConversationID: conversationID,
		ClaimID:        claimID,
		ToolCallID:     "toolu_unheld",
		ToolName:       "Bash",
		ExpiresAt:      &expires,
	}); err != nil {
		t.Fatalf("seed pending row: %v", err)
	}

	code, pending := getPendingPermissions(t, s, conversationID)
	if code != http.StatusOK || len(pending) != 1 {
		t.Fatalf("status = %d, pending = %+v; want the one prompt", code, pending)
	}
	if got := pending[0].TimeoutMs; got < 55_000 || got > 60_000 {
		t.Fatalf("timeout_ms = %d, want ~60000 from the stored expiry", got)
	}
}

// TestHandleAgentPermission_ResolvedWithNoRowStillAnswers200 pins the shape
// the resolve endpoint must never fork into two: a prompt raised with no
// claim (Create refuses the write — see PermissionStore.Create's doc)
// leaves no audit row anywhere, so `permission` is absent, but the decision
// still reached the agent and the response is still {"status":"resolved"},
// never a differently-typed body and never a failure.
func TestHandleAgentPermission_ResolvedWithNoRowStillAnswers200(t *testing.T) {
	s := newTestServer(t)
	spawner := delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6")
	s.SetSpawner(spawner)
	conversationID := seedSteerConversation(t, s.db, "perms-norow", "running")

	// No claim in scope: BrowserPermissionHandler still parks the agent, but
	// recordPermissionRequest's Create is refused (no claim_id), so nothing
	// is ever written to conversation_permissions for this tool call.
	got := make(chan agentproc.PermissionDecision, 1)
	h := spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, conversationID, "", delegate.AbsentAutoDeny{})
	go func() { got <- h(agentproc.PermissionRequest{ToolCallID: "toolu_norow", ToolName: "Bash"}) }()

	var rec = doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/permissions/toolu_norow",
		map[string]string{"behavior": "allow"})
	deadline := time.Now().Add(2 * time.Second)
	for rec.Code == http.StatusNotFound && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		rec = doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/permissions/toolu_norow",
			map[string]string{"behavior": "allow"})
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200 even with no row to show", rec.Code)
	}
	var resolvedBody struct {
		Status     string                         `json:"status"`
		Permission *domain.ConversationPermission `json:"permission"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resolvedBody); err != nil {
		t.Fatalf("decode resolve response: %v (body %s)", err, rec.Body.String())
	}
	if resolvedBody.Status != "resolved" {
		t.Fatalf("status = %q, want resolved", resolvedBody.Status)
	}
	if resolvedBody.Permission != nil {
		t.Fatalf("permission = %+v, want absent — nothing was ever recorded to show", resolvedBody.Permission)
	}
	select {
	case d := <-got:
		if d.Behavior != "allow" {
			t.Fatalf("handler decision = %q, want allow", d.Behavior)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never received the decision")
	}
}

// TestHandleAgentPermissions_EmptyForAQuietConversation: a conversation nobody is
// waiting on answers with an empty list, not a 404 — "nothing pending" and "no
// such conversation" are different answers and the board reads both.
func TestHandleAgentPermissions_EmptyForAQuietConversation(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))
	conversationID := seedSteerConversation(t, s.db, "perms-quiet", "running")

	code, pending := getPendingPermissions(t, s, conversationID)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no prompts, got %+v", pending)
	}
}

// TestHandleAgentPermissions_UnknownConversationNotFound pins the authorization gate:
// the pending set is only readable through a conversation the caller's org can
// see, so an unknown/invisible id is 404 rather than an empty list that would
// confirm nothing either way.
func TestHandleAgentPermissions_UnknownConversationNotFound(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6"))

	rec := doJSON(t, s, "GET", "/api/agent/conversations/"+uuid.New().String()+"/permissions", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
