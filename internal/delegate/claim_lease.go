package delegate

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/suspendclock"
)

// DefaultClaimRenewInterval, DefaultClaimSelfFenceDeadline and
// DefaultClaimLease are the per-claim lease timings: how often a holder
// renews its claim, how long since its last successful renewal it waits
// before fencing its own engagement, and how long the lease it renews lasts
// on database time.
//
// Constants, not operator knobs. The ordering between them is a correctness
// property — a holder must have stopped before the lease it can no longer
// prove lapses — so moving one without the others configures a takeover that
// races a live engagement. And none of the three is deployment-shaped: they
// bound how long a goroutine can go without writing one row, which does not
// vary with the size of a fleet or the shape of a host. TestClaimLeaseTimings
// holds the ordering.
//
// The lease's value lives in internal/db, where the store that stamps it is:
// the number is spelled once.
const (
	DefaultClaimRenewInterval     = 20 * time.Second
	DefaultClaimSelfFenceDeadline = 45 * time.Second
	DefaultClaimLease             = db.DefaultClaimLease
)

// errClaimLeaseLost and errClaimSelfFenced are the two lease causes a claim
// context can be cancelled with, and they are separate because they answer
// different questions in a log: the first is the database saying this
// engagement is not the owner, the second is this process saying it can no
// longer prove it is. Both mean the same thing to every writer downstream —
// write nothing.
//
// errStopRequested is the third cause, and the opposite answer: the renewal
// read a pending stop off the conversation, so the engagement settles it as a
// deliberate stop, exactly as it settles a local cancel. The lease is still
// held, which is what lets that fenced park land.
//
// errStalled is the stall watchdog's cause (activity.go), and a stop for the
// same reason: the engagement still holds its claim and settles it through
// its fenced park, which is why leaseFenced does not match it. It differs
// from a requested stop only in the reason the park records (stopParkReason).
//
// errDispatcherShutdown is the last: the dispatcher this engagement runs
// under is stopping with the process. Nobody asked for the conversation to
// stop, so it is handed back to the queue rather than parked — see
// handBackOnShutdown. The lease is still held here too, which is what lets
// the hand-back's fenced release land.
var (
	errClaimLeaseLost     = errors.New("delegate: claim lease lost")
	errClaimSelfFenced    = errors.New("delegate: claim self-fenced after renewal failures")
	errStopRequested      = errors.New("delegate: stop requested")
	errStalled            = errors.New("delegate: engagement stalled")
	errDispatcherShutdown = errors.New("delegate: dispatcher shutting down")
)

// leaseFenced reports whether ctx was cancelled by the claim's lease rather
// than by a stop or a shutdown.
//
// Every arm that maps a cancelled step context to a deliberate park asks this
// first, because a lease fence is not a stop and must not be recorded as one.
// After expiry the park is refused anyway; between the self-fence and expiry
// it would succeed, and land "stopped by user" on a conversation nobody
// stopped.
func leaseFenced(ctx context.Context) bool {
	cause := context.Cause(ctx)
	return errors.Is(cause, errClaimLeaseLost) || errors.Is(cause, errClaimSelfFenced)
}

// shutdownCancelled reports whether ctx was cancelled because the dispatcher
// is shutting down, and by nothing that got there first.
//
// First cancel wins, and that is the rule the runtimes need: a stop that
// landed before the shutdown was a person's request and parks as one, while
// a stop that lands after is still pending as an intent when the claim goes
// back, and the next dispatcher's settlement parks it.
func shutdownCancelled(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errDispatcherShutdown)
}

// setClaimLease overrides the three claim-lease timings. Zero on any of them
// falls back to that timing's package default at use time, the same shape
// selfFenceDeadline has. Nothing in the running product calls it: the
// defaults above are the values, and this exists so the renewal loop can be
// driven at test speed.
func (s *Spawner) setClaimLease(renew, selfFence, lease time.Duration) {
	s.mu.Lock()
	s.claimRenewInterval = renew
	s.claimSelfFenceDeadline = selfFence
	s.claimLease = lease
	s.mu.Unlock()
}

func (s *Spawner) claimRenewIntervalOrDefault() time.Duration {
	s.mu.Lock()
	d := s.claimRenewInterval
	s.mu.Unlock()
	if d <= 0 {
		return DefaultClaimRenewInterval
	}
	return d
}

func (s *Spawner) claimSelfFenceDeadlineOrDefault() time.Duration {
	s.mu.Lock()
	d := s.claimSelfFenceDeadline
	s.mu.Unlock()
	if d <= 0 {
		return DefaultClaimSelfFenceDeadline
	}
	return d
}

func (s *Spawner) claimLeaseOrDefault() time.Duration {
	s.mu.Lock()
	d := s.claimLease
	s.mu.Unlock()
	if d <= 0 {
		return DefaultClaimLease
	}
	return d
}

// renewalCallTimeout bounds one renewal round trip. Half the cadence, so a
// slow call cannot still be outstanding when the next tick comes, and floored
// at a second so an aggressively short cadence (tests) does not fail healthy
// calls. It is a bound on the CALL, not on the fence: the watchdog below runs
// on its own timer precisely so a driver that ignores this deadline cannot
// stop the fence from firing.
func renewalCallTimeout(cadence time.Duration) time.Duration {
	d := cadence / 2
	if d < time.Second {
		return time.Second
	}
	return d
}

// renewClaimLease keeps one engagement's claim alive for as long as the
// engagement runs, and fences it the moment it cannot.
//
// anchor is the instant the dispatcher took BEFORE calling
// ClaimNextConversation — the claim's own acquisition, measured
// conservatively at request start rather than at response, so network delay
// can only make the watchdog fire early and never late. Every later re-arm
// takes the same posture, anchoring on the renewal's issue time.
//
// claimCtx is the claim context and fence its cancel. Nothing else holds the
// cancel: cancelling it cancels the engagement's step context, which is
// exactly what a stop does — the runtime returns, the sidecar and jail are
// torn down on the way out, the subprocess is killed. Killing the cell needs
// nothing new. The loop reads claimCtx only to see whether it was already
// fenced by its lease, which no recovery may undo.
//
// A renewal that reads a pending stop cancels the claim context with
// errStopRequested and keeps renewing: the engagement settles the stop through
// its fenced park, and that park needs the lease to still be live when it
// lands. ctx is not the claim context, so a stop does not end this loop; a
// fired watchdog does.
//
// A renewal refused because the lease lapsed is not always the end. The loop
// registers its lease on the spawner and, like every fenced write, asks
// recoverClaimLease whether the lapse was a system suspend it can take the
// claim back from; only when the answer is no does it fence. It also reads
// the suspend clock every suspendPollInterval and renews at once when the
// reading has moved, so a woken engagement takes its claim back within a
// second of wake rather than at its next tick.
//
// The loop stops when the engagement returns, so a renewal can be in flight
// at the moment the engagement's own terminal write releases the claim. That
// renewal is then refused, logged as a lost lease, and fences a context
// nothing is using any more. It is accurate — the lease really is gone — and
// harmless, so it is not special-cased.
func (s *Spawner) renewClaimLease(ctx context.Context, conv *domain.Conversation, anchor time.Time, claimCtx context.Context, fence context.CancelCauseFunc) {
	cadence := s.claimRenewIntervalOrDefault()
	deadline := s.claimSelfFenceDeadlineOrDefault()
	lease := s.claimLeaseOrDefault()

	// A fired watchdog is terminal for this holder, so it ends the loop as
	// well as the engagement. A renewal that kept going would extend the
	// lease of an engagement that is tearing down and will write nothing,
	// holding the conversation from its successor for up to a full lease
	// after the database came back.
	ctx, stopRenewing := context.WithCancel(ctx)
	defer stopRenewing()

	// st.lastRenewal is the issue time of the most recent renewal the
	// database accepted, and the watchdog below decides on it rather than on
	// having been rescheduled. Reset cannot unschedule a callback the runtime
	// has already dispatched — that is what its false return means, by which
	// point the callback may be running — so a renewal that succeeds right at
	// the deadline would otherwise fence a healthy engagement that still owns
	// its claim.
	st := newClaimLeaseState(conv, anchor, claimCtx, fence, deadline, lease, renewalCallTimeout(cadence))

	// A separate timer rather than a second case in the select below, and
	// rather than a check inside the loop body: a renewal call that blocks
	// past its own deadline — a driver that ignores its context, a
	// black-holed connection — would starve any check sharing this
	// goroutine, and that is precisely the failure the fence exists for. The
	// runtime's timer fires regardless of what this goroutine is doing.
	//
	// It runs on the monotonic clock, which a system suspend stops, so a
	// sleep does not fire it; the lapse a suspend causes is met by the
	// refusal and recovered from there.
	st.watchdog = time.AfterFunc(time.Until(anchor.Add(deadline)), func() {
		if elapsed := time.Since(*st.lastRenewal.Load()); elapsed < deadline {
			return
		}
		dispatchLog.Warn("claim lease could not be renewed within the self-fence deadline; fencing this engagement",
			"conversation", conv.ID, "claim", conv.ClaimID, "deadline", deadline)
		st.fenceWith(errClaimSelfFenced)
		stopRenewing()
	})
	defer st.stopWatchdog()
	s.registerClaimLease(st)
	defer s.deregisterClaimLease(st)

	// The suspend poll. A platform that cannot report suspended time gets no
	// poll at all, and the loop is exactly its cadence.
	var suspendTick <-chan time.Time
	lastSeen, canSee := suspendclock.Suspended()
	if canSee {
		t := time.NewTicker(suspendPollInterval)
		defer t.Stop()
		suspendTick = t.C
	}

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	stopObserved := false
	observeStop := func(renewal db.ClaimRenewal) {
		if renewal.StopRequested && !stopObserved {
			stopObserved = true
			dispatchLog.Info("claim renewal observed a pending stop; stopping this engagement",
				"conversation", conv.ID, "claim", conv.ClaimID, "requested_by", renewal.StopRequestedBy)
			fence(errStopRequested)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-suspendTick:
			seen, ok := suspendclock.Suspended()
			if !ok {
				continue
			}
			slept := seen - lastSeen
			lastSeen = seen
			if slept < suspendThreshold {
				continue
			}
			dispatchLog.Info("system suspend observed; renewing the claim lease now",
				"conversation", conv.ID, "claim", conv.ClaimID, "suspended", slept)
		}
		// A select with both cases ready picks either; a tick buffered while
		// the previous call blocked must not outvote the stop.
		if ctx.Err() != nil {
			return
		}

		// Issue time, not response time: time.Since on it can only
		// over-count the elapsed window, never under-count it, which is the
		// direction a self-fence deadline has to err in. The suspend reading
		// is taken at the same instant, so a sleep that starts while the call
		// is out counts against the base this renewal sets.
		issuedAt := time.Now()
		suspended, suspendOK := suspendclock.Suspended()
		reacquired := st.reacquired.Load()
		// The engagement's activity rides the renewal, so the claim row shows
		// how long it has been idle, what it is waiting on, and how much of
		// its workspace a hard kill would lose, without a second write. A
		// conversation with no tracker reports none, and one that does not
		// checkpoint reports no checkpoint age.
		var activity db.ClaimActivity
		activity.Idle, activity.Op = s.activityFor(conv.ID).snapshot()
		if age, ok := s.checkpointerFor(conv.ID).age(); ok {
			activity.CheckpointAge = &age
		}
		callCtx, cancel := context.WithTimeout(ctx, renewalCallTimeout(cadence))
		renewal, err := s.conversationQueue.RenewClaimLeaseSystem(callCtx, conv.OrgID, conv.ID, conv.ClaimID, lease, activity)
		cancel()

		switch {
		case err == nil:
			st.accepted(issuedAt, suspended, suspendOK)
			dispatchLog.Debug("claim lease renewed", "conversation", conv.ID, "claim", conv.ClaimID, "expires_at", renewal.ExpiresAt)
			observeStop(renewal)
		case errors.Is(err, db.ErrClaimLeaseExpired):
			if taken, ok := s.recoverClaimLease(ctx, st, reacquired); ok {
				observeStop(taken)
				continue
			}
			if st.fenced.Load() {
				// The recovery tried, failed, and fenced; it said why.
				return
			}
			dispatchLog.Info("claim lease lapsed; fencing this engagement",
				"conversation", conv.ID, "claim", conv.ClaimID, "error", err)
			st.fenceWith(errClaimLeaseLost)
			return
		case errors.Is(err, db.ErrClaimReleased):
			// Definite: the claim is gone on database time. A retry cannot
			// bring authority back, and there may be no successor at all.
			dispatchLog.Info("claim lease lost; fencing this engagement",
				"conversation", conv.ID, "claim", conv.ClaimID, "error", err)
			st.fenceWith(errClaimLeaseLost)
			return
		case ctx.Err() != nil:
			return
		default:
			// Indefinite: a timeout, a connection blip. Keep ticking; the
			// watchdog decides when persistent failure becomes a fence.
			dispatchLog.Warn("claim lease renewal failed; retrying on the next tick",
				"conversation", conv.ID, "claim", conv.ClaimID, "error", err)
		}
	}
}
