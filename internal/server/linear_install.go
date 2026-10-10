package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/linearoauth"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The workspace install ceremony: a Linear workspace admin installs the org's
// resolved Linear OAuth app with actor=app, and the org's Linear credential
// becomes the app user that install creates. TF polls and acts as that app
// user, which holds no human's key and takes no seat.
//
//   GET /api/orgs/{org_id}/linear/install/start?return_to=
//   GET /api/linear/install/callback?code=&state=
//
// The callback is static: Linear matches redirect URIs exactly, and one app
// must be able to serve every org, so the org and user the ceremony belongs to
// travel in an HMAC-signed state cookie rather than in the path. Only the CSRF
// nonce passes through Linear.
//
// The ceremony ends in a write to an org credential, so both legs require an
// org admin. Every outcome short of a completed install writes nothing and
// redirects back with linear_error set to one of:
//
//	state           the cookie is missing, expired, forged, or not this caller's
//	denied          the admin declined consent in Linear
//	no_app          no Linear OAuth app resolves for the org
//	install_failed  the code exchange or the identity check failed
//	workspace_taken the workspace is installed in another org

const (
	linearInstallStateCookieName = "tf_linear_install_state"
	linearInstallStatePath       = "/api/linear/install/"
)

// linearInstallReturn is where a finished ceremony lands: the caller's
// return_to when it named one, the org's settings page otherwise, with key set
// to value on its query. A refusal that could not learn an org lands on the
// root.
func linearInstallReturn(orgID, returnTo, key, value string) string {
	dest := normalizeReturnTo(returnTo)
	if dest == "/" && orgID != "" {
		dest = settingsRedirectPath(orgID)
	}
	u, err := url.Parse(dest)
	if err != nil {
		u = &url.URL{Path: settingsRedirectPath(orgID)}
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// redirectLinearInstallError bounces an install that did not complete back to
// where it started, naming the reason.
func redirectLinearInstallError(w http.ResponseWriter, r *http.Request, orgID, returnTo, code string) {
	http.Redirect(w, r, linearInstallReturn(orgID, returnTo, "linear_error", code), http.StatusFound)
}

// linearInstallCookie builds the ceremony cookie, the set and the clear.
func (s *Server) linearInstallCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     linearInstallStateCookieName,
		Value:    value,
		Path:     linearInstallStatePath,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// handleLinearInstallStart sends an org admin to Linear's consent page for the
// org's resolved app, with actor=app. An org whose credential is an API key
// may start one: the callback replaces the key in the same transaction that
// stores the install, so the org is never without a credential.
//
// GET /api/orgs/{org_id}/linear/install/start?return_to=/some/path
func (s *Server) handleLinearInstallStart(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		notFound(w, "route")
		return
	}
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	// The callback refuses API tokens, so a ceremony a token starts could
	// never finish.
	if httpx.TokenAuthFrom(r.Context()) != nil {
		forbidden(w, "installing Linear needs a signed-in session; an API token cannot complete it")
		return
	}
	returnTo := normalizeReturnTo(r.URL.Query().Get("return_to"))

	app, _, err := s.linearOAuthApps.Resolve(r.Context(), orgID)
	if errors.Is(err, linear.ErrNoLinearOAuthApp) {
		redirectLinearInstallError(w, r, orgID, returnTo, "no_app")
		return
	}
	if err != nil {
		internalError(w, "linear-install", err)
		return
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		internalError(w, "linear-install", err)
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
		internalError(w, "linear-install", err)
		return
	}
	http.SetCookie(w, s.linearInstallCookie(r, signed, 600))
	http.Redirect(w, r, linearoauth.InstallAuthorizeURL(app, s.linearCallbackURL(linearInstallCallbackPath), st.CSRF), http.StatusFound)
}

// handleLinearInstallCallback completes the ceremony: it proves the cookie,
// the caller and the consent, exchanges the code, confirms the token acts as
// an app user, and stores the install.
//
// The caller must be the admin who started it, signed in with a session. A
// Bearer API token is refused: its org is sealed at mint and checked against
// the path everywhere else, and this path names no org, so a token could
// otherwise complete a ceremony for an org it was never minted for. The
// ceremony is browser state end to end, so no token holder can have started
// one.
//
// GET /api/linear/install/callback?code=...&state=...
func (s *Server) handleLinearInstallCallback(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		notFound(w, "route")
		return
	}
	ctx := r.Context()

	// Cleared first, so a cookie is spent by any visit and can never be
	// replayed, whichever arm below answers.
	cookie, cookieErr := r.Cookie(linearInstallStateCookieName)
	http.SetCookie(w, s.linearInstallCookie(r, "", -1))

	claims := httpx.ClaimsFrom(ctx)
	if claims == nil || claims.Subject == "" || httpx.TokenAuthFrom(ctx) != nil {
		writeUnauth(w)
		return
	}
	userID := claims.Subject
	fallbackOrg := httpx.OrgIDFrom(ctx)

	if cookieErr != nil {
		redirectLinearInstallError(w, r, fallbackOrg, "", "state")
		return
	}
	cs, err := parseConnectState(cookie.Value, s.deployCfg.hmacKey)
	if err != nil {
		linearInstallLog.Warn("state cookie refused", "error", err)
		redirectLinearInstallError(w, r, fallbackOrg, "", "state")
		return
	}
	if cs.CSRF == "" || r.URL.Query().Get("state") != cs.CSRF || cs.UserID != userID {
		redirectLinearInstallError(w, r, fallbackOrg, "", "state")
		return
	}
	// From here the org is the cookie's, and the role is read again rather
	// than trusted from the start: minutes have passed.
	orgID := cs.OrgID
	isAdmin, err := s.az.UserIsOrgAdmin(ctx, userID, orgID)
	if err != nil {
		internalError(w, "linear-install", err)
		return
	}
	if !isAdmin {
		redirectLinearInstallError(w, r, fallbackOrg, "", "state")
		return
	}
	returnTo := cs.ReturnTo

	if oErr := r.URL.Query().Get("error"); oErr != "" {
		linearInstallLog.Warn("linear oauth error", "linear_error", oErr, "org", orgID)
		redirectLinearInstallError(w, r, orgID, returnTo, "denied")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		redirectLinearInstallError(w, r, orgID, returnTo, "install_failed")
		return
	}

	app, _, err := s.linearOAuthApps.Resolve(ctx, orgID)
	if errors.Is(err, linear.ErrNoLinearOAuthApp) {
		redirectLinearInstallError(w, r, orgID, returnTo, "no_app")
		return
	}
	if err != nil {
		internalError(w, "linear-install", err)
		return
	}

	tok, err := s.linearOAuthMinter.ExchangeCode(ctx, app, code, s.linearCallbackURL(linearInstallCallbackPath))
	if err != nil {
		linearInstallLog.Warn("code exchange failed", "org", orgID, "error", err)
		redirectLinearInstallError(w, r, orgID, returnTo, "install_failed")
		return
	}

	viewer, org, err := s.validateLinear(ctx, linear.Bearer(tok.AccessToken))
	if err != nil {
		linearInstallLog.Warn("whoami under the install token failed", "org", orgID, "error", err)
		s.revokeLinearTokens(ctx, orgID, app, tok)
		redirectLinearInstallError(w, r, orgID, returnTo, "install_failed")
		return
	}
	if !viewer.IsApp {
		// A person here means the authorize URL lost actor=app on its way
		// through the browser. Storing it would make a human's grant the org's
		// credential, so the grant is revoked instead.
		linearInstallLog.Warn("install token acts as a person, not an app user; refusing", "org", orgID)
		s.revokeLinearTokens(ctx, orgID, app, tok)
		redirectLinearInstallError(w, r, orgID, returnTo, "install_failed")
		return
	}

	superseded, err := s.storeLinearInstall(ctx, orgID, userID, app, tok, viewer, org)
	if errors.Is(err, db.ErrWorkspaceInstalledElsewhere) {
		linearInstallLog.Warn("workspace is installed in another org; refusing", "org", orgID, "workspace", org.URLKey)
		s.revokeLinearTokens(ctx, orgID, app, tok)
		redirectLinearInstallError(w, r, orgID, returnTo, "workspace_taken")
		return
	}
	if errors.Is(err, errLinearAppChanged) {
		linearInstallLog.Warn("the org's linear app changed during the install; refusing", "org", orgID, "client_id", app.ClientID)
		s.revokeLinearTokens(ctx, orgID, app, tok)
		redirectLinearInstallError(w, r, orgID, returnTo, "install_failed")
		return
	}
	if err != nil {
		s.revokeLinearTokens(ctx, orgID, app, tok)
		internalError(w, "linear-install", err)
		return
	}
	if superseded != "" {
		// The install this one replaced. Its grant is dead to TF either way;
		// revoking it is a courtesy to the workspace, so a failure only logs.
		if err := s.linearOAuthMinter.Revoke(ctx, app, superseded, linearoauth.HintRefreshToken); err != nil {
			linearInstallLog.Warn("revoke superseded install token failed", "org", orgID, "error", err)
		}
	}

	linearInstallLog.Info("installed linear app",
		"org", orgID, "user", userID, "workspace", org.URLKey, "app_user", viewer.ID, "client_id", app.ClientID)
	s.kickLinearChanged(r, orgID)
	http.Redirect(w, r, linearInstallReturn(orgID, returnTo, "linear", "installed"), http.StatusFound)
}

// errLinearAppChanged is an install whose OAuth app was replaced or removed
// between the code exchange and the store. Its refresh token needs the secret
// of the app that minted it, which is no longer the org's, so it is refused
// rather than stored unable to refresh.
var errLinearAppChanged = errors.New("linear: the oauth app changed during the install")

// newLinearInstallID mints an install's id: random, so two installs of one
// org can never share one.
func newLinearInstallID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// storeLinearInstall writes a completed install: the org_linear_installs row,
// and in one transaction the envelope, the app_install marker, the dropped
// API key, the bound-as record, the workspace columns and the change log. It
// returns the refresh token of an install this one replaced, for the caller
// to revoke, when the same app minted it for a different app user, meaning a
// different workspace. A re-install into the same workspace is answered by
// the same app user, and Linear has not been shown to keep the two grants
// apart, so revoking the old token could end the install just made; it is
// left alone, forgotten with its envelope.
//
// It holds the org's Linear credential lock throughout, so what it reads
// first — the app the code was exchanged against, and the install row it is
// about to replace — is still true when it writes, on every pod: an app
// change since the exchange is errLinearAppChanged, and a failed store puts
// back the row it read.
//
// The installs row is written on the admin pool in Postgres, so it commits
// ahead of the transaction rather than with it; a transaction that then fails
// puts back the row that was there (see restoreLinearInstall). On SQLite the
// row is part of the transaction and its rollback already did. A process
// that dies between the two on Postgres leaves the new row live beside the
// old credential, holding its workspace until the org's next install
// replaces it or a disconnect releases it.
func (s *Server) storeLinearInstall(ctx context.Context, orgID, userID string, app linear.OAuthApp, tok linearoauth.Token, viewer *auth.LinearUser, org *auth.LinearOrganization) (superseded string, err error) {
	// The app user's name under the same bound-as record an API key's person
	// is stored in, so the access status reads one record for either shape.
	binding, err := json.Marshal(linearBinding{Name: viewer.Name, DisplayName: viewer.DisplayName, WorkspaceName: org.Name})
	if err != nil {
		return "", fmt.Errorf("marshal linear binding: %w", err)
	}
	installID, err := newLinearInstallID()
	if err != nil {
		return "", fmt.Errorf("mint linear install id: %w", err)
	}

	release, err := s.lockLinearCredential(ctx, orgID)
	if err != nil {
		return "", err
	}
	defer release()

	current, _, err := s.linearOAuthApps.Resolve(ctx, orgID)
	switch {
	case errors.Is(err, linear.ErrNoLinearOAuthApp):
		return "", errLinearAppChanged
	case err != nil:
		return "", err
	case current.ClientID != app.ClientID:
		return "", errLinearAppChanged
	}
	prior, err := s.linearInstalls.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("read linear install: %w", err)
	}

	restore, unlock, err := s.guardLocalLinearWrite(ctx, orgID)
	if err != nil {
		return "", err
	}
	defer unlock()

	var (
		rowWritten bool
		priorEnv   string
	)
	err = s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if _, err := tx.LinearInstalls.UpsertSystem(ctx, domain.OrgLinearInstall{
			OrgID:             orgID,
			InstallID:         installID,
			WorkspaceID:       org.ID,
			WorkspaceURLKey:   org.URLKey,
			AppUserID:         viewer.ID,
			AppClientID:       app.ClientID,
			InstalledByUserID: userID,
			InstalledAt:       timeNow().UTC(),
		}); err != nil {
			return err
		}
		rowWritten = true
		envelope, err := linear.MarshalInstallCredential(linear.InstallCredential{
			InstallID:    installID,
			WorkspaceID:  org.ID,
			AppUserID:    viewer.ID,
			RefreshToken: tok.RefreshToken,
			ClientID:     app.ClientID,
		})
		if err != nil {
			return err
		}
		if priorEnv, err = tx.Secrets.Get(ctx, orgID, integrations.KeyLinearAppInstall); err != nil {
			return fmt.Errorf("read prior linear install credential: %w", err)
		}
		// Keys in ClearLinear's order (api key, marker, install, bound-as), so
		// two writers of the same rows never take their locks in opposite
		// orders.
		if err := integrations.ClearLinearOtherScheme(ctx, tx.Secrets, orgID, linear.AuthMethodAppInstall); err != nil {
			return fmt.Errorf("clear linear api key: %w", err)
		}
		for _, kv := range [][2]string{
			{integrations.KeyLinearAuthMethod, string(linear.AuthMethodAppInstall)},
			{integrations.KeyLinearAppInstall, envelope},
			{integrations.KeyLinearBoundAs, string(binding)},
		} {
			if err := tx.Secrets.Put(ctx, orgID, kv[0], kv[1], ""); err != nil {
				return fmt.Errorf("store %s: %w", kv[0], err)
			}
		}
		if _, err := tx.Orgs.SetLinearWorkspace(ctx, orgID, org.ID, org.URLKey); err != nil {
			return fmt.Errorf("save linear workspace: %w", err)
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  linearInstallAccessDetail(org.URLKey, viewer.ID),
		})
	})
	if err != nil {
		if restore != nil {
			restore()
		}
		if rowWritten {
			s.restoreLinearInstall(ctx, orgID, prior, installID)
		}
		return "", err
	}

	if priorEnv != "" {
		if old, perr := linear.ParseInstallCredential(priorEnv); perr == nil && old.ClientID == app.ClientID && old.AppUserID != viewer.ID && old.RefreshToken != tok.RefreshToken {
			superseded = old.RefreshToken
		}
	}
	return superseded, nil
}

// restoreLinearInstall puts back the install row a failed store replaced, as
// it was: live, or removed with its reason, so a revoked install still reads
// as revoked. When there was none, the new install (installID) is marked
// failed so it holds no workspace. The caller still holds the org's Linear
// credential lock, so prior is still what was there. Best-effort: it runs
// after a failure already being reported, so its own failure only logs.
func (s *Server) restoreLinearInstall(ctx context.Context, orgID string, prior *domain.OrgLinearInstall, installID string) {
	ctx = context.WithoutCancel(ctx)
	var err error
	if prior != nil {
		_, err = s.linearInstalls.UpsertSystem(ctx, *prior)
	} else {
		_, err = s.linearInstalls.MarkRemovedSystem(ctx, orgID, installID, domain.LinearInstallRemovedFailed)
	}
	if err != nil {
		linearInstallLog.Error("restore linear install row after a failed install failed", "org", orgID, "error", err)
	}
}

// revokeLinearTokens revokes both halves of a pair this server minted and will
// not keep. Best-effort.
func (s *Server) revokeLinearTokens(ctx context.Context, orgID string, app linear.OAuthApp, tok linearoauth.Token) {
	ctx = context.WithoutCancel(ctx)
	if err := s.linearOAuthMinter.Revoke(ctx, app, tok.AccessToken, linearoauth.HintAccessToken); err != nil {
		linearInstallLog.Warn("revoke minted access token failed", "org", orgID, "error", err)
	}
	if err := s.linearOAuthMinter.Revoke(ctx, app, tok.RefreshToken, linearoauth.HintRefreshToken); err != nil {
		linearInstallLog.Warn("revoke minted refresh token failed", "org", orgID, "error", err)
	}
}

// linearInstallAccessDetail is the change-log detail of an install: the
// credential kind, the workspace as its name, the shape, and the app user.
func linearInstallAccessDetail(workspace, appUserID string) string {
	b, _ := json.Marshal(struct {
		Kind      string `json:"kind"`
		Name      string `json:"name,omitempty"`
		Shape     string `json:"shape"`
		Workspace string `json:"workspace"`
		AppUserID string `json:"app_user_id"`
	}{
		Kind:      domain.CredentialKindLinearOrg,
		Name:      workspace,
		Shape:     string(linear.AuthMethodAppInstall),
		Workspace: workspace,
		AppUserID: appUserID,
	})
	return string(b)
}
