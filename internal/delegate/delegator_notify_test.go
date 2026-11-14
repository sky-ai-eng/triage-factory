package delegate

import (
	"context"
	"sync"
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

// noticeDoorbell records the orgs a spawner's delegator-notice doorbell rang for.
type noticeDoorbell struct {
	mu    sync.Mutex
	rings []string
}

func ringOn(s *Spawner) *noticeDoorbell {
	d := &noticeDoorbell{}
	s.SetOnDelegatorNotice(func(orgID string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.rings = append(d.rings, orgID)
	})
	return d
}

func (d *noticeDoorbell) rang() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.rings...)
}

// passOnce runs one DelegatorNotices pass over s's conversations.
func passOnce(s *Spawner) {
	NewDelegatorNotices(s.conversations, time.Hour).pass(context.Background())
}

// A failure rings the noticeDoorbell only when this engagement recorded it, and the
// sweep tells the delegator only of a recorded failure, in the words the
// failure stored. A verdict that settled the row first is the conversation's
// answer, and a lasting notice saying otherwise would be false.
func TestFailConversation_TellsTheDelegatorOnlyOfAFailureItRecorded(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	rec := notifytest.Record(t)

	settled := newStallFixture(t, "r-fail-settled", activityTimings{idle: time.Hour})
	settledRings := ringOn(settled.s)
	// Concluded: a verdict recorded, the shape a racing completion leaves.
	if _, err := settled.database.Exec(`UPDATE conversations SET status = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`, domain.StatusOpen, settled.conversationID); err != nil {
		t.Fatalf("settle the conversation: %v", err)
	}
	settled.s.failConversation(org, settled.conversationID, settled.task.ID, settled.claimID, "late", domain.ConversationFailureCrash)
	if got := settledRings.rang(); len(got) != 0 {
		t.Errorf("noticeDoorbell rang %v for a failure a verdict overtook, want no ring", got)
	}
	passOnce(settled.s)
	rec.Inactive(t, org, kinds.ConversationFailed)

	f := newStallFixture(t, "r-fail-recorded", activityTimings{idle: time.Hour})
	rings := ringOn(f.s)
	f.s.failConversation(org, f.conversationID, f.task.ID, f.claimID, "exit 1", domain.ConversationFailureCrash)
	if got := rings.rang(); len(got) != 1 || got[0] != org {
		t.Errorf("noticeDoorbell rang %v, want once for %s", got, org)
	}
	passOnce(f.s)
	open := rec.Open(org, kinds.ConversationFailed)
	if len(open) != 1 {
		t.Fatalf("failed notices = %+v, want one", open)
	}
	want := kinds.FailedConversation{
		ConversationID: f.conversationID,
		UserID:         runmode.LocalDefaultUserID,
		Subject:        "owner/repo#r-fail-recorded",
		FailureKind:    domain.ConversationFailureCrash,
		Summary:        "exit 1",
	}
	if got := open[0].Params.(kinds.FailedConversation); got != want {
		t.Errorf("failed notice = %+v, want %+v", got, want)
	}
}

// One pass posts the failure notice and opens the paused notice owed, and
// marks both rows told, so the next pass sends neither again.
func TestDelegatorNotices_PassTellsEachDueRowOnce(t *testing.T) {
	f := newStallFixture(t, "r-sweep-failed", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	f.s.failConversation(org, f.conversationID, f.task.ID, f.claimID, "exit 1", domain.ConversationFailureCrash)
	seedConversation(t, f.database, "r-sweep-paused", "", "")
	pausedClaim := markEngaged(t, f.database, "r-sweep-paused")
	if ok, err := f.s.conversations.ParkOpenForClaimSystem(ctx, org, "r-sweep-paused", pausedClaim, db.ParkStopped(domain.ParkReasonSandboxLost, "")); err != nil || !ok {
		t.Fatalf("park: ok=%v err=%v", ok, err)
	}

	sweep := NewDelegatorNotices(f.s.conversations, time.Hour)
	sweep.pass(ctx)

	failed := rec.Open(org, kinds.ConversationFailed)
	if len(failed) != 1 || failed[0].Key != f.conversationID {
		t.Fatalf("failed notices = %+v, want one for %s", failed, f.conversationID)
	}
	paused := rec.Open(org, kinds.ConversationPaused)
	if len(paused) != 1 {
		t.Fatalf("paused notices = %+v, want one", paused)
	}
	wantPaused := kinds.PausedConversation{
		ConversationID: "r-sweep-paused",
		UserID:         runmode.LocalDefaultUserID,
		Subject:        "owner/repo#r-sweep-paused",
		Reason:         domain.ParkReasonSandboxLost,
	}
	if got := paused[0].Params.(kinds.PausedConversation); got != wantPaused {
		t.Errorf("paused notice = %+v, want %+v", got, wantPaused)
	}
	var failureMarked, pauseMarked bool
	if err := f.database.QueryRow(`SELECT failure_noticed_at IS NOT NULL FROM conversations WHERE id = ?`, f.conversationID).Scan(&failureMarked); err != nil {
		t.Fatalf("read the failure mark: %v", err)
	}
	if err := f.database.QueryRow(`SELECT pause_noticed_parked_at IS parked_at FROM conversations WHERE id = ?`, "r-sweep-paused").Scan(&pauseMarked); err != nil {
		t.Fatalf("read the pause mark: %v", err)
	}
	if !failureMarked || !pauseMarked {
		t.Errorf("marks after the pass: failure=%v pause=%v, want both", failureMarked, pauseMarked)
	}

	sweep.pass(ctx)
	for _, k := range []struct {
		name string
		occs []notifytest.Occurrence
	}{{"failed", rec.Open(org, kinds.ConversationFailed)}, {"paused", rec.Open(org, kinds.ConversationPaused)}} {
		if len(k.occs) != 1 || k.occs[0].Seen != 1 {
			t.Errorf("%s notices after a second pass = %+v, want the one, sent once", k.name, k.occs)
		}
	}
}

// A park that waits on the delegator rings the noticeDoorbell; the sweep opens its
// notice, keeps it while the row waits, and resolves it once a message wakes
// the conversation.
func TestPausedConversation_OpensFromTheRowsAndResolvesWhenWoken(t *testing.T) {
	f := newStallFixture(t, "r-paused", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	rings := ringOn(f.s)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: org, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkStopped(domain.ParkReasonStalled, ""),
	})
	if fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v, want a park that landed", fenced, flipped)
	}
	if got := rings.rang(); len(got) != 1 || got[0] != org {
		t.Errorf("noticeDoorbell rang %v, want once for %s", got, org)
	}
	rec.Inactive(t, org, kinds.ConversationPaused)

	passOnce(f.s)
	rec.Active(t, org, kinds.ConversationPaused, f.conversationID)
	passOnce(f.s)
	rec.Active(t, org, kinds.ConversationPaused, f.conversationID)

	if ok, err := f.s.conversations.MarkQueuedForResume(ctx, org, f.conversationID); err != nil || !ok {
		t.Fatalf("resume: ok=%v err=%v", ok, err)
	}
	passOnce(f.s)
	rec.Inactive(t, org, kinds.ConversationPaused)
}

// An idle park is a turn that ended, not a wait: it rings nothing and the
// sweep opens nothing for it.
func TestPausedConversation_AnIdleParkOpensNothing(t *testing.T) {
	f := newStallFixture(t, "r-idle", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	rings := ringOn(f.s)

	if fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkIdle(),
	}); fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v, want a park that landed", fenced, flipped)
	}
	if got := rings.rang(); len(got) != 0 {
		t.Errorf("noticeDoorbell rang %v for an idle park, want no ring", got)
	}
	passOnce(f.s)
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.ConversationPaused)
}

// A turn that ends while the stall watchdog's stop is pending parks stalled,
// whatever the park asked for, and that park waits on the delegator.
func TestPausedConversation_AnIdleParkThatLandsAStallOpensIt(t *testing.T) {
	f := newStallFixture(t, "r-idle-stalled", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	rings := ringOn(f.s)
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
	if got := rings.rang(); len(got) != 1 {
		t.Errorf("noticeDoorbell rang %v, want once", got)
	}
	passOnce(f.s)
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.ConversationPaused, f.conversationID)
}
