package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// ssoGateFor asks the login extension whether orgID is barred to userID signed
// in through providerID. Every "Require SSO" enforcement point calls it, so
// each asks the same question the same way. Local mode
// has no sign-in to judge: the SSO extension answers not barred there, and the
// no-op never bars.
func (s *Server) ssoGateFor(ctx context.Context, orgID, userID, providerID string) (SSOGateResult, error) {
	if orgID == "" || userID == "" {
		return SSOGateResult{}, nil
	}
	return s.loginExtension().SSOGate(ctx, orgID, userID, providerID)
}

// requestSSOGate is ssoGateFor for the request ctx belongs to: its principal
// and the provider its credential was signed in through, as withSession seeded
// them.
func (s *Server) requestSSOGate(ctx context.Context, orgID string) (SSOGateResult, error) {
	claims := httpx.ClaimsFrom(ctx)
	if claims == nil {
		return SSOGateResult{}, nil
	}
	return s.ssoGateFor(ctx, orgID, claims.Subject, httpx.SignInProviderFrom(ctx))
}

// refuseSSOBarred answers 403 SSO_REQUIRED and returns true when orgID is
// barred to this request, for the API routes whose org comes from a body, a
// query or a stored record rather than from the credential or the path. A
// browser callback asks requestSSOGate itself and answers the way its other
// refusals do, with a page or a redirect. A failed read answers 500 and also
// returns true: an org whose requirement cannot be read is not one this
// request may act in.
//
// Callers run it after their own membership gate, so an org the caller cannot
// see keeps its own answer and nobody learns from this one whether an org they
// do not belong to requires SSO.
func (s *Server) refuseSSOBarred(w http.ResponseWriter, r *http.Request, orgID string) bool {
	gate, err := s.requestSSOGate(r.Context(), orgID)
	if err != nil {
		internalError(w, "auth", fmt.Errorf("sso gate for org %s: %w", orgID, err))
		return true
	}
	if gate.Barred {
		httpx.WriteSSORequired(w)
		return true
	}
	return false
}
