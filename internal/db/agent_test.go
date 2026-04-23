package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestActiveRunIDsForTask verifies the terminal-state filter matches the one
// used by HasActiveRunForTask — the close cascade depends on this query
// returning exactly the runs that should be cancelled when a task closes.
func TestActiveRunIDsForTask(t *testing.T) {
	database := newTestDB(t)

	entity, _, err := FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test", "https://example.com/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := FindOrCreateTask(database, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := CreatePrompt(database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("create prompt: %v", err)
	}

	// Seed runs in a mix of states. Non-terminal ones should appear in the
	// returned list; terminal ones (including pending_approval, which is
	// "terminal for the purposes of this gate") must not.
	runs := []struct {
		id     string
		status string
		active bool
	}{
		{"run-init", "initializing", true},
		{"run-cloning", "cloning", true},
		{"run-running", "running", true},
		{"run-completed", "completed", false},
		{"run-failed", "failed", false},
		{"run-cancelled", "cancelled", false},
		{"run-unsolvable", "task_unsolvable", false},
		{"run-pending", "pending_approval", false},
	}
	for _, r := range runs {
		if err := CreateAgentRun(database, domain.AgentRun{
			ID:       r.id,
			TaskID:   task.ID,
			PromptID: "test-prompt",
			Status:   r.status,
			Model:    "claude-sonnet-4-6",
		}); err != nil {
			t.Fatalf("create run %s: %v", r.id, err)
		}
		if r.status != "initializing" {
			if _, err := database.Exec(`UPDATE runs SET status = ? WHERE id = ?`, r.status, r.id); err != nil {
				t.Fatalf("set run %s status: %v", r.id, err)
			}
		}
	}

	ids, err := ActiveRunIDsForTask(database, task.ID)
	if err != nil {
		t.Fatalf("ActiveRunIDsForTask: %v", err)
	}

	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, r := range runs {
		if r.active && !got[r.id] {
			t.Errorf("expected active run %s (status=%s) in result, missing", r.id, r.status)
		}
		if !r.active && got[r.id] {
			t.Errorf("unexpected terminal run %s (status=%s) in result", r.id, r.status)
		}
	}
}

// TestActiveRunIDsForTask_Empty returns nil (not error) when the task has
// no runs at all.
func TestActiveRunIDsForTask_Empty(t *testing.T) {
	database := newTestDB(t)
	ids, err := ActiveRunIDsForTask(database, "no-such-task")
	if err != nil {
		t.Fatalf("ActiveRunIDsForTask on missing task: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 ids, got %d", len(ids))
	}
}
