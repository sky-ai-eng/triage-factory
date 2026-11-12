package delegate

import (
	"context"
	"time"
)

// A warm claim holds what any engagement holds: an executor slot, host memory
// and disk, and a place under its org's max_concurrent_runs. It is worth
// holding them only while nothing else wants them, so the dispatcher lets a
// warm claim go when claimable work is queued behind one of them. Each check
// asks the store first whether that work exists, because a claim let go for
// nothing costs its conversation's next message a cold start.

// letGoWarmBehindOrgCaps lets go of the oldest warm claim of each org that has
// one on this executor, is at its max_concurrent_runs, and has work queued
// that the cap alone holds back. It reports whether it let any go.
func (s *Spawner) letGoWarmBehindOrgCaps(ctx context.Context) bool {
	if s.conversationQueue == nil {
		return false
	}
	letGo := false
	for _, orgID := range s.warmWaits.OrgsWaiting() {
		blocked, err := s.conversationQueue.OrgCapBlockedWorkExistsSystem(ctx, orgID)
		if err != nil {
			dispatchLog.Warn("check for work held back by the org cap failed; asking again on the next scan",
				"org_id", orgID, "error", err)
			continue
		}
		if !blocked {
			continue
		}
		if conversationID, ok := s.warmWaits.LetGoOldestInOrg(orgID); ok {
			letGo = true
			dispatchLog.Info("let go of a warm claim: its org is at max_concurrent_runs and has work queued behind the cap",
				"conversation", conversationID, "org_id", orgID)
		}
	}
	return letGo
}

// letGoWarmForClaimableWork lets go of this executor's oldest warm claim when
// work this executor could claim is queued, and reports whether it let one
// go. waitingOn names what the work waits for, for the log. minWait is the
// probe's wait on work that is not this executor's own: the scan interval,
// within which an executor with a free slot has had its chance at it.
func (s *Spawner) letGoWarmForClaimableWork(ctx context.Context, minWait time.Duration, waitingOn string) bool {
	if s.conversationQueue == nil || s.warmWaits.Len() == 0 {
		return false
	}
	executorID, _ := s.executorIdentity()
	claimable, err := s.conversationQueue.ClaimableExistsSystem(ctx, executorID, s.claimPlacement(), minWait)
	if err != nil {
		dispatchLog.Warn("check for claimable work failed; asking again on the next scan", "error", err)
		return false
	}
	if !claimable {
		return false
	}
	conversationID, ok := s.warmWaits.LetGoOldest()
	if ok {
		dispatchLog.Info("let go of a warm claim: claimable work is queued behind it",
			"conversation", conversationID, "waiting_on", waitingOn)
	}
	return ok
}

// awaitSlot blocks until sem has a free slot, and reports false, holding no
// slot, when ctx ends or the executor starts to drain first. Once per scan
// interval it checks for queued work a warm claim here holds back: behind an
// org's cap, then behind this executor's slots. Over the whole wait it lets
// go of at most one warm claim for a slot, because the slot that claim frees
// ends the wait, and a second claim let go while the first is still tearing
// down would free a slot nothing is waiting for.
func (s *Spawner) awaitSlot(ctx context.Context, sem chan struct{}, scanInterval time.Duration) bool {
	tick := time.NewTicker(scanInterval)
	defer tick.Stop()
	slotFreeing := false
	for {
		select {
		case sem <- struct{}{}:
			return true
		case <-ctx.Done():
			return false
		case <-tick.C:
			// A full executor waits here rather than at the drain check, so
			// the drain is read here too: every warm claim goes, and the
			// slots they free start no new claim.
			if s.Draining() {
				s.warmWaits.LetGoAll()
				return false
			}
			// A claim let go for an org's cap frees a slot here as well.
			if s.letGoWarmBehindOrgCaps(ctx) {
				slotFreeing = true
			}
			if !slotFreeing {
				slotFreeing = s.letGoWarmForClaimableWork(ctx, scanInterval, "slot")
			}
		}
	}
}

// scanConversationQueue is the dispatcher's scan tick: the drain, with the
// checks for queued work a warm claim holds back by its org's cap, before
// the claim, or by the memory or disk gate, once the drain has stopped at
// one. Those checks run on the tick and not on a wake, so a burst of wakes
// lets go of no more warm claims than one tick does. A claim let go for an
// org's cap frees memory and disk too, so a tick that let one go does not
// let a second go for the gate.
func (s *Spawner) scanConversationQueue(ctx context.Context, scanInterval time.Duration) {
	letGoForCap := s.letGoWarmBehindOrgCaps(ctx)
	if gated := s.drainConversationQueue(ctx, scanInterval); gated && !letGoForCap {
		s.letGoWarmForClaimableWork(ctx, scanInterval, "memory_or_disk")
	}
}
