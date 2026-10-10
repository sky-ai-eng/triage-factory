package linearoauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// refreshSkew is how long before an access token's stated expiry the cache
// treats it as stale, so a token never runs out mid-request.
const refreshSkew = 60 * time.Second

// refreshFlightTimeout bounds one refresh, which no caller's ctx bounds: up
// to two token requests (each with the minter's own 30s limit), the waits on
// the org's credential lock, and the writes.
const refreshFlightTimeout = 2 * time.Minute

// refresher is the slice of *Minter the cache uses, an interface so the
// rotation write-back is testable without an HTTP endpoint.
type refresher interface {
	Refresh(ctx context.Context, app linear.OAuthApp, refreshToken string) (Token, error)
}

// errInstallReplaced is a refresh that finished after the envelope it read was
// replaced by a different install, or removed. Its token pair belongs to an
// install that is no longer the org's, so it is neither written back nor
// cached.
var errInstallReplaced = errors.New("linearoauth: install replaced during refresh")

// errEnvelopeMoved is a refresh token Linear refused that is no longer the one
// stored: another process rotated or replaced it meanwhile, so the refusal
// says nothing about the install as it stands now.
var errEnvelopeMoved = errors.New("linearoauth: refused refresh token is no longer the stored one")

// TokenCache mints and caches the access token of an org's app install, and
// writes each rotated refresh token back to the install's envelope. It
// implements linear.InstallTokenSource.
//
// Every read starts from the stored envelope, so a cached token is served only
// while the envelope still names the install it was minted for (its
// install_id). A new install or a disconnect anywhere in the deployment is
// therefore seen on the next read, with no invalidation message to deliver.
//
// A singleflight keyed by org coalesces one process's concurrent refreshes.
// Every write — the rotation write-back and a revoke — holds the org's
// CredentialLock, so it is one step against the credential handlers and
// against another process's write, and is a compare-and-swap against the
// envelope its refresh read: a rotation lands only over the token it
// rotated, so it can neither resurrect a disconnected install nor overwrite
// a newer one. The refresh itself, a request to Linear, runs outside the
// lock. Linear answers a refresh token replayed within its grace window with
// the identical new pair, so two processes refreshing the same token at once
// converge on one pair whichever of them stores it.
type TokenCache struct {
	minter   refresher
	apps     linear.OAuthAppResolver
	secrets  db.SecretStore
	installs db.LinearInstallsStore
	lock     *CredentialLock

	// now is injectable for tests; nil is time.Now.
	now func() time.Time

	group singleflight.Group

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	installID   string
	accessToken string
	expiresAt   time.Time
}

// NewTokenCache builds a TokenCache over a minter, the org's OAuth-app
// resolver (the client credentials a refresh authenticates with), the secret
// store (the envelope), the installs store (marking a revoked install
// removed) and the lock the org's other credential writers hold.
func NewTokenCache(minter *Minter, apps linear.OAuthAppResolver, secrets db.SecretStore, installs db.LinearInstallsStore, lock *CredentialLock) *TokenCache {
	return newTokenCache(minter, apps, secrets, installs, lock)
}

func newTokenCache(minter refresher, apps linear.OAuthAppResolver, secrets db.SecretStore, installs db.LinearInstallsStore, lock *CredentialLock) *TokenCache {
	return &TokenCache{
		minter:   minter,
		apps:     apps,
		secrets:  secrets,
		installs: installs,
		lock:     lock,
		cache:    make(map[string]cachedToken),
	}
}

func (c *TokenCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// AccessTokenForOrg returns a live access token for the org's app user,
// refreshing when the cache holds none for the current install or the one it
// holds is near expiry. An org with no install envelope, or whose install
// Linear has revoked, is linear.ErrNoLinearSystemCredential.
func (c *TokenCache) AccessTokenForOrg(ctx context.Context, orgID string) (string, time.Time, error) {
	cred, _, err := c.readEnvelope(ctx, orgID)
	if err != nil {
		return "", time.Time{}, err
	}
	if ct, ok := c.fresh(orgID, cred.InstallID); ok {
		return ct.accessToken, ct.expiresAt, nil
	}

	// The refresh runs detached from every caller's ctx. Linear may have
	// rotated the token by the time a caller gives up, and a write-back
	// abandoned there leaves the old token stored; once it falls out of
	// Linear's grace window, its refusal reads as a revoked install. A caller
	// that gives up stops waiting, and the refresh carries on for the rest.
	ch := c.group.DoChan(orgID, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshFlightTimeout)
		defer cancel()
		return c.refresh(fctx, orgID)
	})
	select {
	case <-ctx.Done():
		return "", time.Time{}, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return "", time.Time{}, res.Err
		}
		ct := res.Val.(cachedToken)
		return ct.accessToken, ct.expiresAt, nil
	}
}

// fresh is the cached token for orgID when it was minted for the install
// installID names and has headroom left.
func (c *TokenCache) fresh(orgID, installID string) (cachedToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct, ok := c.cache[orgID]
	if !ok || ct.installID != installID || !c.timeNow().Add(refreshSkew).Before(ct.expiresAt) {
		return cachedToken{}, false
	}
	return ct, true
}

// readEnvelope reads and parses the org's install envelope, returning the raw
// value too for a compare-and-delete. No envelope is ErrNoLinearSystemCredential.
func (c *TokenCache) readEnvelope(ctx context.Context, orgID string) (linear.InstallCredential, string, error) {
	raw, err := c.secrets.GetSystem(ctx, orgID, integrations.KeyLinearAppInstall)
	if err != nil {
		return linear.InstallCredential{}, "", fmt.Errorf("linearoauth: read install credential for org %s: %w", orgID, err)
	}
	if raw == "" {
		return linear.InstallCredential{}, "", fmt.Errorf("%w: org=%s (no install credential)", linear.ErrNoLinearSystemCredential, orgID)
	}
	cred, err := linear.ParseInstallCredential(raw)
	if err != nil {
		return linear.InstallCredential{}, raw, fmt.Errorf("linearoauth: org %s: %w", orgID, err)
	}
	return cred, raw, nil
}

// refresh mints a fresh access token (refreshOnce) and answers the ways a
// refresh can lose its footing.
//
// A refresh that outlives its install (a disconnect, or a new install,
// landing while the request was in flight) is retried once against whatever
// is stored now.
//
// A refresh token Linear refuses with invalid_grant means the app was removed
// from the workspace (revokeInstall), unless it is no longer the stored one:
// a process that held an old token past Linear's grace window is refused
// while the install is fine, so the stored token is tried once instead.
func (c *TokenCache) refresh(ctx context.Context, orgID string) (cachedToken, error) {
	// Re-checked under the flight: a sibling caller may have just refreshed.
	if cred, _, err := c.readEnvelope(ctx, orgID); err == nil {
		if ct, ok := c.fresh(orgID, cred.InstallID); ok {
			return ct, nil
		}
	}
	for attempt := 0; ; attempt++ {
		ct, cred, raw, err := c.refreshOnce(ctx, orgID)
		if RefusedGrant(err) {
			err = c.revokeInstall(ctx, orgID, cred, raw, err)
		}
		switch {
		case err == nil:
			return ct, nil
		case (errors.Is(err, errInstallReplaced) || errors.Is(err, errEnvelopeMoved)) && attempt == 0:
			continue
		default:
			return cachedToken{}, err
		}
	}
}

// revokeInstall answers Linear refusing the install's refresh token, refused.
// It holds the org's credential lock from the check that the stored envelope
// is still the refused one through both writes, so no rotation, new install
// or disconnect lands between them. The install is marked removed first and
// the envelope deleted after, so a failure between them leaves a removed row
// and a dead envelope, which the next read refuses and removes again; the
// other order could leave the envelope gone and the row live, holding the
// workspace with nothing left to retry.
func (c *TokenCache) revokeInstall(ctx context.Context, orgID string, cred linear.InstallCredential, raw string, refused error) error {
	release, err := c.lock.Lock(ctx, orgID)
	if err != nil {
		return fmt.Errorf("linearoauth: lock org %s's linear credential: %w", orgID, err)
	}
	defer release()
	_, current, err := c.readEnvelope(ctx, orgID)
	switch {
	case errors.Is(err, linear.ErrNoLinearSystemCredential):
		return fmt.Errorf("%w: %w", errInstallReplaced, refused)
	case err != nil:
		return err
	case current != raw:
		return fmt.Errorf("%w: %w", errEnvelopeMoved, refused)
	}
	if _, err := c.installs.MarkRemovedSystem(ctx, orgID, cred.InstallID, domain.LinearInstallRemovedRevoked); err != nil {
		return fmt.Errorf("linearoauth: mark org %s's revoked install removed: %w", orgID, err)
	}
	c.forget(orgID)
	if _, err := c.secrets.DeleteSystemIfValue(ctx, orgID, integrations.KeyLinearAppInstall, raw); err != nil {
		// The row is removed, so the workspace is free; the envelope is
		// refused again and removed on the next read.
		cacheLog.Warn("removing a revoked install's credential failed", "org", orgID, "error", err)
	}
	cacheLog.Warn("linear refused the install's refresh token: the app was removed from the workspace; removed the install, so an admin has to install again",
		"org", orgID)
	return fmt.Errorf("%w: org=%s (install revoked in linear): %w", linear.ErrNoLinearSystemCredential, orgID, refused)
}

// refreshOnce reads the envelope, refreshes off its refresh token, writes the
// rotated token back, and caches the access token. It returns the envelope it
// read, parsed and raw, for refresh to act on a refusal.
func (c *TokenCache) refreshOnce(ctx context.Context, orgID string) (_ cachedToken, _ linear.InstallCredential, raw string, _ error) {
	cred, raw, err := c.readEnvelope(ctx, orgID)
	if err != nil {
		return cachedToken{}, cred, raw, err
	}
	app, _, err := c.apps.Resolve(ctx, orgID)
	if err != nil {
		return cachedToken{}, cred, raw, fmt.Errorf("linearoauth: resolve oauth app for org %s: %w", orgID, err)
	}
	if app.ClientID != cred.ClientID {
		// The app that minted the install is no longer the one that resolves;
		// its secret is the only one Linear accepts for this refresh token.
		return cachedToken{}, cred, raw, fmt.Errorf("linearoauth: org %s's install was minted by OAuth app %s, but app %s resolves now; install again under the current app",
			orgID, cred.ClientID, app.ClientID)
	}

	tok, err := c.minter.Refresh(ctx, app, cred.RefreshToken)
	if err != nil {
		return cachedToken{}, cred, raw, err
	}

	// The write-back lands only over the envelope this refresh read, and
	// before the cache, so a crash after it still leaves a live refresh token
	// for the next refresh.
	rotated := cred
	rotated.RefreshToken = tok.RefreshToken
	env, err := linear.MarshalInstallCredential(rotated)
	if err != nil {
		return cachedToken{}, cred, raw, err
	}
	release, err := c.lock.Lock(ctx, orgID)
	if err != nil {
		return cachedToken{}, cred, raw, fmt.Errorf("linearoauth: lock org %s's linear credential: %w", orgID, err)
	}
	defer release()
	swapped, err := c.secrets.PutSystemIfValue(ctx, orgID, integrations.KeyLinearAppInstall, raw, env, "Linear app install")
	if err != nil {
		return cachedToken{}, cred, raw, fmt.Errorf("linearoauth: persist rotated refresh token for org %s: %w", orgID, err)
	}
	if !swapped {
		// Something wrote the envelope while this refresh was in flight. A
		// disconnect or a new install means this pair belongs to an install
		// the org no longer has. Another process's rotation of the same
		// install is the newer token and stays; this pair's access token is
		// still good until it expires.
		current, _, err := c.readEnvelope(ctx, orgID)
		switch {
		case errors.Is(err, linear.ErrNoLinearSystemCredential):
			return cachedToken{}, cred, raw, fmt.Errorf("%w: org=%s (install removed)", errInstallReplaced, orgID)
		case err != nil:
			return cachedToken{}, cred, raw, err
		case current.InstallID != cred.InstallID:
			return cachedToken{}, cred, raw, fmt.Errorf("%w: org=%s", errInstallReplaced, orgID)
		}
	}

	ct := cachedToken{installID: cred.InstallID, accessToken: tok.AccessToken, expiresAt: tok.ExpiresAt}
	c.mu.Lock()
	c.cache[orgID] = ct
	c.mu.Unlock()
	return ct, cred, raw, nil
}

func (c *TokenCache) forget(orgID string) {
	c.mu.Lock()
	delete(c.cache, orgID)
	c.mu.Unlock()
}
