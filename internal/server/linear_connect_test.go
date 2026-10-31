package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/linear/linearoauth"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const (
	linearOrgKey        = "lin_api_org"
	linearUserKey       = "lin_api_ada"
	acmeWorkspaceID     = "org-acme"
	acmeWorkspaceURLKey = "acme"
)

func linearIdentityPath() string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/linear/identity"
}

func linearConnectStartPath(returnTo string) string {
	p := "/api/orgs/" + runmode.LocalDefaultOrgID + "/linear/connect/start"
	if returnTo != "" {
		p += "?return_to=" + url.QueryEscape(returnTo)
	}
	return p
}

func linearConnectCallback(params url.Values) string {
	return linearConnectCallbackPath + "?" + params.Encode()
}

// linearConnectRig is the install rig with a person answering whoami, the
// user token cache pointed at the fake OAuth endpoint, and the org bound to
// the Acme workspace with an API key.
type linearConnectRig struct {
	*linearInstallRig
	// clientID is the app Connect is expected to authorize against.
	clientID string
}

func newLinearConnectRig(t *testing.T) *linearConnectRig {
	t.Helper()
	base := newLinearInstallRig(t)
	base.fake.set("person")
	base.s.linearResolver = linear.NewResolverWithOAuth(base.stores.Secrets, base.stores.Orgs,
		eventsource.ResolverGate(base.stores.OrgEventSources, eventsource.KindLinear),
		linearoauth.NewTokenCache(base.s.linearOAuthMinter, base.s.linearOAuthApps, base.stores.Secrets, base.stores.LinearInstalls, base.s.linearCredentialLock),
		linearoauth.NewUserTokenCache(base.s.linearOAuthMinter, base.s.linearOAuthApps, base.stores.Secrets, base.stores.Tx, base.s.linearUserCredentialLock))
	r := &linearConnectRig{linearInstallRig: base, clientID: "lin-client-1"}
	r.bind(t, linearOrgKey)
	return r
}

// useDeploymentApp gives the server a deployment app beside the org's own,
// and the resolver the user token cache built over it.
func (r *linearConnectRig) useDeploymentApp(deployment linear.OAuthApp) {
	r.s.linearOAuthApps = linear.NewOAuthAppResolver(r.stores.LinearApps, r.stores.Secrets, deployment)
	r.s.linearResolver = linear.NewResolverWithOAuth(r.stores.Secrets, r.stores.Orgs,
		eventsource.ResolverGate(r.stores.OrgEventSources, eventsource.KindLinear), nil,
		linearoauth.NewUserTokenCache(r.s.linearOAuthMinter, r.s.linearOAuthApps, r.stores.Secrets, r.stores.Tx, r.s.linearUserCredentialLock))
}

// lockHeldElsewhere reports whether the user's credential lock is held now,
// by trying to take it briefly.
func (r *linearConnectRig) lockHeldElsewhere(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	release, err := r.s.linearUserCredentialLock.Lock(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err != nil {
		return true
	}
	release()
	return false
}

func (r *linearConnectRig) userSecret(t *testing.T) string {
	t.Helper()
	v, err := r.stores.Secrets.GetUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, linear.UserTokenKey(acmeWorkspaceID))
	if err != nil {
		t.Fatalf("read user credential: %v", err)
	}
	return v
}

func (r *linearConnectRig) userCredential(t *testing.T) linear.UserCredential {
	t.Helper()
	raw := r.userSecret(t)
	if raw == "" {
		t.Fatal("no user credential stored")
	}
	cred, err := linear.ParseUserCredential(raw)
	if err != nil {
		t.Fatalf("parse user credential: %v", err)
	}
	return cred
}

// identity is the user's identity row in the Acme workspace, source included;
// all "" when there is none.
func (r *linearConnectRig) identity(t *testing.T) (linearUserID, displayName, source string) {
	t.Helper()
	row := r.s.db.QueryRow(`SELECT linear_user_id, COALESCE(display_name, ''), source FROM user_linear_identities WHERE user_id = ? AND workspace_id = ?`,
		runmode.LocalDefaultUserID, acmeWorkspaceID)
	if err := row.Scan(&linearUserID, &displayName, &source); err != nil && !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("read identity: %v", err)
	}
	return linearUserID, displayName, source
}

// userCredentialRows is the change log's personal Linear credential rows,
// newest first.
func (r *linearConnectRig) userCredentialRows(t *testing.T) []domain.AccessChange {
	t.Helper()
	rows, _, err := r.stores.AccessChangeLog.ListByOrg(t.Context(), runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list access log: %v", err)
	}
	var out []domain.AccessChange
	for _, row := range rows {
		var d struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal([]byte(row.DetailJSON), &d)
		if d.Kind == domain.CredentialKindLinearUser {
			out = append(out, row)
		}
	}
	return out
}

// userCredentialActions is the actions of userCredentialRows, sorted: two
// rows written in the same second have no order between them.
func (r *linearConnectRig) userCredentialActions(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, row := range r.userCredentialRows(t) {
		out = append(out, row.Action)
	}
	slices.Sort(out)
	return out
}

func (r *linearConnectRig) status(t *testing.T) linearIdentityStatusResponse {
	t.Helper()
	rec := doJSON(t, r.s, http.MethodGet, linearIdentityPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET linear identity: %d: %s", rec.Code, rec.Body.String())
	}
	var got linearIdentityStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func (r *linearConnectRig) pasteKey(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, r.s, http.MethodPost, linearIdentityPath()+"/api-key", map[string]any{"api_key": key})
}

// startConnect runs the start leg and returns the state cookie and the nonce
// Linear would echo back.
func (r *linearConnectRig) startConnect(t *testing.T, returnTo string) (*http.Cookie, string) {
	t.Helper()
	rec := r.get(t, linearConnectStartPath(returnTo))
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if loc.Host != "linear.app" || loc.Path != "/oauth/authorize" {
		t.Fatalf("start redirected to %s, want Linear's authorize page", loc)
	}
	q := loc.Query()
	if q.Has("actor") || q.Get("scope") != "read,write" || q.Get("client_id") != r.clientID ||
		q.Get("redirect_uri") != "http://localhost:3000/api/linear/connect/callback" {
		t.Errorf("authorize query = %v, want the user actor, read,write, the org's app and the static callback", q)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == linearConnectStateCookieName {
			if c.Path != linearConnectStatePath || !c.HttpOnly || c.MaxAge != 600 {
				t.Errorf("state cookie = %+v, want HttpOnly, path %s, ten minutes", c, linearConnectStatePath)
			}
			return c, q.Get("state")
		}
	}
	t.Fatal("start set no state cookie")
	return nil, ""
}

// connect runs Connect through a successful callback.
func (r *linearConnectRig) connect(t *testing.T) {
	t.Helper()
	cookie, state := r.startConnect(t, "")
	rec := r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d to %q, want 302 to the org's home: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

// assertNoUserCredential pins the refusal contract: nothing stored in the
// user's scope, no identity, no audit row.
func (r *linearConnectRig) assertNoUserCredential(t *testing.T) {
	t.Helper()
	if v := r.userSecret(t); v != "" {
		t.Errorf("user credential = %q, want none", v)
	}
	if id, _, _ := r.identity(t); id != "" {
		t.Errorf("identity = %q, want none", id)
	}
	if rows := r.userCredentialRows(t); len(rows) != 0 {
		t.Errorf("personal credential rows = %+v, want none", rows)
	}
}

// expectConnectError asserts a refused Connect landed on the gate page with
// code, keeping returnTo.
func expectConnectError(t *testing.T, rec *httptest.ResponseRecorder, code, returnTo string) {
	t.Helper()
	loc := expectRedirect(t, rec, "error", code)
	if want := "/orgs/" + runmode.LocalDefaultOrgID + "/connect-linear"; loc.Path != want {
		t.Errorf("refusal landed on %s, want %s", loc.Path, want)
	}
	if got := loc.Query().Get("return_to"); got != returnTo {
		t.Errorf("return_to = %q, want %q", got, returnTo)
	}
}

func TestLinearIdentity_StatusWithoutLinear(t *testing.T) {
	r := newLinearInstallRig(t)
	rec := doJSON(t, r.s, http.MethodGet, linearIdentityPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d: %s", rec.Code, rec.Body.String())
	}
	var got linearIdentityStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Connected || got.WorkspaceURLKey != "" || !got.ConnectAvailable {
		t.Errorf("status = %+v, want not connected, no workspace (nothing to gate), Connect available", got)
	}
}

func TestLinearIdentity_APIKeyBinds(t *testing.T) {
	r := newLinearConnectRig(t)
	if st := r.status(t); st.Connected || st.WorkspaceURLKey != acmeWorkspaceURLKey || !st.ConnectAvailable {
		t.Fatalf("status before = %+v, want not connected in acme with Connect available", st)
	}

	rec := r.pasteKey(t, "  "+linearUserKey+"  ")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST api-key: %d: %s", rec.Code, rec.Body.String())
	}
	var got linearIdentityCaptureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != (linearIdentityCaptureResponse{Account: "Ada", WorkspaceURLKey: acmeWorkspaceURLKey}) {
		t.Errorf("response = %+v, want Ada in acme", got)
	}

	want := linear.UserCredential{Method: linear.AuthMethodAPIKey, Token: linearUserKey}
	if cred := r.userCredential(t); cred != want {
		t.Errorf("user credential = %+v, want %+v", cred, want)
	}
	// The org's own credential is untouched, and nothing landed in the org
	// scope under the user's key.
	if v := r.secret(t, integrations.KeyLinearAPIKey); v != linearOrgKey {
		t.Errorf("org key = %q, want %q", v, linearOrgKey)
	}
	if v := r.secret(t, linear.UserTokenKey(acmeWorkspaceID)); v != "" {
		t.Errorf("org-scope %s = %q, want nothing", linear.UserTokenKey(acmeWorkspaceID), v)
	}
	if id, name, source := r.identity(t); id != "lu-ada" || name != "Ada" || source != "api_key" {
		t.Errorf("identity = (%q, %q, %q), want (lu-ada, Ada, api_key)", id, name, source)
	}
	rows := r.userCredentialRows(t)
	if len(rows) != 1 || rows[0].Action != domain.AccessActionCredentialSet || rows[0].ActorUserID != runmode.LocalDefaultUserID {
		t.Errorf("personal credential rows = %+v, want one credential_set by the user", rows)
	}
	if st := r.status(t); !st.Connected || st.Account != "Ada" {
		t.Errorf("status after = %+v, want connected as Ada", st)
	}

	c, err := r.s.linearResolver.ForUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err != nil || c == nil {
		t.Fatalf("ForUser after the paste = (%v, %v), want a client", c, err)
	}
}

func TestLinearIdentity_APIKeyRefusals(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(r *linearConnectRig)
		body     map[string]any
		wantCode int
		reason   string
	}{
		{name: "empty key", body: map[string]any{"api_key": "  "}, wantCode: http.StatusBadRequest, reason: "MISSING_FIELD"},
		{name: "unknown field", body: map[string]any{"api_key": linearUserKey, "token": "x"}, wantCode: http.StatusBadRequest, reason: "UNKNOWN_FIELD"},
		{name: "rejected by linear", setup: func(r *linearConnectRig) { r.fake.set("rejected") }, body: map[string]any{"api_key": linearUserKey}, wantCode: http.StatusUnprocessableEntity, reason: "UPSTREAM_REJECTED"},
		{name: "linear unreachable", setup: func(r *linearConnectRig) { r.fake.set("down") }, body: map[string]any{"api_key": linearUserKey}, wantCode: http.StatusBadGateway, reason: "UPSTREAM_UNAVAILABLE"},
		{name: "app user", setup: func(r *linearConnectRig) { r.fake.set("app") }, body: map[string]any{"api_key": linearUserKey}, wantCode: http.StatusUnprocessableEntity, reason: "UPSTREAM_REJECTED"},
		{name: "other workspace", setup: func(r *linearConnectRig) {
			r.fake.setWorkspace(linear.Organization{ID: "org-other", Name: "Other", URLKey: "other"})
		}, body: map[string]any{"api_key": linearUserKey}, wantCode: http.StatusUnprocessableEntity, reason: "UPSTREAM_REJECTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLinearConnectRig(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			rec := doJSON(t, r.s, http.MethodPost, linearIdentityPath()+"/api-key", tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if items := errorItems(t, rec); items[0].Reason != tc.reason {
				t.Errorf("reason = %q, want %q", items[0].Reason, tc.reason)
			}
			r.assertNoUserCredential(t)
		})
	}

	t.Run("org has no linear", func(t *testing.T) {
		r := newLinearInstallRig(t)
		r.fake.set("person")
		rec := doJSON(t, r.s, http.MethodPost, linearIdentityPath()+"/api-key", map[string]any{"api_key": linearUserKey})
		if rec.Code != http.StatusConflict || errorItems(t, rec)[0].Reason != "NOT_CONFIGURED" {
			t.Fatalf("status = %d %s, want 409 NOT_CONFIGURED", rec.Code, rec.Body.String())
		}
		if r.fake.Calls() != 0 {
			t.Error("validated a key the org has no workspace to bind it in")
		}
	})
}

func TestLinearIdentity_Delete(t *testing.T) {
	r := newLinearConnectRig(t)
	if rec := r.pasteKey(t, linearUserKey); rec.Code != http.StatusOK {
		t.Fatalf("paste: %d: %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, r.s, http.MethodDelete, linearIdentityPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	if v := r.userSecret(t); v != "" {
		t.Errorf("user credential = %q after the unbind, want none", v)
	}
	if id, _, _ := r.identity(t); id != "" {
		t.Errorf("identity = %q after the unbind, want none", id)
	}
	if got := r.userCredentialActions(t); !slices.Equal(got, []string{domain.AccessActionCredentialRemoved, domain.AccessActionCredentialSet}) {
		t.Errorf("personal credential actions = %v, want one set and one removed", got)
	}
	if len(r.oauth.Revoked()) != 0 {
		t.Errorf("revoked %v; a pasted key is the user's own and is not revoked", r.oauth.Revoked())
	}
	// The org's credential is not the user's to withdraw.
	if v := r.secret(t, integrations.KeyLinearAPIKey); v != linearOrgKey {
		t.Errorf("org key = %q after the user's unbind, want it kept", v)
	}
	if _, err := r.s.linearResolver.ForUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID); !errors.Is(err, linear.ErrNoLinearUserCredential) {
		t.Errorf("ForUser after the unbind = %v, want ErrNoLinearUserCredential", err)
	}

	if again := doJSON(t, r.s, http.MethodDelete, linearIdentityPath(), nil); again.Code != http.StatusOK {
		t.Fatalf("second DELETE: %d: %s", again.Code, again.Body.String())
	}
	if rows := r.userCredentialRows(t); len(rows) != 2 {
		t.Errorf("personal credential rows = %d after an idempotent unbind, want still 2", len(rows))
	}
}

func TestLinearConnect_HappyPath(t *testing.T) {
	r := newLinearConnectRig(t)
	r.connect(t)

	cred := r.userCredential(t)
	if cred.Method != linear.AuthMethodConnectOAuth || cred.RefreshToken != "ref-install" || cred.ClientID != "lin-client-1" || cred.GrantID == "" || cred.Token != "" {
		t.Errorf("user credential = %+v, want connect_oauth holding the refresh token, the app and a grant, and no access token", cred)
	}
	if strings.Contains(r.userSecret(t), "acc-install") {
		t.Error("the access token was stored")
	}
	if id, name, source := r.identity(t); id != "lu-ada" || name != "Ada" || source != "connect_oauth" {
		t.Errorf("identity = (%q, %q, %q), want (lu-ada, Ada, connect_oauth)", id, name, source)
	}
	if rows := r.userCredentialRows(t); len(rows) != 1 || rows[0].Action != domain.AccessActionCredentialSet {
		t.Errorf("personal credential rows = %+v, want one credential_set", rows)
	}
	if st := r.status(t); !st.Connected || st.Account != "Ada" {
		t.Errorf("status = %+v, want connected as Ada", st)
	}
	if len(r.oauth.Revoked()) != 0 {
		t.Errorf("revoked %v on a completed Connect", r.oauth.Revoked())
	}

	// ForUser mints from the stored refresh token and writes the rotation
	// back under the same grant.
	if _, err := r.s.linearResolver.ForUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	after := r.userCredential(t)
	if after.RefreshToken != "ref-refreshed" || after.GrantID != cred.GrantID {
		t.Errorf("after ForUser = %+v, want the rotated token under grant %s", after, cred.GrantID)
	}
}

func TestLinearConnect_ReturnTo(t *testing.T) {
	r := newLinearConnectRig(t)
	cookie, state := r.startConnect(t, "/setup")
	rec := r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/setup" {
		t.Errorf("callback = %d to %q, want 302 to /setup", rec.Code, rec.Header().Get("Location"))
	}
}

func TestLinearConnect_Refusals(t *testing.T) {
	// The org started requiring an SSO the session did not come through while
	// the person was at Linear. Refused before the code is exchanged, so there
	// is no grant to revoke, and redirected like every other refusal here: the
	// org's page is where the app offers its sign-in.
	t.Run("sso required", func(t *testing.T) {
		r := newLinearConnectRig(t)
		ext := &barringLoginExt{}
		r.s.loginExt = ext
		cookie, state := r.startConnect(t, "/setup")
		ext.bar(runmode.LocalDefaultOrgID)
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie), "sso_required", "/setup")
		if got := r.oauth.Revoked(); len(got) != 0 {
			t.Errorf("revoked %v, want nothing: no code was exchanged", got)
		}
		r.assertNoUserCredential(t)
	})
	t.Run("workspace mismatch revokes the pair", func(t *testing.T) {
		r := newLinearConnectRig(t)
		cookie, state := r.startConnect(t, "/setup")
		r.fake.setWorkspace(linear.Organization{ID: "org-other", Name: "Other", URLKey: "other"})
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie), "workspace_mismatch", "/setup")
		if got := r.oauth.Revoked(); !slices.Equal(got, []string{"acc-install", "ref-install"}) {
			t.Errorf("revoked %v, want the minted pair", got)
		}
		r.assertNoUserCredential(t)
	})
	t.Run("app user revokes the pair", func(t *testing.T) {
		r := newLinearConnectRig(t)
		cookie, state := r.startConnect(t, "")
		r.fake.set("app")
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie), "connect_failed", "")
		if got := r.oauth.Revoked(); len(got) != 2 {
			t.Errorf("revoked %v, want the minted pair", got)
		}
		r.assertNoUserCredential(t)
	})
	t.Run("denied", func(t *testing.T) {
		r := newLinearConnectRig(t)
		cookie, state := r.startConnect(t, "")
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"error": {"access_denied"}, "state": {state}}), cookie), "denied", "")
		r.assertNoUserCredential(t)
	})
	// Only access_denied is a refusal; any other OAuth error is a failure to
	// connect and must not read as cancelled or awaiting approval.
	for _, oErr := range []string{"server_error", "temporarily_unavailable", "invalid_scope"} {
		t.Run(oErr, func(t *testing.T) {
			r := newLinearConnectRig(t)
			cookie, state := r.startConnect(t, "")
			expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"error": {oErr}, "state": {state}}), cookie), "connect_failed", "")
			r.assertNoUserCredential(t)
		})
	}
	t.Run("failed exchange", func(t *testing.T) {
		r := newLinearConnectRig(t)
		cookie, state := r.startConnect(t, "")
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"code": {"bad-code"}, "state": {state}}), cookie), "connect_failed", "")
		r.assertNoUserCredential(t)
	})
	t.Run("missing cookie", func(t *testing.T) {
		r := newLinearConnectRig(t)
		_, state := r.startConnect(t, "")
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}})), "state", "")
		r.assertNoUserCredential(t)
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		r := newLinearConnectRig(t)
		cookie, _ := r.startConnect(t, "")
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {"forged"}}), cookie), "state", "")
		r.assertNoUserCredential(t)
	})
	t.Run("expired cookie", func(t *testing.T) {
		r := newLinearConnectRig(t)
		cookie, state := r.startConnect(t, "")
		prev := timeNow
		timeNow = func() time.Time { return prev().Add(11 * time.Minute) }
		t.Cleanup(func() { timeNow = prev })
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie), "state", "")
		r.assertNoUserCredential(t)
	})
	t.Run("forged cookie", func(t *testing.T) {
		r := newLinearConnectRig(t)
		cookie, state := r.startConnect(t, "")
		st := connectState{OrgID: runmode.LocalDefaultOrgID, UserID: runmode.LocalDefaultUserID, CSRF: state, ExpiresAt: timeNow().Add(time.Minute).Unix()}
		forged, err := st.sign([32]byte{9})
		if err != nil {
			t.Fatal(err)
		}
		cookie.Value = forged
		expectConnectError(t, r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie), "state", "")
		r.assertNoUserCredential(t)
	})
	t.Run("no app", func(t *testing.T) {
		r := newLinearConnectRig(t)
		if rec := doJSON(t, r.s, http.MethodDelete, linearAppPath(), nil); rec.Code != http.StatusOK {
			t.Fatalf("delete app: %d: %s", rec.Code, rec.Body.String())
		}
		expectConnectError(t, r.get(t, linearConnectStartPath("/setup")), "no_app", "/setup")
		if st := r.status(t); st.ConnectAvailable {
			t.Error("connect_available with no app")
		}
	})
	t.Run("org has no linear", func(t *testing.T) {
		r := newLinearInstallRig(t)
		loc := expectRedirect(t, r.get(t, linearConnectStartPath("")), "error", "not_configured")
		if loc.Host != "" {
			t.Errorf("not_configured went to %s, want the gate page", loc)
		}
	})
}

func TestLinearConnect_DeleteRevokesTheGrant(t *testing.T) {
	r := newLinearConnectRig(t)
	r.connect(t)
	if rec := doJSON(t, r.s, http.MethodDelete, linearIdentityPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	if got := r.oauth.Revoked(); !slices.Equal(got, []string{"ref-install"}) {
		t.Errorf("revoked %v, want the stored refresh token", got)
	}
	r.assertNoUserCredentialButAudit(t)
}

// assertNoUserCredentialButAudit is assertNoUserCredential after a bind and
// its unbind, which each left an audit row.
func (r *linearConnectRig) assertNoUserCredentialButAudit(t *testing.T) {
	t.Helper()
	if v := r.userSecret(t); v != "" {
		t.Errorf("user credential = %q, want none", v)
	}
	if id, _, _ := r.identity(t); id != "" {
		t.Errorf("identity = %q, want none", id)
	}
	if got := r.userCredentialActions(t); !slices.Equal(got, []string{domain.AccessActionCredentialRemoved, domain.AccessActionCredentialSet}) {
		t.Errorf("personal credential actions = %v, want one set and one removed", got)
	}
}

func TestLinearConnect_KeyPasteRevokesTheReplacedGrant(t *testing.T) {
	r := newLinearConnectRig(t)
	r.connect(t)
	if rec := r.pasteKey(t, linearUserKey); rec.Code != http.StatusOK {
		t.Fatalf("paste: %d: %s", rec.Code, rec.Body.String())
	}
	if got := r.oauth.Revoked(); !slices.Equal(got, []string{"ref-install"}) {
		t.Errorf("revoked %v, want the replaced grant's refresh token", got)
	}
	if cred := r.userCredential(t); cred.Method != linear.AuthMethodAPIKey {
		t.Errorf("user credential = %+v, want the pasted key", cred)
	}
}

func TestLinearConnect_ReconnectLeavesTheSameGrantAlone(t *testing.T) {
	r := newLinearConnectRig(t)
	r.connect(t)
	first := r.userCredential(t)
	r.connect(t)
	second := r.userCredential(t)
	if second.GrantID == first.GrantID || second.RefreshToken != "ref-install-2" {
		t.Errorf("after reconnecting = %+v, want a new grant holding ref-install-2", second)
	}
	if got := r.oauth.Revoked(); len(got) != 0 {
		t.Errorf("revoked %v; a second Connect through the same app as the same person may be the same grant", got)
	}
}

func TestLinearConnect_RefusedRefreshClearsTheBinding(t *testing.T) {
	r := newLinearConnectRig(t)
	r.connect(t)
	r.oauth.mu.Lock()
	r.oauth.refuse["ref-install"] = true
	r.oauth.mu.Unlock()

	_, err := r.s.linearResolver.ForUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("ForUser = %v, want ErrLinearUserCredentialRefused", err)
	}
	if cred := r.userCredential(t); cred != linear.RefusedUserCredential() {
		t.Errorf("user credential = %+v after Linear refused it, want the refused marker", cred)
	}
	if id, _, _ := r.identity(t); id != "" {
		t.Errorf("identity = %q after Linear refused the credential, want none", id)
	}
	if st := r.status(t); st.Connected || st.DisconnectedReason != linearDisconnectedRefused || st.Account != "" {
		t.Errorf("status = %+v after Linear refused the credential, want not connected, reason refused", st)
	}
	// The marker answers the next read itself, without asking Linear again.
	grants := len(r.oauth.Grants())
	if _, err := r.s.linearResolver.ForUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Errorf("ForUser on the marker = %v, want ErrLinearUserCredentialRefused", err)
	}
	if got := len(r.oauth.Grants()); got != grants {
		t.Errorf("token requests = %d after reading the marker, want %d", got, grants)
	}
	// The org's credential was never reached for in its place.
	if v := r.secret(t, integrations.KeyLinearAPIKey); v != linearOrgKey {
		t.Errorf("org key = %q, want it untouched", v)
	}
}

func TestLinearConnect_ConnectedFollowsTheApp(t *testing.T) {
	r := newLinearConnectRig(t)
	r.connect(t)
	// Replacing the org's app overwrites the secret of the app that minted
	// the grant, so nothing can refresh or revoke it any more.
	r.registerApp(t, "lin-client-2")
	if st := r.status(t); st.Connected || st.DisconnectedReason != linearDisconnectedAppRemoved {
		t.Errorf("status = %+v; a Connect credential the org's app can no longer refresh must read as not connected, reason app_removed", st)
	}
	if _, err := r.s.linearResolver.ForUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID); !errors.Is(err, linear.ErrLinearUserCredentialAppRemoved) {
		t.Errorf("ForUser = %v, want ErrLinearUserCredentialAppRemoved", err)
	}
	if cred := r.userCredential(t); cred.ClientID != "lin-client-1" || cred.RefreshToken == "" {
		t.Errorf("user credential = %+v, want the grant kept for its app to come back", cred)
	}
	// A pasted key does not depend on the app.
	if rec := r.pasteKey(t, linearUserKey); rec.Code != http.StatusOK {
		t.Fatalf("paste: %d: %s", rec.Code, rec.Body.String())
	}
	if st := r.status(t); !st.Connected || st.DisconnectedReason != "" {
		t.Errorf("status = %+v after a key paste, want connected with no reason", st)
	}
	// Linear accepts a revoke only from the app that minted the token, and
	// its secret is gone, so the replaced grant is not revoked with another.
	if got := r.oauth.Revoked(); len(got) != 0 {
		t.Errorf("revoked %v with an app that did not mint it", got)
	}
}

// TestLinearIdentity_AppRemovedIsDerivedOnRead: app_removed is read off the
// app the grant names on every status read, never stored, so restoring the
// same client ID brings the connection back.
func TestLinearIdentity_AppRemovedIsDerivedOnRead(t *testing.T) {
	r := newLinearConnectRig(t)
	r.connect(t)
	if rec := doJSON(t, r.s, http.MethodDelete, linearAppPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("delete app: %d: %s", rec.Code, rec.Body.String())
	}
	if st := r.status(t); st.Connected || st.DisconnectedReason != linearDisconnectedAppRemoved {
		t.Errorf("status = %+v with the app gone, want not connected, reason app_removed", st)
	}
	r.registerApp(t, "lin-client-1")
	if st := r.status(t); !st.Connected || st.DisconnectedReason != "" || st.Account != "Ada" {
		t.Errorf("status = %+v with the same app back, want connected as Ada with no reason", st)
	}
}

// TestLinearIdentity_NoDisconnectedReason: a reason is given only for a
// refused marker or a grant whose app is gone; every other state reports none.
func TestLinearIdentity_NoDisconnectedReason(t *testing.T) {
	cases := []struct {
		name      string
		stored    string
		connected bool
	}{
		{name: "never connected"},
		{name: "a pasted key", stored: `{"method":"api_key","token":"lin_api_ada"}`, connected: true},
		{name: "an unusable connect credential", stored: `{"method":"connect_oauth","refresh_token":"ref-0","client_id":"lin-client-1"}`},
		{name: "an empty api key", stored: `{"method":"api_key"}`},
		{name: "an unknown method", stored: `{"method":"carrier_pigeon","token":"x"}`},
		{name: "unparsable", stored: `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLinearConnectRig(t)
			if tc.stored != "" {
				r.putUserSecret(t, tc.stored)
			}
			st := r.status(t)
			if st.Connected != tc.connected || st.DisconnectedReason != "" {
				t.Errorf("status = %+v, want connected=%v with no reason", st, tc.connected)
			}
		})
	}
	t.Run("connected through Connect", func(t *testing.T) {
		r := newLinearConnectRig(t)
		r.connect(t)
		if st := r.status(t); !st.Connected || st.DisconnectedReason != "" {
			t.Errorf("status = %+v, want connected with no reason", st)
		}
	})
}

// TestLinearIdentity_RefusedMarkerIsReplacedOrRemoved: a key paste or a
// Connect over a refused marker reads as connected with no reason, and the
// user's own disconnect removes a marker, with its audit row, revoking
// nothing.
func TestLinearIdentity_RefusedMarkerIsReplacedOrRemoved(t *testing.T) {
	t.Run("key paste", func(t *testing.T) {
		r := newLinearConnectRig(t)
		r.putRefusedMarker(t)
		if st := r.status(t); st.Connected || st.DisconnectedReason != linearDisconnectedRefused {
			t.Fatalf("status = %+v over a marker, want not connected, reason refused", st)
		}
		if rec := r.pasteKey(t, linearUserKey); rec.Code != http.StatusOK {
			t.Fatalf("paste: %d: %s", rec.Code, rec.Body.String())
		}
		if st := r.status(t); !st.Connected || st.DisconnectedReason != "" || st.Account != "Ada" {
			t.Errorf("status = %+v after the paste, want connected as Ada with no reason", st)
		}
		if got := r.oauth.Revoked(); len(got) != 0 {
			t.Errorf("revoked %v replacing a marker, which holds no token", got)
		}
	})
	t.Run("connect", func(t *testing.T) {
		r := newLinearConnectRig(t)
		r.putRefusedMarker(t)
		r.connect(t)
		if st := r.status(t); !st.Connected || st.DisconnectedReason != "" {
			t.Errorf("status = %+v after Connect, want connected with no reason", st)
		}
		if got := r.oauth.Revoked(); len(got) != 0 {
			t.Errorf("revoked %v replacing a marker, which holds no token", got)
		}
	})
	t.Run("disconnect", func(t *testing.T) {
		r := newLinearConnectRig(t)
		r.putRefusedMarker(t)
		if rec := doJSON(t, r.s, http.MethodDelete, linearIdentityPath(), nil); rec.Code != http.StatusOK {
			t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
		}
		if v := r.userSecret(t); v != "" {
			t.Errorf("user credential = %q after the disconnect, want none", v)
		}
		if got := r.userCredentialActions(t); !slices.Equal(got, []string{domain.AccessActionCredentialRemoved}) {
			t.Errorf("personal credential actions = %v, want one removed", got)
		}
		if got := r.oauth.Revoked(); len(got) != 0 {
			t.Errorf("revoked %v removing a marker, which holds no token", got)
		}
		if st := r.status(t); st.Connected || st.DisconnectedReason != "" {
			t.Errorf("status = %+v after the disconnect, want not connected with no reason", st)
		}
	})
	t.Run("member sweep", func(t *testing.T) {
		r := newLinearConnectRig(t)
		r.putRefusedMarker(t)
		if err := r.s.sweepLinearUserCredential(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if v := r.userSecret(t); v != "" {
			t.Errorf("user credential = %q after the sweep, want none", v)
		}
		if got := r.oauth.Revoked(); len(got) != 0 {
			t.Errorf("revoked %v sweeping a marker, which holds no token", got)
		}
	})
}

// putUserSecret stores value as the user's credential for the Acme workspace,
// bypassing the bind.
func (r *linearConnectRig) putUserSecret(t *testing.T, value string) {
	t.Helper()
	if err := r.stores.Secrets.PutUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, linear.UserTokenKey(acmeWorkspaceID), value, "Linear user access token"); err != nil {
		t.Fatalf("store user credential: %v", err)
	}
}

// putRefusedMarker stores the marker a refused Connect credential leaves.
func (r *linearConnectRig) putRefusedMarker(t *testing.T) {
	t.Helper()
	env, err := linear.MarshalUserCredential(linear.RefusedUserCredential())
	if err != nil {
		t.Fatal(err)
	}
	r.putUserSecret(t, env)
}

// TestLinearConnect_ReplacedGrantRevokedWithTheAppThatMintedIt: a grant
// minted by the deployment app stays usable after the org registers its own
// app, and connecting again through the org's app revokes it with the
// deployment app, the only one Linear accepts the revoke from.
func TestLinearConnect_ReplacedGrantRevokedWithTheAppThatMintedIt(t *testing.T) {
	r := newLinearConnectRig(t)
	if rec := doJSON(t, r.s, http.MethodDelete, linearAppPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("delete app: %d: %s", rec.Code, rec.Body.String())
	}
	r.useDeploymentApp(linear.OAuthApp{ClientID: "lin-deploy", ClientSecret: "dep-secret"})
	r.clientID = "lin-deploy"
	r.connect(t)
	if cred := r.userCredential(t); cred.ClientID != "lin-deploy" {
		t.Fatalf("credential minted by %q, want lin-deploy", cred.ClientID)
	}

	r.registerApp(t, "lin-client-1")
	if st := r.status(t); !st.Connected {
		t.Error("a grant the org can still refresh reads as not connected after it registered its own app")
	}
	if _, err := r.s.linearResolver.ForUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID); err != nil {
		t.Errorf("ForUser on the deployment app's grant: %v", err)
	}
	stored := r.userCredential(t).RefreshToken

	r.clientID = "lin-client-1"
	r.connect(t)
	if got := r.oauth.Revoked(); !slices.Equal(got, []string{stored}) {
		t.Errorf("revoked %v, want the replaced grant's refresh token %q", got, stored)
	}
	if got := r.oauth.RevokedBy(); !slices.Equal(got, []string{"lin-deploy"}) {
		t.Errorf("revoked with %v, want the deployment app that minted it", got)
	}
}

// TestLinearConnect_LinearCallsRunOutsideTheUserLock: with the user's lock
// held elsewhere, the callback still exchanges its code and checks who it
// acts as; only the store waits for the lock. In multi mode the lock holds a
// pool connection, so nothing holds it across a call to Linear.
func TestLinearConnect_LinearCallsRunOutsideTheUserLock(t *testing.T) {
	r := newLinearConnectRig(t)
	cookie, state := r.startConnect(t, "")
	release := r.holdUserLock(t)
	callsBefore := r.fake.Calls()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.fake.Calls() == callsBefore && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Contains(r.oauth.Grants(), "authorization_code") || r.fake.Calls() == callsBefore {
		t.Fatalf("the callback did not reach Linear while the user's lock was held elsewhere (grants %v)", r.oauth.Grants())
	}
	select {
	case rec := <-done:
		t.Fatalf("the callback stored (%d) while the user's lock was held elsewhere", rec.Code)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case rec := <-done:
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
			t.Fatalf("callback = %d to %q, want 302 home", rec.Code, rec.Header().Get("Location"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the callback did not finish after the lock was released")
	}
	if cred := r.userCredential(t); cred.Method != linear.AuthMethodConnectOAuth {
		t.Errorf("stored %+v, want the Connect credential", cred)
	}
}

// TestLinearIdentity_RevokesRunOutsideTheUserLock: a disconnect's revoke and
// a replaced grant's revoke both run after the user's lock is released.
func TestLinearIdentity_RevokesRunOutsideTheUserLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(r *linearConnectRig) *httptest.ResponseRecorder
	}{
		{"disconnect", func(r *linearConnectRig) *httptest.ResponseRecorder {
			return doJSON(t, r.s, http.MethodDelete, linearIdentityPath(), nil)
		}},
		{"key replacing a grant", func(r *linearConnectRig) *httptest.ResponseRecorder {
			return r.pasteKey(t, linearUserKey)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLinearConnectRig(t)
			r.connect(t)
			var heldDuringRevoke []bool
			r.oauth.mu.Lock()
			r.oauth.onRevoke = func() { heldDuringRevoke = append(heldDuringRevoke, r.lockHeldElsewhere(t)) }
			r.oauth.mu.Unlock()
			if rec := tc.act(r); rec.Code != http.StatusOK {
				t.Fatalf("%s: %d: %s", tc.name, rec.Code, rec.Body.String())
			}
			if !slices.Equal(heldDuringRevoke, []bool{false}) {
				t.Errorf("user lock held during each revoke = %v, want one revoke with it free", heldDuringRevoke)
			}
		})
	}
}

func TestLinearConnect_RoutesRegistered(t *testing.T) {
	r := newLinearInstallRig(t)
	for _, path := range []string{linearIdentityPath(), linearConnectStartPath(""), linearConnectCallbackPath} {
		rec := r.get(t, path)
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "route") {
			t.Errorf("GET %s is not routed", path)
		}
	}
}

// TestLinearIdentity_MultiMode walks the per-user slot on Postgres with two
// members of one org: each binds in their own secret scope under RLS, one
// cannot spend the other's Connect cookie, a token cannot finish a Connect,
// /api/me carries the binding, and removing a member sweeps their credential.
func TestLinearIdentity_MultiMode(t *testing.T) {
	rig := newAuthRig(t)
	s := rig.srv
	whoami := newLinearWhoamiFake(t)
	s.validateLinear = func(ctx context.Context, cfg linear.Config) (*auth.LinearUser, *auth.LinearOrganization, error) {
		cfg.Endpoint = whoami.URL
		return auth.ValidateLinear(ctx, cfg)
	}
	oauth := newLinearOAuthFake(t)
	s.linearOAuthMinter = linearoauth.NewMinterWithEndpoints(oauth.URL+"/token", oauth.URL+"/revoke")
	stores := pgstore.New(rig.h.AdminDB, rig.h.AppDB, pgtest.SecretKey)
	s.linearResolver = linear.NewResolverWithOAuth(stores.Secrets, stores.Orgs,
		eventsource.ResolverGate(stores.OrgEventSources, eventsource.KindLinear), nil,
		linearoauth.NewUserTokenCache(s.linearOAuthMinter, s.linearOAuthApps, stores.Secrets, stores.Tx, s.linearUserCredentialLock))

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org")
	sidA := rig.signIn(alice)
	carol := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(
		`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		carol.String(), orgA.String()); err != nil {
		t.Fatalf("seed carol membership: %v", err)
	}
	sidC := rig.signIn(carol)

	orgPath := "/api/orgs/" + orgA.String()
	if rec := rig.tokensJSON(http.MethodPut, orgPath+"/linear/access/credential", map[string]any{"api_key": linearOrgKey}, sidA, ""); rec.Code != http.StatusOK {
		t.Fatalf("org bind: %d: %s", rec.Code, rec.Body.String())
	}
	if rec := rig.tokensJSON(http.MethodPost, orgPath+"/linear/app", map[string]any{"client_id": "lin-client-1", "client_secret": "s"}, sidA, ""); rec.Code != http.StatusOK {
		t.Fatalf("register app: %d: %s", rec.Code, rec.Body.String())
	}
	status := func(sid string) linearIdentityStatusResponse {
		t.Helper()
		rec := rig.tokensJSON(http.MethodGet, orgPath+"/linear/identity", nil, sid, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d: %s", rec.Code, rec.Body.String())
		}
		var got linearIdentityStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}
	userKey := linear.UserTokenKey(acmeWorkspaceID)
	userSecret := func(user uuid.UUID) string {
		t.Helper()
		v, err := stores.Secrets.GetUserSystem(t.Context(), orgA.String(), user.String(), userKey)
		if err != nil {
			t.Fatalf("read user secret: %v", err)
		}
		return v
	}

	// A member with the org on Linear and nothing bound is the gate's case.
	if st := status(sidC); st.Connected || st.WorkspaceURLKey != acmeWorkspaceURLKey || !st.ConnectAvailable {
		t.Fatalf("carol before = %+v, want not connected in acme with Connect available", st)
	}

	// Carol pastes her key: it lands in her scope only.
	whoami.setViewerID("lu-carol")
	if rec := rig.tokensJSON(http.MethodPost, orgPath+"/linear/identity/api-key", map[string]any{"api_key": "lin_api_carol"}, sidC, ""); rec.Code != http.StatusOK {
		t.Fatalf("carol paste: %d: %s", rec.Code, rec.Body.String())
	}
	if v := userSecret(carol); !strings.Contains(v, "lin_api_carol") {
		t.Errorf("carol's secret = %q, want her key", v)
	}
	if v := userSecret(alice); v != "" {
		t.Errorf("alice's secret = %q, want none", v)
	}
	if v, _ := stores.Secrets.GetSystem(t.Context(), orgA.String(), userKey); v != "" {
		t.Errorf("org-scope %s = %q, want nothing", userKey, v)
	}
	if v, _ := stores.Secrets.GetSystem(t.Context(), orgA.String(), integrations.KeyLinearAPIKey); v != linearOrgKey {
		t.Errorf("org key = %q, want it untouched", v)
	}
	if st := status(sidC); !st.Connected {
		t.Errorf("carol after = %+v, want connected", st)
	}
	if st := status(sidA); st.Connected {
		t.Errorf("alice = %+v; carol's credential is not hers", st)
	}

	// /api/me carries the binding for the member who made it.
	me := rig.tokensJSON(http.MethodGet, "/api/me", nil, sidC, "")
	var meBody struct {
		LinearUserID string `json:"linear_user_id"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &meBody); err != nil || meBody.LinearUserID != "lu-carol" {
		t.Errorf("/api/me linear_user_id = %q (%v), want lu-carol", meBody.LinearUserID, err)
	}

	// ForUser builds carol's client and answers alice, who bound nothing,
	// with no credential.
	if _, err := s.linearResolver.ForUser(t.Context(), orgA.String(), carol.String()); err != nil {
		t.Errorf("ForUser(carol) = %v, want a client", err)
	}
	if _, err := s.linearResolver.ForUser(t.Context(), orgA.String(), alice.String()); !errors.Is(err, linear.ErrNoLinearUserCredential) {
		t.Errorf("ForUser(alice) = %v, want ErrNoLinearUserCredential", err)
	}

	get := func(path, sid, bearer string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if sid != "" {
			req.AddCookie(&http.Cookie{Name: s.sidCookieName(), Value: sid})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		return rec
	}
	start := func(sid string) (*http.Cookie, string) {
		t.Helper()
		rec := get(orgPath+"/linear/connect/start", sid, "", nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("start: %d: %s", rec.Code, rec.Body.String())
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		for _, c := range rec.Result().Cookies() {
			if c.Name == linearConnectStateCookieName {
				return c, loc.Query().Get("state")
			}
		}
		t.Fatal("no state cookie")
		return nil, ""
	}
	callback := func(state string) string {
		return linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}})
	}

	_, bearer := rig.mintToken(carol, orgA, "ci")
	if rec := get(orgPath+"/linear/connect/start", "", bearer, nil); rec.Code != http.StatusForbidden {
		t.Errorf("start over a Bearer token = %d, want 403", rec.Code)
	}
	cookie, state := start(sidC)
	if rec := get(callback(state), "", bearer, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("callback over a Bearer token = %d, want 401", rec.Code)
	}
	// Alice cannot spend carol's cookie.
	cookie, state = start(sidC)
	expectRedirect(t, get(callback(state), sidA, "", cookie), "error", "state")
	if len(oauth.Grants()) != 0 {
		t.Fatalf("a refused callback exchanged a code: %v", oauth.Grants())
	}

	cookie, state = start(sidC)
	if rec := get(callback(state), sidC, "", cookie); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/orgs/"+orgA.String() {
		t.Fatalf("carol's Connect = %d to %q, want 302 to the org's home: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	cred, err := linear.ParseUserCredential(userSecret(carol))
	if err != nil || cred.Method != linear.AuthMethodConnectOAuth || cred.RefreshToken != "ref-install" {
		t.Fatalf("carol's credential after Connect = %+v (%v), want connect_oauth", cred, err)
	}
	// Her pasted key was replaced by the Connect, and revoking a key is not
	// TF's to do.
	if got := oauth.Revoked(); len(got) != 0 {
		t.Errorf("revoked %v on carol's Connect", got)
	}

	// Alice removes carol: her credential, her identity and her grant go.
	if rec := rig.tokensJSON(http.MethodDelete, orgPath+"/members/"+carol.String(), nil, sidA, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove carol: %d: %s", rec.Code, rec.Body.String())
	}
	if v := userSecret(carol); v != "" {
		t.Errorf("carol's secret = %q after her removal, want none", v)
	}
	if id, _, err := stores.Users.GetLinearIdentitySystem(t.Context(), carol.String(), acmeWorkspaceID); err != nil || id != "" {
		t.Errorf("carol's identity = %q (%v) after her removal, want none", id, err)
	}
	if got := oauth.Revoked(); !slices.Equal(got, []string{"ref-install"}) {
		t.Errorf("revoked %v on carol's removal, want her refresh token", got)
	}
}

// TestLinearIdentity_RemovalRacingABind: a member removed while their bind
// is in flight ends with no credential, and the bind is refused as a
// non-member's rather than failing. The route's membership check runs
// before the user's lock, so the removal lands after it here; the bind reads
// membership again under the lock instead of relying on the access-log
// row's policy to abort its transaction.
func TestLinearIdentity_RemovalRacingABind(t *testing.T) {
	rig := newAuthRig(t)
	s := rig.srv
	whoami := newLinearWhoamiFake(t)
	s.validateLinear = func(ctx context.Context, cfg linear.Config) (*auth.LinearUser, *auth.LinearOrganization, error) {
		cfg.Endpoint = whoami.URL
		return auth.ValidateLinear(ctx, cfg)
	}
	oauth := newLinearOAuthFake(t)
	s.linearOAuthMinter = linearoauth.NewMinterWithEndpoints(oauth.URL+"/token", oauth.URL+"/revoke")
	stores := pgstore.New(rig.h.AdminDB, rig.h.AppDB, pgtest.SecretKey)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org")
	sidA := rig.signIn(alice)
	orgPath := "/api/orgs/" + orgA.String()
	if rec := rig.tokensJSON(http.MethodPut, orgPath+"/linear/access/credential", map[string]any{"api_key": linearOrgKey}, sidA, ""); rec.Code != http.StatusOK {
		t.Fatalf("org bind: %d: %s", rec.Code, rec.Body.String())
	}
	if rec := rig.tokensJSON(http.MethodPost, orgPath+"/linear/app", map[string]any{"client_id": "lin-client-1", "client_secret": "s"}, sidA, ""); rec.Code != http.StatusOK {
		t.Fatalf("register app: %d: %s", rec.Code, rec.Body.String())
	}
	addMember := func() (uuid.UUID, string) {
		t.Helper()
		u := rig.seedUser()
		if _, err := rig.h.AdminDB.Exec(`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
			u.String(), orgA.String()); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
		return u, rig.signIn(u)
	}
	stored := func(u uuid.UUID) (secret, identity string) {
		t.Helper()
		secret, err := stores.Secrets.GetUserSystem(t.Context(), orgA.String(), u.String(), linear.UserTokenKey(acmeWorkspaceID))
		if err != nil {
			t.Fatalf("read secret: %v", err)
		}
		identity, _, err = stores.Users.GetLinearIdentitySystem(t.Context(), u.String(), acmeWorkspaceID)
		if err != nil {
			t.Fatalf("read identity: %v", err)
		}
		return secret, identity
	}
	remove := func(u uuid.UUID) *httptest.ResponseRecorder {
		return rig.tokensJSON(http.MethodDelete, orgPath+"/members/"+u.String(), nil, sidA, "")
	}

	t.Run("key paste", func(t *testing.T) {
		carol, sidC := addMember()
		whoami.setViewerID("lu-carol")
		// The removal, its sweep included, completes while carol's key is
		// being validated: after the route let her in, before her bind
		// takes her lock.
		var removed *httptest.ResponseRecorder
		var once sync.Once
		whoami.mu.Lock()
		whoami.onCall = func() { once.Do(func() { removed = remove(carol) }) }
		whoami.mu.Unlock()
		t.Cleanup(func() {
			whoami.mu.Lock()
			whoami.onCall = nil
			whoami.mu.Unlock()
		})

		rec := rig.tokensJSON(http.MethodPost, orgPath+"/linear/identity/api-key", map[string]any{"api_key": "lin_api_carol"}, sidC, "")
		if removed == nil || removed.Code != http.StatusNoContent {
			t.Fatalf("the removal did not run mid-bind: %+v", removed)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("paste after removal = %d %s, want 404", rec.Code, rec.Body.String())
		}
		if secret, identity := stored(carol); secret != "" || identity != "" {
			t.Errorf("after the race carol holds secret %q and identity %q, want neither", secret, identity)
		}
	})

	t.Run("connect", func(t *testing.T) {
		dave, sidD := addMember()
		whoami.setViewerID("lu-dave")
		get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(&http.Cookie{Name: s.sidCookieName(), Value: sidD})
			if cookie != nil {
				req.AddCookie(cookie)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			return rec
		}
		start := get(orgPath+"/linear/connect/start", nil)
		loc, _ := url.Parse(start.Header().Get("Location"))
		var cookie *http.Cookie
		for _, c := range start.Result().Cookies() {
			if c.Name == linearConnectStateCookieName {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatalf("start: %d: %s", start.Code, start.Body.String())
		}

		// The removal, its sweep included, commits while the code is being
		// exchanged, after the callback's early membership read.
		removed := make(chan *httptest.ResponseRecorder, 1)
		oauth.mu.Lock()
		oauth.onExchange = func() {
			go func() { removed <- remove(dave) }()
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				var n int
				if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM public.org_memberships WHERE user_id = $1 AND org_id = $2`,
					dave.String(), orgA.String()).Scan(&n); err == nil && n == 0 {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Error("the removal never committed")
		}
		oauth.mu.Unlock()
		t.Cleanup(func() {
			oauth.mu.Lock()
			oauth.onExchange = nil
			oauth.mu.Unlock()
		})

		revokedBefore := len(oauth.Revoked())
		rec := get(linearConnectCallback(url.Values{"code": {"good-code"}, "state": {loc.Query().Get("state")}}), cookie)
		expectRedirect(t, rec, "error", "state")
		select {
		case res := <-removed:
			if res.Code != http.StatusNoContent {
				t.Fatalf("removal: %d: %s", res.Code, res.Body.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the removal did not finish")
		}
		if secret, identity := stored(dave); secret != "" || identity != "" {
			t.Errorf("after the race dave holds secret %q and identity %q, want neither", secret, identity)
		}
		if got := len(oauth.Revoked()) - revokedBefore; got != 2 {
			t.Errorf("revoked %d tokens, want the pair minted for the refused bind", got)
		}
	})
}

var betaWorkspace = linear.Organization{ID: "org-beta", Name: "Beta", URLKey: "beta"}

// moveOrgWorkspace rebinds the org's key to another workspace, which moves
// org_settings.linear_workspace_*, and points the whoami fake back at what
// it answered before, so a user's credential still reads as Acme's.
func (r *linearConnectRig) moveOrgWorkspace(t *testing.T, to linear.Organization) {
	t.Helper()
	r.fake.setWorkspace(to)
	rec := doJSON(t, r.s, http.MethodPut, linearCredentialPath(), map[string]any{"api_key": "lin_api_org_" + to.URLKey})
	r.fake.setWorkspace(linear.Organization{ID: acmeWorkspaceID, Name: "Acme", URLKey: acmeWorkspaceURLKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("move org workspace: %d: %s", rec.Code, rec.Body.String())
	}
	if got := r.orgSettings(t).LinearWorkspaceID; got != to.ID {
		t.Fatalf("org workspace = %q after the move, want %q", got, to.ID)
	}
}

// holdUserLock takes the user's credential lock, as another of their requests
// would, until the returned release.
func (r *linearConnectRig) holdUserLock(t *testing.T) func() {
	t.Helper()
	release, err := r.s.linearUserCredentialLock.Lock(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	return release
}

func (r *linearConnectRig) userSecretIn(t *testing.T, workspaceID string) string {
	t.Helper()
	v, err := r.stores.Secrets.GetUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, linear.UserTokenKey(workspaceID))
	if err != nil {
		t.Fatalf("read user credential: %v", err)
	}
	return v
}

// TestLinearIdentity_WorkspaceMovesUnderAWaitingBind: a key checked against
// the org's workspace, whose bind then waits on the user's lock while the org
// moves to another workspace, is refused rather than stored under the
// workspace the org had when the request began.
func TestLinearIdentity_WorkspaceMovesUnderAWaitingBind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		move     func(r *linearConnectRig)
		wantCode int
		reason   string
	}{
		{"moved", func(r *linearConnectRig) { r.moveOrgWorkspace(t, betaWorkspace) }, http.StatusUnprocessableEntity, "UPSTREAM_REJECTED"},
		{"unbound", func(r *linearConnectRig) {
			if rec := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil); rec.Code != http.StatusOK {
				t.Fatalf("unbind: %d: %s", rec.Code, rec.Body.String())
			}
		}, http.StatusConflict, "NOT_CONFIGURED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLinearConnectRig(t)
			release := r.holdUserLock(t)
			callsBefore := r.fake.Calls()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- r.pasteKey(t, linearUserKey) }()
			// The paste has checked its key against Acme and is waiting on
			// the lock.
			deadline := time.Now().Add(5 * time.Second)
			for r.fake.Calls() == callsBefore && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			tc.move(r)
			release()

			rec := <-done
			if rec.Code != tc.wantCode || errorItems(t, rec)[0].Reason != tc.reason {
				t.Fatalf("paste = %d %s, want %d %s", rec.Code, rec.Body.String(), tc.wantCode, tc.reason)
			}
			for _, ws := range []string{acmeWorkspaceID, betaWorkspace.ID} {
				if v := r.userSecretIn(t, ws); v != "" {
					t.Errorf("credential stored under %s: %q", ws, v)
				}
			}
			if id, _, _ := r.identity(t); id != "" {
				t.Errorf("identity %q stored for the workspace the org left", id)
			}
		})
	}
}

// TestLinearConnect_WorkspaceMovesDuringTheExchange: the org moving while a
// Connect's code is exchanged refuses the store as a workspace mismatch and
// revokes the minted pair, rather than storing the grant under the workspace
// the callback read first.
func TestLinearConnect_WorkspaceMovesDuringTheExchange(t *testing.T) {
	r := newLinearConnectRig(t)
	cookie, state := r.startConnect(t, "")
	var lockFree, userLockFree bool
	r.oauth.mu.Lock()
	r.oauth.onExchange = func() {
		// Neither lock is held while Linear is being called.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if release, err := r.s.lockLinearCredential(ctx, runmode.LocalDefaultOrgID); err == nil {
			lockFree = true
			release()
		}
		userLockFree = !r.lockHeldElsewhere(t)
		r.moveOrgWorkspace(t, betaWorkspace)
	}
	r.oauth.mu.Unlock()

	rec := r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
	expectConnectError(t, rec, "workspace_mismatch", "")
	if !lockFree || !userLockFree {
		t.Errorf("lock free during the code exchange: org %v, user %v; want both", lockFree, userLockFree)
	}
	if got := r.oauth.Revoked(); len(got) != 2 {
		t.Errorf("revoked %v, want the minted pair", got)
	}
	for _, ws := range []string{acmeWorkspaceID, betaWorkspace.ID} {
		if v := r.userSecretIn(t, ws); v != "" {
			t.Errorf("credential stored under %s: %q", ws, v)
		}
	}
}

// TestLinearConnect_AppChangesDuringTheExchange: the code is exchanged
// against the app that resolved when the callback started. An app replaced or
// removed before the grant is stored would leave it unable to refresh, so the
// store refuses, the minted pair is revoked with the app that minted it, and
// nothing is written.
func TestLinearConnect_AppChangesDuringTheExchange(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, r *linearConnectRig){
		"replaced": func(t *testing.T, r *linearConnectRig) { r.registerApp(t, "lin-client-2") },
		"removed": func(t *testing.T, r *linearConnectRig) {
			if rec := doJSON(t, r.s, http.MethodDelete, linearAppPath(), nil); rec.Code != http.StatusOK {
				t.Errorf("DELETE app mid-exchange: %d: %s", rec.Code, rec.Body.String())
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLinearConnectRig(t)
			cookie, state := r.startConnect(t, "")
			r.oauth.mu.Lock()
			r.oauth.onExchange = func() { change(t, r) }
			r.oauth.mu.Unlock()

			rec := r.get(t, linearConnectCallback(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
			expectConnectError(t, rec, "connect_failed", "")
			r.assertNoUserCredential(t)
			if got := r.oauth.Revoked(); len(got) != 2 {
				t.Errorf("revoked %v, want the minted pair", got)
			}
			if got := r.oauth.RevokedBy(); !slices.Equal(got, []string{"lin-client-1", "lin-client-1"}) {
				t.Errorf("revoked with %v, want the app that minted the pair", got)
			}
		})
	}
}

// TestLinearIdentity_DisconnectActsOnTheWorkspaceItRunsIn: a disconnect that
// waits on the user's lock while the org moves withdraws the credential for
// the workspace the org is on when it runs, not the one it was on when the
// request arrived.
func TestLinearIdentity_DisconnectActsOnTheWorkspaceItRunsIn(t *testing.T) {
	r := newLinearConnectRig(t)
	ctx := t.Context()
	for _, ws := range []string{acmeWorkspaceID, betaWorkspace.ID} {
		if err := r.stores.Secrets.PutUser(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, linear.UserTokenKey(ws), `{"method":"api_key","token":"lin_api_`+ws+`"}`, ""); err != nil {
			t.Fatalf("seed %s: %v", ws, err)
		}
	}
	release := r.holdUserLock(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doJSON(t, r.s, http.MethodDelete, linearIdentityPath(), nil) }()
	time.Sleep(100 * time.Millisecond)
	r.moveOrgWorkspace(t, betaWorkspace)
	release()

	if rec := <-done; rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	if v := r.userSecretIn(t, betaWorkspace.ID); v != "" {
		t.Errorf("the credential for the org's current workspace survived the disconnect: %q", v)
	}
	if v := r.userSecretIn(t, acmeWorkspaceID); v == "" {
		t.Error("the disconnect removed the credential for the workspace the org left")
	}
}

// TestLinearIdentity_SweepActsOnTheWorkspaceItRunsIn: a removal whose sweep
// waits on the member's lock while the org moves to another workspace
// removes the member's credential for the workspace the org is on when the
// sweep runs.
func TestLinearIdentity_SweepActsOnTheWorkspaceItRunsIn(t *testing.T) {
	rig := newAuthRig(t)
	s := rig.srv
	whoami := newLinearWhoamiFake(t)
	s.validateLinear = func(ctx context.Context, cfg linear.Config) (*auth.LinearUser, *auth.LinearOrganization, error) {
		cfg.Endpoint = whoami.URL
		return auth.ValidateLinear(ctx, cfg)
	}
	stores := pgstore.New(rig.h.AdminDB, rig.h.AppDB, pgtest.SecretKey)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org")
	sidA := rig.signIn(alice)
	orgPath := "/api/orgs/" + orgA.String()
	if rec := rig.tokensJSON(http.MethodPut, orgPath+"/linear/access/credential", map[string]any{"api_key": linearOrgKey}, sidA, ""); rec.Code != http.StatusOK {
		t.Fatalf("org bind: %d: %s", rec.Code, rec.Body.String())
	}
	carol := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		carol.String(), orgA.String()); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	secret := func(ws string) string {
		t.Helper()
		v, err := stores.Secrets.GetUserSystem(t.Context(), orgA.String(), carol.String(), linear.UserTokenKey(ws))
		if err != nil {
			t.Fatalf("read secret: %v", err)
		}
		return v
	}
	for _, ws := range []string{acmeWorkspaceID, betaWorkspace.ID} {
		if err := stores.Secrets.PutUserSystem(t.Context(), orgA.String(), carol.String(), linear.UserTokenKey(ws), `{"method":"api_key","token":"lin_api_`+ws+`"}`, ""); err != nil {
			t.Fatalf("seed %s: %v", ws, err)
		}
	}

	release, err := s.linearUserCredentialLock.Lock(t.Context(), orgA.String(), carol.String())
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- rig.tokensJSON(http.MethodDelete, orgPath+"/members/"+carol.String(), nil, sidA, "") }()
	// The removal has committed and its sweep waits on carol's lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := rig.h.AdminDB.QueryRow(`SELECT count(*) FROM public.org_memberships WHERE user_id = $1 AND org_id = $2`,
			carol.String(), orgA.String()).Scan(&n); err == nil && n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the removal never committed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	whoami.setWorkspace(betaWorkspace)
	if rec := rig.tokensJSON(http.MethodPut, orgPath+"/linear/access/credential", map[string]any{"api_key": "lin_api_org_beta"}, sidA, ""); rec.Code != http.StatusOK {
		t.Fatalf("move org workspace: %d: %s", rec.Code, rec.Body.String())
	}
	release()

	if rec := <-done; rec.Code != http.StatusNoContent {
		t.Fatalf("remove carol: %d: %s", rec.Code, rec.Body.String())
	}
	if v := secret(betaWorkspace.ID); v != "" {
		t.Errorf("carol's credential for the org's current workspace survived her removal: %q", v)
	}
}
