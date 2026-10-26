package linearoauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// errGrantReplaced is a refresh that finished after the credential it read
// was replaced by a different Connect or a pasted key. Its token pair belongs
// to a grant the user no longer holds, so it is neither written back nor
// cached.
var errGrantReplaced = errors.New("linearoauth: user credential replaced during refresh")

// UserTokenCache mints and caches the access token of a user's Connect
// credential, and writes each rotated refresh token back to the user's
// envelope. It implements linear.UserTokenSource. Access tokens live only
// here, in memory; the envelope stores the refresh token alone.
//
// Every read starts from the stored envelope, so a cached token is served only
// while the envelope still names the grant it was minted for (its grant_id).
// A disconnect or a new Connect on any process is seen on the next read. A
// rotation by another process keeps the grant, so it does not make this
// process refresh again: the token this cache holds stays good until it
// expires.
//
// A singleflight keyed by (org, user, workspace) coalesces one process's
// concurrent refreshes. The rotation write-back is a compare-and-swap against
// the envelope its refresh read, under the user's UserCredentialLock, so it
// never resurrects a disconnected credential or overwrites a newer one. A
// refresh token Linear refuses with invalid_grant means the user revoked TF's
// access in Linear: the identity row derived from the envelope and then the
// envelope are removed under the same lock (dropRefused), and the user has to
// connect again.
type UserTokenCache struct {
	minter  refresher
	apps    linear.OAuthAppResolver
	secrets db.SecretStore
	tx      db.TxRunner
	lock    *UserCredentialLock

	// now is injectable for tests; nil is time.Now.
	now func() time.Time

	group singleflight.Group

	mu    sync.Mutex
	cache map[string]cachedUserToken
}

type cachedUserToken struct {
	grantID     string
	accessToken string
	expiresAt   time.Time
}

// NewUserTokenCache builds a UserTokenCache over a minter, the org's OAuth-app
// resolver, the secret store (the envelope), the transaction runner that
// removes a refused credential with its identity, and the lock the user's
// other credential writers hold.
func NewUserTokenCache(minter *Minter, apps linear.OAuthAppResolver, secrets db.SecretStore, tx db.TxRunner, lock *UserCredentialLock) *UserTokenCache {
	return newUserTokenCache(minter, apps, secrets, tx, lock)
}

func newUserTokenCache(minter refresher, apps linear.OAuthAppResolver, secrets db.SecretStore, tx db.TxRunner, lock *UserCredentialLock) *UserTokenCache {
	return &UserTokenCache{
		minter:  minter,
		apps:    apps,
		secrets: secrets,
		tx:      tx,
		lock:    lock,
		cache:   make(map[string]cachedUserToken),
	}
}

func (c *UserTokenCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func userCacheKey(orgID, userID, workspaceID string) string {
	return orgID + "\x00" + userID + "\x00" + workspaceID
}

// AccessTokenForUser returns a live access token for the user's Connect
// credential in workspaceID, refreshing when the cache holds none for the
// stored grant or the one it holds has less than a minute left. A user with
// no Connect credential, one minted by an app the org can no longer
// authenticate as, one Linear has refused, or one that changed during two
// refreshes in a row is linear.ErrNoLinearUserCredential.
func (c *UserTokenCache) AccessTokenForUser(ctx context.Context, orgID, userID, workspaceID string) (string, error) {
	key := userCacheKey(orgID, userID, workspaceID)
	cred, _, err := c.readEnvelope(ctx, orgID, userID, workspaceID)
	if err != nil {
		return "", err
	}
	if ct, ok := c.fresh(key, cred.GrantID); ok {
		return ct.accessToken, nil
	}
	// The refresh runs detached from every caller's ctx, for the reason
	// TokenCache.refreshShared gives: a write-back abandoned after Linear
	// rotated the token leaves a dead one stored.
	ch := c.group.DoChan(key, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshFlightTimeout)
		defer cancel()
		return c.refresh(fctx, orgID, userID, workspaceID)
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		return res.Val.(cachedUserToken).accessToken, nil
	}
}

// fresh is the token this cache holds for grantID with more than refreshSkew
// left.
func (c *UserTokenCache) fresh(key, grantID string) (cachedUserToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct, ok := c.cache[key]
	if !ok || ct.grantID != grantID || !c.timeNow().Add(refreshSkew).Before(ct.expiresAt) {
		return cachedUserToken{}, false
	}
	return ct, true
}

// readEnvelope reads and parses the user's envelope for workspaceID, returning
// the raw value too for a compare-and-swap. No envelope, or one that is not a
// usable Connect credential, is ErrNoLinearUserCredential: a pasted key has no
// access token to mint, and the resolver reads it before ever calling here.
func (c *UserTokenCache) readEnvelope(ctx context.Context, orgID, userID, workspaceID string) (linear.UserCredential, string, error) {
	raw, err := c.secrets.GetUserSystem(ctx, orgID, userID, linear.UserTokenKey(workspaceID))
	if err != nil {
		return linear.UserCredential{}, "", fmt.Errorf("linearoauth: read user credential for org %s user %s: %w", orgID, userID, err)
	}
	if raw == "" {
		return linear.UserCredential{}, "", fmt.Errorf("%w: org=%s user=%s workspace=%s", linear.ErrNoLinearUserCredential, orgID, userID, workspaceID)
	}
	cred, err := linear.ParseUserCredential(raw)
	if err != nil {
		return linear.UserCredential{}, raw, fmt.Errorf("linearoauth: org %s user %s: %w", orgID, userID, err)
	}
	if cred.Method != linear.AuthMethodConnectOAuth || !cred.Usable() {
		return linear.UserCredential{}, raw, fmt.Errorf("%w: org=%s user=%s workspace=%s (stored credential is not a usable connect_oauth credential, method=%q)",
			linear.ErrNoLinearUserCredential, orgID, userID, workspaceID, cred.Method)
	}
	return cred, raw, nil
}

// refresh mints a fresh access token (refreshOnce) and answers the ways a
// refresh can lose its footing. A refresh that outlives its grant (the user
// connecting again or pasting a key while the request was in flight) is
// retried once against what is stored now. A refresh token Linear refuses is
// removed (dropRefused), unless it is no longer the stored one: another
// process rotated it, so the stored token is tried once instead.
//
// A retry that loses to another writer again is one this refresh cannot
// settle. The credential is still the user's own, so it gets the answer of
// one with none, ErrNoLinearUserCredential: connecting again replaces
// whatever is stored by then.
func (c *UserTokenCache) refresh(ctx context.Context, orgID, userID, workspaceID string) (cachedUserToken, error) {
	key := userCacheKey(orgID, userID, workspaceID)
	// Re-checked under the flight: a sibling caller may have just refreshed.
	if cred, _, err := c.readEnvelope(ctx, orgID, userID, workspaceID); err == nil {
		if ct, ok := c.fresh(key, cred.GrantID); ok {
			return ct, nil
		}
	}
	for attempt := 0; ; attempt++ {
		ct, raw, err := c.refreshOnce(ctx, orgID, userID, workspaceID)
		if RefusedGrant(err) {
			err = c.dropRefused(ctx, orgID, userID, workspaceID, raw, err)
		}
		lost := errors.Is(err, errGrantReplaced) || errors.Is(err, errEnvelopeMoved)
		switch {
		case err == nil:
			return ct, nil
		case lost && attempt == 0:
			continue
		case lost:
			return cachedUserToken{}, fmt.Errorf("%w: org=%s user=%s (the stored credential changed during two refreshes in a row): %w",
				linear.ErrNoLinearUserCredential, orgID, userID, err)
		default:
			return cachedUserToken{}, err
		}
	}
}

// refreshOnce reads the envelope, refreshes off its refresh token, writes the
// rotated token back, and caches the access token. raw is the envelope it
// read, for refresh to act on a refusal.
func (c *UserTokenCache) refreshOnce(ctx context.Context, orgID, userID, workspaceID string) (_ cachedUserToken, raw string, _ error) {
	cred, raw, err := c.readEnvelope(ctx, orgID, userID, workspaceID)
	if err != nil {
		return cachedUserToken{}, raw, err
	}
	// The refresh token is accepted only with the secret of the app that
	// minted it, which need not be the app the org resolves to now.
	app, err := c.apps.ResolveClient(ctx, orgID, cred.ClientID)
	if errors.Is(err, linear.ErrNoLinearOAuthApp) {
		// It stays stored: connecting again or pasting a key replaces it.
		return cachedUserToken{}, raw, fmt.Errorf("%w: org=%s user=%s (connected through OAuth app %s, which the org can no longer authenticate as; connect again or paste a key)",
			linear.ErrNoLinearUserCredential, orgID, userID, cred.ClientID)
	}
	if err != nil {
		return cachedUserToken{}, raw, fmt.Errorf("linearoauth: resolve oauth app %s for org %s: %w", cred.ClientID, orgID, err)
	}

	tok, err := c.minter.Refresh(ctx, app, cred.RefreshToken)
	if err != nil {
		return cachedUserToken{}, raw, err
	}

	// The write-back lands only over the envelope this refresh read, and
	// before the cache, so a crash after it still leaves a live refresh token.
	rotated := cred
	rotated.RefreshToken = tok.RefreshToken
	env, err := linear.MarshalUserCredential(rotated)
	if err != nil {
		return cachedUserToken{}, raw, err
	}
	release, err := c.lock.Lock(ctx, orgID, userID)
	if err != nil {
		return cachedUserToken{}, raw, fmt.Errorf("linearoauth: lock org %s user %s's linear credential: %w", orgID, userID, err)
	}
	defer release()
	swapped, err := c.secrets.PutUserSystemIfValue(ctx, orgID, userID, linear.UserTokenKey(workspaceID), raw, env, "Linear user access token")
	if err != nil {
		return cachedUserToken{}, raw, fmt.Errorf("linearoauth: persist rotated refresh token for org %s user %s: %w", orgID, userID, err)
	}
	if !swapped {
		// Something wrote the envelope while this refresh was in flight. A
		// disconnect or a new grant means this pair belongs to a credential
		// the user no longer holds. Another process's rotation of the same
		// grant is the newer token and stays; this pair's access token is
		// still good until it expires.
		current, _, err := c.readEnvelope(ctx, orgID, userID, workspaceID)
		switch {
		case errors.Is(err, linear.ErrNoLinearUserCredential):
			return cachedUserToken{}, raw, fmt.Errorf("%w: org=%s user=%s", errGrantReplaced, orgID, userID)
		case err != nil:
			return cachedUserToken{}, raw, err
		case current.GrantID != cred.GrantID:
			return cachedUserToken{}, raw, fmt.Errorf("%w: org=%s user=%s", errGrantReplaced, orgID, userID)
		}
	}

	ct := cachedUserToken{grantID: cred.GrantID, accessToken: tok.AccessToken, expiresAt: tok.ExpiresAt}
	c.mu.Lock()
	c.cache[userCacheKey(orgID, userID, workspaceID)] = ct
	c.mu.Unlock()
	return ct, raw, nil
}

// dropRefused answers Linear refusing the user's refresh token, refused,
// which every error it returns wraps. Under the user's credential lock, and
// only while the stored envelope is still the refused one, it removes the
// identity row derived from the envelope and then the envelope itself.
//
// The order is what keeps the two from drifting apart for good. The envelope
// is the one whose presence brings a later read back here: the next refresh
// off it is refused again and this runs again. So it is deleted last, and
// only once the identity clear has committed. A failure anywhere before that
// leaves the envelope, and the removal is finished by the next read. Deleting
// it inside the identity's transaction would not do: in local mode the
// keychain delete lands outside the SQLite transaction, so a failed commit
// would roll back the identity clear with the envelope already gone, and
// nothing would come back to clear the identity.
func (c *UserTokenCache) dropRefused(ctx context.Context, orgID, userID, workspaceID, raw string, refused error) error {
	release, err := c.lock.Lock(ctx, orgID, userID)
	if err != nil {
		return fmt.Errorf("linearoauth: lock org %s user %s's linear credential: %w: %w", orgID, userID, err, refused)
	}
	defer release()
	key := linear.UserTokenKey(workspaceID)
	moved := false
	err = c.tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		// Every writer of the envelope holds the lock, so it cannot change
		// between this read and the delete below.
		current, err := tx.Secrets.GetUser(ctx, orgID, userID, key)
		if err != nil {
			return fmt.Errorf("read user credential: %w", err)
		}
		if current != raw {
			moved = true
			return nil
		}
		if err := tx.Users.ClearLinearIdentity(ctx, userID, workspaceID); err != nil {
			return fmt.Errorf("clear linear identity: %w", err)
		}
		return nil
	})
	if err == nil && !moved {
		var deleted bool
		// TODO(TFAC-1204): replace the refused envelope with a refused marker
		// (a compare-and-swap) instead of deleting it, so the identity status
		// can tell the user why they were disconnected.
		deleted, err = c.secrets.DeleteUserSystemIfValue(ctx, orgID, userID, key, raw)
		if err == nil && !deleted {
			moved = true
		}
	}
	switch {
	case err != nil:
		// Still dead, so the caller gets the same answer. The envelope stays
		// and the next read finishes the removal.
		cacheLog.Warn("linear refused a user's refresh token and removing it failed",
			"org", orgID, "user", userID, "workspace", workspaceID, "error", err)
		return fmt.Errorf("%w: org=%s user=%s (refused by linear): %w", linear.ErrNoLinearUserCredential, orgID, userID, refused)
	case moved:
		return fmt.Errorf("%w: %w", errEnvelopeMoved, refused)
	}
	c.mu.Lock()
	delete(c.cache, userCacheKey(orgID, userID, workspaceID))
	c.mu.Unlock()
	cacheLog.Warn("linear refused a user's refresh token: they revoked Triage Factory's access in Linear; removed the credential, so they have to connect again",
		"org", orgID, "user", userID, "workspace", workspaceID)
	return fmt.Errorf("%w: org=%s user=%s (refused by linear): %w", linear.ErrNoLinearUserCredential, orgID, userID, refused)
}
