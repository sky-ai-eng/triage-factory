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
// reached. Like a real store, each door fails on a cancelled ctx.
type fakeSecrets struct {
	db.SecretStore
	mu  sync.Mutex
	bag map[string]string
}

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{bag: map[string]string{}} }

func (f *fakeSecrets) GetSystem(ctx context.Context, orgID, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bag[orgID+"|"+key], nil
}

// set writes a value directly, the way a handler or another process would.
func (f *fakeSecrets) set(orgID, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bag[orgID+"|"+key] = value
}

func (f *fakeSecrets) PutSystemIfValue(ctx context.Context, orgID, key, old, value, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := orgID + "|" + key
	if cur, ok := f.bag[k]; !ok || cur != old {
		return false, nil
	}
	f.bag[k] = value
	return true, nil
}

func (f *fakeSecrets) DeleteSystemIfValue(ctx context.Context, orgID, key, value string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := orgID + "|" + key
	if cur, ok := f.bag[k]; !ok || cur != value {
		return false, nil
	}
	delete(f.bag, k)
	return true, nil
}

// fakeInstalls records MarkRemovedSystem calls, failing them while err is set.
type fakeInstalls struct {
	db.LinearInstallsStore
	mu      sync.Mutex
	removed []string
	err     error
}

func (f *fakeInstalls) MarkRemovedSystem(_ context.Context, orgID, installID, reason string) (*domain.OrgLinearInstall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.removed = append(f.removed, orgID+":"+installID+":"+reason)
	return &domain.OrgLinearInstall{OrgID: orgID, InstallID: installID, RemovedReason: reason}, nil
}

func (f *fakeInstalls) Removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
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

const firstInstall = "inst-1"

func seedInstall(t *testing.T, secrets *fakeSecrets, refreshToken, installID string) {
	t.Helper()
	env, err := linear.MarshalInstallCredential(linear.InstallCredential{
		InstallID: installID, WorkspaceID: "ws-1", AppUserID: "app-user-1", RefreshToken: refreshToken, ClientID: "client-1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	secrets.set(cOrg, integrations.KeyLinearAppInstall, env)
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
	seedInstall(t, r.secrets, "ref-0", firstInstall)
	return r
}

func (r *rig) now() time.Time { return r.clock }

func (r *rig) cache() *TokenCache {
	c := newTokenCache(r.ref, fakeAppResolver{}, r.secrets, r.installs, NewCredentialLock(nil))
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
	if stored.WorkspaceID != "ws-1" || stored.AppUserID != "app-user-1" || stored.ClientID != "client-1" || stored.InstallID != firstInstall {
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
// install_id, written by an install in another process, is refreshed on the
// next read.
func TestTokenCache_NewInstallReplacesTheCachedToken(t *testing.T) {
	r := newRig(t)
	cache := r.cache()
	if _, _, err := cache.AccessTokenForOrg(context.Background(), cOrg); err != nil {
		t.Fatalf("first: %v", err)
	}

	seedInstall(t, r.secrets, "ref-100", "inst-2")
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
	r.secrets.mu.Lock()
	r.secrets.bag = map[string]string{}
	r.secrets.mu.Unlock()
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
	if got, want := r.installs.Removed(), cOrg+":"+firstInstall+":"+domain.LinearInstallRemovedRevoked; len(got) != 1 || got[0] != want {
		t.Errorf("installs removed = %v, want [%s]", got, want)
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
			seedInstall(t, r.secrets, "ref-7", firstInstall)
		}
	}

	access, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
	if err != nil {
		t.Fatalf("AccessTokenForOrg: %v", err)
	}
	if access != "acc-8" {
		t.Errorf("access = %q, want acc-8 from the token stored meanwhile", access)
	}
	if got := r.installs.Removed(); len(got) != 0 {
		t.Errorf("install marked removed over a token that was not the stored one: %v", got)
	}
}

// TestTokenCache_DisconnectDuringRefreshIsNotResurrected pins the write-back
// guard: an envelope deleted while its refresh was in flight stays deleted,
// and a new install written meanwhile is not overwritten by the old one's
// rotation.
func TestTokenCache_DisconnectDuringRefreshIsNotResurrected(t *testing.T) {
	t.Run("disconnect", func(t *testing.T) {
		r := newRig(t)
		r.ref.onRefresh = func(string) {
			r.secrets.mu.Lock()
			r.secrets.bag = map[string]string{}
			r.secrets.mu.Unlock()
		}

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
		r.ref.onRefresh = func(tok string) {
			if tok == "ref-0" {
				seedInstall(t, r.secrets, "ref-50", "inst-2")
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
		if stored.RefreshToken != "ref-51" || stored.InstallID != "inst-2" {
			t.Errorf("stored = %+v, want the new install rotated to ref-51", stored)
		}
	})
}

func TestTokenCache_AppMismatchDoesNotRefresh(t *testing.T) {
	r := newRig(t)
	c := newTokenCache(r.ref, fakeAppResolver{clientID: "client-2"}, r.secrets, r.installs, NewCredentialLock(nil))
	c.now = r.now

	_, _, err := c.AccessTokenForOrg(context.Background(), cOrg)
	if err == nil || errors.Is(err, linear.ErrNoLinearSystemCredential) {
		t.Errorf("err = %v, want a mismatch error that does not read as unconfigured", err)
	}
	if r.ref.calls.Load() != 0 {
		t.Error("refreshed a token minted by another app with this app's secret")
	}
}

// TestTokenCache_FailedRemovalKeepsTheEnvelope pins the order of the revoked
// path: the row is marked removed before the envelope goes, so a failed mark
// leaves the envelope for the next read to refuse and remove again, rather
// than a live row holding the workspace with nothing left to retry it.
func TestTokenCache_FailedRemovalKeepsTheEnvelope(t *testing.T) {
	r := newRig(t)
	r.ref.refuse = map[string]bool{"ref-0": true}
	r.installs.err = errors.New("db down")

	_, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
	if err == nil || errors.Is(err, linear.ErrNoLinearSystemCredential) {
		t.Fatalf("err = %v, want the failed removal, not unconfigured", err)
	}
	if _, ok := storedInstall(t, r.secrets); !ok {
		t.Fatal("the envelope was deleted although the install row is still live")
	}

	r.installs.mu.Lock()
	r.installs.err = nil
	r.installs.mu.Unlock()
	if _, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg); !errors.Is(err, linear.ErrNoLinearSystemCredential) {
		t.Fatalf("retry err = %v, want the install removed", err)
	}
	if got := r.installs.Removed(); len(got) != 1 {
		t.Errorf("installs removed = %v, want the one install", got)
	}
	if _, ok := storedInstall(t, r.secrets); ok {
		t.Error("the envelope survived the retried removal")
	}
}

// TestTokenCache_WriteBackNeverOverwritesANewerRotation pins the
// compare-and-swap: when another process stores a newer rotation of the same
// install while this refresh is in flight, the newer token stays, and this
// refresh's access token is still served.
func TestTokenCache_WriteBackNeverOverwritesANewerRotation(t *testing.T) {
	r := newRig(t)
	r.ref.onRefresh = func(tok string) {
		if tok == "ref-0" {
			seedInstall(t, r.secrets, "ref-9", firstInstall)
		}
	}

	access, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
	if err != nil {
		t.Fatalf("AccessTokenForOrg: %v", err)
	}
	if access != "acc-1" {
		t.Errorf("access = %q, want this refresh's acc-1", access)
	}
	if stored, _ := storedInstall(t, r.secrets); stored.RefreshToken != "ref-9" {
		t.Errorf("stored refresh token = %q, want the newer ref-9 kept", stored.RefreshToken)
	}
}

// TestTokenCache_WritesWaitOnTheCredentialLock pins that the cache's writes
// hold the org's credential lock, the one the credential handlers hold, so
// neither lands inside a handler's check-and-write or between its write and
// local mode's restore. The refresh itself, a request to Linear, does not
// wait.
func TestTokenCache_WritesWaitOnTheCredentialLock(t *testing.T) {
	held := func(t *testing.T, r *rig, during func()) {
		t.Helper()
		release, err := NewCredentialLock(nil).Lock(context.Background(), cOrg)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		t.Cleanup(release)
		done := make(chan error, 1)
		go func() {
			_, _, err := r.cache().AccessTokenForOrg(context.Background(), cOrg)
			done <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for r.ref.calls.Load() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the refresh waited on the lock")
			}
			time.Sleep(time.Millisecond)
		}
		select {
		case err := <-done:
			t.Fatalf("the read completed (err=%v) while the lock was held", err)
		case <-time.After(100 * time.Millisecond):
		}
		during()
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the read did not complete after the lock was released")
		}
	}

	t.Run("write-back", func(t *testing.T) {
		r := newRig(t)
		held(t, r, func() {
			if stored, _ := storedInstall(t, r.secrets); stored.RefreshToken != "ref-0" {
				t.Errorf("stored = %q while the lock was held, want ref-0", stored.RefreshToken)
			}
		})
		if stored, _ := storedInstall(t, r.secrets); stored.RefreshToken != "ref-1" {
			t.Errorf("stored = %q after the release, want ref-1", stored.RefreshToken)
		}
	})
	t.Run("revoke", func(t *testing.T) {
		r := newRig(t)
		r.ref.refuse = map[string]bool{"ref-0": true}
		held(t, r, func() {
			if got := r.installs.Removed(); len(got) != 0 {
				t.Errorf("install marked removed while the lock was held: %v", got)
			}
			if _, ok := storedInstall(t, r.secrets); !ok {
				t.Error("envelope deleted while the lock was held")
			}
		})
		if got := r.installs.Removed(); len(got) != 1 {
			t.Errorf("installs removed = %v after the release, want the refused install", got)
		}
		if _, ok := storedInstall(t, r.secrets); ok {
			t.Error("the refused envelope is still stored after the release")
		}
	})
}

// TestTokenCache_CancelledCallerKeepsTheRotation pins that a refresh outlives
// the caller that started it. Linear has rotated the token by the time the
// caller gives up; the write-back still lands, so the stored refresh token is
// the live one rather than one that ages out of Linear's grace window and
// reads as a revoked install.
func TestTokenCache_CancelledCallerKeepsTheRotation(t *testing.T) {
	r := newRig(t)
	cache := r.cache()
	ctx, cancel := context.WithCancel(context.Background())
	r.ref.onRefresh = func(string) { cancel() }

	if _, _, err := cache.AccessTokenForOrg(ctx, cOrg); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if stored, _ := storedInstall(t, r.secrets); stored.RefreshToken == "ref-1" {
			break
		}
		if time.Now().After(deadline) {
			stored, _ := storedInstall(t, r.secrets)
			t.Fatalf("stored = %q, want the rotation ref-1 written back after the caller gave up", stored.RefreshToken)
		}
		time.Sleep(time.Millisecond)
	}

	r.ref.onRefresh = nil
	access, _, err := cache.AccessTokenForOrg(context.Background(), cOrg)
	if err != nil {
		t.Fatalf("next read: %v", err)
	}
	if access != "acc-1" || r.ref.calls.Load() != 1 {
		t.Errorf("next read = %q after %d refreshes, want acc-1 from the cache after 1", access, r.ref.calls.Load())
	}
}
