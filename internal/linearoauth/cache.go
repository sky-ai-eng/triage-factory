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

// TokenCache mints and caches the access token of an org's app install, and
// writes each rotated refresh token back to the install's envelope. It
// implements linear.InstallTokenSource.
//
// Every read starts from the stored envelope, so a cached token is served only
// while the envelope still names the install it was minted for (its
// installed_at). A new install or a disconnect anywhere in the deployment is
// therefore seen on the next read, with no invalidation message to deliver.
//
// A singleflight keyed by org coalesces one process's concurrent refreshes.
// Across processes none is needed: Linear answers a refresh token replayed
// within its grace window with the identical new pair, so two processes
// refreshing at once converge, and whichever writes back last writes the same
// token.
type TokenCache struct {
	minter   refresher
	apps     linear.OAuthAppResolver
	secrets  db.SecretStore
	installs db.LinearInstallsStore

	// now is injectable for tests; nil is time.Now.
	now func() time.Time

	group singleflight.Group

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	installedAt time.Time
	accessToken string
	expiresAt   time.Time
}

// NewTokenCache builds a TokenCache over a minter, the org's OAuth-app
// resolver (the client credentials a refresh authenticates with), the secret
// store (the envelope) and the installs store (marking a revoked install
// removed).
func NewTokenCache(minter *Minter, apps linear.OAuthAppResolver, secrets db.SecretStore, installs db.LinearInstallsStore) *TokenCache {
	return newTokenCache(minter, apps, secrets, installs)
}

func newTokenCache(minter refresher, apps linear.OAuthAppResolver, secrets db.SecretStore, installs db.LinearInstallsStore) *TokenCache {
	return &TokenCache{
		minter:   minter,
		apps:     apps,
		secrets:  secrets,
		installs: installs,
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
	if ct, ok := c.fresh(orgID, cred.InstalledAt); ok {
		return ct.accessToken, ct.expiresAt, nil
	}

	// The shared call runs under the first caller's ctx, so a cancellation
	// there reaches every coalesced waiter. That is the cost of one refresh
	// per rotation, which the rotating token depends on.
	res, err, _ := c.group.Do(orgID, func() (any, error) {
		return c.refresh(ctx, orgID)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	ct := res.(cachedToken)
	return ct.accessToken, ct.expiresAt, nil
}

// fresh is the cached token for orgID when it was minted for the install
// installedAt names and has headroom left.
func (c *TokenCache) fresh(orgID string, installedAt time.Time) (cachedToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct, ok := c.cache[orgID]
	if !ok || !ct.installedAt.Equal(installedAt) || !c.timeNow().Add(refreshSkew).Before(ct.expiresAt) {
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

// refresh mints a fresh access token (refreshOnce) and answers the two ways a
// refresh can lose its footing.
//
// A refresh token Linear refuses with invalid_grant means the app was removed
// from the workspace: the install is marked removed, its envelope deleted, and
// the org reads as having no Linear credential until an admin installs again.
// The envelope goes only while it still holds the refused token. If another
// process stored a different one meanwhile, that one is tried once instead.
//
// A refresh that outlives its install (a disconnect, or a new install,
// landing while the request was in flight) is retried once against whatever
// is stored now.
func (c *TokenCache) refresh(ctx context.Context, orgID string) (cachedToken, error) {
	// Re-checked under the flight: a sibling caller may have just refreshed.
	if cred, _, err := c.readEnvelope(ctx, orgID); err == nil {
		if ct, ok := c.fresh(orgID, cred.InstalledAt); ok {
			return ct, nil
		}
	}
	for attempt := 0; ; attempt++ {
		ct, raw, err := c.refreshOnce(ctx, orgID)
		switch {
		case err == nil:
			return ct, nil
		case errors.Is(err, errInstallReplaced):
			if attempt > 0 {
				return cachedToken{}, err
			}
			continue
		case !RefusedGrant(err):
			return cachedToken{}, err
		}

		deleted, derr := c.secrets.DeleteSystemIfValue(ctx, orgID, integrations.KeyLinearAppInstall, raw)
		switch {
		case derr != nil:
			// The token is still dead and installing again replaces it, so
			// the org gets the same answer; only the settings card is behind.
			cacheLog.Warn("linear refused an install's refresh token and removing it failed",
				"org", orgID, "error", derr)
		case deleted:
			c.forget(orgID)
			if _, merr := c.installs.MarkRemovedSystem(ctx, orgID, domain.LinearInstallRemovedRevoked); merr != nil {
				cacheLog.Warn("mark revoked linear install removed failed", "org", orgID, "error", merr)
			}
			cacheLog.Warn("linear refused the install's refresh token: the app was removed from the workspace; removed the install, so an admin has to install again",
				"org", orgID)
		case attempt == 0:
			continue
		}
		return cachedToken{}, fmt.Errorf("%w: org=%s (install revoked in linear): %w", linear.ErrNoLinearSystemCredential, orgID, err)
	}
}

// refreshOnce reads the envelope, refreshes off its refresh token, writes the
// rotated token back, and caches the access token. raw is the envelope read,
// for refresh to remove when its token was refused.
func (c *TokenCache) refreshOnce(ctx context.Context, orgID string) (_ cachedToken, raw string, _ error) {
	cred, raw, err := c.readEnvelope(ctx, orgID)
	if err != nil {
		return cachedToken{}, raw, err
	}
	app, _, err := c.apps.Resolve(ctx, orgID)
	if err != nil {
		return cachedToken{}, raw, fmt.Errorf("linearoauth: resolve oauth app for org %s: %w", orgID, err)
	}
	if app.ClientID != cred.ClientID {
		// The app that minted the install is no longer the one that resolves;
		// its secret is the only one Linear accepts for this refresh token.
		return cachedToken{}, raw, fmt.Errorf("linearoauth: org %s's install was minted by OAuth app %s, but app %s resolves now; install again under the current app",
			orgID, cred.ClientID, app.ClientID)
	}

	tok, err := c.minter.Refresh(ctx, app, cred.RefreshToken)
	if err != nil {
		return cachedToken{}, raw, err
	}

	// A disconnect or a new install may have landed while the request was in
	// flight. Writing this install's rotation back over it would resurrect a
	// disconnected credential or overwrite a newer one, so the envelope is read
	// again and the write made only while it still names this install. The
	// window left is the read and the write, not the round trip.
	current, _, err := c.readEnvelope(ctx, orgID)
	if errors.Is(err, linear.ErrNoLinearSystemCredential) {
		return cachedToken{}, raw, fmt.Errorf("%w: org=%s (install removed)", errInstallReplaced, orgID)
	}
	if err != nil {
		return cachedToken{}, raw, err
	}
	if !current.InstalledAt.Equal(cred.InstalledAt) || current.ClientID != cred.ClientID {
		return cachedToken{}, raw, fmt.Errorf("%w: org=%s", errInstallReplaced, orgID)
	}

	// The write-back comes before the cache: a crash after it still leaves a
	// live refresh token for the next refresh.
	rotated := cred
	rotated.RefreshToken = tok.RefreshToken
	env, err := linear.MarshalInstallCredential(rotated)
	if err != nil {
		return cachedToken{}, raw, err
	}
	if err := c.secrets.PutSystem(ctx, orgID, integrations.KeyLinearAppInstall, env, "Linear app install"); err != nil {
		return cachedToken{}, raw, fmt.Errorf("linearoauth: persist rotated refresh token for org %s: %w", orgID, err)
	}

	ct := cachedToken{installedAt: cred.InstalledAt, accessToken: tok.AccessToken, expiresAt: tok.ExpiresAt}
	c.mu.Lock()
	c.cache[orgID] = ct
	c.mu.Unlock()
	return ct, raw, nil
}

func (c *TokenCache) forget(orgID string) {
	c.mu.Lock()
	delete(c.cache, orgID)
	c.mu.Unlock()
}
