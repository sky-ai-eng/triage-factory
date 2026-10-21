package agenthost

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
)

// TestRelayRuntime_TaskOwnRepository_AnsweredByTheOrchestrator pins where the
// repo gate's task-repo arm is decided. The sidecar hosting the verbs is
// capless and holds no stores, and the relay envelope carries no identity —
// the orchestrator binds it from its own ConversationInfo — so a sidecar
// cannot name a repo as its task's. It has to ask, and the answer is resolved
// against the orchestrator's conversation → task → entity chain.
//
// The gate asks the way authorizeRepo does: the name it was handed is resolved
// to a registry row once (GetRepo, relayed too), and the task-repo arm is asked
// about that row's id.
func TestRelayRuntime_TaskOwnRepository_AnsweredByTheOrchestrator(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	ctx := context.Background()
	repoID := dbseed.GitHubEntityRepository(t, conn, info.OrgID, "https://github.com", "octo/repo#7")
	if _, err := conn.Exec(`INSERT INTO entities (id, source, source_id, kind, scope, repository_id) VALUES ('ent-1', 'github', 'octo/repo#7', 'pr', 'https://github.com', ?)`, repoID); err != nil {
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
	dbseed.GitHubRepository(t, conn, info.OrgID, "https://github.com", "octo", "elsewhere")

	rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)
	ownByName := func(owner, repo string) bool {
		t.Helper()
		row, err := rt.GetRepo(ctx, owner+"/"+repo)
		if err != nil {
			t.Fatalf("GetRepo(%s/%s): %v", owner, repo, err)
		}
		if row == nil {
			return false
		}
		own, err := rt.TaskOwnRepository(ctx, row.ID)
		if err != nil {
			t.Fatalf("TaskOwnRepository(%s/%s): %v", owner, repo, err)
		}
		return own
	}

	if !ownByName("Octo", "Repo") {
		t.Fatal("task's own repo, named in another casing, is not the task's")
	}
	if ownByName("octo", "elsewhere") {
		t.Fatal("another repo is the task's")
	}
	if own, err := rt.TaskOwnRepository(ctx, ""); err != nil || own {
		t.Fatalf("TaskOwnRepository(\"\") = (%v, %v), want (false, nil)", own, err)
	}

	// The task's repository is the row its entity references, so a rename that
	// rewrites no key moves the answer with the row: the new name is the
	// task's, and the old one, which the key still spells, names nothing.
	dbseed.RenameRepositoryOnly(t, conn, repoID, "octo", "renamed")
	if !ownByName("octo", "renamed") {
		t.Fatal("task's repo under its new name is not the task's")
	}
	if ownByName("octo", "repo") {
		t.Fatal("the name the task's key still spells is the task's")
	}

	// A task polled from a GitHub host other than the org's current one is about
	// a repository on that host. The same owner/repo on the current host, which
	// is what the asking gate reaches, is not the task's.
	gheRepoID := dbseed.GitHubRepository(t, conn, info.OrgID, "https://ghe.example.com", "octo", "renamed")
	if _, err := conn.Exec(`UPDATE entities SET scope = 'https://ghe.example.com', repository_id = ? WHERE id = 'ent-1'`, gheRepoID); err != nil {
		t.Fatalf("move entity to another host: %v", err)
	}
	if ownByName("octo", "renamed") {
		t.Fatal("task's repo is the task's on the current host, though the task was polled from another host")
	}
}
