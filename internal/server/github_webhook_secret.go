package server

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The per-org GitHub webhook receiver is pre-auth, and the secret it verifies a
// delivery against is reachable only through the org named in the URL: the org's
// credential class, then its App registration, then the vault entry that
// registration points at. Three reads, all of them spent before the signature
// is checked, and spent on behalf of a caller that has proved nothing yet.
//
// This file holds the short-TTL per-org cache that keeps a burst of forged
// deliveries from multiplying into that read traffic, and the invalidation the
// App lifecycle fires at it.

// webhookSecretTTL bounds how long one org's resolved receiver secret is served
// from memory. Short on purpose: explicit invalidation on the paths that write
// or tear down webhook_secret_ref is what actually keeps a rotation from being
// served stale, and this is the backstop for anything that rotates the secret
// without going through them — seconds of staleness, not minutes.
const webhookSecretTTL = 30 * time.Second

// webhookSecretCacheMax caps how many orgs the cache holds. The receiver takes
// an org id from an unauthenticated URL, so the set of keys is attacker-chosen:
// without a ceiling, a flood across distinct (well-formed but nonexistent) org
// ids would grow the map for as long as it ran. See webhookSecretCachePut for
// what happens at the cap.
const webhookSecretCacheMax = 4096

// webhookSecretEntry is one org's resolved receiver secret plus the instant it
// stops being trusted. An empty secret is a cached NEGATIVE — "this org has
// nothing to verify a delivery against" — and is worth caching for exactly the
// same reason as a positive one: a flood aimed at an org with no App is the
// traffic this cache exists to absorb.
type webhookSecretEntry struct {
	secret    string
	expiresAt time.Time
}

// webhookSecretFor returns the secret that verifies a GitHub delivery for
// orgID, or "" when the org has nothing to verify against. Cached per org for
// webhookSecretTTL, negatives included.
//
// Concurrent misses on the same org each resolve — no single-flight — because
// the limiter in front of the route already bounds how many can be in flight,
// and a duplicated resolution is cheaper than the coordination that would
// prevent it.
//
// The generation the miss hands back is what keeps that concurrency from
// defeating the invalidation. A resolution runs outside the lock, so a rotation
// can land while one is in flight; without the check below, the older
// resolution's write would arrive after the invalidation and reinstate the
// secret that was just replaced — for a full TTL, on the very deliveries the
// invalidation exists to get right. Presenting the generation makes that write
// lose instead of win: the next delivery misses and resolves the new secret.
func (s *Server) webhookSecretFor(ctx context.Context, orgID string) (string, error) {
	secret, gen, ok := s.webhookSecretCacheGet(orgID)
	if ok {
		return secret, nil
	}
	secret, err := s.resolveWebhookSecret(ctx, orgID)
	if err != nil {
		return "", err
	}
	s.webhookSecretCachePut(orgID, secret, gen)
	return secret, nil
}

// resolveWebhookSecret reads the secret an org's deliveries verify against,
// uncached.
//
// Which secret that is follows from the org's credential class. A per-org
// registration's own webhook secret is the BYO-App answer; a PAT org has no App
// and therefore nothing a delivery could be verified against, so every delivery
// claiming to be for it is unauthenticated by construction. Reading a missing
// registration row as "PAT" would be an inference that holds only while PAT is
// the single rowless shape — an org on the deployment's shared App has no row
// either, and its deliveries verify against a deployment secret this function
// deliberately never reaches.
//
// System (claims-free) reads throughout: the receiver is pre-auth and has only
// a trusted org_id from the URL.
//
// The five ways an org can have no secret — a credential class this build
// doesn't understand, a PAT org, a managed org, no registration row, a hookless
// App — collapse into one empty return on purpose. The caller answers every one
// of them exactly as it answers a bad signature, so an unauthenticated caller
// cannot use the reply to learn which orgs have an App registered. Only a
// genuine store failure comes back as an error.
func (s *Server) resolveWebhookSecret(ctx context.Context, orgID string) (string, error) {
	class, err := s.githubCredentialClass(ctx, orgID)
	if err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			githubAppLog.Warn("webhook delivery for org with unknown credential class", "org", orgID)
			return "", nil
		}
		return "", err
	}
	switch class {
	case domain.GitHubCredentialClassBYOApp:
		// The one class with a secret to find. Falls through to the registration
		// read below.
	case domain.GitHubCredentialClassManagedApp:
		// A managed org has a webhook secret — the deployment's — and this
		// function must still answer nothing, which is why the arm exists rather
		// than being folded into a catch-all.
		//
		// Its deliveries do not arrive here at all. The per-org URL names a
		// tenant in its path and is verified against that tenant's own secret;
		// the shared App signs every tenant's deliveries with ONE secret, so
		// returning it here would make this endpoint accept a delivery for any
		// workspace as long as the URL named one — an attacker picking the org id
		// and GitHub supplying the signature. The shared App's deliveries land on
		// a route with no org in its path, which verifies first and only then
		// works out whose installation it was.
		return "", nil
	case domain.GitHubCredentialClassPAT:
		return "", nil
	default:
		// Unreachable: githubCredentialClass refuses an unknown class above.
		return "", nil
	}
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return "", err
	}
	if app == nil || app.WebhookSecretRef == "" {
		return "", nil
	}
	return s.secrets.GetSystem(ctx, orgID, app.WebhookSecretRef)
}

// webhookSecretCacheGet returns the cached secret for orgID when present and
// unexpired. A hit on a cached negative returns ("", _, true) — distinct from
// the ("", _, false) miss, which is what sends the caller to the store.
//
// The generation is the cache's invalidation counter read at the same instant
// as the lookup, and it is the token a subsequent Put has to present. Callers
// hand it straight back; it is meaningful only as the pair.
func (s *Server) webhookSecretCacheGet(orgID string) (string, uint64, bool) {
	s.webhookSecretMu.Lock()
	defer s.webhookSecretMu.Unlock()
	e, ok := s.webhookSecretCache[orgID]
	if !ok || time.Now().After(e.expiresAt) {
		return "", s.webhookSecretGen, false
	}
	return e.secret, s.webhookSecretGen, true
}

// webhookSecretCachePut stores one org's resolution with a fresh TTL, unless an
// invalidation has happened since gen was read — in which case the resolution
// is older than the rotation that invalidated it and is dropped rather than
// written. The cost of dropping it is one extra resolution on the next
// delivery; the cost of writing it would be a TTL of the wrong secret.
//
// Reads report a miss on expiry but don't delete, so the sweep lives here. It
// runs at most once per TTL window on the common path — the same
// once-per-window discipline the rate limiter's bucket sweep uses — and
// unconditionally once the map reaches its cap. A map still at the cap after
// sweeping means every entry is live, which is a flood across distinct org ids
// rather than a working set; the whole map goes, since this is a cache and the
// rebuild costs one resolution per org actually delivering.
func (s *Server) webhookSecretCachePut(orgID, secret string, gen uint64) {
	now := time.Now()
	s.webhookSecretMu.Lock()
	defer s.webhookSecretMu.Unlock()
	if gen != s.webhookSecretGen {
		return
	}
	if s.webhookSecretCache == nil {
		s.webhookSecretCache = make(map[string]webhookSecretEntry)
	}
	if now.Sub(s.webhookSecretSweep) >= webhookSecretTTL || len(s.webhookSecretCache) >= webhookSecretCacheMax {
		s.webhookSecretSweep = now
		for k, e := range s.webhookSecretCache {
			if now.After(e.expiresAt) {
				delete(s.webhookSecretCache, k)
			}
		}
		if len(s.webhookSecretCache) >= webhookSecretCacheMax {
			clear(s.webhookSecretCache)
		}
	}
	s.webhookSecretCache[orgID] = webhookSecretEntry{
		secret:    secret,
		expiresAt: now.Add(webhookSecretTTL),
	}
}

// invalidateWebhookSecret drops the org's cached resolution. Called by every
// path that writes or tears down webhook_secret_ref — App registration, BYOA
// import, the App→PAT switch, and the staged-registration discard — so a
// rotation takes effect on the next delivery instead of at the end of the TTL.
// Both directions matter: a stale positive would reject the deliveries signed
// with the new secret, and a stale negative would reject every delivery to an
// App that has just been registered.
//
// Deleting the entry is only half of it. A resolution that started before this
// call is still in flight and still holds the pre-rotation secret, so the
// generation moves too — that write now presents a stale token and is dropped
// (see webhookSecretCachePut). The counter is deployment-wide rather than
// per-org: an in-flight resolution for some other org loses its write to an
// unrelated rotation, which costs one extra resolution and keeps the counter
// from becoming a second attacker-keyed map.
func (s *Server) invalidateWebhookSecret(orgID string) {
	s.webhookSecretMu.Lock()
	defer s.webhookSecretMu.Unlock()
	delete(s.webhookSecretCache, orgID)
	s.webhookSecretGen++
}
