package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// handleWS wraps the hub's HandleWS so the websocket package stays
// free of any dependency on internal/server. Identity (userID, orgID)
// is pulled out of r.Context() before the upgrade and passed to the
// hub, which captures them on the per-connection struct for the
// Broadcast-side scoping filter.
//
// /api/ws is mounted via s.api(...), so withSession has already run
// by the time we get here: in local mode both values are the sentinel
// org/user, in multi mode they're the real session identity.
//
// Three identity shapes flow through here:
//
//   - Both empty (no claims, no orgID): pre-auth or test-harness path.
//     Falls through to the hub as an unscoped client that receives
//     every event — matches pre-D9b behavior.
//   - Both set: normal local-mode (sentinels) or normal multi-mode
//     (authenticated user with an active org). Scoped to that (user,
//     org) pair by the hub filter.
//   - userID set, orgID empty: authenticated multi-mode session whose
//     active_org_id is NULL (user has zero memberships, or hasn't
//     called POST /api/me/active-org yet). Rejected here with 409 +
//     no_active_org rather than registered as unscoped — the
//     unscoped carveout exists for the no-identity path, not for
//     authenticated callers with no org. Without this gate such a
//     client would receive every tenant's broadcasts.
//
// Stale-active-org gate (TFAC-75): withSession resolves orgID from the
// session's active_org_id WITHOUT re-checking membership (the FK only
// references orgs(id), and the HTTP hot path leaves that compensation
// to /api/me). On the WebSocket upgrade — a rare event, unlike every
// HTTP request — we can afford the membership check, and it's
// load-bearing here: after an admin removes the user from their active
// org we actively close the socket, and this gate stops the immediate
// auto-reconnect from re-scoping to (and re-streaming) the org the user
// no longer belongs to. A non-member active org is treated as
// no_active_org, the same 409 the FE handles by re-picking.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	var userID string
	if claims := ClaimsFrom(r.Context()); claims != nil {
		userID = claims.Subject
	}
	orgID := OrgIDFrom(r.Context())
	if userID != "" && orgID == "" {
		s.writeNoActiveOrg(w)
		return
	}
	// Multi-mode only (authDeps != nil): re-validate that the resolved
	// active org is still one the user belongs to. Local mode's sentinel
	// org has no membership row and never revokes, so skip it there.
	if s.authDeps != nil && userID != "" && orgID != "" {
		ok, err := s.az.UserHasOrgAccess(r.Context(), userID, orgID)
		if err != nil {
			internalError(w, "ws", err)
			return
		}
		if !ok {
			s.writeNoActiveOrg(w)
			return
		}
	}

	var sid string
	if sess := SessionFrom(r.Context()); sess != nil {
		sid = sess.ID.String()
	}
	s.ws.HandleWS(w, r, userID, orgID, sid)
}

// writeNoActiveOrg renders the 409 the FE treats as "this connection has
// no usable org — re-pick one." Shared by the NULL-active-org case and
// the stale-membership gate so both speak the same shape to the client —
// and the same envelope RequireOrg emits, so the handshake isn't a second
// dialect of the same condition.
func (s *Server) writeNoActiveOrg(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonNoActiveOrg,
		Message: "websocket handshake requires an active org; call POST /api/me/active-org to choose one",
	})
}
