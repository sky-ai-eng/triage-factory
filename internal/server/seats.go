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
// surfaced through entitlements.Active().Limit(LimitSeats)) is set and the
// logging-in user is a NEW distinct active user for the current billing period
// beyond that cap, it records a seat_limit_rejected auth event and returns true.
// The caller then denies the session (redirect to /login) — the same
// block-before-CreateSystem mechanism SSO domain enforcement uses.
//
// The cap is DEPLOYMENT-wide (self-host licensing is auto-all, and this critical
// edge may have no org yet — a zero-membership first login), which is why it
// resolves through Active(), not For(orgID). It counts DISTINCT AUTHENTICATED
// users (login_success), so service/bot identities (the GitHub App bot, the Jira
// service account) — which never traverse the OAuth login path — are excluded
// for free; provisioned-but-never-logged-in accounts are likewise not counted.
//
// Never blocks:
//   - Uncapped (no license / no maxSeats / lapsed license → the limit is unset):
//     a no-op, every login proceeds.
//   - A returning user already counted this period: they already hold one of the
//     cap's seats, so they are admitted even when the deployment is at cap. Only
//     a NEW distinct user beyond the cap is blocked, so within a period seats are
//     claimed first-come — the (maxSeats+1)th distinct human is turned away until
//     the operator buys more seats or the month rolls over.
//
// Fail-open: on a count/store error it logs at ERROR and returns false (allow).
// A licensing cap is a business rule, not an auth-integrity check — a transient
// DB error must not lock every user out of the deployment. Because a blocked
// login records no login_success, blocked users never consume a seat, so the
// distinct count stays exactly the number of admitted users.
func (s *Server) enforceSeatLimit(r *http.Request, userID string) (blocked bool) {
	seatCap, capped := entitlements.Active().Limit(entitlements.LimitSeats)
	if !capped || seatCap <= 0 {
		return false // uncapped — nothing to enforce
	}
	if s.authEvents == nil {
		// No audit store wired (a bare test rig) — cannot count, so allow.
		return false
	}
	periodStart := billingPeriodStart(timeNow())
	distinct, userActive, err := s.authEvents.SeatUsageSystem(r.Context(), periodStart, userID)
	if err != nil {
		seatsLog.Error("seat usage count failed; allowing login (fail-open)",
			"user", userID, "error", err)
		return false
	}
	if userActive {
		// Already active this period → already holds a seat; never bounce.
		return false
	}
	if distinct < seatCap {
		// Admitting this new user keeps the count within the cap.
		return false
	}

	period := periodStart.Format("2006-01")
	seatsLog.Warn("seat limit reached: blocking new distinct user beyond committed cap",
		"user", userID, "max_seats", seatCap, "active_users", distinct, "period", period)
	e := authEventBase(r, domain.AuthEventSeatLimitRejected)
	e.UserID = userID // deployment-scoped rejection: OrgID left NULL by design
	e.DetailJSON = authDetailJSON(map[string]any{
		"max_seats":    seatCap,
		"active_users": distinct,
		"period":       period,
	})
	s.recordAuthEvent(r.Context(), e)
	return true
}
