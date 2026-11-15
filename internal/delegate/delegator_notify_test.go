package delegate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A run's toasts are for the person who delegated it, and nothing a run says
// reaches the org at large: a turn that did not finish toasts its delegator
// alone.
func TestTurnIncompletePark_ToastsOnlyTheDelegator(t *testing.T) {
	f := newStallFixture(t, "r-toast", activityTimings{idle: time.Hour})
	hub, captured := capturingHub(t)
	f.s.wsHub = hub

	if fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkTurnIncomplete(),
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

// passOnce runs one DelegatorNotices pass over s's conversations, with the
// guards that mark each row running in s's database.
func passOnce(t *testing.T, s *Spawner) {
	t.Helper()
	notifytest.GuardsIn(t, s.tx)
	NewDelegatorNotices(s.conversations, time.Hour).pass(context.Background())
}

// A failure rings the doorbell only when this engagement recorded it, and the
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
		t.Errorf("doorbell rang %v for a failure a verdict overtook, want no ring", got)
	}
	passOnce(t, settled.s)
	rec.Inactive(t, org, kinds.ConversationFailed)

	f := newStallFixture(t, "r-fail-recorded", activityTimings{idle: time.Hour})
	rings := ringOn(f.s)
	f.s.failConversation(org, f.conversationID, f.task.ID, f.claimID, "exit 1", domain.ConversationFailureCrash)
	if got := rings.rang(); len(got) != 1 || got[0] != org {
		t.Errorf("doorbell rang %v, want once for %s", got, org)
	}
	passOnce(t, f.s)
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

	notifytest.GuardsIn(t, f.s.tx)
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

// staleDueLists answers both due lists with what they held at the first
// read, the view of a second pass that read the rows before the first pass
// marked them.
type staleDueLists struct {
	db.ConversationStore
	failures []domain.FailureNoticeDue
	pauses   []domain.PauseNoticeDue
}

func (s *staleDueLists) ListFailureNoticesDueSystem(context.Context, int) ([]domain.FailureNoticeDue, error) {
	return s.failures, nil
}

func (s *staleDueLists) ListPauseNoticesDueSystem(context.Context, int) ([]domain.PauseNoticeDue, error) {
	return s.pauses, nil
}

// Two passes that read the same due rows, as two brains across a lease
// handover can, tell each row once: the second pass's mark finds the row
// marked, and its notice is not written.
func TestDelegatorNotices_APassThatReadRowsAnotherPassToldSendsNothing(t *testing.T) {
	f := newStallFixture(t, "r-overlap-failed", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	notifytest.GuardsIn(t, f.s.tx)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	f.s.failConversation(org, f.conversationID, f.task.ID, f.claimID, "exit 1", domain.ConversationFailureCrash)
	seedConversation(t, f.database, "r-overlap-paused", "", "")
	pausedClaim := markEngaged(t, f.database, "r-overlap-paused")
	if ok, err := f.s.conversations.ParkOpenForClaimSystem(ctx, org, "r-overlap-paused", pausedClaim, db.ParkStopped(domain.ParkReasonStalled, "")); err != nil || !ok {
		t.Fatalf("park: ok=%v err=%v", ok, err)
	}
	failures, err := f.s.conversations.ListFailureNoticesDueSystem(ctx, 100)
	if err != nil {
		t.Fatalf("ListFailureNoticesDueSystem: %v", err)
	}
	pauses, err := f.s.conversations.ListPauseNoticesDueSystem(ctx, 100)
	if err != nil {
		t.Fatalf("ListPauseNoticesDueSystem: %v", err)
	}
	if len(failures) != 1 || len(pauses) != 1 {
		t.Fatalf("due before either pass: failures %+v, pauses %+v, want one of each", failures, pauses)
	}

	NewDelegatorNotices(f.s.conversations, time.Hour).pass(ctx)
	// Resolve the paused notice, so a second open would show as an open
	// occurrence again rather than as the one already there.
	kinds.ConversationPaused.Set(ctx, org, kinds.PausedFor(pauses[0].ConversationAwaitingPerson), false)
	NewDelegatorNotices(&staleDueLists{ConversationStore: f.s.conversations, failures: failures, pauses: pauses}, time.Hour).pass(ctx)

	if failed := rec.Open(org, kinds.ConversationFailed); len(failed) != 1 || failed[0].Seen != 1 {
		t.Errorf("failed notices after both passes = %+v, want one, posted once", failed)
	}
	rec.Inactive(t, org, kinds.ConversationPaused)
}

// awaitingNone answers ListAwaitingPersonSystem with nothing, the read of a
// resolve step that ran before a conversation parked.
type awaitingNone struct{ db.ConversationStore }

func (awaitingNone) ListAwaitingPersonSystem(context.Context, string) ([]domain.ConversationAwaitingPerson, error) {
	return nil, nil
}

// A park that lands after the resolve step's read is opened by the open step
// that follows it in the same pass, and stays open: the resolve step's sync,
// which keeps only what its read saw, has already run.
func TestDelegatorNotices_AParkTheResolveStepDidNotSeeStaysOpen(t *testing.T) {
	f := newStallFixture(t, "r-unseen", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	notifytest.GuardsIn(t, f.s.tx)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	if ok, err := f.s.conversations.ParkOpenForClaimSystem(ctx, org, f.conversationID, f.claimID, db.ParkStopped(domain.ParkReasonStalled, "")); err != nil || !ok {
		t.Fatalf("park: ok=%v err=%v", ok, err)
	}
	NewDelegatorNotices(awaitingNone{f.s.conversations}, time.Hour).pass(ctx)
	rec.Active(t, org, kinds.ConversationPaused, f.conversationID)
}

// refusingNotifications fails every notification write.
type refusingNotifications struct{ db.NotificationStore }

func (refusingNotifications) UpsertOpenSystem(context.Context, db.NotificationOccurrence) (db.NotificationUpsert, error) {
	return db.NotificationUpsert{}, errors.New("notification write refused")
}

// refusingTx is a TxRunner whose system transactions refuse every
// notification write, as a database that fails the write would.
type refusingTx struct{ db.TxRunner }

func (r refusingTx) SystemWithTx(ctx context.Context, orgID string, fn func(db.SystemTxStores) error) error {
	return r.TxRunner.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
		tx.Notifications = refusingNotifications{tx.Notifications}
		return fn(tx)
	})
}

// A notice whose write fails leaves its row unmarked, so the next pass sends
// it. This runs the notify engine itself, over the fixture's database.
func TestDelegatorNotices_AFailedNoticeWriteLeavesTheRowDue(t *testing.T) {
	f := newStallFixture(t, "r-refused", activityTimings{idle: time.Hour})
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	stores := sqlitestore.New(f.database)
	notifytest.Isolate(t)
	install := func(tx db.TxRunner) {
		notify.Install(notify.Backend{
			Tx: tx, Notifications: stores.Notifications, Orgs: stores.Orgs,
			LinearTeamRules: stores.LinearTeamRules, Local: true,
		})
	}
	posted := func() int {
		t.Helper()
		var n int
		if err := f.database.QueryRow(`SELECT COUNT(*) FROM notifications WHERE kind = ?`, kinds.ConversationFailed.Name()).Scan(&n); err != nil {
			t.Fatalf("count notices: %v", err)
		}
		return n
	}
	due := func() int {
		t.Helper()
		got, err := f.s.conversations.ListFailureNoticesDueSystem(ctx, 100)
		if err != nil {
			t.Fatalf("ListFailureNoticesDueSystem: %v", err)
		}
		return len(got)
	}
	f.s.failConversation(org, f.conversationID, f.task.ID, f.claimID, "exit 1", domain.ConversationFailureCrash)

	install(refusingTx{stores.Tx})
	NewDelegatorNotices(f.s.conversations, time.Hour).pass(ctx)
	if n, d := posted(), due(); n != 0 || d != 1 {
		t.Fatalf("after a refused write: %d notices and %d due, want none posted and the failure still due", n, d)
	}

	install(stores.Tx)
	NewDelegatorNotices(f.s.conversations, time.Hour).pass(ctx)
	if n, d := posted(), due(); n != 1 || d != 0 {
		t.Errorf("after the write succeeds: %d notices and %d due, want one posted and nothing due", n, d)
	}
}

// A park that waits on the delegator rings the doorbell; the sweep opens its
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
		t.Errorf("doorbell rang %v, want once for %s", got, org)
	}
	rec.Inactive(t, org, kinds.ConversationPaused)

	passOnce(t, f.s)
	rec.Active(t, org, kinds.ConversationPaused, f.conversationID)
	passOnce(t, f.s)
	rec.Active(t, org, kinds.ConversationPaused, f.conversationID)

	if ok, err := f.s.conversations.MarkQueuedForResume(ctx, org, f.conversationID); err != nil || !ok {
		t.Fatalf("resume: ok=%v err=%v", ok, err)
	}
	passOnce(t, f.s)
	rec.Inactive(t, org, kinds.ConversationPaused)
}

// A turn_incomplete park is not one of the reasons that wait on a person: it
// rings nothing and the sweep opens nothing for it.
func TestPausedConversation_ATurnIncompleteParkOpensNothing(t *testing.T) {
	f := newStallFixture(t, "r-idle", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	rings := ringOn(f.s)

	if fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkTurnIncomplete(),
	}); fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v, want a park that landed", fenced, flipped)
	}
	if got := rings.rang(); len(got) != 0 {
		t.Errorf("doorbell rang %v for a turn_incomplete park, want no ring", got)
	}
	passOnce(t, f.s)
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.ConversationPaused)
}

// A turn that stops while the stall watchdog's stop is pending parks stalled,
// whatever the park asked for, and that park waits on the delegator.
func TestPausedConversation_ATurnIncompleteParkThatLandsAStallOpensIt(t *testing.T) {
	f := newStallFixture(t, "r-idle-stalled", activityTimings{idle: time.Hour})
	rec := notifytest.Record(t)
	rings := ringOn(f.s)
	if _, err := f.database.Exec(`UPDATE conversations SET stop_requested_at = CURRENT_TIMESTAMP, stop_requested_reason = ? WHERE id = ?`,
		domain.ParkReasonStalled, f.conversationID); err != nil {
		t.Fatalf("stage the pending stop: %v", err)
	}

	if fenced, flipped := f.s.markConversationOpenFlipped(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		reason: db.ParkTurnIncomplete(),
	}); fenced || !flipped {
		t.Fatalf("park: fenced=%v flipped=%v, want a park that landed", fenced, flipped)
	}
	if got := rings.rang(); len(got) != 1 {
		t.Errorf("doorbell rang %v, want once", got)
	}
	passOnce(t, f.s)
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.ConversationPaused, f.conversationID)
}
