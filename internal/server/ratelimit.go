package server

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Pre-auth allowlist rate-limit tuning (TFAC-433). One shared per-IP
// budget governs every pre-auth raw mount the limiter wraps (discovery,
// oauth start, logout, invite preview — the OAuth callback is excluded;
// see routes()). The numbers target the
// threat the ticket was filed for — systematic domain reconnaissance via
// thousands of cheap POST /api/sso/discover probes — while staying well
// above any real interactive use:
//
//   - burst tolerates a short flurry (a human correcting a typed email,
//     a login retried a couple of times, several users behind one office
//     NAT arriving together) without a 429.
//   - the sustained rate is what an attacker is left with once the burst
//     is spent: ~60 requests/min/IP turns "thousands of instant probes"
//     into a slow, conspicuous trickle. It does not make enumeration
//     impossible — the edge/WAF layer (explicitly out of scope here) is
//     the airtight tier; this blunts a single-source flood.
//
// Single-tier on purpose for the human-login allowlist: the ticket's locked
// proposal was one wrapper over the whole allowlist. Per-route tiers within
// that allowlist (a tighter cap on discovery, a looser one on the
// login-retry paths) and a shared/distributed store for multi-replica
// deployments are noted there as open questions — deferred, not built here.
// A signed, self-authenticating webhook is a genuinely different shape of
// route rather than a variant of the allowlist, so it gets its own tier
// below instead of a subdivision of this one.
const (
	// preAuthRatePerSec is the sustained per-IP token refill (1/sec =
	// 60/min) once the burst is spent.
	preAuthRatePerSec = 1.0
	// preAuthBurst is the bucket capacity — the largest instantaneous
	// flurry from one IP that passes before the sustained rate bites.
	preAuthBurst = 20.0
)

// Signed-webhook pre-auth rate-limit tuning. A route in this tier
// authenticates every request itself via a cryptographic signature (Slack's
// Events API receiver and the per-org GitHub App receiver) before any side
// effect, so the anti-recon rationale behind the tight human-login tier above
// doesn't apply — a forged request fails signature verification regardless of
// how generous this budget is. What it does bound is the work spent GETTING to
// that rejection: the GitHub receiver has to resolve the org's webhook secret
// (settings row, registration row, vault) before it can check a signature at
// all, which is why a cheap per-IP cap in front of it is worth having even
// though nothing forged gets past it.
//
// The numbers, though, are sized by the opposite failure mode — the one where
// the budget is too TIGHT: Slack's own delivery cap is 30k events/hr per
// (app, workspace) ≈ 8.3 req/s average, well past the 1 req/s human tier, so
// sharing that tier would 429 legitimate deliveries from a subscribed
// workspace — Slack then retries (amplifying the flood it's already
// causing), the failure ratio stays high, and Slack eventually disables the
// app's event subscription outright, taking ingest down with it. The
// numbers here are sized to clear that ceiling with real headroom (several
// apps/workspaces potentially sharing one of Slack's small set of static
// egress IPs) rather than to resist recon — cheap pre-verification
// rejection at this rate is the actual DoS defense on this route. GitHub's
// deliveries clear it by a wide margin too: a busy org's App fans out from
// GitHub's hook egress at a small fraction of this rate, and a redelivery
// backlog lands inside the burst.
const (
	// signedWebhookRatePerSec is the sustained per-IP token refill. Well
	// above Slack's ~8.3 req/s per-(app,workspace) average so a legitimate
	// subscription is never throttled into looking like a failing endpoint.
	signedWebhookRatePerSec = 20.0
	// signedWebhookBurst is deliberately generous — five seconds of
	// sustained-rate traffic — to absorb a delivery flurry (a busy channel,
	// or Slack redelivering a backlog after a brief outage) without a 429.
	signedWebhookBurst = 100.0
)

// API-token authentication-failure tuning. This tier meters something the two
// above do not: not requests, but FAILURES. A caller presenting a valid token
// never touches the bucket however fast it calls — the routes it reaches are
// authenticated, and capping them would be a throughput limit on legitimate
// automation, which is not what this is for. What it bounds is guessing: a
// token is 32 random bytes, so nobody is finding one by search, but a bucket in
// front of the lookup keeps a flood of forged Bearers from spending a database
// round-trip each on the way to their 401.
//
// The human-login numbers are right for that, and for the same reason: twenty
// consecutive failures from one address is already past any explicable
// misconfiguration (a stale token in a deploy, a typo pasted into a CI secret),
// and one per second after that leaves a broken client retrying without ever
// looking like a search. It is a separate limiter from the pre-auth tier rather
// than the same one because the populations are unrelated — a shared budget
// would let a login flood lock out an org's automation, and vice versa.
const (
	// tokenAuthFailureRatePerSec is the sustained per-IP refill of the
	// failure budget.
	tokenAuthFailureRatePerSec = 1.0
	// tokenAuthFailureBurst is how many failures one address may spend
	// before the sustained rate bites.
	tokenAuthFailureBurst = 20.0
)

// rateLimitBucketTTL bounds how long an idle per-IP bucket is retained,
// shared by every tier above. A flood of distinct source IPs is the
// workload that grows the map, and it is also what drives the opportunistic
// sweep that reclaims it, so the live set stays roughly proportional to
// recently-active IPs.
const rateLimitBucketTTL = 10 * time.Minute

// ipRateLimiter is an in-process, per-client-IP token-bucket limiter. One
// bucket per IP refills at a fixed rate up to a burst ceiling; each
// allowed request spends one token. State is per-instance (per process) —
// the simplest first cut that blunts a single-source flood. A
// shared/distributed store (Redis, etc.) is only needed once TF runs
// multiple replicas behind a balancer, and is deliberately deferred.
//
// Keyed on the value clientIP returns. Since TFAC-488 that is non-spoofable
// by construction: X-Forwarded-For is honored only when the direct peer is a
// configured trusted proxy (TF_TRUSTED_PROXY_CIDR), via a right→left walk;
// otherwise the cap keys on the real peer (RemoteAddr). The one caveat is a
// *proxied-but-unconfigured* deployment — XFF ignored, every request keyed on
// the LB IP — which collapses this to a single global bucket; the multi-mode
// startup warning flags exactly that. With capture disabled
// (TF_CAPTURE_CLIENT_IP=false) clientIP returns "" and the limiter degrades to
// one global bucket by design. The edge/WAF layer (out of scope) remains the
// spoofing-resistant complement.
type ipRateLimiter struct {
	rate  float64       // tokens added per second (sustained ceiling)
	burst float64       // bucket capacity (max instantaneous flurry)
	ttl   time.Duration // idle buckets older than this are swept

	// now is the clock seam — time.Now in production, stubbed in tests so
	// refill, refusal, and eviction are all deterministic.
	now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*ipBucket
	lastSweep time.Time
}

// ipBucket is one IP's token state. last marks the most recent refill
// instant; seen marks the most recent request, used for TTL eviction.
type ipBucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

// newIPRateLimiter builds a limiter with the given sustained rate (tokens
// per second), burst capacity, and idle-bucket TTL.
func newIPRateLimiter(ratePerSec, burst float64, ttl time.Duration) *ipRateLimiter {
	return &ipRateLimiter{
		rate:    ratePerSec,
		burst:   burst,
		ttl:     ttl,
		now:     time.Now,
		buckets: make(map[string]*ipBucket),
	}
}

// allow reports whether a request from ip may proceed, consuming one
// token when it does. On refusal the second return value is the time
// until the next token accrues, for the Retry-After header.
func (l *ipRateLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refillLocked(ip)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, l.waitLocked(b)
}

// peek answers the same question as allow without spending anything — the
// check half of a budget the caller charges separately, when what the budget
// meters is the OUTCOME of the work rather than the request that asked for it.
// The API-token path is that shape: a valid credential must cost its holder
// nothing, so the bucket is consulted before the lookup and charged only once
// the lookup has failed.
func (l *ipRateLimiter) peek(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refillLocked(ip)
	if b.tokens >= 1 {
		return true, 0
	}
	return false, l.waitLocked(b)
}

// charge spends one token for ip, peek's other half. It reports nothing: the
// caller has already decided to refuse this request, and whether the bucket ran
// dry on THIS charge or the previous one only changes what the NEXT request is
// told. The floor at zero is what keeps that true — without it a flood would
// bank a negative balance the IP has to refill through before its first honest
// request is served again.
func (l *ipRateLimiter) charge(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refillLocked(ip)
	if b.tokens = b.tokens - 1; b.tokens < 0 {
		b.tokens = 0
	}
}

// refillLocked returns ip's bucket, creating it full on first sight and
// otherwise crediting the tokens that accrued since its last touch. It stamps
// seen, so every caller (spending or not) keeps the bucket off the sweep.
// Assumes l.mu is held.
func (l *ipRateLimiter) refillLocked(ip string) *ipBucket {
	now := l.now()
	l.sweepLocked(now)

	b := l.buckets[ip]
	if b == nil {
		b = &ipBucket{tokens: l.burst, last: now, seen: now}
		l.buckets[ip] = b
		return b
	}

	// Refill for the elapsed interval, capped at burst. A non-monotonic
	// clock could yield a negative delta; guard against it draining nothing
	// rather than adding.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	b.seen = now
	return b
}

// waitLocked is how long until b holds a whole token, for the Retry-After
// header on a refusal. Guards a non-positive rate (a misconfigured limiter with
// no refill) so the division can't panic — production passes a positive rate,
// but keep the refusal path total for any caller. With no refill a spent bucket
// never recovers, so report a fixed back-off rather than +Inf. Assumes l.mu is
// held; the receiver is only for l.rate.
func (l *ipRateLimiter) waitLocked(b *ipBucket) time.Duration {
	if l.rate <= 0 {
		return time.Second
	}
	return time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// sweepLocked evicts buckets idle longer than ttl so the map can't grow
// without bound under a flood of distinct source IPs. It runs at most once
// per ttl window (cheap on the common path) and assumes l.mu is held.
func (l *ipRateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < l.ttl {
		return
	}
	l.lastSweep = now
	for ip, b := range l.buckets {
		if now.Sub(b.seen) >= l.ttl {
			delete(l.buckets, ip)
		}
	}
}

// preAuthRateLimit caps per-IP request rate on the pre-auth allowlist
// routes it wraps (TFAC-433). These run before any session exists, so the
// implicit "needs a valid session" bound the s.api/apiMutating routes get
// for free does not apply — without this, an anonymous caller can flood
// them. Returns 429 with a Retry-After header once the per-IP bucket is
// empty.
//
// No-op in local mode: a local install is N=1 (one user behind the
// binary), so there is nothing to rate-limit and a cap would only risk
// throttling that one user. The mode is read at request time, mirroring
// withSession — mode is fixed before routes() runs, but the request-time
// check keeps the wrapper correct regardless of wiring order and lets
// tests flip mode per case.
func (s *Server) preAuthRateLimit(next http.Handler) http.Handler {
	return rateLimitWith(s.preAuthLimiter, next)
}

// signedWebhookRateLimit caps per-IP request rate on pre-auth mounts that
// authenticate every request themselves via a cryptographic signature
// before any side effect (the Slack Events API receiver and the per-org
// GitHub App receiver; see signedWebhookRatePerSec above for why this is a
// separate, much higher-throughput tier than preAuthRateLimit's human-login
// budget).
// Same no-op-in-local-mode and 429/Retry-After contract as preAuthRateLimit.
func (s *Server) signedWebhookRateLimit(next http.Handler) http.Handler {
	return rateLimitWith(s.signedWebhookLimiter, next)
}

// rateLimitWith is the shared wrapper body behind preAuthRateLimit and
// signedWebhookRateLimit — same bucket-check, 429, and Retry-After
// mechanics; the two callers differ only in which limiter (and therefore
// which tier's rate/burst) they consult.
func rateLimitWith(limiter *ipRateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limiter == nil || runmode.Current() == runmode.ModeLocal {
			next.ServeHTTP(w, r)
			return
		}
		if ok, retryAfter := limiter.allow(clientIP(r)); !ok {
			writeRateLimited(w, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeRateLimited renders the one 429 every tier speaks, so a caller that
// meters a route and one that meters an outcome (the API-token failure budget,
// which has no wrapper of its own) are indistinguishable to the client.
//
// Retry-After is delay-seconds (RFC 7231). Round up so a sub-second wait still
// tells the caller to back off, and floor at 1 — a 0 would invite an instant
// retry that is still capped.
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	httpx.WriteErrors(w, http.StatusTooManyRequests, httpx.ErrorItem{
		Reason: httpx.ReasonRateLimited, Message: "rate limit exceeded",
	})
}
