package server

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// DefaultSessionRevalidateInterval is the cross-pod kick's backstop timer
// (TFAC-584): a kick travels over tf_ctl NOTIFY, which is lossy by
// contract, so RunSessionRevalidation independently re-checks every
// locally-connected socket's session on this cadence — bounding a missed
// kick at one interval regardless of whether the loss was a dropped
// NOTIFY or this pod's LISTEN connection being mid-reconnect. The timer
// is the guarantee; the NOTIFY (SetWSBackplane's PublishKick) is only the
// latency optimization. Env-tunable via
// TF_WS_SESSION_REVALIDATE_SECONDS (internal/wsbackplane.RevalidateIntervalFromEnv).
const DefaultSessionRevalidateInterval = 60 * time.Second

// RunSessionRevalidation runs the kick backstop until ctx is cancelled:
// on each tick it asks the Hub for every distinct session id backing a
// live local connection and closes the ones that no longer look up as an
// active session. A no-op when auth isn't wired (local mode never calls
// this — see internal/app's gating) so it's safe to start unconditionally
// from any role that serves HTTP.
func (s *Server) RunSessionRevalidation(ctx context.Context, interval time.Duration) {
	if s.authDeps == nil {
		serverLog.Warn("session revalidation not started: auth not wired")
		return
	}
	if interval <= 0 {
		interval = DefaultSessionRevalidateInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.revalidateSessionsOnce(ctx)
		}
	}
}

func (s *Server) revalidateSessionsOnce(ctx context.Context) {
	n := s.ws.RevalidateSessions(func(sid string) bool {
		id, err := uuid.Parse(sid)
		if err != nil {
			// A malformed sid can't back a real session — treat as invalid
			// (close it) rather than silently ignoring it.
			return false
		}
		sess, err := s.authDeps.sessions.LookupSystem(ctx, id)
		if err != nil {
			// Fail open on a transient DB error: a lookup blip must not
			// kick every connected user off. The next tick retries.
			serverLog.Warn("session revalidation lookup failed; treating as valid this tick", "sid", sessions.LogID(id), "error", err)
			return true
		}
		// LookupSystem already filters revoked/expired rows to nil (see
		// handleLogout's comment on the same call), so a non-nil result is
		// exactly "still active."
		return sess != nil
	})
	if n > 0 {
		serverLog.Info("session revalidation kicked stale local connections", "n", n)
	}
}
