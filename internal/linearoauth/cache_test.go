package linearoauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

const cOrg = "org-1"

// fakeSecrets is an in-memory org secret bag over the three system doors the
// cache uses. Embeds the interface so the rest compile-satisfy and panic if
// reached.
type fakeSecrets struct {
	db.SecretStore
	mu   sync.Mutex
	bag  map[string]string
	puts int
}

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{bag: map[string]string{}} }

func (f *fakeSecrets) GetSystem(_ context.Context, orgID, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bag[orgID+"|"+key], nil
}

func (f *fakeSecrets) PutSystem(_ context.Context, orgID, key, value, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bag[orgID+"|"+key] = value
	f.puts++
	return nil
}

func (f *fakeSecrets) DeleteSystemIfValue(_ context.Context, orgID, key, value string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := orgID + "|" + key
	if cur, ok := f.bag[k]; !ok || cur != value {
		return false, nil
	}
	delete(f.bag, k)
	return true, nil
}

// fakeInstalls records MarkRemovedSystem calls.
type fakeInstalls struct {
	db.LinearInstallsStore
	mu      sync.Mutex
	removed []string
}

func (f *fakeInstalls) MarkRemovedSystem(_ context.Context, orgID, reason string) (*domain.OrgLinearInstall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, orgID+":"+reason)
	return &domain.OrgLinearInstall{OrgID: orgID, RemovedReason: reason}, nil
}

type fakeAppResolver struct{ clientID string }

func (f fakeAppResolver) Resolve(context.Context, string) (linear.OAuthApp, linear.OAuthAppSource, error) {
	id := f.clientID
	if id == "" {
		id = "client-1"
	}
	return linear.OAuthApp{ClientID: id, ClientSecret: "secret"}, linear.SourceOrgOverride, nil
}

// graceRefresher behaves the way Linear does inside its grace window: a given
// refresh token always rotates to the same pair, ref-N → (acc-N+1, ref-N+1),
// whoever presents it. onRefresh, when set, runs before the answer.
type graceRefresher struct {
	mu        sync.Mutex
	seen      []string
	calls     atomic.Int32
	expires   time.Duration
	now       func() time.Time
	refuse    map[string]bool
	onRefresh func(refreshToken string)
}

func (g *graceRefresher) Refresh(_ context.Context, _ linear.OAuthApp, refreshToken string) (Token, error) {
	g.calls.Add(1)
	g.mu.Lock()
	g.seen = append(g.seen, refreshToken)
	refused := g.refuse[refreshToken]
	hook := g.onRefresh
	g.mu.Unlock()
	if hook != nil {
		hook(refreshToken)
	}
	if refused {
		return Token{}, &StatusError{Op: opToken, StatusCode: http.StatusBadRequest, Code: "invalid_grant", Class: upstream.Rejected}
	}
	n, err := strconv.Atoi(strings.TrimPrefix(refreshToken, "ref-"))
	if err != nil {
		return Token{}, fmt.Errorf("unexpected refresh token %q", refreshToken)
	}
	return Token{
		AccessToken:  fmt.Sprintf("acc-%d", n+1),
		RefreshToken: fmt.Sprintf("ref-%d", n+1),
		ExpiresAt:    g.now().Add(g.expires),
	}, nil
}

var installedAt = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func seedInstall(t *testing.T, secrets *fakeSecrets, refreshToken string, at time.Time) {
	t.Helper()
	env, err := linear.MarshalInstallCredential(linear.InstallCredential{
		WorkspaceID: "ws-1", AppUserID: "app-user-1", RefreshToken: refreshToken, ClientID: "client-1", InstalledAt: at,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := secrets.PutSystem(context.Background(), cOrg, integrations.KeyLinearAppInstall, env, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func storedInstall(t *testing.T, secrets *fakeSecrets) (linear.InstallCredential, bool) {
	t.Helper()
	raw, _ := secrets.GetSystem(context.Background(), cOrg, integrations.KeyLinearAppInstall)
	if raw == "" {
		return linear.InstallCredential{}, false
	}
	cred, err := linear.ParseInstallCredential(raw)
	if err != nil {
		t.Fatalf("parse stored: %v", err)
	}
	return cred, true
}

type rig struct {
	secrets  *fakeSecrets
	installs *fakeInstalls
	ref      *graceRefresher
	clock    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{secrets: newFakeSecrets(), installs: &fakeInstalls{}, clock: installedAt}
	r.ref = &graceRefresher{expires: 24 * time.Hour, now: r.now}
	seedInstall(t, r.secrets, "ref-0", installedAt)
	r.secrets.puts = 0
	return r
}

func (r *rig) now() time.Time { return r.clock }

func (r *rig) cache() *TokenCache {
	c := newTokenCache(r.ref, fakeAppResolver{}, r.secrets, r.installs)
	c.now = r.now
	return c
}

func TestTokenCache_RotationWriteBack(t *testing.T) {
	r := newRig(t)
	cache := r.cache()

	access, exp, err := cache.AccessTokenForOrg(context.Background(), cOrg)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if access != "acc-1" || !exp.Equal(r.clock.Add(24*time.Hour)) {
		t.Errorf("first mint = (%q, %v), want acc-1 expiring in 24h", access, exp)
	}
	stored, _ := storedInstall(t, r.secrets)
	if stored.RefreshToken != "ref-1" {
		t.Fatalf("stored refresh token = %q, want the rotated ref-1", stored.RefreshToken)
	}
	if stored.WorkspaceID != "ws-1" || stored.AppUserID != "app-user-1" || stored.ClientID != "client-1" || !stored.InstalledAt.Equal(installedAt) {
		t.Errorf("rotation rewrote the install's identity: %+v", stored)
	}

	r.clock = r.clock.Add(24 * time.Hour)
	access, _, err = cache.AccessTokenForOrg(context.Background(), cOrg)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if access != "acc-2" {
		t.Errorf("second mint = %q, want acc-2", access)
	}
	if got := r.ref.seen; len(got) != 2 || got[0] != "ref-0" || got[1] != "ref-1" {
		t.Errorf("refresh tokens presented = %v, want [ref-0 ref-1]", got)
	}
}

func TestTokenCache_CachesWithinExpiry(t *testing.T) {
	r := newRig(t)
	cache := r.cache()

	if _, _, err := cache.AccessTokenForOrg(context.Background(), cOrg); err != nil {
		t.Fatalf("first: %v", err)
	}
	r.clock = r.clock.Add(23 * time.Hour)
	access, _, err := cache.AccessTokenForOrg(context.Background(), cOrg)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if access != "acc-1" || r.ref.calls.Load() != 1 {
		t.Errorf("second read = %q after %d refreshes, want the cached acc-1 and one refresh", access, r.ref.calls.Load())
	}

	// Inside the skew the token counts as expired.
	r.clock = installedAt.Add(24*time.Hour - 30*time.Second)
	if access, _, _ := cache.AccessTokenForOrg(context.Background(), cOrg); access != "acc-2" {
		t.Errorf("read inside the refresh skew = %q, want a fresh acc-2", access)
	}
}

func TestTokenCache_ConcurrentReadsRefreshOnce(t *testing.T) {
	r := newRig(t)
	cache := r.cache()

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if access, _, err := cache.AccessTokenForOrg(context.Background(), cOrg); err != nil || access != "acc-1" {
				t.Errorf("concurrent read = (%q, %v), want acc-1", access, err)
			}
		}()
	}
	wg.Wait()
	if n := r.ref.calls.Load(); n != 1 {
		t.Errorf("refreshes = %d, want 1 (the flight coalesces)", n)
	}
}

// TestTokenCache_ConcurrentCachesConverge is the multi-pod case: two caches
// over one envelope both read ref-0 before either writes back. Linear answers
// the replayed token with the identical pair, so both hold the same access
// token and the stored envelope holds the one rotated token, whichever wrote
// last.
func TestTokenCache_ConcurrentCachesConverge(t *testing.T) {
	r := newRig(t)
	podA, podB := r.cache(), r.cache()

	var arrived sync.WaitGroup
	arrived.Add(2)
	r.ref.onRefresh = func(string) {
		arrived.Done()
		arrived.Wait() // both have read ref-0 and are mid-request
	}

	var wg sync.WaitGroup
	got := make([]string, 2)
	for i, c := range []*TokenCache{podA, podB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			access, _, err := c.AccessTokenForOrg(context.Background(), cOrg)
			if err != nil {
				t.Errorf("pod %d: %v", i, err)
			}
			got[i] = access
		}()
	}
	wg.Wait()
	r.ref.onRefresh = nil

	if got[0] != "acc-1" || got[1] != "acc-1" {
		t.Errorf("pods hold %v, want both acc-1", got)
	}
	if seen := r.ref.seen; len(seen) != 2 || seen[0] != "ref-0" || seen[1] != "ref-0" {
		t.Errorf("refresh tokens presented = %v, want ref-0 twice", seen)
	}
	stored, _ := storedInstall(t, r.secrets)
	if stored.RefreshToken != "ref-1" {
		t.Errorf("stored refresh token = %q, want ref-1", stored.RefreshToken)
	}

	// A rotation by one pod does not make the other refresh again: its cached
	// token is for the same install and still fresh.
	r.clock = r.clock.Add(24 * time.Hour)
	if _, _, err := podA.AccessTokenForOrg(context.Background(), cOrg); err != nil {
		t.Fatalf("pod A second mint: %v", err)
	}
	r.clock = r.clock.Add(-23 * time.Hour)
	before := r.ref.calls.Load()
	if access, _, _ := podB.AccessTokenForOrg(context.Background(), cOrg); access != "acc-1" || r.ref.calls.Load() != before {
		t.Errorf("pod B after pod A rotated = %q with %d new refreshes, want its cached acc-1 and none", access, r.ref.calls.Load()-before)
	}
}

// TestTokenCache_NewInstallReplacesTheCachedToken pins that a cached token is
// served only for the install it was minted for: an envelope with a different
// installed_at, written by an install in another process, is refreshed on the
// next read.
func TestTokenCache_NewInstallReplacesTheCachedToken(t *testing.T) {
	r := newRig(t)
	cache := r.cache()
	if _, _, err := cache.AccessTokenForOrg(context.Background(), cOrg); err != nil {
		t.Fatalf("first: %v", err)
	}

	seedInstall(t, r.secrets, "ref-100", installedAt.Add(time.Hour))
	access, _, err := cache.AccessTokenForOrg(context.Background(), cOrg)
	if err != nil {
		t.Fatalf("after re-install: %v", err)
	}
	if access != "acc-101" {
		t.Errorf("access after a new install = %q, want acc-101 minted from the new install", access)
	}
}

func TestTokenCache_NoEnvelopeIsUnconfigured(t *testing.T) {
	r := newRig(t)
	r.secrets.bag = map[string]string{}
	_, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
	if !errors.Is(err, linear.ErrNoLinearSystemCredential) {
		t.Errorf("err = %v, want ErrNoLinearSystemCredential", err)
	}
	if r.ref.calls.Load() != 0 {
		t.Error("refreshed with no envelope")
	}
}

// TestTokenCache_InvalidGrantRemovesTheInstall is the revoked-install path:
// the envelope goes, the install row is marked install_revoked, and the org
// reads as having no Linear credential.
func TestTokenCache_InvalidGrantRemovesTheInstall(t *testing.T) {
	r := newRig(t)
	r.ref.refuse = map[string]bool{"ref-0": true}

	_, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
	if !errors.Is(err, linear.ErrNoLinearSystemCredential) || !RefusedGrant(err) {
		t.Errorf("err = %v, want ErrNoLinearSystemCredential carrying the refusal", err)
	}
	if _, ok := storedInstall(t, r.secrets); ok {
		t.Error("the refused envelope is still stored")
	}
	if got := r.installs.removed; len(got) != 1 || got[0] != cOrg+":"+domain.LinearInstallRemovedRevoked {
		t.Errorf("installs removed = %v, want [%s:%s]", got, cOrg, domain.LinearInstallRemovedRevoked)
	}
}

// TestTokenCache_RefusalAfterARotationRetries pins that an envelope replaced
// while its refresh was refused is not the dead one: it stays, and is tried.
func TestTokenCache_RefusalAfterARotationRetries(t *testing.T) {
	r := newRig(t)
	r.ref.refuse = map[string]bool{"ref-0": true}
	r.ref.onRefresh = func(tok string) {
		if tok == "ref-0" {
			// Another process rotated the token while this one was refused.
			seedInstall(t, r.secrets, "ref-7", installedAt)
		}
	}

	access, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
	if err != nil {
		t.Fatalf("AccessTokenForOrg: %v", err)
	}
	if access != "acc-8" {
		t.Errorf("access = %q, want acc-8 from the token stored meanwhile", access)
	}
	if len(r.installs.removed) != 0 {
		t.Errorf("install marked removed over a token that was not the stored one: %v", r.installs.removed)
	}
}

// TestTokenCache_DisconnectDuringRefreshIsNotResurrected pins the write-back
// guard: an envelope deleted while its refresh was in flight stays deleted,
// and a new install written meanwhile is not overwritten by the old one's
// rotation.
func TestTokenCache_DisconnectDuringRefreshIsNotResurrected(t *testing.T) {
	t.Run("disconnect", func(t *testing.T) {
		r := newRig(t)
		r.ref.onRefresh = func(string) { r.secrets.bag = map[string]string{} }

		_, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
		if !errors.Is(err, linear.ErrNoLinearSystemCredential) {
			t.Errorf("err = %v, want ErrNoLinearSystemCredential", err)
		}
		if _, ok := storedInstall(t, r.secrets); ok {
			t.Error("the refresh wrote a disconnected install's envelope back")
		}
	})
	t.Run("new install", func(t *testing.T) {
		r := newRig(t)
		newInstall := installedAt.Add(time.Hour)
		r.ref.onRefresh = func(tok string) {
			if tok == "ref-0" {
				seedInstall(t, r.secrets, "ref-50", newInstall)
			}
		}

		access, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
		if err != nil {
			t.Fatalf("AccessTokenForOrg: %v", err)
		}
		if access != "acc-51" {
			t.Errorf("access = %q, want acc-51 from the new install", access)
		}
		stored, _ := storedInstall(t, r.secrets)
		if stored.RefreshToken != "ref-51" || !stored.InstalledAt.Equal(newInstall) {
			t.Errorf("stored = %+v, want the new install rotated to ref-51", stored)
		}
	})
}

func TestTokenCache_AppMismatchDoesNotRefresh(t *testing.T) {
	r := newRig(t)
	c := newTokenCache(r.ref, fakeAppResolver{clientID: "client-2"}, r.secrets, r.installs)
	c.now = r.now

	_, _, err := c.AccessTokenForOrg(context.Background(), cOrg)
	if err == nil || errors.Is(err, linear.ErrNoLinearSystemCredential) {
		t.Errorf("err = %v, want a mismatch error that does not read as unconfigured", err)
	}
	if r.ref.calls.Load() != 0 {
		t.Error("refreshed a token minted by another app with this app's secret")
	}
}
