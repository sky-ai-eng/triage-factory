package memoryprovision

import (
	"context"
	"time"
)

// RunSweep settles every conversation owing a memory, on a ticker, until ctx
// is cancelled — brain-owned and started/stopped with the rest of the brain
// exactly like credprovision.RunAwaitingSweep. mgr nil is a no-op, the same
// nil-checked shape every other brain-unit member uses.
//
// It is the completion path, not the fast one: the doorbell shortens the wait
// from a boundary to its memory, but the relay is lossy by contract and this
// is what makes the debt settle anyway. Without a doorbell at all, latency is
// one interval.
func RunSweep(ctx context.Context, mgr *Manager, interval time.Duration) {
	if mgr == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mgr.sweep(ctx, AttemptBackoff, "")
		}
	}
}

// Nudge is the doorbell: a boundary that just ended a conversation, or a
// configuration save that may have fixed what the last attempts failed on.
//
// With a conversation id it settles that one. With an empty id it re-sweeps
// the org ignoring the backoff — which is what makes a save of the
// background-jobs model or an LLM credential the re-kick for every task that
// setting left waiting, rather than a wait for the backoff to age out.
//
// Fire-and-forget, and bounded: the caller is a relay dispatch that must not
// block on a model call, and must not be able to leak a goroutine per
// notification either.
func (m *Manager) Nudge(orgID, conversationID string) {
	if m == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), nudgeTimeout)
		defer cancel()
		if conversationID == "" {
			m.sweep(ctx, 0, orgID)
			return
		}
		if err := m.Fulfil(ctx, orgID, conversationID); err != nil {
			log.Warn("memory doorbell: settling the owed memory failed", "conversation", conversationID, "org", orgID, "error", err)
		}
	}()
}

// sweep works one page of owed conversations. backoff is how recently a
// conversation must have been attempted for the read to skip it — zero takes
// everything, which is the doorbell's org-wide form. orgID empty is every org,
// which is the ticker's.
//
// Serial by design: the page is ordered open tasks first and then oldest
// boundary first, which is the order a person waiting would choose, and
// running it in parallel would spend an org's whole background budget on a
// backlog that nobody is watching all of.
func (m *Manager) sweep(ctx context.Context, backoff time.Duration, orgID string) {
	owed, err := m.stores.Conversations.ListMemoryOwedSystem(ctx, backoff, sweepLimit)
	if err != nil {
		log.Warn("list conversations owing a memory failed; retrying next tick", "error", err)
		return
	}
	for _, o := range owed {
		if ctx.Err() != nil {
			return
		}
		// The read is fleet-wide (the brain holds no org scope), so the
		// org-wide doorbell narrows it here rather than in SQL — one org's
		// configuration save is not a reason to re-sweep every other org's
		// backlog ahead of schedule.
		if orgID != "" && o.OrgID != orgID {
			continue
		}
		if err := m.Fulfil(ctx, o.OrgID, o.ConversationID); err != nil {
			log.Warn("settling an owed memory failed", "conversation", o.ConversationID, "org", o.OrgID, "error", err)
		}
	}
}
