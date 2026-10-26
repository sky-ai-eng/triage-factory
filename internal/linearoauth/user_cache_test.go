package linearoauth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

const (
	uUser      = "user-1"
	uWorkspace = "ws-1"
	uGrant     = "grant-1"
)

// fakeUserSecrets is an in-memory per-user secret bag over the system doors
// the user cache uses. Embeds the interface so the rest panic if reached.
type fakeUserSecrets struct {
	db.SecretStore
	mu  sync.Mutex
	bag map[string]string
	// putErr fails PutUserSystemIfValue while set.
	putErr error
}

func newFakeUserSecrets() *fakeUserSecrets { return &fakeUserSecrets{bag: map[string]string{}} }

func userBagKey(orgID, userID, key string) string { return orgID + "|" + userID + "|" + key }

func (f *fakeUserSecrets) GetUserSystem(ctx context.Context, orgID, userID, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bag[userBagKey(orgID, userID, key)], nil
}

func (f *fakeUserSecrets) PutUserSystemIfValue(ctx context.Context, orgID, userID, key, old, value, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return false, f.putErr
	}
	k := userBagKey(orgID, userID, key)
	if cur, ok := f.bag[k]; !ok || cur != old {
		return false, nil
	}
	f.bag[k] = value
	return true, nil
}

func (f *fakeUserSecrets) GetUser(ctx context.Context, orgID, userID, key string) (string, error) {
	return f.GetUserSystem(ctx, orgID, userID, key)
}

func (f *fakeUserSecrets) DeleteUser(ctx context.Context, orgID, userID, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := userBagKey(orgID, userID, key)
	_, ok := f.bag[k]
	delete(f.bag, k)
	return ok, nil
}

func (f *fakeUserSecrets) set(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if value == "" {
		delete(f.bag, userBagKey(cOrg, uUser, linear.UserTokenKey(uWorkspace)))
		return
	}
	f.bag[userBagKey(cOrg, uUser, linear.UserTokenKey(uWorkspace))] = value
}

func (f *fakeUserSecrets) stored(t *testing.T) (linear.UserCredential, bool) {
	t.Helper()
	f.mu.Lock()
	raw := f.bag[userBagKey(cOrg, uUser, linear.UserTokenKey(uWorkspace))]
	f.mu.Unlock()
	if raw == "" {
		return linear.UserCredential{}, false
	}
	cred, err := linear.ParseUserCredential(raw)
	if err != nil {
		t.Fatalf("parse stored: %v", err)
	}
	return cred, true
}

// storedMarker reports whether what is stored is the refused marker.
func (f *fakeUserSecrets) storedMarker(t *testing.T) bool {
	t.Helper()
	cred, ok := f.stored(t)
	return ok && cred == linear.RefusedUserCredential()
}

func refusedMarker(t *testing.T) string {
	t.Helper()
	env, err := linear.MarshalUserCredential(linear.RefusedUserCredential())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return env
}

// fakeIdentities records ClearLinearIdentity calls, failing them while err
// is set.
type fakeIdentities struct {
	db.UsersStore
	mu      sync.Mutex
	cleared []string
	err     error
}

func (f *fakeIdentities) ClearLinearIdentity(_ context.Context, userID, workspaceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.cleared = append(f.cleared, userID+":"+workspaceID)
	return nil
}

// fakeTx runs a synthetic-claims transaction over the fake stores, recording
// whom it ran as. It does not roll back, so a body's write order is what a
// failure leaves behind. A secret write in the body lands at once, as the
// local keychain's does; commitErr, while set, fails the transaction after
// the body succeeds, the way a SQLite commit can.
type fakeTx struct {
	db.TxRunner
	secrets   *fakeUserSecrets
	ids       *fakeIdentities
	as        []string
	commitErr error
}

func (f *fakeTx) SyntheticClaimsWithTx(_ context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	f.as = append(f.as, orgID+":"+userID)
	if err := fn(db.TxStores{Secrets: f.secrets, Users: f.ids}); err != nil {
		return err
	}
	return f.commitErr
}

func connectEnvelope(t *testing.T, refreshToken, grantID, clientID string) string {
	t.Helper()
	env, err := linear.MarshalUserCredential(linear.UserCredential{
		Method: linear.AuthMethodConnectOAuth, RefreshToken: refreshToken, ClientID: clientID, GrantID: grantID,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return env
}

type userRig struct {
	secrets *fakeUserSecrets
	ids     *fakeIdentities
	tx      *fakeTx
	ref     *graceRefresher
	clock   time.Time
	apps    linear.OAuthAppResolver
}

func newUserRig(t *testing.T) *userRig {
	t.Helper()
	r := &userRig{secrets: newFakeUserSecrets(), ids: &fakeIdentities{}, clock: installedAt, apps: fakeAppResolver{}}
	r.tx = &fakeTx{secrets: r.secrets, ids: r.ids}
	r.ref = &graceRefresher{expires: 24 * time.Hour, now: func() time.Time { return r.clock }}
	r.secrets.set(connectEnvelope(t, "ref-0", uGrant, "client-1"))
	return r
}

func (r *userRig) cache() *UserTokenCache {
	c := newUserTokenCache(r.ref, r.apps, r.secrets, r.tx, NewUserCredentialLock(nil))
	c.now = func() time.Time { return r.clock }
	return c
}

func (r *userRig) token(t *testing.T, c *UserTokenCache) (string, error) {
	t.Helper()
	return c.AccessTokenForUser(context.Background(), cOrg, uUser, uWorkspace)
}

func TestUserTokenCache_RefreshesOnceAndWritesBack(t *testing.T) {
	r := newUserRig(t)
	c := r.cache()

	for i := 0; i < 3; i++ {
		tok, err := r.token(t, c)
		if err != nil {
			t.Fatalf("AccessTokenForUser: %v", err)
		}
		if tok != "acc-1" {
			t.Fatalf("token = %q, want acc-1", tok)
		}
	}
	if n := r.ref.calls.Load(); n != 1 {
		t.Errorf("refreshes = %d, want 1 (served from the cache after)", n)
	}
	cred, ok := r.secrets.stored(t)
	if !ok {
		t.Fatal("envelope gone after a refresh")
	}
	want := linear.UserCredential{Method: linear.AuthMethodConnectOAuth, RefreshToken: "ref-1", ClientID: "client-1", GrantID: uGrant}
	if cred != want {
		t.Errorf("stored = %+v, want %+v (rotated token written back, grant kept, no access token)", cred, want)
	}
}

func TestUserTokenCache_RefreshesNearExpiry(t *testing.T) {
	r := newUserRig(t)
	c := r.cache()
	if _, err := r.token(t, c); err != nil {
		t.Fatalf("first: %v", err)
	}
	r.clock = r.clock.Add(24*time.Hour - 30*time.Second)
	tok, err := r.token(t, c)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if tok != "acc-2" || r.ref.calls.Load() != 2 {
		t.Errorf("token = %q after %d refreshes, want acc-2 after 2", tok, r.ref.calls.Load())
	}
}

func TestUserTokenCache_AnotherProcessRotationKeepsTheCachedToken(t *testing.T) {
	r := newUserRig(t)
	c := r.cache()
	if _, err := r.token(t, c); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Another process rotated the same grant.
	r.secrets.set(connectEnvelope(t, "ref-7", uGrant, "client-1"))
	tok, err := r.token(t, c)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if tok != "acc-1" || r.ref.calls.Load() != 1 {
		t.Errorf("token = %q after %d refreshes; another process's rotation must not cost this one a refresh", tok, r.ref.calls.Load())
	}
}

func TestUserTokenCache_NewGrantIsNotServedTheOldToken(t *testing.T) {
	r := newUserRig(t)
	c := r.cache()
	if _, err := r.token(t, c); err != nil {
		t.Fatalf("first: %v", err)
	}
	// The user connected again.
	r.secrets.set(connectEnvelope(t, "ref-40", "grant-2", "client-1"))
	tok, err := r.token(t, c)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if tok != "acc-41" {
		t.Errorf("token = %q, want acc-41 minted from the new grant", tok)
	}
}

func TestUserTokenCache_NoConnectCredential(t *testing.T) {
	apiKey, err := linear.MarshalUserCredential(linear.UserCredential{Method: linear.AuthMethodAPIKey, Token: "lin_api_user"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"nothing stored":   "",
		"a pasted key":     apiKey,
		"no grant id":      connectEnvelope(t, "ref-0", "", "client-1"),
		"no refresh token": connectEnvelope(t, "", uGrant, "client-1"),
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			r := newUserRig(t)
			r.secrets.set(env)
			_, err := r.token(t, r.cache())
			if !errors.Is(err, linear.ErrNoLinearUserCredential) {
				t.Errorf("err = %v, want ErrNoLinearUserCredential", err)
			}
			if errors.Is(err, linear.ErrLinearUserCredentialRefused) || errors.Is(err, linear.ErrLinearUserCredentialAppRemoved) {
				t.Errorf("err = %v, names a cause this credential does not have", err)
			}
			if r.ref.calls.Load() != 0 {
				t.Error("refreshed without a usable credential")
			}
		})
	}
}

func TestUserTokenCache_RefusedGrantLeavesAMarkerAndClearsTheIdentity(t *testing.T) {
	r := newUserRig(t)
	r.ref.refuse = map[string]bool{"ref-0": true}

	_, err := r.token(t, r.cache())
	if !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if !RefusedGrant(err) {
		t.Errorf("err = %v; the refusal should still be readable", err)
	}
	if cred, ok := r.secrets.stored(t); !ok || cred != linear.RefusedUserCredential() {
		t.Errorf("stored = %+v (%v), want the refused marker in place of the credential", cred, ok)
	}
	if got := r.ids.cleared; len(got) != 1 || got[0] != uUser+":"+uWorkspace {
		t.Errorf("identities cleared = %v, want [%s:%s]", got, uUser, uWorkspace)
	}
}

// TestUserTokenCache_StoredMarkerIsRefusedWithoutARefresh: a refused marker
// answers with the refusal and asks Linear nothing.
func TestUserTokenCache_StoredMarkerIsRefusedWithoutARefresh(t *testing.T) {
	r := newUserRig(t)
	r.secrets.set(refusedMarker(t))

	if _, err := r.token(t, r.cache()); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if n := r.ref.calls.Load(); n != 0 {
		t.Errorf("refreshes = %d, want none for a marker", n)
	}
	if !r.secrets.storedMarker(t) {
		t.Error("the marker was not left as it was")
	}
	if len(r.ids.cleared) != 0 {
		t.Errorf("identities cleared = %v, want none: the refusal that wrote the marker already did", r.ids.cleared)
	}
}

// TestUserTokenCache_RefusalRemovalIsAllOrNothing: a refused credential whose
// identity cannot be cleared keeps its envelope too, so the two never
// disagree, and the next read clears the identity and writes the marker.
func TestUserTokenCache_RefusalRemovalIsAllOrNothing(t *testing.T) {
	r := newUserRig(t)
	r.ref.refuse = map[string]bool{"ref-0": true}
	r.ids.err = errors.New("identities down")
	c := r.cache()

	if _, err := r.token(t, c); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if cred, ok := r.secrets.stored(t); !ok || cred.RefreshToken != "ref-0" {
		t.Fatalf("stored = %+v (%v); the envelope was replaced while its identity stayed", cred, ok)
	}

	r.ids.mu.Lock()
	r.ids.err = nil
	r.ids.mu.Unlock()
	if _, err := r.token(t, c); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("second err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if !r.secrets.storedMarker(t) {
		t.Error("the envelope is not the refused marker after the second refusal")
	}
	if len(r.ids.cleared) != 1 {
		t.Errorf("identities cleared = %v, want one", r.ids.cleared)
	}
}

func TestUserTokenCache_RefusalOfARotatedTokenRetriesTheStoredOne(t *testing.T) {
	r := newUserRig(t)
	r.ref.refuse = map[string]bool{"ref-0": true}
	// Another process rotated the grant while this refresh was in flight, so
	// Linear refuses the token this one read.
	r.ref.onRefresh = func(tok string) {
		if tok == "ref-0" {
			r.secrets.set(connectEnvelope(t, "ref-9", uGrant, "client-1"))
		}
	}

	tok, err := r.token(t, r.cache())
	if err != nil {
		t.Fatalf("AccessTokenForUser: %v", err)
	}
	if tok != "acc-10" {
		t.Errorf("token = %q, want acc-10 from the stored token", tok)
	}
	if cred, ok := r.secrets.stored(t); !ok || cred.RefreshToken != "ref-10" {
		t.Errorf("stored = %+v (%v), want ref-10", cred, ok)
	}
	if len(r.ids.cleared) != 0 {
		t.Errorf("identity cleared %v for a credential that is fine", r.ids.cleared)
	}
}

// A refresh that loses to another writer on its retry too has nothing more to
// try; the caller gets the answer connecting again fixes, and the credential
// the other writer stored is left alone.
func TestUserTokenCache_LosingTheRetryIsNoCredential(t *testing.T) {
	cases := []struct {
		name   string
		refuse map[string]bool
		// next is the envelope another writer stores during each refresh,
		// keyed by the refresh token it is presented.
		next map[string]string
		want string
	}{
		{
			name:   "rotated by another process twice",
			refuse: map[string]bool{"ref-0": true, "ref-9": true},
			next: map[string]string{
				"ref-0": connectEnvelope(t, "ref-9", uGrant, "client-1"),
				"ref-9": connectEnvelope(t, "ref-19", uGrant, "client-1"),
			},
			want: "ref-19",
		},
		{
			name: "connected again during both refreshes",
			next: map[string]string{
				"ref-0": connectEnvelope(t, "ref-5", "grant-2", "client-1"),
				"ref-5": connectEnvelope(t, "ref-7", "grant-3", "client-1"),
			},
			want: "ref-7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newUserRig(t)
			r.ref.refuse = tc.refuse
			r.ref.onRefresh = func(tok string) {
				if env, ok := tc.next[tok]; ok {
					r.secrets.set(env)
				}
			}

			if _, err := r.token(t, r.cache()); !errors.Is(err, linear.ErrNoLinearUserCredential) {
				t.Fatalf("err = %v, want ErrNoLinearUserCredential", err)
			}
			if n := r.ref.calls.Load(); n != 2 {
				t.Errorf("refreshes = %d, want 2", n)
			}
			if cred, ok := r.secrets.stored(t); !ok || cred.RefreshToken != tc.want {
				t.Errorf("stored = %+v (%v), want the other writer's %s", cred, ok, tc.want)
			}
			if len(r.ids.cleared) != 0 {
				t.Errorf("identity cleared %v for a credential another writer holds", r.ids.cleared)
			}
		})
	}
}

func TestUserTokenCache_DisconnectDuringRefreshIsNotResurrected(t *testing.T) {
	r := newUserRig(t)
	r.ref.onRefresh = func(string) { r.secrets.set("") }

	if _, err := r.token(t, r.cache()); !errors.Is(err, linear.ErrNoLinearUserCredential) {
		t.Fatalf("err = %v, want ErrNoLinearUserCredential", err)
	}
	if _, ok := r.secrets.stored(t); ok {
		t.Error("the write-back brought back a credential the user disconnected")
	}
}

func TestUserTokenCache_AppChangedIsAppRemoved(t *testing.T) {
	r := newUserRig(t)
	r.apps = fakeAppResolver{clientID: "client-2"}

	if _, err := r.token(t, r.cache()); !errors.Is(err, linear.ErrLinearUserCredentialAppRemoved) {
		t.Fatalf("err = %v, want ErrLinearUserCredentialAppRemoved", err)
	}
	if r.ref.calls.Load() != 0 {
		t.Error("refreshed with an app that did not mint the token")
	}
	if _, ok := r.secrets.stored(t); !ok {
		t.Error("the credential was removed; it stays for the app to come back")
	}
}

type noApp struct{}

func (noApp) Resolve(context.Context, string) (linear.OAuthApp, linear.OAuthAppSource, error) {
	return linear.OAuthApp{}, linear.SourceNone, linear.ErrNoLinearOAuthApp
}

func (noApp) ResolveClient(context.Context, string, string) (linear.OAuthApp, error) {
	return linear.OAuthApp{}, linear.ErrNoLinearOAuthApp
}

// mintingAppStillKnown resolves a different app now, but still knows the one
// that minted the stored token, the way the deployment app stays known after
// an org registers its own.
type mintingAppStillKnown struct{}

func (mintingAppStillKnown) Resolve(context.Context, string) (linear.OAuthApp, linear.OAuthAppSource, error) {
	return linear.OAuthApp{ClientID: "client-2", ClientSecret: "s2"}, linear.SourceOrgOverride, nil
}

func (mintingAppStillKnown) ResolveClient(_ context.Context, _ string, clientID string) (linear.OAuthApp, error) {
	switch clientID {
	case "client-1", "client-2":
		return linear.OAuthApp{ClientID: clientID, ClientSecret: "s"}, nil
	}
	return linear.OAuthApp{}, linear.ErrNoLinearOAuthApp
}

func TestUserTokenCache_RefreshesWithTheMintingApp(t *testing.T) {
	r := newUserRig(t)
	r.apps = mintingAppStillKnown{}
	tok, err := r.token(t, r.cache())
	if err != nil || tok != "acc-1" {
		t.Fatalf("token = %q, %v; want acc-1 refreshed with the app that minted it", tok, err)
	}
	if cred, _ := r.secrets.stored(t); cred.ClientID != "client-1" {
		t.Errorf("stored client = %q after the refresh, want the minting app kept", cred.ClientID)
	}
}

func TestUserTokenCache_NoAppIsAppRemoved(t *testing.T) {
	r := newUserRig(t)
	r.apps = noApp{}
	if _, err := r.token(t, r.cache()); !errors.Is(err, linear.ErrLinearUserCredentialAppRemoved) {
		t.Fatalf("err = %v, want ErrLinearUserCredentialAppRemoved", err)
	}
	if cred, ok := r.secrets.stored(t); !ok || cred.RefreshToken != "ref-0" {
		t.Errorf("stored = %+v (%v), want the credential kept for the app to come back", cred, ok)
	}
}

func TestUserTokenCache_TransientFailureKeepsTheCredential(t *testing.T) {
	r := newUserRig(t)
	r.ref.down.Store(true)

	_, err := r.token(t, r.cache())
	if err == nil || errors.Is(err, linear.ErrNoLinearUserCredential) {
		t.Fatalf("err = %v, want an outage that is not \"no credential\"", err)
	}
	if cred, ok := r.secrets.stored(t); !ok || cred.RefreshToken != "ref-0" {
		t.Errorf("stored = %+v (%v), want ref-0 untouched", cred, ok)
	}
	if len(r.ids.cleared) != 0 {
		t.Error("an outage cleared the identity")
	}
}

func TestUserTokenCache_ConcurrentCallersShareOneRefresh(t *testing.T) {
	r := newUserRig(t)
	gate := make(chan struct{})
	r.ref.onRefresh = func(string) { <-gate }
	c := r.cache()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.token(t, c)
			errs <- err
		}()
	}
	// Give the callers time to join the flight before it answers.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AccessTokenForUser: %v", err)
		}
	}
	if n := r.ref.calls.Load(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

// TestUserTokenCache_FailedCommitKeepsTheEnvelope: the identity clear's
// transaction failing on commit, after its body ran, leaves the envelope. In
// local mode that commit rolls the identity clear back, so an envelope deleted
// inside the body would be gone with the identity still there, and with no
// envelope no later read would come back to clear it.
func TestUserTokenCache_FailedCommitKeepsTheEnvelope(t *testing.T) {
	r := newUserRig(t)
	r.ref.refuse = map[string]bool{"ref-0": true}
	r.tx.commitErr = errors.New("commit failed")
	c := r.cache()

	if _, err := r.token(t, c); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if cred, ok := r.secrets.stored(t); !ok || cred.RefreshToken != "ref-0" {
		t.Fatalf("stored = %+v (%v); the envelope was replaced although the identity clear did not commit", cred, ok)
	}

	r.tx.commitErr = nil
	if _, err := r.token(t, c); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("second err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if !r.secrets.storedMarker(t) {
		t.Error("the envelope is not the refused marker after the identity clear committed")
	}
}

// TestUserTokenCache_FailedSwapIsFinishedByTheNextRead: the marker swap
// failing after the identity clear committed leaves the original envelope,
// and the next read, refused again, writes the marker.
func TestUserTokenCache_FailedSwapIsFinishedByTheNextRead(t *testing.T) {
	r := newUserRig(t)
	r.ref.refuse = map[string]bool{"ref-0": true}
	r.secrets.putErr = errors.New("keychain locked")
	c := r.cache()

	if _, err := r.token(t, c); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if cred, ok := r.secrets.stored(t); !ok || cred.RefreshToken != "ref-0" {
		t.Fatalf("stored = %+v (%v), want the original envelope kept after its swap failed", cred, ok)
	}
	if len(r.ids.cleared) != 1 {
		t.Fatalf("identities cleared = %v, want one", r.ids.cleared)
	}

	r.secrets.mu.Lock()
	r.secrets.putErr = nil
	r.secrets.mu.Unlock()
	if _, err := r.token(t, c); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("second err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if n := r.ref.calls.Load(); n != 2 {
		t.Errorf("refreshes = %d, want the next read refused again", n)
	}
	if !r.secrets.storedMarker(t) {
		t.Error("the next read did not write the refused marker")
	}
}

// TestUserTokenCache_MarkerWrittenDuringARefreshStays: a refused marker that
// another process writes while this refresh is in flight stays: the rotated
// token's write-back does not overwrite it, and the caller is told the
// credential was refused.
func TestUserTokenCache_MarkerWrittenDuringARefreshStays(t *testing.T) {
	r := newUserRig(t)
	marker := refusedMarker(t)
	r.ref.onRefresh = func(string) { r.secrets.set(marker) }

	if _, err := r.token(t, r.cache()); !errors.Is(err, linear.ErrLinearUserCredentialRefused) {
		t.Fatalf("err = %v, want ErrLinearUserCredentialRefused", err)
	}
	if !r.secrets.storedMarker(t) {
		t.Error("the write-back replaced the refused marker")
	}
}
