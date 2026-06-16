package jiraoauth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// refreshSkew is how long before an access token's stated expiry the cache
// treats it as stale and refreshes. Bounds the chance of handing out a token
// that expires mid-request.
const refreshSkew = 60 * time.Second

// refresher is the slice of *Minter the cache depends on — the refresh-grant
// exchange. Narrowed to an interface so the cache's rotation/write-back logic
// is testable without a live HTTP endpoint. *Minter satisfies it.
type refresher interface {
	Refresh(ctx context.Context, app jira.OAuthApp, refreshToken string) (Token, error)
}

// TokenCache mints + caches per-user Cloud OAuth access tokens, and performs
// the refresh-token rotation write-back. It implements
// jira.UserAccessTokenSource, so the write-actor resolver's AuthMethodCloudOAuth
// branch delegates straight to it.
//
// Caching is the point: the resolver builds a fresh client on every per-user
// write, but each refresh ROTATES the refresh token, so refreshing per request
// would both burn the Atlassian token endpoint and churn the stored credential.
// The cache holds the short-lived access token until ~expiry and only then
// refreshes — at which point it persists the rotated refresh token back so the
// NEXT refresh (after the new token also expires) still has a live credential.
//
// Concurrency: a singleflight keyed by (org,user,host) coalesces concurrent
// refreshes for the same user into one. Without it, two requests racing past an
// expired cache would both refresh with the same stored refresh token — and
// because Atlassian invalidates a refresh token the moment it's used, the loser
// would fail invalid_grant and (worse) could write back a now-dead token.
type TokenCache struct {
	minter  refresher
	apps    jira.OAuthAppResolver
	secrets db.SecretStore

	// now is injectable for tests; production leaves it nil → time.Now.
	now func() time.Time

	group singleflight.Group

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	cloudID     string
	accessToken string
	expiresAt   time.Time
}

// NewTokenCache builds a TokenCache over a minter, the org's OAuth-app
// resolver (for client credentials), and the secret store (read the envelope,
// write back the rotated refresh token).
func NewTokenCache(minter *Minter, apps jira.OAuthAppResolver, secrets db.SecretStore) *TokenCache {
	return newTokenCache(minter, apps, secrets)
}

// newTokenCache is the unexported constructor taking the refresher interface,
// so tests can inject a fake without a live token endpoint.
func newTokenCache(minter refresher, apps jira.OAuthAppResolver, secrets db.SecretStore) *TokenCache {
	return &TokenCache{
		minter:  minter,
		apps:    apps,
		secrets: secrets,
		cache:   make(map[string]cachedToken),
	}
}

func (c *TokenCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func cacheKey(orgID, userID, host string) string {
	return orgID + "\x00" + userID + "\x00" + host
}

// AccessTokenForUser returns a live Cloud OAuth access token for the user,
// minting (and caching) one from the stored refresh token when the cache is
// empty or stale. Implements jira.UserAccessTokenSource.
func (c *TokenCache) AccessTokenForUser(ctx context.Context, orgID, userID, host string) (string, string, error) {
	key := cacheKey(orgID, userID, host)

	// Fast path: a cached token with comfortable headroom.
	c.mu.Lock()
	if ct, ok := c.cache[key]; ok && c.timeNow().Add(refreshSkew).Before(ct.expiresAt) {
		c.mu.Unlock()
		return ct.cloudID, ct.accessToken, nil
	}
	c.mu.Unlock()

	// Slow path: coalesce concurrent refreshes for this user into one. Note the
	// standard singleflight tradeoff — the shared call runs under the FIRST
	// caller's ctx, so if that caller's context is cancelled before the refresh
	// completes, every coalesced waiter (even ones with ample deadline) gets the
	// cancellation. This is intentional (the same shape ghclient's token cache
	// uses); don't "fix" it by threading per-caller contexts, which would defeat
	// the coalescing the rotating refresh token depends on.
	res, err, _ := c.group.Do(key, func() (any, error) {
		// Re-check the cache under the singleflight — a sibling caller may have
		// just refreshed while we waited.
		c.mu.Lock()
		if ct, ok := c.cache[key]; ok && c.timeNow().Add(refreshSkew).Before(ct.expiresAt) {
			c.mu.Unlock()
			return ct, nil
		}
		c.mu.Unlock()
		return c.refresh(ctx, orgID, userID, host, key)
	})
	if err != nil {
		return "", "", err
	}
	ct := res.(cachedToken)
	return ct.cloudID, ct.accessToken, nil
}

// refresh reads the stored envelope, mints a fresh access token off its refresh
// token, persists the rotated refresh token back, and caches the result.
func (c *TokenCache) refresh(ctx context.Context, orgID, userID, host, key string) (cachedToken, error) {
	raw, err := c.secrets.GetUserSystem(ctx, orgID, userID, jira.UserTokenKey(host))
	if err != nil {
		return cachedToken{}, fmt.Errorf("jiraoauth: read stored credential: %w", err)
	}
	if raw == "" {
		return cachedToken{}, fmt.Errorf("jiraoauth: no stored credential for org=%s user=%s host=%s", orgID, userID, host)
	}
	cred, err := jira.ParseUserCredential(raw)
	if err != nil {
		return cachedToken{}, fmt.Errorf("jiraoauth: parse stored credential: %w", err)
	}
	if cred.Method != jira.AuthMethodCloudOAuth || cred.RefreshToken == "" || cred.CloudID == "" {
		return cachedToken{}, fmt.Errorf("jiraoauth: stored credential is not a usable cloud oauth credential (method=%q)", cred.Method)
	}

	app, _, err := c.apps.Resolve(ctx, orgID)
	if err != nil {
		return cachedToken{}, fmt.Errorf("jiraoauth: resolve oauth app: %w", err)
	}

	tok, err := c.minter.Refresh(ctx, app, cred.RefreshToken)
	if err != nil {
		return cachedToken{}, err
	}

	// Rotation write-back — the load-bearing step. Persist the NEW refresh
	// token (preserving cloud_id) before caching the access token, so a crash
	// after this point still leaves a live credential for the next refresh.
	// PutUserSystem (not PutUser): the cache runs on the claims-free system
	// pool, the write-side mirror of the GetUserSystem read above.
	rotated := cred
	rotated.RefreshToken = tok.RefreshToken
	env, err := jira.MarshalUserCredential(rotated)
	if err != nil {
		return cachedToken{}, fmt.Errorf("jiraoauth: marshal rotated credential: %w", err)
	}
	if err := c.secrets.PutUserSystem(ctx, orgID, userID, jira.UserTokenKey(host), env, "Jira user access token"); err != nil {
		return cachedToken{}, fmt.Errorf("jiraoauth: persist rotated refresh token: %w", err)
	}

	ct := cachedToken{cloudID: cred.CloudID, accessToken: tok.AccessToken, expiresAt: tok.ExpiresAt}
	c.mu.Lock()
	c.cache[key] = ct
	c.mu.Unlock()
	return ct, nil
}

// Invalidate drops any cached access token for the user — call after a
// re-Connect / credential rotation so the next read mints fresh rather than
// serving a token tied to a superseded refresh token.
func (c *TokenCache) Invalidate(orgID, userID, host string) {
	c.mu.Lock()
	delete(c.cache, cacheKey(orgID, userID, host))
	c.mu.Unlock()
}
