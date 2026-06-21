package server

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
// Single-tier on purpose: the ticket's locked proposal is one wrapper
// over the whole allowlist. Per-route tiers (a tighter cap on discovery,
// a looser one on the login-retry paths) and a shared/distributed store
// for multi-replica deployments are noted there as open questions —
// deferred, not built here.
const (
	// preAuthRatePerSec is the sustained per-IP token refill (1/sec =
	// 60/min) once the burst is spent.
	preAuthRatePerSec = 1.0
	// preAuthBurst is the bucket capacity — the largest instantaneous
	// flurry from one IP that passes before the sustained rate bites.
	preAuthBurst = 20.0
	// preAuthBucketTTL bounds how long an idle per-IP bucket is retained.
	// A flood of distinct source IPs is the workload that grows the map,
	// and it is also what drives the opportunistic sweep that reclaims it,
	// so the live set stays roughly proportional to recently-active IPs.
	preAuthBucketTTL = 10 * time.Minute
)

// ipRateLimiter is an in-process, per-client-IP token-bucket limiter. One
// bucket per IP refills at a fixed rate up to a burst ceiling; each
// allowed request spends one token. State is per-instance (per process) —
// the simplest first cut that blunts a single-source flood. A
// shared/distributed store (Redis, etc.) is only needed once TF runs
// multiple replicas behind a balancer, and is deliberately deferred.
//
// Keyed on the value clientIP returns, which prefers the proxy-forwarded
// header (X-Forwarded-For). That makes the cap only as trustworthy as the
// deployment's proxy: a caller able to spoof X-Forwarded-For can rotate
// the key and slip the limiter. The hosted deployment terminates at a
// trusted proxy that sets the header, and the edge/WAF layer (out of
// scope) is the spoofing-resistant complement.
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
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b := l.buckets[ip]
	if b == nil {
		// First request from this IP: a full bucket, less the token this
		// request spends.
		l.buckets[ip] = &ipBucket{tokens: l.burst - 1, last: now, seen: now}
		return true, 0
	}

	// Refill for the elapsed interval, capped at burst. A non-monotonic
	// clock could yield a negative delta; guard against it draining nothing
	// rather than adding.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	b.seen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Not enough for a whole token: hand back the wait until one accrues.
	// Guard a non-positive rate (a misconfigured limiter with no refill) so
	// the division can't panic — production passes preAuthRatePerSec=1, but
	// keep allow() total for any caller. With no refill a spent bucket never
	// recovers, so report a fixed back-off rather than +Inf. Placed here, not
	// at the top, so a zero-rate limiter still honors its burst first.
	if l.rate <= 0 {
		return false, time.Second
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.preAuthLimiter == nil || runmode.Current() == runmode.ModeLocal {
			next.ServeHTTP(w, r)
			return
		}
		if ok, retryAfter := s.preAuthLimiter.allow(clientIP(r)); !ok {
			// Retry-After is delay-seconds (RFC 7231). Round up so a
			// sub-second wait still tells the caller to back off, and floor
			// at 1 — a 0 would invite an instant retry that is still capped.
			secs := int(math.Ceil(retryAfter.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
