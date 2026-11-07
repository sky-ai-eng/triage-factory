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
)

// A run's toasts are for the person who delegated it, and nothing a run says
// reaches the org at large: a turn that ends toasts its delegator alone.
func TestIdlePark_ToastsOnlyTheDelegator(t *testing.T) {
	f := newStallFixture(t, "r-toast", activityTimings{idle: time.Hour})
	hub, captured := capturingHub(t)
	f.s.wsHub = hub

	if fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkIdle(),
	}); fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v, want a park that landed", fenced, flipped)
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	var users []string
	for _, e := range captured.events {
		if e.Type == "toast" {
			users = append(users, e.UserID)
		}
	}
	if len(users) != 1 || users[0] != runmode.LocalDefaultUserID {
		t.Errorf("toast recipients = %q, want only the delegator %s", users, runmode.LocalDefaultUserID)
	}
}

// A failure is told only when this engagement recorded it. A verdict that
// settled the row first is the conversation's answer, and a lasting notice
// saying otherwise would be false.
func TestFailConversation_TellsTheDelegatorOnlyOfAFailureItRecorded(t *testing.T) {
	settled := newStallFixture(t, "r-fail-settled", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	// Concluded: a verdict recorded, the shape a racing completion leaves.
	if _, err := settled.database.Exec(`UPDATE conversations SET status = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`, domain.StatusOpen, settled.conversationID); err != nil {
		t.Fatalf("settle the conversation: %v", err)
	}
	settled.s.failConversation(runmode.LocalDefaultOrgID, settled.conversationID, settled.task.ID, settled.claimID, "manual", "late", domain.ConversationFailureCrash)
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.ConversationFailed)

	f := newStallFixture(t, "r-fail-recorded", activityTimings{idle: time.Hour})
	f.s.failConversation(runmode.LocalDefaultOrgID, f.conversationID, f.task.ID, f.claimID, "manual", "exit 1", domain.ConversationFailureCrash)
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.ConversationFailed, f.conversationID)
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

// A turn that ends while the stall watchdog's stop is pending parks stalled,
// whatever the park asked for, and that park waits on the delegator.
func TestPausedConversation_AnIdleParkThatLandsAStallOpensIt(t *testing.T) {
	f := newStallFixture(t, "r-idle-stalled", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	if _, err := f.database.Exec(`UPDATE conversations SET stop_requested_at = CURRENT_TIMESTAMP, stop_requested_reason = ? WHERE id = ?`,
		domain.ParkReasonStalled, f.conversationID); err != nil {
		t.Fatalf("stage the pending stop: %v", err)
	}

	if fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkIdle(),
	}); fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v, want a park that landed", fenced, flipped)
	}
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.ConversationPaused, f.conversationID)
}

// The sweep resolves and never opens: a conversation parked before anything
// told its delegator, or whose park failed to, is not raised by a pass.
func TestPausedConversationSync_NeverOpensAParkNothingOpened(t *testing.T) {
	f := newStallFixture(t, "r-told", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	seedConversation(t, f.database, "r-untold", "", "")
	untoldClaim := markEngaged(t, f.database, "r-untold")
	if ok, err := f.s.conversations.ParkOpenForClaimSystem(ctx, org, "r-untold", untoldClaim, db.ParkStopped(domain.ParkReasonStalled, "")); err != nil || !ok {
		t.Fatalf("park the untold conversation: ok=%v err=%v", ok, err)
	}
	if fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: org, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkStopped(domain.ParkReasonStalled, ""),
	}); fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v", fenced, flipped)
	}

	syncPausedConversations(ctx, f.s.conversations)
	open := rec.Open(org, kinds.ConversationPaused)
	if len(open) != 1 || open[0].Key != f.conversationID || open[0].Seen != 1 {
		t.Errorf("paused notifications = %+v, want only %s, raised once by its park", open, f.conversationID)
	}
}

// awaitingSequence answers ListAwaitingPersonSystem from a script, one answer
// per call, so a test can stage a park landing between a pass's two reads.
type awaitingSequence struct {
	db.ConversationStore
	answers [][]domain.ConversationAwaitingPerson
}

func (a *awaitingSequence) ListAwaitingPersonSystem(context.Context, string) ([]domain.ConversationAwaitingPerson, error) {
	next := a.answers[0]
	a.answers = a.answers[1:]
	return next, nil
}

// A conversation that parks after a pass's read opens its notification
// before the pass syncs, so the sync resolves it; the pass's second read finds
// it waiting and opens it again.
func TestPausedConversationSync_ReopensARunThatParkedDuringThePass(t *testing.T) {
	rec := notifytest.Record(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	parked := domain.ConversationAwaitingPerson{
		ConversationID: "r-mid-pass", CreatorUserID: runmode.LocalDefaultUserID,
		ParkReason: domain.ParkReasonStalled, EntitySourceID: "owner/repo#1",
	}
	// Open before the sync, as the park's own write would be.
	kinds.ConversationPaused.Set(ctx, org, kinds.PausedFor(parked), true)

	syncPausedConversations(ctx, &awaitingSequence{answers: [][]domain.ConversationAwaitingPerson{nil, {parked}}})
	rec.Active(t, org, kinds.ConversationPaused, parked.ConversationID)
}
