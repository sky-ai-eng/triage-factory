package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/linearoauth"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Per-user Linear access — the Linear sibling of jira_connect.go. Every write
// a person makes in Linear through Triage Factory is made as that person, so
// the user's credential is access, stored in their own secret scope, and the
// identity is derived from it. It is mandatory once the org has Linear: the
// frontend gate sends a user without one to /orgs/{org_id}/connect-linear.
//
//   GET    /api/orgs/{org_id}/linear/identity
//   POST   /api/orgs/{org_id}/linear/identity/api-key
//   DELETE /api/orgs/{org_id}/linear/identity
//   GET    /api/orgs/{org_id}/linear/connect/start?return_to=
//   GET    /api/linear/connect/callback?code=&state=
//
// The credential is keyed by the org's Linear workspace,
// linear.UserTokenKey(org_settings.linear_workspace_id), and holds either a
// personal API key or a Connect refresh token (actor=user, scopes read,write).
// Connect's access tokens are never stored; linearoauth.UserTokenCache mints
// them. The callback is static for the reason the install's is: Linear
// matches redirect URIs exactly and one app serves every org, so the org and
// user travel in an HMAC-signed state cookie.
//
// Every writer of a user's credential holds that user's
// linearoauth.UserCredentialLock across its database work: the membership
// read, the workspace read and the write or delete. So the envelope and the
// identity row derived from it always come from the same bind, and a bind
// racing the user's removal either sees it or is swept after it. The org's
// Linear workspace, which keys the credential, can change under any of them:
// the org's key bind, its install and its unbind all write it while holding
// the org's Linear credential lock. So a writer here reads the workspace,
// and writes or deletes under it, while holding that lock too
// (holdLinearWorkspace), taken after the user lock. Nothing takes the two in
// the other order.
//
// Neither lock is held across a call to Linear (the code exchange, the
// whoami, a revoke). In multi mode each holds a Postgres connection for as
// long as it is held, and a slow Linear would otherwise pin the pool. The
// cost is one race the locks no longer close: Linear has not been shown to
// keep two grants of one app and one person apart, so a revoke of the grant
// a user is replacing or withdrawing can land while their own new Connect is
// between its exchange and its store, and end that grant too. It is the
// same person racing themselves, and the result repairs itself: the dead
// grant is refused on its first refresh, removed with its identity, and the
// gate asks them to connect again.
//
// A Connect that does not complete writes nothing and redirects to the gate
// page with error set to one of:
//
//	state              the cookie is missing, expired, forged, or not this caller's
//	denied             Linear answered access_denied: the user declined consent,
//	                   or their workspace refused the app
//	connect_failed     any other OAuth error, the code exchange or the
//	                   identity check failed, or the org's OAuth app was
//	                   replaced or removed during the exchange
//	no_app             no Linear OAuth app resolves for the org
//	not_configured     the org has no Linear workspace
//	workspace_mismatch the Linear account authorized is in a different workspace

const (
	linearConnectStateCookieName = "tf_linear_connect_state"
	linearConnectStatePath       = "/api/linear/connect/"
)

// linearIdentityStatusResponse is the gate's read. connected reflects a stored
// credential ForUser can use, not merely an identity row; account is the bound
// Linear display name; workspace_url_key is the org's Linear workspace, ""
// when the org has no Linear and so nothing to gate; connect_available is
// whether a Linear OAuth app resolves for the org, which one-click Connect
// needs. disconnected_reason, set only when connected is false, says why a
// credential the user holds stopped working: linearDisconnectedRefused or
// linearDisconnectedAppRemoved. It is absent when the user never connected or
// the cause is not one of those.
type linearIdentityStatusResponse struct {
	Connected          bool   `json:"connected"`
	Account            string `json:"account,omitempty"`
	WorkspaceURLKey    string `json:"workspace_url_key"`
	ConnectAvailable   bool   `json:"connect_available"`
	DisconnectedReason string `json:"disconnected_reason,omitempty"`
}

// The disconnected_reason values.
const (
	// linearDisconnectedRefused is a refused marker: Linear refused the
	// Connect credential's refresh token, so the user revoked Triage
	// Factory's access in Linear or the grant expired.
	linearDisconnectedRefused = "refused"
	// linearDisconnectedAppRemoved is a usable Connect credential whose app
	// the org can no longer authenticate as. Derived on every read, never
	// stored, so restoring the same client ID brings the connection back.
	linearDisconnectedAppRemoved = "app_removed"
)

// linearIdentityAPIKeyRequest is the key-paste body.
type linearIdentityAPIKeyRequest struct {
	APIKey string `json:"api_key"`
}

// linearIdentityCaptureResponse is a successful paste: the bound account and
// the workspace it belongs to.
type linearIdentityCaptureResponse struct {
	Account         string `json:"account"`
	WorkspaceURLKey string `json:"workspace_url_key"`
}

// linearAccountName is the label a Linear user is shown by: their display
// name, else their name, else their id.
func linearAccountName(displayName, name, id string) string {
	switch {
	case displayName != "":
		return displayName
	case name != "":
		return name
	default:
		return id
	}
}

// handleLinearIdentityStatus reports whether the caller holds a usable Linear
// credential for the org's workspace. Any org member; read-only.
//
// GET /api/orgs/{org_id}/linear/identity
func (s *Server) handleLinearIdentityStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var (
		workspaceID string
		urlKey      string
		raw         string
		account     string
	)
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return err
		}
		workspaceID, urlKey = orgSet.LinearWorkspaceID, orgSet.LinearWorkspaceURLKey
		if workspaceID == "" {
			return nil
		}
		if raw, err = tx.Secrets.GetUser(ctx, orgID, userID, linear.UserTokenKey(workspaceID)); err != nil {
			return err
		}
		linearUserID, displayName, err := tx.Users.GetLinearIdentity(ctx, userID, workspaceID)
		if err != nil {
			return err
		}
		account = linearAccountName(displayName, "", linearUserID)
		return nil
	}); err != nil {
		internalError(w, "linear-identity", err)
		return
	}
	if workspaceID == "" {
		// No Linear in this org, so nothing to gate. urlKey is cleared with
		// the id, so the two cannot disagree here.
		writeJSON(w, http.StatusOK, linearIdentityStatusResponse{ConnectAvailable: s.resolveLinearAppSource(ctx, orgID) != linear.SourceNone})
		return
	}

	resp := linearIdentityStatusResponse{
		WorkspaceURLKey:  urlKey,
		ConnectAvailable: s.resolveLinearAppSource(ctx, orgID) != linear.SourceNone,
	}
	if raw != "" {
		cred, err := linear.ParseUserCredential(raw)
		switch {
		case err != nil:
			// Reported as not connected, so the gate asks for a new bind,
			// which replaces it.
			linearConnectLog.Warn("stored credential unparsable, reporting not connected", "org", orgID, "user", userID, "error", err)
		case cred.Refused:
			// Ahead of !Usable(), which a marker also matches and which
			// reports no reason.
			resp.DisconnectedReason = linearDisconnectedRefused
		case !cred.Usable():
		case cred.Method == linear.AuthMethodAPIKey:
			resp.Connected = true
		default:
			// A Connect refresh token is accepted only with the secret of
			// the app that minted it: usable while the org can still
			// authenticate as that app, dead to TF once it cannot.
			_, err := s.linearOAuthApps.ResolveClient(ctx, orgID, cred.ClientID)
			switch {
			case err == nil:
				resp.Connected = true
			case errors.Is(err, linear.ErrNoLinearOAuthApp):
				resp.DisconnectedReason = linearDisconnectedAppRemoved
			default:
				// Whether the credential still works is unknown, so the read
				// fails rather than send a connected user to the gate.
				internalError(w, "linear-identity", err)
				return
			}
		}
	}
	if resp.Connected {
		resp.Account = account
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleLinearIdentityAPIKey binds the caller's Linear access from a personal
// API key they paste: it validates the key live, refuses one from another
// workspace or one that acts as an app user, and stores it with the identity
// it validated as. A key Linear rejects is 422; Linear unreachable is 502.
//
// POST /api/orgs/{org_id}/linear/identity/api-key
func (s *Server) handleLinearIdentityAPIKey(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var req linearIdentityAPIKeyRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "A Linear API key is required.", Field: "api_key"})
		return
	}

	orgSet, err := s.readLinearOrgSettings(ctx, orgID, userID)
	if err != nil {
		internalError(w, "linear-identity", err)
		return
	}
	if orgSet.LinearWorkspaceID == "" {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonNotConfigured, Message: "Your workspace hasn't connected Linear yet. Ask an admin to connect it in Workspace Settings first.", Field: "api_key"})
		return
	}

	viewer, org, err := s.validateLinear(ctx, linear.APIKey(key))
	switch {
	case errors.Is(err, linear.ErrUnauthorized):
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "Linear rejected this API key.", Field: "api_key"})
		return
	case err != nil:
		writeLinearUpstream(w, orgID, "who this API key belongs to", err)
		return
	case viewer.IsApp:
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "This key acts as an app user, not a person. Paste your own personal API key.", Field: "api_key"})
		return
	case org.ID != orgSet.LinearWorkspaceID:
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "This key belongs to a different Linear workspace than the one connected to Triage Factory.", Field: "api_key"})
		return
	}

	cred := linear.UserCredential{Method: linear.AuthMethodAPIKey, Token: key}
	urlKey, err := s.storeLinearUserCredential(ctx, orgID, userID, org.ID, cred, viewer)
	switch {
	case errors.Is(err, errLinearNotOrgMember):
		// Removed while the key was being checked: the answer the route
		// gives any non-member.
		notFound(w, "org")
		return
	case errors.Is(err, errLinearNotConfigured):
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonNotConfigured, Message: "Your workspace hasn't connected Linear yet. Ask an admin to connect it in Workspace Settings first.", Field: "api_key"})
		return
	case errors.Is(err, errLinearWorkspaceChanged):
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "This key belongs to a different Linear workspace than the one connected to Triage Factory.", Field: "api_key"})
		return
	case err != nil:
		internalError(w, "linear-identity", err)
		return
	}
	linearConnectLog.Info("bound user, api key stored", "org", orgID, "user", userID, "linear_user", viewer.ID, "workspace", org.URLKey)
	writeJSON(w, http.StatusOK, linearIdentityCaptureResponse{
		Account:         linearAccountName(viewer.DisplayName, viewer.Name, viewer.ID),
		WorkspaceURLKey: urlKey,
	})
}

// handleLinearIdentityDelete withdraws the caller's own Linear credential for
// the org's workspace: the envelope and the identity row go together, and a
// Connect grant is revoked in Linear. Idempotent: with nothing stored it is a
// 200 and no audit row.
//
// DELETE /api/orgs/{org_id}/linear/identity
func (s *Server) handleLinearIdentityDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	release, err := s.linearUserCredentialLock.Lock(ctx, orgID, userID)
	if err != nil {
		internalError(w, "linear-identity", err)
		return
	}
	defer release()

	raw, urlKey, err := s.dropLinearUserCredential(ctx, orgID, userID)
	release()
	if err != nil {
		internalError(w, "linear-identity", err)
		return
	}
	if raw != "" {
		s.revokeLinearUserGrant(ctx, orgID, userID, raw)
		linearConnectLog.Info("unbound user", "org", orgID, "user", userID, "workspace", urlKey)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// handleLinearConnectStart sends the caller to Linear's consent page for the
// org's resolved app, with no actor, so the grant is to the user. Any org
// member binds their own access.
//
// GET /api/orgs/{org_id}/linear/connect/start?return_to=/some/path
func (s *Server) handleLinearConnectStart(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		notFound(w, "route")
		return
	}
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	// The callback refuses API tokens, so a Connect a token starts could
	// never finish.
	if httpx.TokenAuthFrom(r.Context()) != nil {
		forbidden(w, "connecting Linear needs a signed-in session; an API token cannot complete it")
		return
	}
	returnTo := normalizeReturnTo(r.URL.Query().Get("return_to"))

	orgSet, err := s.readLinearOrgSettings(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "linear-connect", err)
		return
	}
	if orgSet.LinearWorkspaceID == "" {
		redirectLinearConnect(w, r, orgID, returnTo, "not_configured")
		return
	}
	app, _, err := s.linearOAuthApps.Resolve(r.Context(), orgID)
	if errors.Is(err, linear.ErrNoLinearOAuthApp) {
		redirectLinearConnect(w, r, orgID, returnTo, "no_app")
		return
	}
	if err != nil {
		internalError(w, "linear-connect", err)
		return
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		internalError(w, "linear-connect", err)
		return
	}
	st := connectState{
		OrgID:     orgID,
		UserID:    userID,
		CSRF:      hex.EncodeToString(nonce),
		ReturnTo:  returnTo,
		ExpiresAt: timeNow().Add(10 * time.Minute).Unix(),
	}
	signed, err := st.sign(s.deployCfg.hmacKey)
	if err != nil {
		internalError(w, "linear-connect", err)
		return
	}
	http.SetCookie(w, s.linearConnectCookie(r, signed, 600))
	http.Redirect(w, r, linearoauth.ConnectAuthorizeURL(app, s.linearCallbackURL(linearConnectCallbackPath), st.CSRF), http.StatusFound)
}

// handleLinearConnectCallback completes Connect: it proves the cookie, the
// caller and the consent, exchanges the code, confirms the token acts as a
// person in the org's workspace, and stores the refresh token. A token this
// server minted and will not keep is revoked.
//
// The caller must be the user who started it, signed in with a session and
// still a member of the cookie's org. A Bearer API token is refused for the
// reason the install callback gives: this path names no org for the token's
// sealed org to be checked against.
//
// GET /api/linear/connect/callback?code=...&state=...
func (s *Server) handleLinearConnectCallback(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		notFound(w, "route")
		return
	}
	ctx := r.Context()

	// Cleared first, so a cookie is spent by any visit.
	cookie, cookieErr := r.Cookie(linearConnectStateCookieName)
	http.SetCookie(w, s.linearConnectCookie(r, "", -1))

	claims := httpx.ClaimsFrom(ctx)
	if claims == nil || claims.Subject == "" || httpx.TokenAuthFrom(ctx) != nil {
		writeUnauth(w)
		return
	}
	userID := claims.Subject
	fallbackOrg := httpx.OrgIDFrom(ctx)

	if cookieErr != nil {
		redirectLinearConnect(w, r, fallbackOrg, "", "state")
		return
	}
	cs, err := parseConnectState(cookie.Value, s.deployCfg.hmacKey)
	if err != nil {
		linearConnectLog.Warn("state cookie refused", "error", err)
		redirectLinearConnect(w, r, fallbackOrg, "", "state")
		return
	}
	if cs.CSRF == "" || r.URL.Query().Get("state") != cs.CSRF || cs.UserID != userID {
		redirectLinearConnect(w, r, fallbackOrg, "", "state")
		return
	}
	// From here the org is the cookie's, and membership is read again rather
	// than trusted from the start: minutes have passed. This read only
	// spares a removed member the round trip to Linear; the one that decides
	// is the store's, under the user's lock.
	orgID := cs.OrgID
	member, err := s.az.UserHasOrgAccess(ctx, userID, orgID)
	if err != nil {
		internalError(w, "linear-connect", err)
		return
	}
	if !member {
		redirectLinearConnect(w, r, fallbackOrg, "", "state")
		return
	}
	returnTo := cs.ReturnTo
	// The cookie's org is the one this writes into, so "Require SSO" is asked
	// about it here rather than left to withSession, which judged the cursor.
	// Redirected like every other refusal here, into the org's own page,
	// where the app offers the org's sign-in.
	gate, err := s.requestSSOGate(ctx, orgID)
	if err != nil {
		internalError(w, "linear-connect", err)
		return
	}
	if gate.Barred {
		redirectLinearConnect(w, r, orgID, returnTo, "sso_required")
		return
	}

	if oErr := r.URL.Query().Get("error"); oErr != "" {
		linearConnectLog.Warn("linear oauth error", "linear_error", oErr, "org", orgID)
		// Only access_denied is the user's or their workspace's refusal;
		// server_error, temporarily_unavailable and the rest are a failure
		// to connect, and say nothing about consent.
		code := "connect_failed"
		if oErr == "access_denied" {
			code = "denied"
		}
		redirectLinearConnect(w, r, orgID, returnTo, code)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		redirectLinearConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	orgSet, err := s.readLinearOrgSettings(ctx, orgID, userID)
	if err != nil {
		internalError(w, "linear-connect", err)
		return
	}
	if orgSet.LinearWorkspaceID == "" {
		redirectLinearConnect(w, r, orgID, returnTo, "not_configured")
		return
	}
	app, _, err := s.linearOAuthApps.Resolve(ctx, orgID)
	if errors.Is(err, linear.ErrNoLinearOAuthApp) {
		redirectLinearConnect(w, r, orgID, returnTo, "no_app")
		return
	}
	if err != nil {
		internalError(w, "linear-connect", err)
		return
	}

	tok, err := s.linearOAuthMinter.ExchangeCode(ctx, app, code, s.linearCallbackURL(linearConnectCallbackPath))
	if err != nil {
		linearConnectLog.Warn("code exchange failed", "org", orgID, "error", err)
		redirectLinearConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}
	viewer, org, err := s.validateLinear(ctx, linear.Bearer(tok.AccessToken))
	if err != nil {
		linearConnectLog.Warn("whoami under the connect token failed", "org", orgID, "error", err)
		s.revokeLinearTokens(ctx, orgID, app, tok)
		redirectLinearConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}
	if viewer.IsApp {
		// An app user here means the authorize URL gained actor=app on its
		// way through the browser. Storing it would make the app user's grant
		// this person's identity.
		linearConnectLog.Warn("connect token acts as an app user, not a person; refusing", "org", orgID, "user", userID)
		s.revokeLinearTokens(ctx, orgID, app, tok)
		redirectLinearConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}
	if org.ID != orgSet.LinearWorkspaceID {
		linearConnectLog.Warn("connected account is in another workspace; refusing", "org", orgID, "user", userID, "workspace", org.URLKey)
		s.revokeLinearTokens(ctx, orgID, app, tok)
		redirectLinearConnect(w, r, orgID, returnTo, "workspace_mismatch")
		return
	}

	grantID, err := newLinearInstallID()
	if err != nil {
		s.revokeLinearTokens(ctx, orgID, app, tok)
		internalError(w, "linear-connect", err)
		return
	}
	cred := linear.UserCredential{
		Method:       linear.AuthMethodConnectOAuth,
		RefreshToken: tok.RefreshToken,
		ClientID:     app.ClientID,
		GrantID:      grantID,
	}
	_, err = s.storeLinearUserCredential(ctx, orgID, userID, org.ID, cred, viewer)
	if err != nil {
		s.revokeLinearTokens(ctx, orgID, app, tok)
	}
	switch {
	case errors.Is(err, errLinearNotOrgMember):
		redirectLinearConnect(w, r, fallbackOrg, "", "state")
		return
	case errors.Is(err, errLinearNotConfigured):
		redirectLinearConnect(w, r, orgID, returnTo, "not_configured")
		return
	case errors.Is(err, errLinearWorkspaceChanged):
		redirectLinearConnect(w, r, orgID, returnTo, "workspace_mismatch")
		return
	case errors.Is(err, errLinearAppChanged):
		linearConnectLog.Warn("the org's linear app changed during the exchange; refusing", "org", orgID, "client_id", app.ClientID)
		redirectLinearConnect(w, r, orgID, returnTo, "connect_failed")
		return
	case err != nil:
		internalError(w, "linear-connect", err)
		return
	}

	linearConnectLog.Info("bound user, refresh token stored", "org", orgID, "user", userID, "linear_user", viewer.ID, "workspace", org.URLKey)
	dest := normalizeReturnTo(returnTo)
	if dest == "/" {
		dest = linearConnectHome(orgID)
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// errLinearNotOrgMember is a bind whose user is no longer a member of the
// org by the time it holds their credential lock.
var errLinearNotOrgMember = errors.New("linear: user is no longer a member of the org")

// errLinearNotConfigured is a bind that finds the org with no Linear
// workspace by the time it holds the workspace stable: it was unbound while
// the credential was being checked.
var errLinearNotConfigured = errors.New("linear: the org has no linear workspace")

// errLinearWorkspaceChanged is a bind that finds the org on a different
// Linear workspace than the one the credential was checked against.
var errLinearWorkspaceChanged = errors.New("linear: the org's linear workspace is not the credential's")

// holdLinearWorkspace takes the org's Linear credential lock, which every
// writer of org_settings.linear_workspace_* holds (the org's key bind, its
// install and its unbind), and reads the org's settings with read under it.
// The workspace it returns stays the org's until release. A per-user writer
// takes it after its user lock and holds it only around its own read and
// write, never across a call to Linear.
func (s *Server) holdLinearWorkspace(ctx context.Context, orgID string, read func(context.Context) (domain.OrgSettings, error)) (domain.OrgSettings, func(), error) {
	release, err := s.lockLinearCredential(ctx, orgID)
	if err != nil {
		return domain.OrgSettings{}, nil, err
	}
	orgSet, err := read(ctx)
	if err != nil {
		release()
		return domain.OrgSettings{}, nil, fmt.Errorf("read org settings: %w", err)
	}
	return orgSet, release, nil
}

// storeLinearUserCredential writes a credential checked against workspaceID,
// and the identity it validated as, with the access-log row, keyed by the
// org's workspace read under holdLinearWorkspace. It refuses when that is no
// longer workspaceID (errLinearWorkspaceChanged, or errLinearNotConfigured
// when the org has none), so a credential is never stored under a workspace
// it was not checked against. A Connect grant is refused the same way when
// the app that minted it no longer resolves (errLinearAppChanged): the org's
// app import and delete hold that lock too, so one that landed during the
// code exchange is seen here, and a grant TF cannot refresh is never stored.
// A Connect grant the new credential replaces is then revoked in Linear,
// unless the new one may be that same grant (see supersededLinearGrant),
// after both locks are released. It returns the workspace's URL key.
//
// Membership is read again here, under the user's credential lock, and a
// user no longer in the org is errLinearNotOrgMember. The route's own check ran before the
// lock, and the secret and identity policies key on the claims, not on
// membership, so a bind racing the user's removal could otherwise write
// after the removal's sweep (sweepLinearUserCredential) found nothing. Only
// the access-log row's policy checks membership, which holds only while that
// row is written in the same transaction, and turns the refusal into a
// failed request. The sweep takes the same lock after the removal commits,
// so a read under the lock either sees the removal or comes before a sweep
// that will see this write.
func (s *Server) storeLinearUserCredential(ctx context.Context, orgID, userID, workspaceID string, cred linear.UserCredential, viewer *auth.LinearUser) (string, error) {
	envelope, err := linear.MarshalUserCredential(cred)
	if err != nil {
		return "", err
	}
	key := linear.UserTokenKey(workspaceID)
	source := string(cred.Method)

	release, err := s.linearUserCredentialLock.Lock(ctx, orgID, userID)
	if err != nil {
		return "", err
	}
	defer release()

	member, err := s.az.UserHasOrgAccess(ctx, userID, orgID)
	if err != nil {
		return "", fmt.Errorf("check org membership: %w", err)
	}
	if !member {
		return "", errLinearNotOrgMember
	}

	orgSet, releaseWorkspace, err := s.holdLinearWorkspace(ctx, orgID, func(ctx context.Context) (domain.OrgSettings, error) {
		return s.readLinearOrgSettings(ctx, orgID, userID)
	})
	if err != nil {
		return "", err
	}
	defer releaseWorkspace()
	switch orgSet.LinearWorkspaceID {
	case workspaceID:
	case "":
		return "", errLinearNotConfigured
	default:
		return "", errLinearWorkspaceChanged
	}
	urlKey := orgSet.LinearWorkspaceURLKey
	if cred.Method == linear.AuthMethodConnectOAuth {
		_, err := s.linearOAuthApps.ResolveClient(ctx, orgID, cred.ClientID)
		if errors.Is(err, linear.ErrNoLinearOAuthApp) {
			return "", errLinearAppChanged
		}
		if err != nil {
			return "", fmt.Errorf("resolve linear oauth app %s: %w", cred.ClientID, err)
		}
	}

	restore, err := s.guardLocalUserSecretWrite(ctx, orgID, userID, key)
	if err != nil {
		return "", err
	}

	var (
		prior         string
		priorLinearID string
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var err error
		if prior, err = tx.Secrets.GetUser(ctx, orgID, userID, key); err != nil {
			return fmt.Errorf("read prior linear credential: %w", err)
		}
		if priorLinearID, _, err = tx.Users.GetLinearIdentity(ctx, userID, workspaceID); err != nil {
			return fmt.Errorf("read prior linear identity: %w", err)
		}
		if err := tx.Users.UpsertLinearIdentity(ctx, userID, workspaceID, viewer.ID, linearAccountName(viewer.DisplayName, viewer.Name, ""), source); err != nil {
			return fmt.Errorf("persist linear identity: %w", err)
		}
		if err := tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindLinearUser, "", urlKey),
		}); err != nil {
			return fmt.Errorf("audit credential set: %w", err)
		}
		// Last: in local mode the keychain write is outside the transaction,
		// so only the commit can fail after it, and the restore covers that.
		if err := tx.Secrets.PutUser(ctx, orgID, userID, key, envelope, "Linear user access token"); err != nil {
			return fmt.Errorf("store linear credential: %w", err)
		}
		return nil
	}); err != nil {
		if restore != nil {
			restore()
		}
		return "", err
	}
	releaseWorkspace()
	release()

	if old := supersededLinearGrant(prior, priorLinearID, cred, viewer.ID); old != "" {
		s.revokeLinearUserGrant(ctx, orgID, userID, old)
	}
	return urlKey, nil
}

// dropLinearUserCredential removes the caller's own credential and identity
// for the org's workspace, read under holdLinearWorkspace, with an audit row
// when there was a credential. It returns the envelope it removed, for the
// caller to revoke once the org's lock is released, and the workspace's URL
// key. The caller holds the user's credential lock, and revokes after
// releasing it.
func (s *Server) dropLinearUserCredential(ctx context.Context, orgID, userID string) (raw, urlKey string, err error) {
	orgSet, releaseWorkspace, err := s.holdLinearWorkspace(ctx, orgID, func(ctx context.Context) (domain.OrgSettings, error) {
		return s.readLinearOrgSettings(ctx, orgID, userID)
	})
	if err != nil {
		return "", "", err
	}
	defer releaseWorkspace()
	workspaceID, urlKey := orgSet.LinearWorkspaceID, orgSet.LinearWorkspaceURLKey
	if workspaceID == "" {
		return "", "", nil
	}
	key := linear.UserTokenKey(workspaceID)
	restore, err := s.guardLocalUserSecretWrite(ctx, orgID, userID, key)
	if err != nil {
		return "", "", err
	}
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var err error
		if raw, err = tx.Secrets.GetUser(ctx, orgID, userID, key); err != nil {
			return fmt.Errorf("read linear credential: %w", err)
		}
		if err := tx.Users.ClearLinearIdentity(ctx, userID, workspaceID); err != nil {
			return fmt.Errorf("clear linear identity: %w", err)
		}
		if raw == "" {
			return nil
		}
		if err := tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindLinearUser, "", urlKey),
		}); err != nil {
			return fmt.Errorf("audit credential removal: %w", err)
		}
		// Last: in local mode the keychain write is outside the transaction,
		// so only the commit can fail after it, and the restore covers that.
		if _, err := tx.Secrets.DeleteUser(ctx, orgID, userID, key); err != nil {
			return fmt.Errorf("delete linear credential: %w", err)
		}
		return nil
	}); err != nil {
		if restore != nil {
			restore()
		}
		return "", "", err
	}
	return raw, urlKey, nil
}

// supersededLinearGrant is the prior envelope when it is a Connect grant the
// new credential replaced and revoking it cannot touch the new one: the new
// credential is a key, or a Connect through a different app or as a different
// Linear user. A second Connect through the same app as the same person has
// not been shown to be a grant of its own, so revoking the old token could end
// the one just stored; it is left alone, forgotten with its envelope.
func supersededLinearGrant(prior, priorLinearID string, next linear.UserCredential, nextLinearID string) string {
	if prior == "" {
		return ""
	}
	old, err := linear.ParseUserCredential(prior)
	if err != nil || old.Method != linear.AuthMethodConnectOAuth || old.RefreshToken == "" {
		return ""
	}
	if next.Method == linear.AuthMethodConnectOAuth && old.ClientID == next.ClientID && priorLinearID == nextLinearID {
		return ""
	}
	return prior
}

// revokeLinearUserGrant revokes a Connect envelope's refresh token in Linear,
// with the app that minted it, which need not be the app the org resolves to
// now. Best-effort: TF has already forgotten the token, so a failure leaves
// only a grant nothing uses, which the user ends from their Linear settings.
// That includes a grant minted by an app whose secret TF no longer holds (an
// org app since replaced or deleted): Linear accepts a revoke only from the
// app that minted the token. An API key is not revoked: it is the user's own,
// and they may use it elsewhere.
func (s *Server) revokeLinearUserGrant(ctx context.Context, orgID, userID, raw string) {
	ctx = context.WithoutCancel(ctx)
	cred, err := linear.ParseUserCredential(raw)
	if err != nil || cred.Method != linear.AuthMethodConnectOAuth || cred.RefreshToken == "" {
		return
	}
	app, err := s.linearOAuthApps.ResolveClient(ctx, orgID, cred.ClientID)
	if err != nil {
		linearConnectLog.Warn("the org can no longer authenticate as the app that minted the user's grant; it stays until they remove it in Linear",
			"org", orgID, "user", userID, "client_id", cred.ClientID, "error", err)
		return
	}
	if err := s.linearOAuthMinter.Revoke(ctx, app, cred.RefreshToken, linearoauth.HintRefreshToken); err != nil {
		linearConnectLog.Warn("revoke user grant failed; it stays until they remove it in Linear", "org", orgID, "user", userID, "error", err)
	}
}

// sweepLinearUserCredential removes a user's Linear credential and identity
// for the org's workspace when they leave the org, and revokes a Connect
// grant. The removal runs after the membership is gone, as the departing user
// (SyntheticClaimsWithTx): the rows are theirs, and the caller is the admin
// removing them or the user leaving. The workspace is read under
// holdLinearWorkspace, the envelope and the identity go in one transaction,
// so neither outlives the other, and the revoke runs once both locks are
// released. It must run after the removal commits: every bind reads
// membership under the same lock (storeLinearUserCredential), so one that
// read it before the removal has committed its write by the time this takes
// the lock, and one after refuses. Best-effort, like the rest of the
// removal's deprovisioning; a failure is returned for the caller to log.
func (s *Server) sweepLinearUserCredential(ctx context.Context, orgID, userID string) error {
	release, err := s.linearUserCredentialLock.Lock(ctx, orgID, userID)
	if err != nil {
		return err
	}
	defer release()
	orgSet, releaseWorkspace, err := s.holdLinearWorkspace(ctx, orgID, func(ctx context.Context) (domain.OrgSettings, error) {
		return s.orgs.GetSettingsSystem(ctx, orgID)
	})
	if err != nil {
		return err
	}
	defer releaseWorkspace()
	workspaceID := orgSet.LinearWorkspaceID
	if workspaceID == "" {
		return nil
	}
	key := linear.UserTokenKey(workspaceID)
	var raw string
	if err := s.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var err error
		if raw, err = tx.Secrets.GetUser(ctx, orgID, userID, key); err != nil {
			return fmt.Errorf("read linear credential: %w", err)
		}
		if err := tx.Users.ClearLinearIdentity(ctx, userID, workspaceID); err != nil {
			return fmt.Errorf("clear linear identity: %w", err)
		}
		if raw == "" {
			return nil
		}
		if _, err := tx.Secrets.DeleteUser(ctx, orgID, userID, key); err != nil {
			return fmt.Errorf("delete linear credential: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	releaseWorkspace()
	release()
	if raw != "" {
		s.revokeLinearUserGrant(ctx, orgID, userID, raw)
	}
	return nil
}

// readLinearOrgSettings reads the org's settings row under the caller's
// claims, for the workspace the user's credential is keyed by.
func (s *Server) readLinearOrgSettings(ctx context.Context, orgID, userID string) (domain.OrgSettings, error) {
	var orgSet domain.OrgSettings
	err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var err error
		orgSet, err = tx.Orgs.GetSettings(ctx, orgID)
		return err
	})
	return orgSet, err
}

// guardLocalUserSecretWrite is guardLocalSecretWrite for one per-user key:
// in local mode it snapshots the user's key and returns the restore to run
// when the transaction writing it fails, since the keychain write survives
// the rollback. The caller holds the user's credential lock, which every
// writer of the key takes. Multi mode returns a nil restore.
func (s *Server) guardLocalUserSecretWrite(ctx context.Context, orgID, userID, key string) (restore func(), err error) {
	if runmode.Current() != runmode.ModeLocal {
		return nil, nil
	}
	prior, err := s.secrets.GetUser(ctx, orgID, userID, key)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", key, err)
	}
	ctx = context.WithoutCancel(ctx)
	return func() {
		var err error
		if prior == "" {
			_, err = s.secrets.DeleteUser(ctx, orgID, userID, key)
		} else {
			err = s.secrets.PutUser(ctx, orgID, userID, key, prior, "Linear user access token")
		}
		if err != nil {
			serverLog.Error("restore user secret failed; the stored credential may not match what the user had", "org", orgID, "user", userID, "key", key, "error", err)
		}
	}, nil
}

// linearConnectHome is where a Connect with no return_to lands: the org's
// home.
func linearConnectHome(orgID string) string {
	if runmode.Current() == runmode.ModeLocal {
		return "/"
	}
	return "/orgs/" + orgID
}

// linearConnectCookie builds the Connect state cookie, the set and the clear.
func (s *Server) linearConnectCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     linearConnectStateCookieName,
		Value:    value,
		Path:     linearConnectStatePath,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// redirectLinearConnect bounces a Connect that did not complete to the gate
// page, naming the reason and keeping the caller's return_to. A refusal that
// could not learn an org lands on the root.
func redirectLinearConnect(w http.ResponseWriter, r *http.Request, orgID, returnTo, code string) {
	if orgID == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	q := url.Values{}
	q.Set("error", code)
	if rt := normalizeReturnTo(returnTo); rt != "/" {
		q.Set("return_to", rt)
	}
	http.Redirect(w, r, "/orgs/"+orgID+"/connect-linear?"+q.Encode(), http.StatusFound)
}
