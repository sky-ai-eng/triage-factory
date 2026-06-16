package jiraoauth

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
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
