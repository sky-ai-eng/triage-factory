package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/toast"
)

// A run's toasts are for the person who delegated it. A conversation a trigger
// fired has nobody who asked, so it toasts nobody, and nothing a run says
// reaches the org at large.
func TestToastDelegator_ReachesOnlyTheDelegator(t *testing.T) {
	s := NewSpawner(nil, testSpawnerStores(newDelegateTestDB(t)), nil, nil, "claude-sonnet-4-5")
	hub, captured := capturingHub(t)
	s.wsHub = hub

	s.toastDelegator(runmode.LocalDefaultOrgID, "", toast.LevelSuccess, "Run abc completed")
	if got := captured.toasts(); len(got) != 0 {
		t.Fatalf("toasts for a run with no delegator = %q, want none", got)
	}
	s.toastDelegator(runmode.LocalDefaultOrgID, "user-1", toast.LevelSuccess, "Run abc completed")
	captured.mu.Lock()
	defer captured.mu.Unlock()
	var users []string
	for _, e := range captured.events {
		if e.Type == "toast" {
			users = append(users, e.UserID)
		}
	}
	if len(users) != 1 || users[0] != "user-1" {
		t.Errorf("toast recipients = %q, want only the delegator user-1", users)
	}
}

// A failure tells the person the engagement ran for, naming the entity its
// task is about, and a conversation nobody delegated tells nobody.
func TestNoteConversationFailed_TellsTheDelegator(t *testing.T) {
	f := newStallFixture(t, "r-failed-note", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)

	f.s.noteConversationFailed(runmode.LocalDefaultOrgID, f.conversationID, "", f.task.ID, domain.ConversationFailureCrash, "exit 1")
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.ConversationFailed)

	f.s.noteDelegatorFailed(runmode.LocalDefaultOrgID, f.conversationID, domain.ConversationFailureMemoryLimit, "killed")
	open := rec.Open(runmode.LocalDefaultOrgID, kinds.ConversationFailed)
	if len(open) != 1 || open[0].Key != f.conversationID {
		t.Fatalf("failed notices = %+v, want one for %s", open, f.conversationID)
	}
	got := open[0].Params.(kinds.FailedConversation)
	want := kinds.FailedConversation{
		ConversationID: f.conversationID,
		UserID:         runmode.LocalDefaultUserID,
		Subject:        "owner/repo#r-failed-note",
		FailureKind:    domain.ConversationFailureMemoryLimit,
		Summary:        "killed",
	}
	if got != want {
		t.Errorf("failed notice = %+v, want %+v", got, want)
	}
}

// The park opens the paused notification from the row it wrote, and the sweep
// keeps it open while the row waits and resolves it once a message wakes the
// conversation.
func TestPausedConversation_OpensAtTheParkAndResolvesFromTheRows(t *testing.T) {
	f := newStallFixture(t, "r-paused", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: org, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkStopped(domain.ParkReasonStalled, ""),
	})
	if fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v, want a park that landed", fenced, flipped)
	}
	open := rec.Open(org, kinds.ConversationPaused)
	if len(open) != 1 {
		t.Fatalf("paused notifications = %+v, want one", open)
	}
	want := kinds.PausedConversation{
		ConversationID: f.conversationID,
		UserID:         runmode.LocalDefaultUserID,
		Subject:        "owner/repo#r-paused",
		Reason:         domain.ParkReasonStalled,
	}
	if got := open[0].Params.(kinds.PausedConversation); got != want {
		t.Errorf("paused notification = %+v, want %+v", got, want)
	}

	syncPausedConversations(ctx, f.s.conversations)
	rec.Active(t, org, kinds.ConversationPaused, f.conversationID)

	if ok, err := f.s.conversations.MarkQueuedForResume(ctx, org, f.conversationID); err != nil || !ok {
		t.Fatalf("resume: ok=%v err=%v", ok, err)
	}
	syncPausedConversations(ctx, f.s.conversations)
	rec.Inactive(t, org, kinds.ConversationPaused)
}

// An idle park is a turn that ended, not a wait: it opens nothing.
func TestPausedConversation_AnIdleParkOpensNothing(t *testing.T) {
	f := newStallFixture(t, "r-idle", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)

	if fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkIdle(),
	}); fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v, want a park that landed", fenced, flipped)
	}
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.ConversationPaused)
}
