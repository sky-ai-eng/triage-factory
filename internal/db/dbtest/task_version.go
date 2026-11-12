package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TaskVersion is the version the task stands at now: what a caller holding a
// fresh read of it passes to a person's write (the TaskStore methods that take
// a version). A test that is not about the version passes this, so the write
// meets only the guards the test is about. A missing task answers 0, which no
// task carries, so a write against a missing id still answers as missing.
// Read on the admin pool, so it sees the row whatever the caller's RLS view.
func TaskVersion(t testing.TB, s db.TaskStore, orgID, taskID string) int64 {
	t.Helper()
	task, err := s.GetSystem(context.Background(), orgID, taskID)
	if err != nil {
		t.Fatalf("read task %s version: %v", taskID, err)
	}
	if task == nil {
		return 0
	}
	return task.Version
}
