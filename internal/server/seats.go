package server

import (
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// billingPeriodStart returns the first instant of now's calendar month in UTC —
// the per-seat billing period the distinct-active-user count resets on. A
// calendar month (not the license's own iat→exp window) is the natural per-seat
// billing cycle: a year-long token would otherwise accumulate for a full year
// with no reset.
func billingPeriodStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// enforceSeatLimit is the per-seat license gate on the multi-mode login critical
// edge. It runs AFTER principal resolution and BEFORE the session is minted:
// when the deployment's committed seat cap (the license's maxSeats claim,
// surfaced through entitlements.Active().Limit(LimitSeats)) is set, it atomically
// claims a seat for userID in the current billing period. If the user is a NEW
// distinct claimant beyond the cap, it records a seat_limit_rejected auth event
// and returns true; the caller then denies the session (redirect to /login) —
// the same block-before-CreateSystem mechanism SSO domain enforcement uses.
//
// The claim (count + insert) is atomic and serialized per period inside
// ClaimSeatSystem, which closes the count-then-mint TOCTOU: concurrent
// first-logins cannot both read an under-cap count and both be admitted past the
// cap. The seat_claims ledger is the source of truth, so a blocked login (which
// never claims) never consumes a seat, and count(*) per period is the billable
// distinct-user total.
//
// The cap is DEPLOYMENT-wide (self-host licensing is auto-all, and this critical
// edge may have no org yet — a zero-membership first login), which is why it
// resolves through Active(), not For(orgID). Only the OAuth login path claims, so
// service/bot identities (the GitHub App bot, the Jira service account) — which
// never traverse it — are excluded for free.
//
// Never blocks:
//   - Uncapped (no license / no maxSeats / lapsed license → the limit is unset):
//     a no-op, every login proceeds.
//   - A returning user who already holds a claim this period: admitted even when
//     the deployment is at cap. Only a NEW distinct claimant beyond the cap is
//     blocked, so within a period seats are claimed first-come — the (maxSeats+1)th
//     distinct human is turned away until seats are freed/bought or the month rolls.
//
// Fail-open: on a store error the CLAIM read/write can't complete, so it logs at
// ERROR and returns false (allow). A licensing cap is a business rule, not an
// auth-integrity check — a transient DB error must not lock every user out of the
// deployment. The rejection record, by contrast, is best-effort (recordAuthEvent
// swallows + logs its write error) and does NOT gate the decision: a user the
// atomic claim already proved to be over-cap stays blocked even if the audit row
// fails to write, because making the block depend on a successful audit write
// would let a transient blip silently admit an over-cap user — the opposite of a
// hard cap.
func (s *Server) enforceSeatLimit(r *http.Request, userID string) (blocked bool) {
	seatCap, capped := entitlements.Active().Limit(entitlements.LimitSeats)
	if !capped || seatCap <= 0 {
		return false // uncapped — nothing to enforce
	}
	if s.authEvents == nil {
		// No seat store wired (a bare test rig) — cannot claim, so allow.
		return false
	}
	period := billingPeriodStart(timeNow()).Format("2006-01")
	admitted, seats, err := s.authEvents.ClaimSeatSystem(r.Context(), period, userID, seatCap)
	if err != nil {
		seatsLog.Error("seat claim failed; allowing login (fail-open)",
			"user", userID, "error", err)
		return false
	}
	if admitted {
		return false
	}

	seatsLog.Warn("seat limit reached: blocking new distinct user beyond committed cap",
		"user", userID, "max_seats", seatCap, "active_users", seats, "period", period)
	e := authEventBase(r, domain.AuthEventSeatLimitRejected)
	e.UserID = userID // deployment-scoped rejection: OrgID left NULL by design
	e.DetailJSON = authDetailJSON(map[string]any{
		"max_seats":    seatCap,
		"active_users": seats,
		"period":       period,
	})
	s.recordAuthEvent(r.Context(), e)
	return true
}
