package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleConversations_CurrentAction drives the conversation-list route over
// one conversation per display state, each carrying a tool call the composition
// would happily describe. Only the working one gets a line: every other state
// already says something honest about itself, and the newest tool call on a
// stopped conversation describes something that is no longer happening.
//
// The last case is the other half of the rule — a conversation that IS working
// but whose newest assistant turn reached for no tool. Absent, not a guess: the
// client renders the state label instead.
func TestHandleConversations_CurrentAction(t *testing.T) {
	s := newTestServer(t)
	store := sqlitestore.New(s.db)

	// seed mints a conversation in the given stored status and appends one
	// assistant message carrying calls. Returns (taskID, conversationID).
	seed := func(suffix, status string, calls []domain.ToolCall) (string, string) {
		t.Helper()
		conversationID := seedSteerConversation(t, s.db, suffix, status)
		if _, err := store.Conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "working", ToolCalls: calls,
		}); err != nil {
			t.Fatalf("InsertMessage(%s): %v", conversationID, err)
		}
		return fixtureUUID("t_" + suffix), conversationID
	}

	bash := []domain.ToolCall{{ID: "t1", Name: "bash", Input: map[string]any{
		"command": "go test ./internal/routing/...", "description": "Vetting the router",
	}}}

	workingTask, working := seed("ca_run", "running", bash)
	silentTask, silent := seed("ca_silent", "running", nil)
	openTask, open := seed("ca_open", "open", bash)
	doneTask, done := seed("ca_done", "completed", bash)
	failedTask, failed := seed("ca_failed", "failed", bash)
	// `queued` is derived, never stored: a conversation with no stored status
	// and nothing driving it displays as queued.
	queuedTask, queued := seed("ca_queued", "running", bash)
	execSQL(t, s.db, `UPDATE conversations SET status = NULL WHERE id = ?`, queued)
	// A working conversation whose newest call names an absolute path into its
	// worktree: the line arrives worktree-relative, the composer's strip
	// applied against the row's own worktree_path.
	const wtRoot = "/var/folders/kx/abc/T/triagefactory-runs/ca"
	strippedTask, stripped := seed("ca_strip", "running", []domain.ToolCall{{
		ID: "t1", Name: "Read", Input: map[string]any{"file_path": wtRoot + "/internal/server/agent.go"},
	}})
	execSQL(t, s.db, `UPDATE conversations SET worktree_path = ? WHERE id = ?`, wtRoot, stripped)

	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{
		"task_ids": []string{workingTask, silentTask, openTask, doneTask, failedTask, queuedTask, strippedTask},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs map[string][]map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}

	row := func(taskID, conversationID string) map[string]any {
		t.Helper()
		for _, r := range resp.Runs[taskID] {
			if r["ID"] == conversationID {
				return r
			}
		}
		t.Fatalf("conversation %s missing from runs[%s]", conversationID, taskID)
		return nil
	}

	if got := row(workingTask, working); got["Status"] != domain.StatusRunning {
		t.Fatalf("working conversation status = %v, want %q", got["Status"], domain.StatusRunning)
	} else if got["current_action"] != "Vetting the router" {
		t.Errorf("current_action = %v, want the authored bash summary", got["current_action"])
	}

	if got := row(strippedTask, stripped); got["current_action"] != "Reading internal/server/agent.go" {
		t.Errorf("current_action = %v, want the worktree-relative path", got["current_action"])
	}

	for _, tc := range []struct {
		what                   string
		taskID, conversationID string
		wantStatus             string
	}{
		{"a working conversation whose newest turn called no tool", silentTask, silent, domain.StatusRunning},
		{"a parked conversation", openTask, open, domain.StatusOpen},
		{"a completed conversation", doneTask, done, domain.StatusCompleted},
		{"a failed conversation", failedTask, failed, domain.StatusFailed},
		{"a queued conversation", queuedTask, queued, domain.StatusQueued},
	} {
		got := row(tc.taskID, tc.conversationID)
		if got["Status"] != tc.wantStatus {
			t.Errorf("%s: status = %v, want %q (fixture is not in the state it claims)", tc.what, got["Status"], tc.wantStatus)
		}
		if v, ok := got["current_action"]; ok {
			t.Errorf("%s: current_action = %v, want the field omitted", tc.what, v)
		}
	}
}
