package dbtest

import (
	"context"
	"testing"
	"time"

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

// snoozeTask snoozes an open, unclaimed task for an hour through the store's
// own door, for a test that needs a snoozed row and is not about the snooze.
func snoozeTask(t testing.TB, s db.TaskStore, orgID, taskID string) {
	t.Helper()
	ok, err := s.SnoozeTask(context.Background(), orgID, taskID, time.Now().Add(time.Hour), TaskVersion(t, s, orgID, taskID))
	if err != nil || !ok {
		t.Fatalf("snooze task %s: ok=%v err=%v", taskID, ok, err)
	}
}
