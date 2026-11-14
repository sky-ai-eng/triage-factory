package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// DefaultDelegatorNoticeInterval is how often DelegatorNotices runs a pass
// with nothing ringing it, which bounds how long a notice waits when its
// doorbell is dropped, and how long kinds.ConversationPaused stays open after
// its conversation leaves the park.
const DefaultDelegatorNoticeInterval = 30 * time.Second

// delegatorNoticePage is how many rows a pass reads from each due list. A
// longer backlog is the next pass's.
const delegatorNoticePage = 100

// DelegatorNotices tells the person who delegated a conversation that it
// failed (kinds.ConversationFailed) or that it is parked waiting on them
// (kinds.ConversationPaused), and resolves the paused notice once it is not.
//
// It reads both states from the conversation rows and runs on the brain. The
// process a conversation fails or parks on is often an executor, which may not
// write notifications, so that process only rings the doorbell (Nudge, through
// the spawner's kickDelegatorNotice) after its write lands. A conversation
// also leaves its park through more doors than one process sees (a message
// that wakes it, a stop, its task closing, an archive), and reading the rows
// covers every one of them.
type DelegatorNotices struct {
	conversations db.ConversationStore
	interval      time.Duration
	// wake holds at most one pending pass, so any number of nudges between
	// two passes run one.
	wake chan struct{}
}

// NewDelegatorNotices builds the sweep over conversations, passing every
// interval.
func NewDelegatorNotices(conversations db.ConversationStore, interval time.Duration) *DelegatorNotices {
	return &DelegatorNotices{conversations: conversations, interval: interval, wake: make(chan struct{}, 1)}
}

// Run passes every interval, and whenever nudged, until ctx is cancelled.
func (d *DelegatorNotices) Run(ctx context.Context) {
	if d == nil || d.conversations == nil {
		return
	}
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
		d.pass(ctx)
	}
}

// Nudge asks for a pass now. It never blocks, and a nil receiver ignores it.
func (d *DelegatorNotices) Nudge() {
	if d == nil {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// pass resolves the paused notices whose conversation no longer waits, then
// opens the ones owed, then posts the failures owed. Resolving first means a
// conversation that parks between the resolve step's read and its sync, which
// has no notice yet, is opened by the step that follows.
//
// Each notice is written in one transaction with the mark that records it,
// the mark first (notify.Guard). A notice whose write fails leaves its row
// unmarked, so the next pass sends it, and two passes that read the same row,
// on one brain or on two across a lease handover, send it once: the second
// mark finds the row already marked, and its notice is not written.
func (d *DelegatorNotices) pass(ctx context.Context) {
	d.resolvePaused(ctx)
	d.openPaused(ctx)
	d.postFailed(ctx)
}

// resolvePaused syncs kinds.ConversationPaused in every org where it is open
// with the conversations still waiting: an occurrence whose conversation still
// waits is kept as it is, and one whose conversation does not is resolved. An
// org whose read fails is left as it is.
func (d *DelegatorNotices) resolvePaused(ctx context.Context) {
	orgs, err := kinds.ConversationPaused.OpenOrgs(ctx)
	if err != nil {
		delegateLog.Warn("list orgs with a paused-run notification failed; resolving none this pass", "error", err)
		return
	}
	for _, orgID := range orgs {
		if ctx.Err() != nil {
			return
		}
		awaiting, err := d.conversations.ListAwaitingPersonSystem(ctx, orgID)
		if err != nil {
			delegateLog.Warn("read the org's paused runs failed; leaving their notifications as they are",
				"org_id", orgID, "error", err)
			continue
		}
		kinds.ConversationPaused.Sync(ctx, orgID, nil, kinds.PausedForAll(awaiting)...)
	}
}

// openPaused opens kinds.ConversationPaused for each park that waits on its
// delegator and has not been told, with the mark that records it. A park
// that ended since the read matches no mark, so its notice is not opened.
func (d *DelegatorNotices) openPaused(ctx context.Context) {
	due, err := d.conversations.ListPauseNoticesDueSystem(ctx, delegatorNoticePage)
	if err != nil {
		delegateLog.Warn("list the paused runs owed a notification failed; opening none this pass", "error", err)
		return
	}
	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		kinds.ConversationPaused.SetGuarded(ctx, p.OrgID, kinds.PausedFor(p.ConversationAwaitingPerson), true,
			func(ctx context.Context, tx db.SystemTxStores) (bool, error) {
				return tx.Conversations.MarkPauseNoticedSystem(ctx, p.OrgID, p.ConversationID, p.ParkedAt)
			})
	}
}

// postFailed posts kinds.ConversationFailed for each failure a person
// delegated that has not been told, with the mark that records it.
func (d *DelegatorNotices) postFailed(ctx context.Context) {
	due, err := d.conversations.ListFailureNoticesDueSystem(ctx, delegatorNoticePage)
	if err != nil {
		delegateLog.Warn("list the failed runs owed a notification failed; posting none this pass", "error", err)
		return
	}
	for _, f := range due {
		if ctx.Err() != nil {
			return
		}
		kinds.ConversationFailed.PostGuarded(ctx, f.OrgID, kinds.FailedConversation{
			ConversationID: f.ConversationID,
			UserID:         f.CreatorUserID,
			Subject:        f.EntitySourceID,
			FailureKind:    f.FailureKind,
			Summary:        f.ResultSummary,
		}, func(ctx context.Context, tx db.SystemTxStores) (bool, error) {
			return tx.Conversations.MarkFailureNoticedSystem(ctx, f.OrgID, f.ConversationID)
		})
	}
}
