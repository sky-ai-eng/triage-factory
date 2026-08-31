package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Avatar proxy (TFAC-480). User/member avatars come from OAuth metadata
// (users.avatar_url — GitHub's avatars.githubusercontent.com, the org's Jira
// host, etc.), which are cross-origin. The app's CSP is `img-src 'self' data:`
// (security_headers.go), so the browser blocks a direct cross-origin <img> with
// a CSP violation and the picture never loads.
//
// Rather than widen img-src to a per-deployment allowlist of provider CDNs, we
// proxy the image through this same-origin endpoint: the browser only ever
// loads /api/avatars/{id} (same-origin → CSP stays `'self'`), and the server
// fetches + caches the upstream image. This is option (a) from the in-code
// TODO the ticket cites; it keeps the CSP tight and avoids composing the Jira
// avatar host into the CSP from org settings.

var avatarLog = logging.Component("avatars")

const (
	// maxAvatarBytes caps a single fetched avatar. Provider avatars are a few
	// KB; this is generous headroom while bounding both the cache and a single
	// response against a hostile/oversized upstream.
	maxAvatarBytes = 2 << 20 // 2 MiB

	// avatarSuccessTTL is how long a fetched image is served from cache before a
	// re-fetch. Avatars change rarely, and a re-login refreshes the stored URL
	// anyway, so a long TTL is fine — at most it serves a picture ~6h stale.
	avatarSuccessTTL = 6 * time.Hour
	// avatarFailureTTL negative-caches a failed fetch (unreachable host, non-image
	// body, SSRF reject) briefly, so a roster full of broken avatars can't make
	// the 15s-polling Usage page hammer the upstream (or us) every cycle.
	avatarFailureTTL = 2 * time.Minute

	// avatarFetchTimeout bounds one upstream fetch end to end.
	avatarFetchTimeout = 10 * time.Second
	// avatarDialTimeout bounds the TCP connect to the upstream.
	avatarDialTimeout = 5 * time.Second
	// avatarMaxRedirects caps redirect hops; each hop is re-validated (https +
	// the dial-time public-IP guard below).
	avatarMaxRedirects = 5
)

// Cache size limits are vars (not consts) so tests can shrink them; production
// never reassigns them. The cache enforces TWO independent bounds because either
// alone is insufficient:
//   - avatarCacheMaxBytes caps total memory. An entry-count cap alone, times the
//     per-image maxAvatarBytes (2 MiB), would permit multi-GiB of resident data.
//   - avatarCacheMaxEntries caps the map size. A flood of distinct FAILED urls
//     fills the cache with zero-byte negative entries the byte budget can't evict.
var (
	avatarCacheMaxBytes   int64 = 64 << 20 // 64 MiB across all cached images
	avatarCacheMaxEntries       = 4096
)

// errAvatarUnavailable is the generic error returned for any non-deliverable
// avatar (bad scheme, SSRF reject, upstream error, non-image body, oversize).
// The handler maps it to a 502 and the frontend Avatar falls back to a monogram.
var errAvatarUnavailable = errors.New("avatar unavailable")

// errBlockedAvatarHost marks a fetch rejected by the dial-time SSRF guard
// (non-public / non-IP resolved address). It's wrapped through the dial Control
// hook so the fetch path can errors.Is it and log at Warn — a private-IP avatar
// url is a probe signal, not a routine broken image. The chain survives the net
// stack (net.OpError / url.Error both Unwrap), verified by TestAvatarFetcher_
// BlockedHostIsClassified.
var errBlockedAvatarHost = errors.New("avatar host is not a public address")

// allowedAvatarTypes is the raster image set the proxy will serve. SVG is
// excluded deliberately: even loaded via <img> (where scripts don't run) it's
// needless attack surface, and the OAuth providers we proxy (GitHub, Gravatar,
// Jira) serve PNG/JPEG.
var allowedAvatarTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// isPublicUnicastIP reports whether ip is a globally-routable public unicast
// address — outside loopback/link-local/multicast/unspecified (IsGlobalUnicast),
// RFC 1918 / ULA (IsPrivate), and the CGNAT/doc/benchmark/reserved ranges in
// nonPublicPrefixes (shared with the GitHub manifest reachability check). It is
// the single SSRF predicate behind both the manifest pre-check and the avatar
// proxy's dial-time guard.
func isPublicUnicastIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	// IsGlobalUnicast rejects loopback, unspecified, multicast, and link-local
	// in one predicate; IsPrivate adds RFC 1918 / unique-local.
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// safeAvatarDialControl is the net.Dialer.Control hook for the avatar HTTP
// client: it runs with the RESOLVED address just before connect, so rejecting
// non-public IPs here defeats SSRF — including DNS rebinding, where a hostname
// resolves to a public IP at validation time but a private one at dial time.
func safeAvatarDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("avatar dial: bad address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("avatar dial: non-IP address %q: %w", host, errBlockedAvatarHost)
	}
	if !isPublicUnicastIP(ip) {
		return fmt.Errorf("avatar dial: refusing non-public address %s: %w", ip, errBlockedAvatarHost)
	}
	return nil
}

// newAvatarHTTPClient builds the SSRF-hardened client the proxy fetches with:
// a dial-time public-IP guard, no proxy (a direct connection so the guard sees
// the real upstream IP), modest timeouts, and a redirect policy that caps hops
// and forbids downgrading off https. The Control guard applies to redirect
// targets too, since they reuse this client's transport.
func newAvatarHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: avatarDialTimeout, Control: safeAvatarDialControl}
	transport := &http.Transport{
		// Proxy intentionally unset: a CONNECT through a proxy would dial the
		// proxy's address, hiding the real upstream IP from the Control guard.
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   avatarDialTimeout,
		ResponseHeaderTimeout: avatarDialTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   avatarFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= avatarMaxRedirects {
				return fmt.Errorf("avatar fetch: too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("avatar fetch: refusing non-https redirect to %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// fetchedAvatar is one cached image: the raw bytes + the validated image
// content-type to serve them with.
type fetchedAvatar struct {
	data        []byte
	contentType string
}

// avatarCacheEntry is one cache slot. A nil img is a negative-cache marker
// (the last fetch failed) so repeated requests don't re-hammer the upstream.
type avatarCacheEntry struct {
	img       *fetchedAvatar
	expiresAt time.Time
}

// avatarFetcher fetches + caches upstream avatars. One instance lives for the
// process lifetime (constructed in routes). Concurrent fetches of the same URL
// collapse via singleflight; results are TTL-cached, success and failure alike.
type avatarFetcher struct {
	client *http.Client
	group  singleflight.Group
	mu     sync.Mutex
	cache  map[string]avatarCacheEntry
	// totalBytes is the sum of cached image sizes (negative entries count 0),
	// guarded by mu — the running figure cachePut keeps under avatarCacheMaxBytes.
	totalBytes int64
}

func newAvatarFetcher() *avatarFetcher {
	return &avatarFetcher{
		client: newAvatarHTTPClient(),
		cache:  make(map[string]avatarCacheEntry),
	}
}

// cacheGet returns the cached entry for key when present and unexpired. failed
// is true for a negative-cache hit (img is nil).
func (f *avatarFetcher) cacheGet(key string) (img *fetchedAvatar, hit, failed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.cache[key]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false, false
	}
	return e.img, true, e.img == nil
}

// entryBytes is the cached-image size of an entry (0 for a negative entry).
func entryBytes(e avatarCacheEntry) int64 {
	if e.img == nil {
		return 0
	}
	return int64(len(e.img.data))
}

// cachePut stores img (nil → negative cache) under key with the matching TTL,
// keeping totalBytes accurate. It sweeps expired entries on the way in, then
// evicts soonest-to-expire entries until BOTH bounds hold for the incoming entry:
// total bytes within avatarCacheMaxBytes (memory) and entry count within
// avatarCacheMaxEntries (map size, the bound that catches a flood of zero-byte
// negative entries). A single image larger than the whole budget still stores
// once into an otherwise-empty cache — the loop never spins on an empty map.
func (f *avatarFetcher) cachePut(key string, img *fetchedAvatar) {
	ttl := avatarSuccessTTL
	if img == nil {
		ttl = avatarFailureTTL
	}
	var size int64
	if img != nil {
		size = int64(len(img.data))
	}
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()

	// Drop any existing entry for this key first, so its old size doesn't linger
	// in totalBytes and the count below reflects the post-insert state.
	if old, exists := f.cache[key]; exists {
		f.totalBytes -= entryBytes(old)
		delete(f.cache, key)
	}
	for k, e := range f.cache {
		if now.After(e.expiresAt) {
			f.totalBytes -= entryBytes(e)
			delete(f.cache, k)
		}
	}
	for len(f.cache) > 0 && (len(f.cache)+1 > avatarCacheMaxEntries || f.totalBytes+size > avatarCacheMaxBytes) {
		var evictKey string
		var soonest time.Time
		for k, e := range f.cache {
			if evictKey == "" || e.expiresAt.Before(soonest) {
				evictKey, soonest = k, e.expiresAt
			}
		}
		f.totalBytes -= entryBytes(f.cache[evictKey])
		delete(f.cache, evictKey)
	}
	f.cache[key] = avatarCacheEntry{img: img, expiresAt: now.Add(ttl)}
	f.totalBytes += size
}

// fetch returns the avatar at rawURL, from cache when fresh. A miss runs one
// fetch (deduped across concurrent callers for the same URL) on a detached,
// timeout-bounded context so a single client disconnecting can't cancel the
// shared fetch. Both success and failure are cached. Returns errAvatarUnavailable
// for a cached failure.
func (f *avatarFetcher) fetch(rawURL string) (*fetchedAvatar, error) {
	if img, hit, failed := f.cacheGet(rawURL); hit {
		if failed {
			return nil, errAvatarUnavailable
		}
		return img, nil
	}
	v, err, _ := f.group.Do(rawURL, func() (any, error) {
		// Re-check: another goroutine may have filled the cache while we queued.
		if img, hit, failed := f.cacheGet(rawURL); hit {
			if failed {
				return nil, errAvatarUnavailable
			}
			return img, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), avatarFetchTimeout)
		defer cancel()
		img, ferr := f.doFetch(ctx, rawURL)
		if ferr != nil {
			// Log here, where the specific cause is still in hand and the
			// singleflight + negative cache naturally rate-limit it to ~once per
			// fetch attempt. A dial-time SSRF rejection is a probe signal (Warn) —
			// e.g. a private IP hand-inserted into users.avatar_url; a routine
			// broken avatar is just noise (Debug). Then negative-cache and return
			// the generic sentinel, so first-failure and cached-failure callers
			// see the same error.
			if errors.Is(ferr, errBlockedAvatarHost) {
				avatarLog.Warn("avatar fetch blocked by host policy", "url", rawURL, "error", ferr)
			} else {
				avatarLog.Debug("avatar fetch failed", "url", rawURL, "error", ferr)
			}
			f.cachePut(rawURL, nil) // negative-cache the failure
			return nil, errAvatarUnavailable
		}
		f.cachePut(rawURL, img)
		return img, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*fetchedAvatar), nil
}

// doFetch performs one upstream GET: https-only, size-capped, and content-type-
// validated. The SSRF IP guard lives on the client's dialer (so it covers
// redirect hops too); the scheme is checked here for the initial request.
func (f *avatarFetcher) doFetch(ctx context.Context, rawURL string) (*fetchedAvatar, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse avatar url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-https avatar url scheme %q", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build avatar request: %w", err)
	}
	req.Header.Set("Accept", "image/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch avatar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("avatar upstream status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAvatarBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read avatar body: %w", err)
	}
	if int64(len(data)) > maxAvatarBytes {
		return nil, fmt.Errorf("avatar exceeds %d bytes", int64(maxAvatarBytes))
	}

	ct := normalizeImageType(data)
	if ct == "" {
		return nil, fmt.Errorf("avatar is not an allowed image type")
	}
	return &fetchedAvatar{data: data, contentType: ct}, nil
}

// normalizeImageType resolves the content-type to serve by SNIFFING the bytes
// (http.DetectContentType) — deliberately NOT the upstream Content-Type header.
// We re-serve third-party content from our own origin, so a spoofed/incorrect
// header must not decide the type the browser sees; the bytes must actually be
// an allowed raster image. Returns the sniffed type when it's allowed, else ""
// (the caller rejects the fetch). The concrete type also matters because the
// global X-Content-Type-Options: nosniff makes our declared type authoritative.
func normalizeImageType(data []byte) string {
	mt, _, err := mime.ParseMediaType(http.DetectContentType(data))
	if err != nil || !allowedAvatarTypes[mt] {
		return ""
	}
	return mt
}

// avatarsHandler serves GET /api/avatars/{user_id}.
type avatarsHandler struct {
	tx      db.TxRunner
	fetcher *avatarFetcher
}

// handleAvatar proxies one user's OAuth avatar same-origin so it renders under
// the app's `img-src 'self'` CSP (TFAC-480).
//
// Gate: any authenticated org member (the session wrap + requireOrg). The target
// user's avatar_url is resolved on the APP pool under the caller's claims, so
// users_select RLS scopes visibility to the caller's own row + co-org-members —
// a cross-org {user_id} resolves to no row (empty avatar → 404), never a leak.
// An absent/empty avatar 404s too, so the frontend Avatar shows its monogram.
//
// GET /api/avatars/{user_id}
func (h *avatarsHandler) handleAvatar(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}

	// A malformed id is "not found" (parity with the other id-path handlers),
	// not a 500 from a uuid cast.
	targetID, ok := uuidPathOr404(w, r, "user_id", "avatar")
	if !ok {
		return
	}

	var avatarURL string
	if err := h.tx.WithTx(r.Context(), orgID, claims.Subject, func(tx db.TxStores) error {
		_, avatar, e := tx.Users.GetProfile(r.Context(), targetID)
		avatarURL = avatar
		return e
	}); err != nil {
		internalError(w, "avatars", err)
		return
	}
	if avatarURL == "" {
		// No avatar configured, or the target isn't visible under the caller's
		// org-scoped RLS. Either way the frontend falls back to a monogram.
		notFound(w, "avatar")
		return
	}

	img, err := h.fetcher.fetch(avatarURL)
	if err != nil {
		// Expected failure mode (unreachable host, non-image body, SSRF reject).
		// fetch() already logged the specific cause (Warn for an SSRF rejection,
		// Debug otherwise), rate-limited by its cache; the frontend Avatar's
		// onError falls back to a monogram, so just 502 here.
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
			Reason: httpx.ReasonUpstreamUnavailable, Message: "avatar unavailable",
		})
		return
	}

	w.Header().Set("Content-Type", img.contentType)
	// Explicit length so net/http sends a sized body rather than chunking a
	// multi-hundred-KB image.
	w.Header().Set("Content-Length", strconv.Itoa(len(img.data)))
	// Authz-gated, so private (never a shared/CDN cache). A short browser cache
	// keeps the 15s Usage poll + re-renders from re-hitting this endpoint.
	w.Header().Set("Cache-Control", "private, max-age=600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img.data)
}
