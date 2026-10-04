package jiraoauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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

// fakeAppResolver always resolves a fixed app.
type fakeAppResolver struct{}

func (fakeAppResolver) Resolve(_ context.Context, _ string) (jira.OAuthApp, jira.OAuthAppSource, error) {
	return jira.OAuthApp{ClientID: "c", ClientSecret: "s"}, jira.SourceOrgOverride, nil
}

// fakeSecrets is an in-memory per-user secret bag exercising GetUserSystem +
// PutUserSystem (the system doors the cache uses). Embeds the interface so the
// unexercised methods compile-satisfy.
type fakeSecrets struct {
	db.SecretStore
	mu   sync.Mutex
	bag  map[string]string
	puts int
}

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{bag: map[string]string{}} }

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

func seedOAuthEnvelope(t *testing.T, secrets *fakeSecrets, refreshToken string) {
	t.Helper()
	env, err := jira.MarshalUserCredential(jira.UserCredential{
		Method:       jira.AuthMethodCloudOAuth,
		CloudID:      "cloud-1",
		RefreshToken: refreshToken,
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
	raw, _ := secrets.GetUserSystem(context.Background(), cOrg, cUser, jira.UserTokenKey(cHost))
	cred, err := jira.ParseUserCredential(raw)
	if err != nil {
		t.Fatalf("parse stored: %v", err)
	}
	return cred.RefreshToken
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
	cache := newTokenCache(ref, fakeAppResolver{}, secrets)
	cache.now = now

	// First mint: refreshes ref-0 → access acc-1 / rotate ref-1, stored back.
	cloudID, access, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if cloudID != "cloud-1" {
		t.Errorf("cloudID = %q, want cloud-1", cloudID)
	}
	if access != "acc-1" {
		t.Errorf("access = %q, want acc-1", access)
	}
	if got := storedRefresh(t, secrets); got != "ref-1" {
		t.Fatalf("stored refresh after first mint = %q, want ref-1 (write-back failed)", got)
	}

	// Advance the clock past the first token's expiry, then mint again. The
	// cache must read the ROTATED token (ref-1) and refresh with it.
	clock = clock.Add(2 * time.Hour)
	_, access2, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if access2 != "acc-2" {
		t.Errorf("second access = %q, want acc-2", access2)
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
	cache := newTokenCache(ref, fakeAppResolver{}, secrets)
	cache.now = now

	if _, _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Advance only a little — still well within the hour.
	clock = clock.Add(5 * time.Minute)
	if _, access, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil || access != "acc-1" {
		t.Fatalf("second (cached) = %q, %v; want acc-1, nil", access, err)
	}
	if ref.n != 1 {
		t.Errorf("refresh called %d times, want 1 (second read should be cached)", ref.n)
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
	cache := newTokenCache(ref, fakeAppResolver{}, secrets)
	cache.now = now

	if _, _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil {
		t.Fatalf("first: %v", err)
	}
	cache.Invalidate(cOrg, cUser, cHost)

	// Still within the hour, but the cache was invalidated — must refresh again.
	clock = clock.Add(time.Minute)
	if _, access, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err != nil || access != "acc-2" {
		t.Fatalf("post-invalidate read = %q, %v; want acc-2, nil", access, err)
	}
	if ref.n != 2 {
		t.Errorf("refresh called %d times, want 2 (invalidate should force a fresh mint)", ref.n)
	}
}

// TestTokenCache_NoCredential errors clearly when there's no stored envelope.
func TestTokenCache_NoCredential(t *testing.T) {
	secrets := newFakeSecrets()
	cache := newTokenCache(&fakeRefresher{}, fakeAppResolver{}, secrets)
	if _, _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err == nil {
		t.Fatal("want error for missing credential, got nil")
	}
}

// TestTokenCache_WrongMethod refuses a non-OAuth envelope (e.g. a leftover DC
// PAT) rather than trying to refresh it.
func TestTokenCache_WrongMethod(t *testing.T) {
	secrets := newFakeSecrets()
	env, _ := jira.MarshalUserCredential(jira.UserCredential{Method: jira.AuthMethodDCPAT, Token: "pat"})
	_ = secrets.PutUserSystem(context.Background(), cOrg, cUser, jira.UserTokenKey(cHost), env, "")

	cache := newTokenCache(&fakeRefresher{}, fakeAppResolver{}, secrets)
	if _, _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost); err == nil {
		t.Fatal("want error for non-oauth credential, got nil")
	}
}

// fakeTokenEndpoint is a fake Atlassian token endpoint behind a real Minter, so the
// refusals the cache reacts to are classified exactly as production classifies
// them. answer decides each request by the refresh token it carries; nil
// answers a fresh rotation.
type fakeTokenEndpoint struct {
	mu     sync.Mutex
	seen   []string
	n      int
	answer func(refreshToken string) (status int, body string)
}

func (e *fakeTokenEndpoint) minter(t *testing.T) *Minter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		token := r.PostForm.Get("refresh_token")
		e.mu.Lock()
		e.seen = append(e.seen, token)
		e.n++
		n := e.n
		answer := e.answer
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if answer != nil {
			if status, body := answer(token); status != 0 {
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

func storedEnvelope(secrets *fakeSecrets) string {
	raw, _ := secrets.GetUserSystem(context.Background(), cOrg, cUser, jira.UserTokenKey(cHost))
	return raw
}

// TestTokenCache_RefusedGrantRemovesTheCredential: a refresh token Atlassian
// refuses with invalid_grant is dead, so the credential holding it is removed
// and the refusal reads as a credential the user has to connect again.
func TestTokenCache_RefusedGrantRemovesTheCredential(t *testing.T) {
	secrets := newFakeSecrets()
	seedOAuthEnvelope(t, secrets, "ref-0")
	endpoint := &fakeTokenEndpoint{answer: func(string) (int, string) {
		return http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Unknown or invalid refresh token."}`
	}}
	cache := newTokenCache(endpoint.minter(t), fakeAppResolver{}, secrets)

	_, _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if !errors.Is(err, jira.ErrJiraUserCredentialRefused) || !errors.Is(err, jira.ErrNoJiraUserCredential) {
		t.Fatalf("err = %v, want ErrJiraUserCredentialRefused, which is ErrNoJiraUserCredential", err)
	}
	if raw := storedEnvelope(secrets); raw != "" {
		t.Errorf("stored credential = %q, want it removed", raw)
	}
	if got := endpoint.tokens(); len(got) != 1 {
		t.Errorf("token requests = %v, want one: nothing replaced the refused token, so nothing is retried", got)
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
	endpoint.answer = func(token string) (int, string) {
		if token != "ref-0" {
			return 0, ""
		}
		// The other process's rotation write-back lands first.
		seedOAuthEnvelope(t, secrets, "ref-other")
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	}
	cache := newTokenCache(endpoint.minter(t), fakeAppResolver{}, secrets)

	_, access, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
	if err != nil {
		t.Fatalf("AccessTokenForUser = %v, want the retry with the rotated token to succeed", err)
	}
	if access != "acc-2" {
		t.Errorf("access token = %q, want the retry's acc-2", access)
	}
	if got := endpoint.tokens(); len(got) != 2 || got[1] != "ref-other" {
		t.Errorf("token requests = %v, want ref-0 then the rotated ref-other", got)
	}
	if got := storedRefresh(t, secrets); got != "ref-2" {
		t.Errorf("stored refresh token = %q, want the retry's rotation ref-2", got)
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
			endpoint := &fakeTokenEndpoint{answer: func(string) (int, string) { return tc.status, tc.body }}
			cache := newTokenCache(endpoint.minter(t), fakeAppResolver{}, secrets)

			_, _, err := cache.AccessTokenForUser(context.Background(), cOrg, cUser, cHost)
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
