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

// noClaimTeam is a well-formed team id that names no team.
const noClaimTeam = "00000000-0000-4000-8000-0000000c1a1f"

// ClaimTeam is the team a claim or a delegate of the task by userID lands on
// when the caller has only one to choose: the first of
// TaskStore.ClaimTeamCandidates. A test that is not about the team passes
// this, so the write meets only the guards the test is about. With no
// candidate it answers an id that names no team, so the write refuses as it
// does for a claimant with no team to land the task on.
func ClaimTeam(t testing.TB, s db.TaskStore, orgID, taskID, userID string) string {
	t.Helper()
	candidates, err := s.ClaimTeamCandidates(context.Background(), orgID, taskID, userID)
	if err != nil {
		t.Fatalf("read task %s claim team candidates: %v", taskID, err)
	}
	if len(candidates) == 0 {
		return noClaimTeam
	}
	return candidates[0]
}
