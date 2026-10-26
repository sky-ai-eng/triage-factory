package jiraoauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// fakeRefresher records the refresh tokens it's handed and rotates: each call
// returns a fresh access + refresh token derived from a monotonic counter, so a
// test can prove which refresh token the cache passed in (write-back) and that
// the rotated one is stored.
type fakeRefresher struct {
	mu      sync.Mutex
	seen    []string // refresh tokens passed in, in call order
	n       int
	expires time.Duration
	now     func() time.Time // shares the cache's clock so expiry is deterministic
	err     error
}

func (f *fakeRefresher) Refresh(_ context.Context, _ jira.OAuthApp, refreshToken string) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Token{}, f.err
	}
	f.seen = append(f.seen, refreshToken)
	f.n++
	exp := f.expires
	if exp == 0 {
		exp = time.Hour
	}
	base := time.Now()
	if f.now != nil {
		base = f.now()
	}
	return Token{
		AccessToken:  fmt.Sprintf("acc-%d", f.n),
		RefreshToken: fmt.Sprintf("ref-%d", f.n),
		ExpiresAt:    base.Add(exp),
	}, nil
}

var (
	orgApp        = jira.OAuthApp{ClientID: "org-client", ClientSecret: "org-secret"}
	deploymentApp = jira.OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"}
)

const orgAppSecretRef = "jira_oauth_client_secret"

// fakeJiraApps holds the org's org_jira_apps row, nil for none. Embeds the
// interface so the unexercised methods compile-satisfy.
type fakeJiraApps struct {
	db.JiraAppsStore
	app *domain.OrgJiraApp
}

func (f *fakeJiraApps) GetForOrgSystem(context.Context, string) (*domain.OrgJiraApp, error) {
	return f.app, nil
}

// testApps is the production resolver over the org's own app (a row and its
// secret) and the deployment app. setOrg changes the org's own app, as an
// admin does; the zero app removes it.
type testApps struct {
	jira.OAuthAppResolver
	rows    *fakeJiraApps
	secrets *fakeSecrets
}

func newTestApps(secrets *fakeSecrets, own, deployment jira.OAuthApp) *testApps {
	a := &testApps{rows: &fakeJiraApps{}, secrets: secrets}
	a.OAuthAppResolver = jira.NewOAuthAppResolver(a.rows, secrets, deployment)
	a.setOrg(own)
	return a
}

// orgAppOnly is an org with its own app and no deployment app.
func orgAppOnly(secrets *fakeSecrets) *testApps {
	return newTestApps(secrets, orgApp, jira.OAuthApp{})
}

func (a *testApps) setOrg(app jira.OAuthApp) {
	if !app.Configured() {
		a.rows.app = nil
		return
	}
	a.rows.app = &domain.OrgJiraApp{OrgID: cOrg, ClientID: app.ClientID, ClientSecretRef: orgAppSecretRef}
	a.secrets.mu.Lock()
	a.secrets.sys[orgAppSecretRef] = app.ClientSecret
	a.secrets.mu.Unlock()
}

// fakeSecrets is an in-memory per-user secret bag exercising GetUserSystem +
// PutUserSystem (the system doors the cache uses). Embeds the interface so the
// unexercised methods compile-satisfy.
type fakeSecrets struct {
	db.SecretStore
	mu   sync.Mutex
	bag  map[string]string
	sys  map[string]string // org-level secrets, by key
	puts int
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{bag: map[string]string{}, sys: map[string]string{}}
}

func (f *fakeSecrets) GetSystem(_ context.Context, _, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sys[key], nil
}

func (f *fakeSecrets) key(orgID, userID, key string) string {
	return orgID + "|" + userID + "|" + key
}

func (f *fakeSecrets) GetUserSystem(_ context.Context, orgID, userID, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bag[f.key(orgID, userID, key)], nil
}

func (f *fakeSecrets) PutUserSystem(_ context.Context, orgID, userID, key, value, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bag[f.key(orgID, userID, key)] = value
	f.puts++
	return nil
}

func (f *fakeSecrets) PutUserSystemIfValue(_ context.Context, orgID, userID, key, old, value, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(orgID, userID, key)
	if cur, ok := f.bag[k]; !ok || cur != old {
		return false, nil
	}
	f.bag[k] = value
	f.puts++
	return true, nil
}

func (f *fakeSecrets) DeleteUserSystemIfValue(_ context.Context, orgID, userID, key, value string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(orgID, userID, key)
	if cur, ok := f.bag[k]; !ok || cur != value {
		return false, nil
	}
	delete(f.bag, k)
	return true, nil
}

const (
	cOrg  = "org-1"
	cUser = "user-1"
	cHost = "https://acme.atlassian.net"
)

// seedOAuthEnvelope stores a credential that does not record the app that
// issued it, as one stored before the envelope carried client_id.
func seedOAuthEnvelope(t *testing.T, secrets *fakeSecrets, refreshToken string) {
	t.Helper()
	seedIssuedEnvelope(t, secrets, refreshToken, "")
}

// seedIssuedEnvelope stores a credential issued by the app with clientID.
func seedIssuedEnvelope(t *testing.T, secrets *fakeSecrets, refreshToken, clientID string) {
	t.Helper()
	env, err := jira.MarshalUserCredential(jira.UserCredential{
		Method:       jira.AuthMethodCloudOAuth,
		CloudID:      "cloud-1",
		RefreshToken: refreshToken,
		ClientID:     clientID,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := secrets.PutUserSystem(context.Background(), cOrg, cUser, jira.UserTokenKey(cHost), env, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func storedRefresh(t *testing.T, secrets *fakeSecrets) string {
	t.Helper()
	return storedCredential(t, secrets).RefreshToken
}

func storedCredential(t *testing.T, secrets *fakeSecrets) jira.UserCredential {
	t.Helper()
	raw, _ := secrets.GetUserSystem(context.Background(), cOrg, cUser, jira.UserTokenKey(cHost))
	cred, err := jira.ParseUserCredential(raw)
	if err != nil {
		t.Fatalf("parse stored: %v", err)
	}
	return cred
}

// TestTokenCache_RotationWriteBack is the acceptance scenario: after the access
// token expires, a mint succeeds and the rotated refresh token is persisted; a
// SECOND mint after that also succeeds, using the ROTATED token (proving the
// write-back landed). The cache_id is returned both times.
func TestTokenCache_RotationWriteBack(t *testing.T) {
	secrets := newFakeSecrets()
	secrets.puts = 0 // ignore the seed put
	seedOAuthEnvelope(t, secrets, "ref-0")
	secrets.puts = 0

	clock := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	ref := &fakeRefresher{expires: time.Hour, now: now}
	cache := newTokenCache(ref, orgAppOnly(secrets), secrets)
	cache.now = now

	// First mint: refreshes ref-0 → access acc-1 / rotate ref-1, stored back.
	tok, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if tok.CloudID != "cloud-1" {
		t.Errorf("cloudID = %q, want cloud-1", tok.CloudID)
	}
	if tok.AccessToken != "acc-1" {
		t.Errorf("access = %q, want acc-1", tok.AccessToken)
	}
	if got := storedCredential(t, secrets); got.RefreshToken != "ref-1" || got.ClientID != orgApp.ClientID {
		t.Fatalf("stored credential after first mint = (refresh %q, client %q), want ref-1 issued by %s (write-back failed)",
			got.RefreshToken, got.ClientID, orgApp.ClientID)
	}

	// Advance the clock past the first token's expiry, then mint again. The
	// cache must read the ROTATED token (ref-1) and refresh with it.
	clock = clock.Add(2 * time.Hour)
	tok2, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if tok2.AccessToken != "acc-2" {
		t.Errorf("second access = %q, want acc-2", tok2.AccessToken)
	}
	if got := storedRefresh(t, secrets); got != "ref-2" {
		t.Errorf("stored refresh after second mint = %q, want ref-2", got)
	}

	// The proof: the second refresh used the rotated ref-1, not the stale ref-0.
	if len(ref.seen) != 2 || ref.seen[0] != "ref-0" || ref.seen[1] != "ref-1" {
		t.Errorf("refresh tokens passed in = %v, want [ref-0 ref-1]", ref.seen)
	}
}

// TestTokenCache_CachesWithinExpiry pins that a second read within the access
// token's lifetime serves the cached token without refreshing (no token churn,
// no extra rotation).
func TestTokenCache_CachesWithinExpiry(t *testing.T) {
	secrets := newFakeSecrets()
	seedOAuthEnvelope(t, secrets, "ref-0")

	clock := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	ref := &fakeRefresher{expires: time.Hour, now: now}
	cache := newTokenCache(ref, orgAppOnly(secrets), secrets)
	cache.now = now

	if _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Advance only a little — still well within the hour.
	clock = clock.Add(5 * time.Minute)
	if tok, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil || tok.AccessToken != "acc-1" {
		t.Fatalf("second (cached) = %q, %v; want acc-1, nil", tok.AccessToken, err)
	}
	if ref.n != 1 {
		t.Errorf("refresh called %d times, want 1 (second read should be cached)", ref.n)
	}
}

// TestTokenCache_RefreshesWithTheAppThatIssuedTheToken is the case an admin
// adding the org's own app used to break: a credential the deployment app
// issued keeps refreshing with the deployment app, since Atlassian accepts a
// refresh token only from the app that issued it. Each token reports the app
// that minted it, whose quota its requests spend.
func TestTokenCache_RefreshesWithTheAppThatIssuedTheToken(t *testing.T) {
	secrets := newFakeSecrets()
	seedIssuedEnvelope(t, secrets, "ref-0", deploymentApp.ClientID)
	endpoint := &fakeTokenEndpoint{}
	minter := endpoint.minter(t)
	clock := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	minter.now = func() time.Time { return clock }
	apps := newTestApps(secrets, jira.OAuthApp{}, deploymentApp)
	cache := newTokenCache(minter, apps, secrets)
	cache.now = func() time.Time { return clock }

	tok, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil || tok.App != jira.SourceDeployment {
		t.Fatalf("first mint = (app %d, %v), want the deployment app", tok.App, err)
	}

	// The admin adds the org's own app. The cached token is still the
	// deployment app's until it expires.
	apps.setOrg(orgApp)
	clock = clock.Add(5 * time.Minute)
	tok, err = cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil || tok.AccessToken != "acc-1" || tok.App != jira.SourceDeployment {
		t.Fatalf("cached read after the app changed = (%q, app %d, %v), want acc-1 minted under the deployment app",
			tok.AccessToken, tok.App, err)
	}

	clock = clock.Add(2 * time.Hour)
	tok, err = cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil || tok.AccessToken != "acc-2" || tok.App != jira.SourceDeployment {
		t.Fatalf("mint after expiry = (%q, app %d, %v), want acc-2 minted under the deployment app",
			tok.AccessToken, tok.App, err)
	}
	if got := endpoint.clientIDs(); !slices.Equal(got, []string{"dep-client", "dep-client"}) {
		t.Errorf("token requests' client_id = %v, want both from the deployment app that issued the token", got)
	}
	if got := storedCredential(t, secrets); got.ClientID != "dep-client" || got.RefreshToken != "ref-2" {
		t.Errorf("stored credential = (client %q, refresh %q), want the deployment app's ref-2", got.ClientID, got.RefreshToken)
	}
}

// TestTokenCache_IssuingAppRemovedKeepsTheCredential: the org can no longer
// authenticate as the app that issued the token, so no app can refresh it.
// That is answered without a token request, and the credential is kept:
// configuring the same app again brings it back.
func TestTokenCache_IssuingAppRemovedKeepsTheCredential(t *testing.T) {
	secrets := newFakeSecrets()
	seedIssuedEnvelope(t, secrets, "ref-0", "gone-client")
	before := storedEnvelope(secrets)
	endpoint := &fakeTokenEndpoint{}
	apps := newTestApps(secrets, orgApp, deploymentApp)
	cache := newTokenCache(endpoint.minter(t), apps, secrets)

	_, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if !errors.Is(err, jira.ErrJiraUserCredentialAppRemoved) || !errors.Is(err, jira.ErrNoJiraUserCredential) {
		t.Fatalf("err = %v, want ErrJiraUserCredentialAppRemoved, which is ErrNoJiraUserCredential", err)
	}
	if errors.Is(err, jira.ErrJiraUserCredentialRefused) {
		t.Errorf("err = %v, want it not to read as a refused grant", err)
	}
	if got := endpoint.tokens(); len(got) != 0 {
		t.Errorf("token requests = %v, want none", got)
	}
	if got := storedEnvelope(secrets); got != before {
		t.Errorf("stored credential changed to %q, want it kept", got)
	}

	apps.setOrg(jira.OAuthApp{ClientID: "gone-client", ClientSecret: "restored-secret"})
	if _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil {
		t.Fatalf("after the app was configured again: %v", err)
	}
	if got := endpoint.clientIDs(); !slices.Equal(got, []string{"gone-client"}) {
		t.Errorf("token requests' client_id = %v, want the restored app", got)
	}
}

// TestTokenCache_RotationWriteBackKeepsTheIssuingApp: the envelope written back
// after a refresh still names the app that issued its token.
func TestTokenCache_RotationWriteBackKeepsTheIssuingApp(t *testing.T) {
	secrets := newFakeSecrets()
	seedIssuedEnvelope(t, secrets, "ref-0", orgApp.ClientID)
	cache := newTokenCache(&fakeRefresher{}, orgAppOnly(secrets), secrets)

	if _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil {
		t.Fatalf("AccessTokenForUser: %v", err)
	}
	got := storedCredential(t, secrets)
	if got.RefreshToken != "ref-1" || got.ClientID != orgApp.ClientID || got.CloudID != "cloud-1" {
		t.Errorf("stored credential = %+v, want ref-1 issued by %s on cloud-1", got, orgApp.ClientID)
	}
}

// TestTokenCache_UnattributedTokenTriesEachApp: a credential that does not
// record its app is refreshed with the app the org resolves now, and then
// with the other one when the first says the token may be another client's.
// The app that succeeds is recorded.
func TestTokenCache_UnattributedTokenTriesEachApp(t *testing.T) {
	secrets := newFakeSecrets()
	seedOAuthEnvelope(t, secrets, "ref-0")
	endpoint := &fakeTokenEndpoint{answer: func(client, _ string) (int, string) {
		if client == orgApp.ClientID {
			return http.StatusBadRequest, `{"error":"invalid_grant"}`
		}
		return 0, ""
	}}
	cache := newTokenCache(endpoint.minter(t), newTestApps(secrets, orgApp, deploymentApp), secrets)

	tok, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil {
		t.Fatalf("AccessTokenForUser = %v, want the deployment app's refresh to succeed", err)
	}
	if tok.App != jira.SourceDeployment {
		t.Errorf("token app = %d, want the deployment app that refreshed it", tok.App)
	}
	if got := endpoint.clientIDs(); !slices.Equal(got, []string{"org-client", "dep-client"}) {
		t.Errorf("token requests' client_id = %v, want the org's app, then the deployment app", got)
	}
	if got := storedCredential(t, secrets); got.ClientID != "dep-client" || got.RefreshToken != "ref-2" {
		t.Errorf("stored credential = (client %q, refresh %q), want the deployment app's ref-2", got.ClientID, got.RefreshToken)
	}
}

// TestTokenCache_UnattributedTokenEveryAppRefusedLeavesARefusedMarker: when
// every app the org can authenticate as refuses the grant, none of them can
// refresh it, so it is replaced with a refused marker as a refused grant is.
func TestTokenCache_UnattributedTokenEveryAppRefusedLeavesARefusedMarker(t *testing.T) {
	secrets := newFakeSecrets()
	seedOAuthEnvelope(t, secrets, "ref-0")
	endpoint := &fakeTokenEndpoint{answer: func(string, string) (int, string) {
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	}}
	cache := newTokenCache(endpoint.minter(t), newTestApps(secrets, orgApp, deploymentApp), secrets)

	_, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if !errors.Is(err, jira.ErrJiraUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrJiraUserCredentialRefused", err)
	}
	assertRefusedMarker(t, secrets)
	if got := endpoint.clientIDs(); !slices.Equal(got, []string{"org-client", "dep-client"}) {
		t.Errorf("token requests' client_id = %v, want each app once", got)
	}
}

// TestTokenCache_UnattributedTokenOtherFailuresKeepTheCredential: a failure
// that says nothing about whose token it is stops the refresh at that app, and
// an app refusing the client rather than the grant means the token may still
// be good. Either way the credential is kept.
func TestTokenCache_UnattributedTokenOtherFailuresKeepTheCredential(t *testing.T) {
	cases := []struct {
		name   string
		answer func(clientID string) (status int, body string)
		want   []string // the token requests' client_id
	}{
		{
			name: "first_unavailable",
			answer: func(string) (int, string) {
				return http.StatusServiceUnavailable, `<html>down</html>`
			},
			want: []string{"org-client"},
		},
		{
			name: "grant_then_client_refused",
			answer: func(client string) (int, string) {
				if client == orgApp.ClientID {
					return http.StatusBadRequest, `{"error":"invalid_grant"}`
				}
				return http.StatusUnauthorized, `{"error":"invalid_client"}`
			},
			want: []string{"org-client", "dep-client"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secrets := newFakeSecrets()
			seedOAuthEnvelope(t, secrets, "ref-0")
			before := storedEnvelope(secrets)
			endpoint := &fakeTokenEndpoint{answer: func(client, _ string) (int, string) { return tc.answer(client) }}
			cache := newTokenCache(endpoint.minter(t), newTestApps(secrets, orgApp, deploymentApp), secrets)

			_, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
			if err == nil || errors.Is(err, jira.ErrNoJiraUserCredential) {
				t.Fatalf("err = %v, want a failure that is not a missing credential", err)
			}
			if got := endpoint.clientIDs(); !slices.Equal(got, tc.want) {
				t.Errorf("token requests' client_id = %v, want %v", got, tc.want)
			}
			if got := storedEnvelope(secrets); got != before {
				t.Errorf("stored credential changed to %q, want it kept", got)
			}
		})
	}
}

// TestTokenCache_Invalidate forces the next read to mint fresh: after a
// re-Connect / paste-over-OAuth the cached token is tied to a superseded refresh
// token, so Invalidate must drop it even well within the access token's lifetime.
func TestTokenCache_Invalidate(t *testing.T) {
	secrets := newFakeSecrets()
	seedOAuthEnvelope(t, secrets, "ref-0")

	clock := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	ref := &fakeRefresher{expires: time.Hour, now: now}
	cache := newTokenCache(ref, orgAppOnly(secrets), secrets)
	cache.now = now

	if _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil {
		t.Fatalf("first: %v", err)
	}
	cache.Invalidate(cOrg, cUser, cHost)

	// Still within the hour, but the cache was invalidated — must refresh again.
	clock = clock.Add(time.Minute)
	if tok, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil || tok.AccessToken != "acc-2" {
		t.Fatalf("post-invalidate read = %q, %v; want acc-2, nil", tok.AccessToken, err)
	}
	if ref.n != 2 {
		t.Errorf("refresh called %d times, want 2 (invalidate should force a fresh mint)", ref.n)
	}
}

// TestTokenCache_NoCredential errors clearly when there's no stored envelope.
func TestTokenCache_NoCredential(t *testing.T) {
	secrets := newFakeSecrets()
	cache := newTokenCache(&fakeRefresher{}, orgAppOnly(secrets), secrets)
	if _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err == nil {
		t.Fatal("want error for missing credential, got nil")
	}
}

// TestTokenCache_WrongMethod refuses a non-OAuth envelope (e.g. a leftover DC
// PAT) rather than trying to refresh it.
func TestTokenCache_WrongMethod(t *testing.T) {
	secrets := newFakeSecrets()
	env, _ := jira.MarshalUserCredential(jira.UserCredential{Method: jira.AuthMethodDCPAT, Token: "pat"})
	_ = secrets.PutUserSystem(context.Background(), cOrg, cUser, jira.UserTokenKey(cHost), env, "")

	cache := newTokenCache(&fakeRefresher{}, orgAppOnly(secrets), secrets)
	if _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err == nil {
		t.Fatal("want error for non-oauth credential, got nil")
	}
}

// fakeTokenEndpoint is a fake Atlassian token endpoint behind a real Minter, so the
// refusals the cache reacts to are classified exactly as production classifies
// them. answer decides each request by the refresh token it carries; nil
// answers a fresh rotation.
type fakeTokenEndpoint struct {
	mu      sync.Mutex
	seen    []string
	clients []string // the client_id of each request, in call order
	n       int
	answer  func(clientID, refreshToken string) (status int, body string)
}

func (e *fakeTokenEndpoint) minter(t *testing.T) *Minter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		token, client := r.PostForm.Get("refresh_token"), r.PostForm.Get("client_id")
		e.mu.Lock()
		e.seen = append(e.seen, token)
		e.clients = append(e.clients, client)
		e.n++
		n := e.n
		answer := e.answer
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if answer != nil {
			if status, body := answer(client, token); status != 0 {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
		}
		fmt.Fprintf(w, `{"access_token":"acc-%d","refresh_token":"ref-%d","expires_in":3600}`, n, n)
	}))
	t.Cleanup(srv.Close)
	return &Minter{httpClient: srv.Client(), tokenURL: srv.URL}
}

func (e *fakeTokenEndpoint) tokens() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...)
}

func (e *fakeTokenEndpoint) clientIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.clients...)
}

func storedEnvelope(secrets *fakeSecrets) string {
	raw, _ := secrets.GetUserSystem(context.Background(), cOrg, cUser, jira.UserTokenKey(cHost))
	return raw
}

// assertRefusedMarker checks that the stored credential is a refused marker,
// carrying nothing of the credential it replaced.
func assertRefusedMarker(t *testing.T, secrets *fakeSecrets) {
	t.Helper()
	got := storedCredential(t, secrets)
	want := jira.UserCredential{Method: jira.AuthMethodCloudOAuth, Refused: true}
	if got != want {
		t.Errorf("stored credential = %+v, want a refused marker %+v", got, want)
	}
}

// TestTokenCache_RefusedGrantLeavesARefusedMarker: a refresh token Atlassian
// refuses with invalid_grant is dead, so the credential holding it is replaced
// with a refused marker and the refusal reads as a credential the user has to
// connect again.
func TestTokenCache_RefusedGrantLeavesARefusedMarker(t *testing.T) {
	secrets := newFakeSecrets()
	seedIssuedEnvelope(t, secrets, "ref-0", orgApp.ClientID)
	endpoint := &fakeTokenEndpoint{answer: func(string, string) (int, string) {
		return http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Unknown or invalid refresh token."}`
	}}
	cache := newTokenCache(endpoint.minter(t), orgAppOnly(secrets), secrets)

	_, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if !errors.Is(err, jira.ErrJiraUserCredentialRefused) || !errors.Is(err, jira.ErrNoJiraUserCredential) {
		t.Fatalf("err = %v, want ErrJiraUserCredentialRefused, which is ErrNoJiraUserCredential", err)
	}
	assertRefusedMarker(t, secrets)
	if got := endpoint.tokens(); len(got) != 1 {
		t.Errorf("token requests = %v, want one: nothing replaced the refused token, so nothing is retried", got)
	}
}

// TestTokenCache_RefusedMarkerAnswersWithoutATokenRequest: a stored refused
// marker has no token to refresh, so it is answered as the refusal it records
// without asking Atlassian, and is left as it is.
func TestTokenCache_RefusedMarkerAnswersWithoutATokenRequest(t *testing.T) {
	secrets := newFakeSecrets()
	env, err := jira.MarshalUserCredential(jira.UserCredential{Method: jira.AuthMethodCloudOAuth, Refused: true})
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := secrets.PutUserSystem(context.Background(), cOrg, cUser, jira.UserTokenKey(cHost), env, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	endpoint := &fakeTokenEndpoint{}
	cache := newTokenCache(endpoint.minter(t), orgAppOnly(secrets), secrets)

	_, err = cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if !errors.Is(err, jira.ErrJiraUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrJiraUserCredentialRefused", err)
	}
	if got := endpoint.tokens(); len(got) != 0 {
		t.Errorf("token requests = %v, want none", got)
	}
	if got := storedEnvelope(secrets); got != env {
		t.Errorf("stored credential = %q, want the marker %q kept", got, env)
	}
}

// TestTokenCache_RefusedGrantOfATokenAnotherProcessRotatedRetries: the token
// was rotated by another process between this one's read and its refresh, so
// Atlassian refuses the old one here. The credential now stored is live: it is
// kept, and the refresh is tried again with it.
func TestTokenCache_RefusedGrantOfATokenAnotherProcessRotatedRetries(t *testing.T) {
	secrets := newFakeSecrets()
	seedOAuthEnvelope(t, secrets, "ref-0")
	endpoint := &fakeTokenEndpoint{}
	endpoint.answer = func(_, token string) (int, string) {
		if token != "ref-0" {
			return 0, ""
		}
		// The other process's rotation write-back lands first.
		seedOAuthEnvelope(t, secrets, "ref-other")
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	}
	cache := newTokenCache(endpoint.minter(t), orgAppOnly(secrets), secrets)

	tok, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil {
		t.Fatalf("AccessTokenForUser = %v, want the retry with the rotated token to succeed", err)
	}
	if tok.AccessToken != "acc-2" {
		t.Errorf("access token = %q, want the retry's acc-2", tok.AccessToken)
	}
	if got := endpoint.tokens(); len(got) != 2 || got[1] != "ref-other" {
		t.Errorf("token requests = %v, want ref-0 then the rotated ref-other", got)
	}
	if got := storedRefresh(t, secrets); got != "ref-2" {
		t.Errorf("stored refresh token = %q, want the retry's rotation ref-2", got)
	}
}

// TestTokenCache_RefusedAgainAfterASecondRotationAsksToConnectAgain: the
// retry's token is rotated away under it too, so the refresh cannot settle the
// credential here. The refusal still reads as one the user fixes by connecting
// again, never as a refusal of the org's OAuth app, which would send them to
// an admin with nothing to fix. The credential another process stored is kept.
func TestTokenCache_RefusedAgainAfterASecondRotationAsksToConnectAgain(t *testing.T) {
	secrets := newFakeSecrets()
	seedOAuthEnvelope(t, secrets, "ref-0")
	endpoint := &fakeTokenEndpoint{}
	endpoint.answer = func(_, token string) (int, string) {
		switch token {
		case "ref-0":
			seedOAuthEnvelope(t, secrets, "ref-other")
		case "ref-other":
			seedOAuthEnvelope(t, secrets, "ref-third")
		}
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	}
	cache := newTokenCache(endpoint.minter(t), orgAppOnly(secrets), secrets)

	_, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if !errors.Is(err, jira.ErrJiraUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrJiraUserCredentialRefused", err)
	}
	if got := storedRefresh(t, secrets); got != "ref-third" {
		t.Errorf("stored refresh token = %q, want the other process's ref-third kept", got)
	}
}

// TestTokenCache_OtherFailuresKeepTheCredential: a refusal of the org's OAuth
// app, a rate limit and an outage say nothing about the user's grant, so the
// credential stays and the error carries its class.
func TestTokenCache_OtherFailuresKeepTheCredential(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"invalid_client", http.StatusUnauthorized, `{"error":"invalid_client"}`},
		{"rate_limited", http.StatusTooManyRequests, `{"error":"rate_limited"}`},
		{"unavailable", http.StatusServiceUnavailable, `<html>down</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secrets := newFakeSecrets()
			seedOAuthEnvelope(t, secrets, "ref-0")
			before := storedEnvelope(secrets)
			endpoint := &fakeTokenEndpoint{answer: func(string, string) (int, string) { return tc.status, tc.body }}
			cache := newTokenCache(endpoint.minter(t), orgAppOnly(secrets), secrets)

			_, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
			if err == nil || errors.Is(err, jira.ErrNoJiraUserCredential) {
				t.Fatalf("err = %v, want a failure that is not a missing credential", err)
			}
			if _, ok := upstream.ClassOf(err); !ok {
				t.Errorf("err = %v, want it to carry an upstream class", err)
			}
			if got := storedEnvelope(secrets); got != before {
				t.Errorf("stored credential changed to %q, want it kept", got)
			}
		})
	}
}
