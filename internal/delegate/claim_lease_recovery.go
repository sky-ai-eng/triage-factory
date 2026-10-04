package delegate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/suspendclock"
)

// suspendPollInterval is how often an engagement's renewal loop reads the
// suspend clock, and suspendThreshold is how far that reading has to move for
// a suspend to count. The poll is what makes a woken engagement renew within
// a second of wake rather than at its next tick, so a run does not sit
// displayed as queued for up to a cadence after the lid opens. The threshold
// is far above the skew two clock reads can show and far below any sleep
// long enough to lapse a lease.
const (
	suspendPollInterval = time.Second
	suspendThreshold    = time.Second
)

// claimLeaseState is one engagement's lease as its renewal loop keeps it. The
// loop registers it on the spawner for its lifetime (keyed by claim id), so a
// claim-fenced write that meets a lapsed lease anywhere in the engagement can
// ask the same question the loop asks of its own refused renewal: did this
// lapse because the machine slept, and can the claim be taken back?
type claimLeaseState struct {
	conv     *domain.Conversation
	claimCtx context.Context
	fence    context.CancelCauseFunc

	deadline    time.Duration // the self-fence deadline
	lease       time.Duration
	callTimeout time.Duration

	// lastRenewal is the issue time of the latest renewal or re-acquire the
	// database accepted. The watchdog decides on it without taking mu, so it
	// is atomic: a watchdog that waited on a lock held across a blocked
	// database call would be no watchdog at all. A pointer, not a unix count,
	// so the monotonic reading survives.
	lastRenewal atomic.Pointer[time.Time]
	// fenced is set before every lease-cause fence this state delivers, by
	// the loop and by the watchdog. It is read beside the claim context's own
	// cause because first cancel wins: an engagement already cancelled by a
	// stop keeps that cause when its lease is then fenced, and must not
	// re-acquire either.
	fenced atomic.Bool
	// reacquired counts successful re-acquires. A write reads it before it is
	// issued, so a refusal can tell whether the lease was taken back after
	// the write went out — and retry on that result — or whether it is the
	// first to meet the lapse.
	reacquired atomic.Uint64

	// mu serializes recovery attempts, so one suspend is one re-acquire
	// however many writes meet it, and guards the fields below.
	mu sync.Mutex
	// suspendBase is the suspend clock's reading at the latest accepted
	// renewal or re-acquire; suspendOK is whether the platform gave one.
	suspendBase time.Duration
	suspendOK   bool
	// watchdog is the self-fence timer, re-armed by every accepted renewal or
	// re-acquire. Nil until the loop arms it.
	watchdog *time.Timer
}

func newClaimLeaseState(conv *domain.Conversation, anchor time.Time, claimCtx context.Context, fence context.CancelCauseFunc, deadline, lease, callTimeout time.Duration) *claimLeaseState {
	st := &claimLeaseState{
		conv:        conv,
		claimCtx:    claimCtx,
		fence:       fence,
		deadline:    deadline,
		lease:       lease,
		callTimeout: callTimeout,
	}
	st.lastRenewal.Store(&anchor)
	st.suspendBase, st.suspendOK = suspendclock.Suspended()
	return st
}

// fenceWith fences the engagement with a lease cause, recording it first so
// no recovery that starts after this can re-acquire for an engagement that is
// tearing down.
func (st *claimLeaseState) fenceWith(cause error) {
	st.fenced.Store(true)
	st.fence(cause)
}

// accepted records a renewal or re-acquire the database accepted, issued at
// issuedAt with the suspend clock reading suspended beside it.
func (st *claimLeaseState) accepted(issuedAt time.Time, suspended time.Duration, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.acceptedLocked(issuedAt, suspended, ok)
}

func (st *claimLeaseState) acceptedLocked(issuedAt time.Time, suspended time.Duration, ok bool) {
	// An acceptance issued before the one already recorded is older news: a
	// renewal that was out across a sleep while a write's recovery took the
	// claim back. Recording it would rewind the suspend base to before that
	// sleep, and a later lapse would count the same sleep again.
	if issuedAt.Before(*st.lastRenewal.Load()) {
		return
	}
	// Published BEFORE the re-arm, so a callback dispatched in between reads
	// this renewal and stands down rather than fencing on the deadline it
	// was armed for.
	st.lastRenewal.Store(&issuedAt)
	st.suspendBase, st.suspendOK = suspended, ok
	// Re-armed from the issue time, so the network delay this call already
	// spent counts against the next deadline.
	if st.watchdog != nil {
		st.watchdog.Reset(st.deadline - time.Since(issuedAt))
	}
}

// stopWatchdog disarms the self-fence for good when the loop ends. Under mu,
// and cleared rather than merely stopped, because a write's recovery that
// looked this state up before the loop deregistered it may still land, and
// its re-arm must not revive a timer for an engagement that has returned.
func (st *claimLeaseState) stopWatchdog() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.watchdog != nil {
		st.watchdog.Stop()
		st.watchdog = nil
	}
}

func (s *Spawner) registerClaimLease(st *claimLeaseState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimLeases == nil {
		s.claimLeases = make(map[string]*claimLeaseState)
	}
	s.claimLeases[st.conv.ClaimID] = st
}

func (s *Spawner) deregisterClaimLease(st *claimLeaseState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimLeases[st.conv.ClaimID] == st {
		delete(s.claimLeases, st.conv.ClaimID)
	}
}

// claimLeaseFor is the registered lease of claimID, nil when no renewal loop
// is running for it — in which case a lapse on it is not recoverable.
func (s *Spawner) claimLeaseFor(claimID string) *claimLeaseState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimLeases[claimID]
}

// recoverClaimLease is the answer to a refusal classified db.ErrClaimLeaseExpired:
// whether the engagement may carry on, having taken its claim back, or must
// fence as it would on any other refusal. seen is st.reacquired as the caller
// read it before issuing the refused call.
//
// A re-acquire is safe only where the engagement provably did nothing while its
// lease was lapsed, which is the case exactly when the whole machine was
// asleep: nothing in it ran, so nothing acted on the conversation. An
// unreleased claim then also proves nobody else did, since a successor needs
// the claim released first. So the claim is re-acquired only when all of these
// hold, and otherwise the refusal stands:
//
//  1. the suspend clock moved by at least suspendThreshold since the last
//     accepted renewal: a suspend happened inside the lapse;
//  2. the last accepted renewal is younger than the self-fence deadline on
//     the monotonic clock, which does not count the suspend: without the
//     sleep this lease would still be live, so the engagement was not
//     already failing to renew when it went down;
//  3. the engagement has not been fenced by its lease. A watchdog that fired
//     is final. A stop or a stall is not a lease cause, and still
//     re-acquires, because those engagements settle through a fenced park
//     that needs a live lease.
//
// Attempts are serialized on st.mu. A caller that finds the claim already
// re-acquired since it issued its call proceeds on that result without asking
// the database again, so two refusals of one suspend spend one re-acquire.
//
// A re-acquire the database refuses means the claim was released during the
// suspend, and one that fails for any other reason cannot show otherwise; both
// fence the engagement here, since the refusal the caller holds is the end of
// it either way. A call the caller abandoned, by cancelling ctx, is neither:
// it says nothing about the claim, so it fences nothing and is not counted,
// the caller's refusal stands, and a later write or renewal can still
// recover.
func (s *Spawner) recoverClaimLease(ctx context.Context, st *claimLeaseState, seen uint64) (db.ClaimRenewal, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fenced.Load() || leaseFenced(st.claimCtx) {
		return db.ClaimRenewal{}, false
	}
	if st.reacquired.Load() != seen {
		return db.ClaimRenewal{}, true
	}
	suspended, ok := suspendclock.Suspended()
	if !ok || !st.suspendOK {
		return db.ClaimRenewal{}, false
	}
	slept := max(0, suspended-st.suspendBase)
	if slept < suspendThreshold {
		return db.ClaimRenewal{}, false
	}
	if time.Since(*st.lastRenewal.Load()) >= st.deadline {
		return db.ClaimRenewal{}, false
	}

	conv := st.conv
	executorID, bootEpoch := s.executorIdentity()
	issuedAt := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, st.callTimeout)
	renewal, err := s.conversationQueue.ReacquireClaimLeaseSystem(callCtx, conv.OrgID, conv.ID, conv.ClaimID, executorID, bootEpoch, st.lease)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, db.ErrClaimReleased):
		dispatchLog.Info("claim was taken during a system suspend; fencing this engagement",
			"conversation", conv.ID, "claim", conv.ClaimID, "suspended", slept)
		recordSuspendRecovery(suspendRecoveryTaken)
		st.fenceWith(errClaimLeaseLost)
		return db.ClaimRenewal{}, false
	case st.fenced.Load():
		// The watchdog fired while the call was out, and its fence may be
		// what cancelled the call.
		recordSuspendRecovery(suspendRecoveryFailed)
		return db.ClaimRenewal{}, false
	case ctx.Err() != nil:
		return db.ClaimRenewal{}, false
	default:
		dispatchLog.Warn("claim lease could not be re-acquired after a system suspend; fencing this engagement",
			"conversation", conv.ID, "claim", conv.ClaimID, "suspended", slept, "error", err)
		recordSuspendRecovery(suspendRecoveryFailed)
		st.fenceWith(errClaimLeaseLost)
		return db.ClaimRenewal{}, false
	}
	// The watchdog may have fired while the call was out. The lease is live
	// again, but the engagement is already tearing down and writes nothing;
	// its own executor releases the claim once the lease lapses.
	if st.fenced.Load() {
		recordSuspendRecovery(suspendRecoveryFailed)
		return db.ClaimRenewal{}, false
	}
	st.acceptedLocked(issuedAt, suspended, ok)
	st.reacquired.Add(1)
	dispatchLog.Info("claim lease re-acquired after a system suspend",
		"conversation", conv.ID, "claim", conv.ClaimID, "suspended", slept)
	recordSuspendRecovery(suspendRecoveryReacquired)
	return renewal, true
}

// The outcomes of one re-acquire attempt, which is what the suspend-recovery
// counter counts. An engagement counts at most one attempt per suspend: a
// success resets the base the next attempt measures from, a failure fences
// it, and a call its caller abandoned is not counted.
//
//   - reacquired: the claim was taken back and the engagement carried on.
//   - taken: the claim was released while the machine slept, so a successor
//     may hold the conversation and the engagement fenced.
//   - failed: the re-acquire errored, or the engagement's own watchdog fenced
//     it while the call was out; either way it did not carry on.
const (
	suspendRecoveryReacquired = "reacquired"
	suspendRecoveryTaken      = "taken"
	suspendRecoveryFailed     = "failed"
)

// suspendRecoveries owns this package's suspend-recovery counter.
// Package-level because a metric instrument is process-global by nature, and
// swapped atomically so a test can back it with a manual reader while a
// renewal loop may still be counting.
var suspendRecoveries atomic.Pointer[suspendRecoveryStats]

func init() {
	suspendRecoveries.Store(newSuspendRecoveryStats(otel.GetMeterProvider()))
}

type suspendRecoveryStats struct {
	attempts metric.Int64Counter
}

// newSuspendRecoveryStats builds the instrument against mp — production passes
// the global provider, which telemetry.Init installs. An instrument-creation
// error can only be a programmer error, and the API hands back a usable no-op
// alongside it, so it is logged rather than propagated.
func newSuspendRecoveryStats(mp metric.MeterProvider) *suspendRecoveryStats {
	counter, err := mp.Meter("internal/delegate").Int64Counter("claims.suspend_recoveries",
		metric.WithDescription("Claim re-acquires attempted after a system suspend lapsed an engagement's lease, by outcome."))
	if err != nil {
		dispatchLog.Warn("suspend recovery counter setup failed", "error", err)
	}
	return &suspendRecoveryStats{attempts: counter}
}

func recordSuspendRecovery(outcome string) {
	stats := suspendRecoveries.Load()
	if stats == nil || stats.attempts == nil {
		return
	}
	stats.attempts.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// leaseRecoveringConversations is the conversation store every engagement
// writes through (NewSpawner installs it), and the one place a claim-fenced
// write that meets a lapsed lease asks whether the lapse is a suspend the
// engagement can recover from. On db.ErrClaimLeaseExpired it hands the claim
// its write named to recoverClaimLease; when that takes the claim back, the
// write is retried once and its result returned, and otherwise the original
// refusal is. Every other outcome passes through untouched, so the handlers
// downstream keep treating a refusal exactly as they did.
//
// A retry cannot double a write: the refused attempt failed at the fence, the
// first statement of its transaction, so it rolled back having written
// nothing.
type leaseRecoveringConversations struct {
	db.ConversationStore
	s *Spawner
}

// retryAfterRecovery runs write, and once more if it was refused on a lapsed
// lease that recoverClaimLease took back.
//
// The re-acquire reads the conversation's stop intent back the way a renewal
// does, and a stop requested while the machine slept is often first seen
// here: the write that meets the lapse is usually ahead of the loop's next
// renewal. So a re-acquire that reports one stops the engagement with
// errStopRequested, as the renewal loop does. The write is still retried,
// since the claim is held again, and the stop settles through the
// engagement's fenced park, which needs that claim.
func (c *leaseRecoveringConversations) retryAfterRecovery(ctx context.Context, claimID string, write func() error) error {
	st := c.s.claimLeaseFor(claimID)
	var seen uint64
	if st != nil {
		seen = st.reacquired.Load()
	}
	err := write()
	if st == nil || !errors.Is(err, db.ErrClaimLeaseExpired) {
		return err
	}
	renewal, ok := c.s.recoverClaimLease(ctx, st, seen)
	if !ok {
		return err
	}
	if renewal.StopRequested {
		dispatchLog.Info("claim re-acquire observed a pending stop; stopping this engagement",
			"conversation", st.conv.ID, "claim", st.conv.ClaimID, "requested_by", renewal.StopRequestedBy)
		st.fence(errStopRequested)
	}
	return write()
}

func (c *leaseRecoveringConversations) InsertMessageForClaimSystem(ctx context.Context, orgID, claimID string, msg *domain.Message) (id int64, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		id, e = c.ConversationStore.InsertMessageForClaimSystem(ctx, orgID, claimID, msg)
		return e
	})
	return id, err
}

func (c *leaseRecoveringConversations) SetSessionForClaimSystem(ctx context.Context, orgID, conversationID, claimID, sessionID string) (conv *domain.Conversation, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		conv, e = c.ConversationStore.SetSessionForClaimSystem(ctx, orgID, conversationID, claimID, sessionID)
		return e
	})
	return conv, err
}

func (c *leaseRecoveringConversations) SetExecutorForClaimSystem(ctx context.Context, orgID, conversationID, claimID, executorID string, bootEpoch int64) (claim *domain.ExecutorClaim, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		claim, e = c.ConversationStore.SetExecutorForClaimSystem(ctx, orgID, conversationID, claimID, executorID, bootEpoch)
		return e
	})
	return claim, err
}

func (c *leaseRecoveringConversations) SetClaimPhaseSystem(ctx context.Context, orgID, conversationID, claimID, phase string) (claim *domain.ExecutorClaim, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		claim, e = c.ConversationStore.SetClaimPhaseSystem(ctx, orgID, conversationID, claimID, phase)
		return e
	})
	return claim, err
}

func (c *leaseRecoveringConversations) SetWorktreePathForClaimSystem(ctx context.Context, orgID, conversationID, claimID, path string) (conv *domain.Conversation, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		conv, e = c.ConversationStore.SetWorktreePathForClaimSystem(ctx, orgID, conversationID, claimID, path)
		return e
	})
	return conv, err
}

func (c *leaseRecoveringConversations) SetSystemBlockForClaimSystem(ctx context.Context, orgID, conversationID, claimID, block string) (conv *domain.Conversation, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		conv, e = c.ConversationStore.SetSystemBlockForClaimSystem(ctx, orgID, conversationID, claimID, block)
		return e
	})
	return conv, err
}

func (c *leaseRecoveringConversations) MarkDeliveredForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, ids []int, subtype string) error {
	return c.retryAfterRecovery(ctx, claimID, func() error {
		return c.ConversationStore.MarkDeliveredForClaimSystem(ctx, orgID, conversationID, claimID, ids, subtype)
	})
}

func (c *leaseRecoveringConversations) CompactForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error {
	return c.retryAfterRecovery(ctx, claimID, func() error {
		return c.ConversationStore.CompactForClaimSystem(ctx, orgID, conversationID, claimID, replyRow, resultRow, inactiveIDs)
	})
}

func (c *leaseRecoveringConversations) SettleCompactionRequestForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) (msg *domain.Message, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		msg, e = c.ConversationStore.SettleCompactionRequestForClaimSystem(ctx, orgID, conversationID, claimID, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, costUSD, reason)
		return e
	})
	return msg, err
}

func (c *leaseRecoveringConversations) CompleteForClaimSystem(ctx context.Context, orgID, conversationID, claimID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) (conv *domain.Conversation, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		conv, e = c.ConversationStore.CompleteForClaimSystem(ctx, orgID, conversationID, claimID, status, costUSD, durationMs, numTurns, resultSummary, outcome, outcomeReason, failureKind)
		return e
	})
	return conv, err
}

func (c *leaseRecoveringConversations) MarkFailedIfActiveForClaimSystem(ctx context.Context, orgID, conversationID, claimID, failureKind string) (changed bool, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		changed, e = c.ConversationStore.MarkFailedIfActiveForClaimSystem(ctx, orgID, conversationID, claimID, failureKind)
		return e
	})
	return changed, err
}

func (c *leaseRecoveringConversations) ParkOpenForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, park db.Park) (parked bool, err error) {
	err = c.retryAfterRecovery(ctx, claimID, func() (e error) {
		parked, e = c.ConversationStore.ParkOpenForClaimSystem(ctx, orgID, conversationID, claimID, park)
		return e
	})
	return parked, err
}
