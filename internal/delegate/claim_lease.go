package delegate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// DefaultClaimRenewInterval, DefaultClaimSelfFenceDeadline and
// DefaultClaimLease are TF_CLAIM_RENEW_SEC, TF_CLAIM_SELF_FENCE_SEC and
// TF_CLAIM_TAKEOVER_SEC's defaults. The ordering between them is the
// correctness property — a holder must have stopped before the lease it can
// no longer prove lapses — and internal/app refuses to boot on a
// configuration that breaks it.
//
// The lease's default lives in internal/db, where the store that stamps it
// is: the number is spelled once.
const (
	DefaultClaimRenewInterval     = 20 * time.Second
	DefaultClaimSelfFenceDeadline = 45 * time.Second
	DefaultClaimLease             = db.DefaultClaimLease
)

// ParseClaimRenewInterval, ParseClaimSelfFenceDeadline and ParseClaimLease
// parse TF_CLAIM_RENEW_SEC, TF_CLAIM_SELF_FENCE_SEC and
// TF_CLAIM_TAKEOVER_SEC. Empty maps to the knob's default; anything else must
// parse as a positive integer second count. Each knows only its own variable
// — internal/app cross-validates the ordering between them, mirroring
// ParseSelfFenceDeadline and internal/lease's per-knob parsers.
func ParseClaimRenewInterval(raw string) (time.Duration, error) {
	return parseClaimSeconds("TF_CLAIM_RENEW_SEC", raw, DefaultClaimRenewInterval)
}

func ParseClaimSelfFenceDeadline(raw string) (time.Duration, error) {
	return parseClaimSeconds("TF_CLAIM_SELF_FENCE_SEC", raw, DefaultClaimSelfFenceDeadline)
}

func ParseClaimLease(raw string) (time.Duration, error) {
	return parseClaimSeconds("TF_CLAIM_TAKEOVER_SEC", raw, DefaultClaimLease)
}

func parseClaimSeconds(name, raw string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s=%q (want a positive integer number of seconds)", name, raw)
	}
	return time.Duration(n) * time.Second, nil
}

// errClaimLeaseLost and errClaimSelfFenced are the two causes a claim context
// can be cancelled with, and they are separate because they answer different
// questions in a log: the first is the database saying this engagement is not
// the owner, the second is this process saying it can no longer prove it is.
// Both mean the same thing to every writer downstream — write nothing.
var (
	errClaimLeaseLost  = errors.New("delegate: claim lease lost")
	errClaimSelfFenced = errors.New("delegate: claim self-fenced after renewal failures")
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

// SetClaimLease installs the three claim-lease timings. Zero on any of them
// falls back to that knob's package default at use time, the same shape
// SetSelfFenceDeadline has. Set once at startup from internal/app, which has
// already refused a boot whose ordering is wrong.
func (s *Spawner) SetClaimLease(renew, selfFence, lease time.Duration) {
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
// fence is the claim context's cancel. Nothing else holds it: cancelling it
// cancels the engagement's step context, which is exactly what a stop does —
// the runtime returns, the sidecar and jail are torn down on the way out, the
// subprocess is killed. Killing the cell needs nothing new.
//
// The loop stops when the engagement returns, so a renewal can be in flight
// at the moment the engagement's own terminal write releases the claim. That
// renewal is then refused, logged as a lost lease, and fences a context
// nothing is using any more. It is accurate — the lease really is gone — and
// harmless, so it is not special-cased.
func (s *Spawner) renewClaimLease(ctx context.Context, conv *domain.Conversation, anchor time.Time, fence context.CancelCauseFunc) {
	cadence := s.claimRenewIntervalOrDefault()
	deadline := s.claimSelfFenceDeadlineOrDefault()
	lease := s.claimLeaseOrDefault()

	// A separate timer rather than a second case in the select below, and
	// rather than a check inside the loop body: a renewal call that blocks
	// past its own deadline — a driver that ignores its context, a
	// black-holed connection — would starve any check sharing this
	// goroutine, and that is precisely the failure the fence exists for. The
	// runtime's timer fires regardless of what this goroutine is doing.
	watchdog := time.AfterFunc(time.Until(anchor.Add(deadline)), func() {
		dispatchLog.Warn("claim lease could not be renewed within the self-fence deadline; fencing this engagement",
			"conversation", conv.ID, "claim", conv.ClaimID, "deadline", deadline)
		fence(errClaimSelfFenced)
	})
	defer watchdog.Stop()

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Issue time, not response time: time.Since on it can only
		// over-count the elapsed window, never under-count it, which is the
		// direction a self-fence deadline has to err in.
		issuedAt := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, renewalCallTimeout(cadence))
		expiry, err := s.conversationQueue.RenewClaimLeaseSystem(callCtx, conv.OrgID, conv.ID, conv.ClaimID, lease)
		cancel()

		switch {
		case err == nil:
			// Re-arm from the issue time, so the network delay this call
			// already spent counts against the next deadline.
			watchdog.Reset(deadline - time.Since(issuedAt))
			dispatchLog.Debug("claim lease renewed", "conversation", conv.ID, "claim", conv.ClaimID, "expires_at", expiry)
		case errors.Is(err, db.ErrClaimReleased):
			// Definite: the lease is gone on database time. A retry cannot
			// bring authority back, and there may be no successor at all —
			// expiry alone ends ownership.
			dispatchLog.Info("claim lease lost; fencing this engagement",
				"conversation", conv.ID, "claim", conv.ClaimID, "error", err)
			fence(errClaimLeaseLost)
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
