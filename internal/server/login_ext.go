package server

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// LoginExtension is the SSO decision seam in core's shared OAuth start +
// callback path (handleOAuthStart / handleOAuthCallback). Those flows log
// in EVERY user — GitHub or SSO — so the SSO-specific forks (SP-initiated
// SAML start, the verify-before-enforce test round-trip, enforcement of a
// non-SSO login on an enforced domain, and JIT provisioning of an SSO
// login) are delegated here rather than hard-coded into the login path.
// Core holds no SSO symbols; the Enterprise Edition registers the real
// implementation via ExtensionAPI.SetLoginExtension during installExtensions.
//
// The default is a no-op (loginExtension() returns noopLoginExtension when
// none is set), which is exactly "SSO off": StartSSO declines, the test
// branch never fires, no enforcement, no JIT — a plain GitHub-only login.
type LoginExtension interface {
	// StartSSO handles a non-"github" provider on GET /api/auth/oauth/{provider}
	// (e.g. "saml"). Returns true if it wrote the response (a redirect to the
	// IdP); false to let core fall through to "unsupported provider".
	StartSSO(w http.ResponseWriter, r *http.Request, provider, returnTo string) (handled bool)

	// OnTestCallback runs at the top of the OAuth callback, before the code
	// exchange, when TF's signed state may carry the verify-before-enforce
	// Test flag. Returns true if it fully handled the response (rendered the
	// pass/fail page) so core returns; false to continue a normal login.
	OnTestCallback(w http.ResponseWriter, r *http.Request, st LoginState) (handled bool)

	// OnLoginResolved runs after core has exchanged the code, verified the
	// JWT, and resolved the principal — but before it mints a session. It
	// folds the two remaining SSO forks:
	//   - enforcement: a non-SSO login whose verified email is on an enforced
	//     domain is rejected (done=true, response is a redirect to /login).
	//   - JIT: an SSO login is provisioned into its provider's org, returned
	//     as activeOrg so core defaults the new session there.
	// done=true means the hook wrote the response and core must return.
	//
	// Response-writing contract (avoid a double write): EITHER write the
	// response and return done=true, err=nil (the enforcement-reject path), OR
	// return err!=nil and write NOTHING — core renders the 500 (the JIT-failure
	// path). Never both. (activeOrg is only honored when done=false, err=nil.)
	OnLoginResolved(w http.ResponseWriter, r *http.Request, in LoginResolved) (activeOrg uuid.NullUUID, done bool, err error)

	// IdPForProviders is the read-side seam: for each SSO provider id that
	// backs one of a principal's login identities, which identity-provider
	// product it is (the closed vocabulary the extension's connection store
	// holds). Ids the extension does not recognise, or whose connection has
	// no known IdP, are absent from the map. Core uses it to let a login
	// identity wear its vendor's mark without core naming any SSO type; the
	// no-op returns nil, which reads as "nothing known".
	IdPForProviders(ctx context.Context, providerIDs []string) (map[string]string, error)
}

// LoginState is the neutral subset of the OAuth state cookie a login hook
// needs — core parses its own stateClaims and projects this for the hook,
// so the hook never sees (or names) core's cookie type.
type LoginState struct {
	ReturnTo     string
	CodeVerifier string
	ProviderID   string
	Test         bool
}

// LoginResolved is what core hands OnLoginResolved after principal
// resolution. Email/EmailVerified come from the verified GoTrue JWT;
// UserID is the resolved principal (public.users row); IsSSO is
// State.ProviderID != "" (TF's own authoritative SSO signal).
type LoginResolved struct {
	State         LoginState
	UserID        uuid.UUID
	Email         string
	EmailVerified bool
	IsSSO         bool
}

// noopLoginExtension is the default when no extension is registered — SSO
// off. Every method declines.
type noopLoginExtension struct{}

func (noopLoginExtension) StartSSO(http.ResponseWriter, *http.Request, string, string) bool {
	return false
}

func (noopLoginExtension) OnTestCallback(http.ResponseWriter, *http.Request, LoginState) bool {
	return false
}

func (noopLoginExtension) OnLoginResolved(http.ResponseWriter, *http.Request, LoginResolved) (uuid.NullUUID, bool, error) {
	return uuid.NullUUID{}, false, nil
}

func (noopLoginExtension) IdPForProviders(context.Context, []string) (map[string]string, error) {
	return nil, nil
}

// loginExtension returns the registered login extension, or the no-op
// default. Always non-nil; safe to call unconditionally from the login path.
func (s *Server) loginExtension() LoginExtension {
	if s.loginExt == nil {
		return noopLoginExtension{}
	}
	return s.loginExt
}

// toLoginState projects core's signed-state cookie onto the neutral
// LoginState the hook sees — so the extension never names stateClaims.
func toLoginState(st *stateClaims) LoginState {
	return LoginState{
		ReturnTo:     st.ReturnTo,
		CodeVerifier: st.CodeVerifier,
		ProviderID:   st.ProviderID,
		Test:         st.Test,
	}
}
