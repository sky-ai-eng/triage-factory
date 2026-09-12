package agenthost

import (
	"context"
	"testing"
)

// TestRelayRuntime_TaskOwnRepo_AnsweredByTheOrchestrator pins where the repo
// gate's task-repo arm is decided. The sidecar hosting the verbs is capless and
// holds no stores, and the relay envelope carries no identity — the
// orchestrator binds it from its own ConversationInfo — so a sidecar cannot
// name a repo as its task's. It has to ask, and the answer is resolved against
// the orchestrator's conversation → task → entity chain.
func TestRelayRuntime_TaskOwnRepo_AnsweredByTheOrchestrator(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO entities (id, source, source_id, kind) VALUES ('ent-1', 'github', 'octo/repo#7', 'pr')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO events (id, event_type, entity_id) VALUES ('evt-1', 'github:pr:ci_check_failed', 'ent-1')`); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO tasks (id, entity_id, event_type, primary_event_id) VALUES ('task-1', 'ent-1', 'github:pr:ci_check_failed', 'evt-1')`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := conn.Exec(`UPDATE conversations SET task_id = 'task-1' WHERE id = ?`, info.ConversationID); err != nil {
		t.Fatalf("link conversation to task: %v", err)
	}

	rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)

	own, err := rt.TaskOwnRepo(ctx, "Octo", "Repo")
	if err != nil || !own {
		t.Fatalf("TaskOwnRepo(task's own repo) = (%v, %v), want (true, nil) — and case-insensitively", own, err)
	}
	other, err := rt.TaskOwnRepo(ctx, "octo", "elsewhere")
	if err != nil || other {
		t.Fatalf("TaskOwnRepo(another repo) = (%v, %v), want (false, nil)", other, err)
	}
}
