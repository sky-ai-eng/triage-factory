package postgres_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTaskMemoryWait_Postgres_ACrossTeamHolderReadsTheWaitTheClaimGateHolds
// stages a reassignment to someone on another team the way the reassign route
// runs it: the claim moves the task to the new holder's team, the old holder's
// conversation ends and stays on the old team, and the new holder's is minted
// on the new team. The new holder cannot read the old conversation, which owes
// a memory. While it does, their task read under RLS, the admin-pool task
// read, the claim gate, and the failing-memory count all say the task waits,
// and once the memory lands all four say it does not.
func TestTaskMemoryWait_Postgres_ACrossTeamHolderReadsTheWaitTheClaimGateHolds(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	// The real app pool beside the admin pool, so the new holder's read goes
	// through the policies rather than past them.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, oldHolder, _ := pgtest.SeedOrgWithUser(t, h, "old-holder")
	taskID := seedPgTask(t, h, orgID, oldHolder)
	var oldTeam string
	if err := h.AdminDB.QueryRow(`SELECT team_id::text FROM tasks WHERE id = $1`, taskID).Scan(&oldTeam); err != nil {
		t.Fatalf("read the task's team: %v", err)
	}
	newHolder := pgtest.SeedUser(t, h, "new-holder")
	newTeam := pgtest.SeedTeam(t, h, orgID, "new-team")
	pgtest.AddOrgMember(t, h, newHolder, orgID, newTeam, "member", "member")
	// Both teams see the task, and the old holder works it in a conversation
	// that has run.
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2), ($1, $3)`, taskID, oldTeam, newTeam)
	pgtest.MustExec(t, h.AdminDB, `UPDATE tasks SET status = 'in_progress', claimed_by_user_id = $2 WHERE id = $1`, taskID, oldHolder)
	old, err := stores.Conversations.MintForPersonClaimSystem(ctx, orgID, taskID, oldHolder)
	if err != nil || old == nil {
		t.Fatalf("mint the old holder's conversation: (%v, %v)", old, err)
	}
	if _, err := stores.Conversations.InsertMessageSystem(ctx, orgID, &domain.Message{
		ConversationID: old.ID, Role: "assistant", Content: "a turn",
	}); err != nil {
		t.Fatalf("give the old conversation a turn: %v", err)
	}

	var version int64
	if err := h.AdminDB.QueryRow(`SELECT version FROM tasks WHERE id = $1`, taskID).Scan(&version); err != nil {
		t.Fatalf("read the task's version: %v", err)
	}
	if ok, err := stores.Tasks.ReassignClaimToUserSystem(ctx, orgID, taskID, oldHolder, newHolder, version); err != nil || !ok {
		t.Fatalf("ReassignClaimToUserSystem = (%v, %v)", ok, err)
	}
	if _, err := stores.Conversations.EndConversationsForTaskSystem(ctx, orgID, taskID, domain.EndedReassigned); err != nil {
		t.Fatalf("EndConversationsForTaskSystem: %v", err)
	}
	held, err := stores.Conversations.MintForPersonClaimSystem(ctx, orgID, taskID, newHolder)
	if err != nil || held == nil {
		t.Fatalf("mint the new holder's conversation: (%v, %v)", held, err)
	}
	if held.TeamID != newTeam {
		t.Fatalf("the new holder's conversation is on team %q, want the new team %q", held.TeamID, newTeam)
	}
	// The new holder's message wakes their conversation, which leaves the
	// owed memory as the only thing holding it.
	delivered := false
	if _, err := stores.Conversations.InsertMessageSystem(ctx, orgID, &domain.Message{
		ConversationID: held.ID, Role: "user", Content: "what happened here?",
		Delivered: &delivered, WindowState: domain.MessageWindowActive,
	}); err != nil {
		t.Fatalf("the new holder's message: %v", err)
	}

	// asHolder reads the task as the new holder, under RLS. It fails unless
	// the old conversation is hidden from them, since that is the case the
	// task read has to answer for.
	asHolder := func(t *testing.T) *domain.Task {
		t.Helper()
		var task *domain.Task
		if err := stores.Tx.WithReadTx(ctx, orgID, newHolder, func(tx db.TxStores) error {
			hidden, err := tx.Conversations.Get(ctx, orgID, old.ID)
			if err != nil {
				return err
			}
			if hidden != nil {
				t.Fatal("the new holder can read the old team's conversation; the fixture no longer stages the hidden case")
			}
			task, err = tx.Tasks.Get(ctx, orgID, taskID)
			return err
		}); err != nil {
			t.Fatalf("read the task as the new holder: %v", err)
		}
		if task == nil {
			t.Fatal("the new holder cannot read the task they hold")
		}
		return task
	}
	tasksWaiting := func(t *testing.T) int {
		t.Helper()
		f, err := stores.MemoryAttempts.FailingSystem(ctx, orgID, 0, time.Hour)
		if err != nil {
			t.Fatalf("FailingSystem: %v", err)
		}
		return f.TasksWaiting
	}
	assertWaiting := func(t *testing.T, want *domain.MemoryAttemptSummary) {
		t.Helper()
		system, err := stores.Tasks.GetSystem(ctx, orgID, taskID)
		if err != nil || system == nil {
			t.Fatalf("Tasks.GetSystem = (%v, %v)", system, err)
		}
		for reader, got := range map[string]*domain.Task{"the new holder's read": asHolder(t), "the admin-pool read": system} {
			if !got.MemoryPending {
				t.Errorf("%s: memory_pending = false while the old conversation owes its memory", reader)
			}
			switch {
			case want == nil && got.MemoryAttempt != nil:
				t.Errorf("%s: memory_attempt = %+v before any attempt ran", reader, *got.MemoryAttempt)
			case want != nil && got.MemoryAttempt == nil:
				t.Errorf("%s: memory_attempt = nil, want %+v", reader, *want)
			case want != nil && (got.MemoryAttempt.Outcome != want.Outcome ||
				got.MemoryAttempt.ErrorKind != want.ErrorKind ||
				got.MemoryAttempt.ErrorMessage != want.ErrorMessage):
				t.Errorf("%s: memory_attempt = %+v, want %+v", reader, *got.MemoryAttempt, *want)
			}
		}
		if claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "memory-wait-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease); err != nil {
			t.Fatalf("ClaimNextConversation: %v", err)
		} else if claimed != nil {
			t.Fatalf("ClaimNextConversation claimed %s while the old conversation owes its memory", claimed.ID)
		}
		if n := tasksWaiting(t); n != 1 {
			t.Errorf("FailingSystem.TasksWaiting = %d, want 1", n)
		}
	}

	assertWaiting(t, nil)

	attempt, err := stores.MemoryAttempts.BeginAttemptSystem(ctx, orgID, old.ID)
	if err != nil {
		t.Fatalf("BeginAttemptSystem: %v", err)
	}
	const msg = "the background-jobs model is unset for this org"
	if _, err := stores.MemoryAttempts.CompleteAttemptSystem(ctx, orgID, attempt.ID,
		domain.MemoryAttemptFailed, domain.MemoryAttemptErrNoModel, msg, "", 3, 0); err != nil {
		t.Fatalf("CompleteAttemptSystem: %v", err)
	}
	assertWaiting(t, &domain.MemoryAttemptSummary{
		Outcome: domain.MemoryAttemptFailed, ErrorKind: domain.MemoryAttemptErrNoModel, ErrorMessage: msg,
	})

	if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, old.ID, "", "the old holder's handoff", domain.MemorySourceGenerated); err != nil {
		t.Fatalf("UpsertAgentMemorySystem: %v", err)
	}
	if got := asHolder(t); got.MemoryPending || got.MemoryAttempt != nil {
		t.Errorf("the new holder's read: memory_pending = %v, memory_attempt = %+v after the memory landed, want neither",
			got.MemoryPending, got.MemoryAttempt)
	}
	if got, err := stores.Tasks.GetSystem(ctx, orgID, taskID); err != nil || got == nil {
		t.Fatalf("Tasks.GetSystem = (%v, %v)", got, err)
	} else if got.MemoryPending || got.MemoryAttempt != nil {
		t.Errorf("the admin-pool read: memory_pending = %v, memory_attempt = %+v after the memory landed, want neither",
			got.MemoryPending, got.MemoryAttempt)
	}
	if n := tasksWaiting(t); n != 0 {
		t.Errorf("FailingSystem.TasksWaiting = %d after the memory landed, want 0", n)
	}
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "memory-wait-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil {
		t.Fatalf("ClaimNextConversation: %v", err)
	}
	if claimed == nil || claimed.ID != held.ID {
		t.Errorf("ClaimNextConversation = %+v after the memory landed, want the new holder's conversation %s", claimed, held.ID)
	}
}

// TestTaskMemoryWait_Postgres_RefusesAnotherOrgUnderClaims pins the function's
// org check: with request claims it answers for the caller's org alone, so a
// caller cannot read another org's wait by passing that org's ids.
func TestTaskMemoryWait_Postgres_RefusesAnotherOrgUnderClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "wait-org-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "wait-org-b")
	taskB := seedPgTask(t, h, orgB, userB)

	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		var pending bool
		return tx.QueryRow(`SELECT (tf.task_memory_wait($1, $2)).pending`, taskB, orgB).Scan(&pending)
	})
	if err == nil || !strings.Contains(err.Error(), "task_memory_wait: requested org") {
		t.Fatalf("tf.task_memory_wait for another org under the caller's claims = %v, want the org check's refusal", err)
	}
}
