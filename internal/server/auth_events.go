package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// recordAuthEvent writes one auth_events row best-effort. It is the single seam
// that owns the SOC2 "capture every authentication outcome" contract's failure
// mode: every event is recorded AFTER its auth action, and on a store error this
// logs at ERROR (so log-based alerting can fire — this ERROR line IS the accepted
// "known gap" guarantee, in lieu of gaplessness) and returns. The auth action
// that called it has ALREADY happened and proceeds regardless — NEVER rolled
// back. Append-only.
//
// Nil-tolerant: a no-op when s.authEvents is unset, so a bare &Server{} test rig
// (or a build that never wired the store) doesn't panic at a write-site. The
// stable "auth event record failed" message + event_type attr are what
// alerting / the best-effort-contract test key on.
func (s *Server) recordAuthEvent(ctx context.Context, e domain.AuthEvent) {
	if s.authEvents == nil {
		return
	}
	if err := s.authEvents.RecordSystem(ctx, e); err != nil {
		authLog.Error("auth event record failed", "event_type", e.EventType, "error", err)
	}
}

// authDetailJSON marshals a best-effort detail payload for an auth event. A nil
// value (e.g. the logout event, which carries no detail) returns "" → SQL NULL.
// On the near-impossible marshal error it logs and returns "" — the event is
// still recorded, just without detail. Keeps the write-sites one-liners.
func authDetailJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		authLog.Error("auth event detail marshal failed", "error", err)
		return ""
	}
	return string(b)
}

// authMethod maps the TF-derived isSSO signal (state.ProviderID != "") to the
// login method recorded in an auth event's detail. Keeps the github/saml choice
// in one place across the success + failure write-sites.
func authMethod(isSSO bool) string {
	if isSSO {
		return "saml"
	}
	return "github"
}

// fillRequestSource sets the request-derived source fields — client IP (via the
// canonical clientIP parsing, not a raw RemoteAddr) and user agent — on e when
// they're unset and r is non-nil. The single source of this logic, shared by
// authEventBase (the core write-sites) and the RecordAuthEvent extension seam
// (ee/sso), so EE captures the same fields without duplicating clientIP. A
// caller-set value is preserved; a nil r (degenerate call) is a no-op.
func fillRequestSource(e *domain.AuthEvent, r *http.Request) {
	if r == nil {
		return
	}
	if e.IPAddress == "" {
		e.IPAddress = clientIP(r)
	}
	if e.UserAgent == "" {
		e.UserAgent = r.UserAgent()
	}
}

// authEventBase builds an AuthEvent pre-filled with eventType + the
// request-derived source fields (client IP + user agent) every request-scoped
// core write-site captures for SOC2. The caller fills the identity
// (org/user/session) + detail. Empty IP/UA (no RemoteAddr) serialize to NULL.
func authEventBase(r *http.Request, eventType string) domain.AuthEvent {
	e := domain.AuthEvent{EventType: eventType}
	fillRequestSource(&e, r)
	return e
}

// recordLoginFailure records a login_failure auth event (best-effort) with the
// rejecting branch as detail {"reason":…}, plus method / email when those are
// known at the branch (the early state-cookie branches know neither; later
// branches know the method, and the post-verify branches the email). login is
// ONE event_type with the branch in detail — not a type per branch.
func (s *Server) recordLoginFailure(r *http.Request, reason, method, email string) {
	detail := map[string]any{"reason": reason}
	if method != "" {
		detail["method"] = method
	}
	if email != "" {
		detail["email"] = email
	}
	e := authEventBase(r, domain.AuthEventLoginFailure)
	e.DetailJSON = authDetailJSON(detail)
	s.recordAuthEvent(r.Context(), e)
}

// recordLoginSuccess records a login_success auth event (best-effort) after a
// session is minted: the principal, the session's default org (nullable for a
// zero-membership first login → NULL), the new sid, and {"method":…,"sso":…}.
func (s *Server) recordLoginSuccess(r *http.Request, userID uuid.UUID, org uuid.NullUUID, sessionID uuid.UUID, isSSO bool) {
	e := authEventBase(r, domain.AuthEventLoginSuccess)
	e.UserID = userID.String()
	if org.Valid {
		e.OrgID = org.UUID.String()
	}
	e.SessionID = sessionID.String()
	e.DetailJSON = authDetailJSON(map[string]any{"method": authMethod(isSSO), "sso": isSSO})
	s.recordAuthEvent(r.Context(), e)
}

// recordLogout records a logout auth event (best-effort) after the session is
// revoked. No detail — the event itself is the signal (a standalone
// session_revoked is deliberately folded into logout/logout_all).
func (s *Server) recordLogout(r *http.Request, userID, sessionID uuid.UUID) {
	e := authEventBase(r, domain.AuthEventLogout)
	e.UserID = userID.String()
	e.SessionID = sessionID.String()
	s.recordAuthEvent(r.Context(), e)
}

// recordLogoutAll records a logout_all auth event (best-effort) after every
// session for the user is revoked, carrying {"count":n}.
func (s *Server) recordLogoutAll(r *http.Request, userID uuid.UUID, count int) {
	e := authEventBase(r, domain.AuthEventLogoutAll)
	e.UserID = userID.String()
	e.DetailJSON = authDetailJSON(map[string]any{"count": count})
	s.recordAuthEvent(r.Context(), e)
}

// recordRefreshFailure records a refresh_failure auth event (best-effort) when
// the inline JWT refresh dance fails (gotrue refresh error / singleflight err),
// forcing re-login. Carries {"reason":…}. refresh_SUCCESS is deliberately not
// captured (token-rotation noise, ~per-minute per active session).
func (s *Server) recordRefreshFailure(r *http.Request, userID, sessionID uuid.UUID, reason string) {
	e := authEventBase(r, domain.AuthEventRefreshFailure)
	e.UserID = userID.String()
	e.SessionID = sessionID.String()
	e.DetailJSON = authDetailJSON(map[string]any{"reason": reason})
	s.recordAuthEvent(r.Context(), e)
}

// recordJWTVerifyFailure records a jwt_verify_failure auth event (best-effort)
// when a PRESENT session's JWT fails verification (rotated signing key / replay)
// — NOT the anonymous missing/invalid-cookie 401s, which are poke noise. Carries
// {"reason":…}.
func (s *Server) recordJWTVerifyFailure(r *http.Request, userID, sessionID uuid.UUID, reason string) {
	e := authEventBase(r, domain.AuthEventJWTVerifyFailure)
	e.UserID = userID.String()
	e.SessionID = sessionID.String()
	e.DetailJSON = authDetailJSON(map[string]any{"reason": reason})
	s.recordAuthEvent(r.Context(), e)
}
