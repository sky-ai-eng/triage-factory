package server

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
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
// on each tick it asks the Hub for every distinct credential backing a
// live local connection and closes the ones that no longer look up as
// active. Both credential kinds are swept — session cookies and API
// tokens — because a revoked token has to stop a stream on the same
// bounded cadence a revoked session does, and nothing else would ever
// close it: a token carries no logout and the removal kick that reaches
// sessions is a per-user, per-org broadcast, not a per-credential one.
// The same tick closes connections whose org now requires SSO their
// credential was not signed in through.
// A no-op when auth isn't wired (local mode never calls this — see
// internal/app's gating) so it's safe to start unconditionally from any
// role that serves HTTP.
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
			s.revalidateCredentialsOnce(ctx)
		}
	}
}

func (s *Server) revalidateCredentialsOnce(ctx context.Context) {
	// The SSO provider each still-live credential was signed in through, read
	// by the liveness passes and handed to the org-access pass below, so a
	// credential costs one lookup per tick however many passes judge it. A
	// credential missing from these maps either just closed as dead or could
	// not be read this tick, and the org-access pass leaves it alone.
	sessionProviders := map[string]string{}
	tokenProviders := map[string]string{}

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
		if sess == nil {
			return false
		}
		sessionProviders[sid] = sess.SSOProviderID
		return true
	})
	if n > 0 {
		serverLog.Info("session revalidation kicked stale local connections", "n", n)
	}

	if s.authDeps.apiTokens != nil {
		tn := s.ws.RevalidateTokens(func(tokenID string) bool {
			// LookupSystem takes the plaintext, not the id, so this asks the
			// question by id directly: is there still a live row here. Same
			// fail-open-on-error posture as the session sweep, for the same
			// reason — a DB blip must not disconnect every headless client.
			live, provider, err := s.authDeps.apiTokens.IsLiveSystem(ctx, tokenID)
			if err != nil {
				serverLog.Warn("api token revalidation lookup failed; treating as valid this tick", "token_id", tokenID, "error", err)
				return true
			}
			if live {
				tokenProviders[tokenID] = provider
			}
			return live
		})
		if tn > 0 {
			serverLog.Info("api token revalidation kicked stale local connections", "n", tn)
		}
	}

	// "Require SSO" for streams: a socket keeps the org it was scoped to at
	// handshake and asks nothing again, so without this pass a requirement
	// turned on after the handshake would never reach it. A barred connection
	// closes with 4002, the code the SPA answers by refetching its orgs and
	// re-handshaking, and the handshake then refuses it. A failed read keeps the
	// connection for this tick, the posture of the passes above: the request
	// path and the handshake fail closed, and a database blip here must not
	// disconnect every client at once onto the database that just blipped.
	bn := s.ws.RevalidateOrgAccess(func(id websocket.ConnIdentity) bool {
		var (
			provider string
			known    bool
		)
		if id.SessionID != "" {
			provider, known = sessionProviders[id.SessionID]
		} else {
			provider, known = tokenProviders[id.TokenID]
		}
		if !known {
			return true
		}
		gate, err := s.ssoGateFor(ctx, id.OrgID, id.UserID, provider)
		if err != nil {
			serverLog.Warn("sso revalidation failed; treating as allowed this tick", "org", id.OrgID, "user", id.UserID, "error", err)
			return true
		}
		return !gate.Barred
	})
	if bn > 0 {
		serverLog.Info("sso revalidation closed connections to orgs that require sso", "n", bn)
	}
}
