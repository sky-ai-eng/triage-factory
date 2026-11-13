package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TaskStatusCheckFixture is one subtest's world for
// RunTaskStatusCheckConformance: the task store, the ids its claim doors
// need, a seeder, and the write no store method makes.
type TaskStatusCheckFixture struct {
	Store                  db.TaskStore
	OrgID, AgentID, UserID string
	Seed                   TaskSeeder
	// WriteStatus writes tasks.status for one task with a bare UPDATE and no
	// claim in view. Every store write of the status goes through a guarded
	// door, so this is the only way to offer the schema a status its
	// constraints exist to refuse.
	WriteStatus func(taskID, status string) error
}

// RunTaskStatusCheckConformance pins the schema constraints behind the task
// store's guarded status writes, in either dialect: a write that skipped
// every door still cannot persist a shape the doors never produce.
func RunTaskStatusCheckConformance(t *testing.T, mk func(t *testing.T) TaskStatusCheckFixture) {
	t.Helper()
	ctx := context.Background()

	// The queue holds nobody's work, and it is the schema that says so rather
	// than the six claim doors agreeing to: pushing a held row back to queued
	// (or to snoozed, the other unclaimed-by-rule status) is the shortest
	// path to the shape no door may persist.
	t.Run("QueueUnclaimed_check_rejects_a_held_queued_row", func(t *testing.T) {
		f := mk(t)
		s := f.Store
		_, _, byUser := f.Seed(t, "queue-held-user")
		_, _, byAgent := f.Seed(t, "queue-held-agent")
		if ok, err := s.ClaimQueuedForUser(ctx, f.OrgID, byUser, f.UserID, TaskVersion(t, s, f.OrgID, byUser)); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		if ok, err := s.StampAgentClaimIfUnclaimed(ctx, f.OrgID, byAgent, f.AgentID, ""); err != nil || !ok {
			t.Fatalf("stamp: ok=%v err=%v", ok, err)
		}
		for _, c := range []struct{ taskID, status string }{
			{byUser, "queued"},
			{byUser, "snoozed"},
			{byAgent, "queued"},
			{byAgent, "snoozed"},
		} {
			if err := f.WriteStatus(c.taskID, c.status); err == nil {
				t.Errorf("writing %s on a held task succeeded; tasks_queue_unclaimed must refuse it", c.status)
			}
			got, _ := s.Get(ctx, f.OrgID, c.taskID)
			if got.Status != "in_progress" {
				t.Errorf("status = %q after a refused write, want in_progress", got.Status)
			}
		}
		// The converse — a row in progress that nobody holds — is not a shape
		// the schema refuses: only requeue returns a task to the queue, and
		// it writes the status itself alongside the cleared claim.
	})

	// The status CHECK is the backstop behind every guarded door: in_review
	// left the vocabulary, so the constraint is what makes it unwritable
	// rather than merely unwritten.
	t.Run("Status_check_rejects_a_retired_value", func(t *testing.T) {
		f := mk(t)
		_, _, taskID := f.Seed(t, "check-in-review")
		if err := f.WriteStatus(taskID, "in_review"); err == nil {
			t.Error("writing in_review succeeded; the tasks status CHECK must refuse it")
		}
		got, _ := f.Store.Get(ctx, f.OrgID, taskID)
		if got.Status != "queued" {
			t.Errorf("status=%q, want queued (a refused write must not land)", got.Status)
		}
	})
}
