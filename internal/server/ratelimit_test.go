package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// frozenLimiter builds an ipRateLimiter whose clock is pinned to *clk, so
// refill, refusal, and eviction are all deterministic. Callers advance time
// by reassigning *clk between allow() calls.
func frozenLimiter(rate, burst float64, ttl time.Duration, clk *time.Time) *ipRateLimiter {
	l := newIPRateLimiter(rate, burst, ttl)
	l.now = func() time.Time { return *clk }
	return l
}

// TestIPRateLimiter_BurstThenRefill exercises the core token-bucket
// behavior: a full bucket allows exactly `burst` requests, the next is
// refused with a positive wait, and time refills tokens up to the cap.
func TestIPRateLimiter_BurstThenRefill(t *testing.T) {
	clk := time.Now()
	l := frozenLimiter(1.0, 3.0, time.Minute, &clk) // 3-burst, 1 token/sec

	// A full bucket grants `burst` requests back to back.
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d refused, want allowed within burst", i+1)
		}
	}

	// Burst spent: the next request is refused with a wait of one refill
	// interval (1 token/sec → ~1s).
	ok, wait := l.allow("1.2.3.4")
	if ok {
		t.Fatalf("4th request allowed, want refused (burst spent)")
	}
	if wait <= 0 || wait > time.Second {
		t.Fatalf("retry wait = %v, want (0, 1s] for a 1/sec refill", wait)
	}

	// Advance 2s → 2 tokens accrue → exactly 2 more allowed, then refused.
	clk = clk.Add(2 * time.Second)
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("post-refill request %d refused, want allowed", i+1)
		}
	}
	if ok, _ := l.allow("1.2.3.4"); ok {
		t.Fatalf("request allowed after 2 refilled tokens spent, want refused")
	}

	// A long idle never banks more than `burst` tokens.
	clk = clk.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("post-idle request %d refused, want allowed up to burst", i+1)
		}
	}
	if ok, _ := l.allow("1.2.3.4"); ok {
		t.Fatalf("request allowed beyond burst cap after long idle, want refused")
	}
}

// TestIPRateLimiter_PerIPIndependent confirms each IP gets its own bucket:
// exhausting one IP's budget leaves another's untouched.
func TestIPRateLimiter_PerIPIndependent(t *testing.T) {
	clk := time.Now()
	l := frozenLimiter(1.0, 2.0, time.Minute, &clk)

	// Drain IP a entirely.
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("10.0.0.1"); !ok {
			t.Fatalf("a request %d refused, want allowed", i+1)
		}
	}
	if ok, _ := l.allow("10.0.0.1"); ok {
		t.Fatalf("a over budget but allowed, want refused")
	}

	// IP b still has its full burst.
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("10.0.0.2"); !ok {
			t.Fatalf("b request %d refused, want b's own bucket untouched", i+1)
		}
	}
}

// TestIPRateLimiter_SweepEvictsIdle proves the idle-bucket sweep bounds the
// map: buckets untouched for longer than the TTL are reclaimed on a later
// allow().
func TestIPRateLimiter_SweepEvictsIdle(t *testing.T) {
	clk := time.Now()
	l := frozenLimiter(1.0, 5.0, time.Minute, &clk)

	l.allow("1.1.1.1") // bucket created at t0
	clk = clk.Add(30 * time.Second)
	l.allow("2.2.2.2") // created at t0+30s; no sweep yet (under TTL window)

	l.mu.Lock()
	if got := len(l.buckets); got != 2 {
		l.mu.Unlock()
		t.Fatalf("bucket count = %d, want 2 before any eviction", got)
	}
	l.mu.Unlock()

	// Jump well past the TTL for both prior IPs and touch a third. The sweep
	// fires (last sweep was > TTL ago) and reclaims the two idle buckets,
	// leaving only the freshly-created one.
	clk = clk.Add(5 * time.Minute)
	l.allow("3.3.3.3")

	l.mu.Lock()
	defer l.mu.Unlock()
	if got := len(l.buckets); got != 1 {
		t.Fatalf("bucket count = %d after sweep, want 1 (only the live IP)", got)
	}
	if _, ok := l.buckets["3.3.3.3"]; !ok {
		t.Fatalf("the live IP's bucket was swept, want it retained")
	}
}

// TestPreAuthRateLimit_LocalModeNoOp asserts the wrapper never throttles in
// local mode (N=1): far more than a burst's worth of requests all pass.
func TestPreAuthRateLimit_LocalModeNoOp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	clk := time.Now()
	s := &Server{preAuthLimiter: frozenLimiter(1.0, 3.0, time.Minute, &clk)}
	h := s.preAuthRateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/sso/discover", nil)
		req.Header.Set("X-Forwarded-For", "9.9.9.9")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("local-mode request %d got %d, want 200 (no limiting at N=1)", i+1, rec.Code)
		}
	}
}

// TestPreAuthRateLimit_MultiMode429 asserts that in multi mode the wrapper
// allows a burst, then returns 429 with a Retry-After header, and that the
// cap is keyed per IP (a different client is unaffected).
func TestPreAuthRateLimit_MultiMode429(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	clk := time.Now()
	s := &Server{preAuthLimiter: frozenLimiter(1.0, 3.0, time.Minute, &clk)}
	h := s.preAuthRateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	fire := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/sso/discover", nil)
		req.Header.Set("X-Forwarded-For", ip)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// First three (the burst) pass.
	for i := 0; i < 3; i++ {
		if rec := fire("203.0.113.7"); rec.Code != http.StatusOK {
			t.Fatalf("burst request %d got %d, want 200", i+1, rec.Code)
		}
	}

	// Fourth is throttled with a usable Retry-After.
	rec := fire("203.0.113.7")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap request got %d, want 429", rec.Code)
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatalf("429 missing Retry-After header")
	}
	if secs, err := strconv.Atoi(ra); err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q, want a positive integer seconds value", ra)
	}

	// A different IP still has its own full budget — the cap is per client.
	if rec := fire("198.51.100.4"); rec.Code != http.StatusOK {
		t.Fatalf("distinct IP got %d, want 200 (independent bucket)", rec.Code)
	}
}

// TestIPRateLimiter_ZeroRateNoPanic guards the division in allow(): a
// limiter configured with a non-positive refill rate must still honor its
// burst and then refuse with a fixed back-off rather than dividing by zero.
func TestIPRateLimiter_ZeroRateNoPanic(t *testing.T) {
	clk := time.Now()
	l := frozenLimiter(0.0, 2.0, time.Minute, &clk) // no refill, burst 2

	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d refused, want allowed within burst even at rate 0", i+1)
		}
	}
	ok, wait := l.allow("1.2.3.4")
	if ok {
		t.Fatalf("3rd request allowed, want refused (burst spent, no refill)")
	}
	if wait <= 0 {
		t.Fatalf("retry wait = %v, want a positive fixed back-off (no divide-by-zero)", wait)
	}
}

// TestPreAuthRateLimit_LogoutCSRFRejectionCostsNoTokens locks in the wrap
// ORDER for the logout mount: withCSRFOriginCheck (outer) runs before
// preAuthRateLimit (inner), so a cross-origin POST the CSRF gate rejects
// (403) never reaches the limiter and consumes no tokens. Without this
// ordering a malicious page could CSRF-spam logout from a victim's browser
// and drain that IP's shared pre-auth budget — starving its login /
// discovery — with requests that never pass CSRF anyway.
func TestPreAuthRateLimit_LogoutCSRFRejectionCostsNoTokens(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	clk := time.Now()
	s := &Server{preAuthLimiter: frozenLimiter(1.0, 3.0, time.Minute, &clk)}
	s.SetDeployConfig("https://tf.test", [32]byte{})

	// The exact composition the logout route mounts: CSRF outer, limiter inner.
	h := s.withCSRFOriginCheck(s.preAuthRateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	post := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// A flood of cross-origin POSTs — many more than the burst — all 403 at
	// the CSRF gate and must NOT touch the limiter.
	for i := 0; i < 25; i++ {
		if rec := post("https://evil.example"); rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin POST %d got %d, want 403", i+1, rec.Code)
		}
	}

	// The full burst is therefore still available to the legitimate
	// same-origin caller — proof the rejected spam consumed nothing.
	for i := 0; i < 3; i++ {
		if rec := post("https://tf.test"); rec.Code != http.StatusOK {
			t.Fatalf("same-origin POST %d got %d, want 200 (budget drained by rejected CSRF spam?)", i+1, rec.Code)
		}
	}
	// And the cap still bites once the legit burst is genuinely spent.
	if rec := post("https://tf.test"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-burst same-origin POST got %d, want 429", rec.Code)
	}
}

// TestPreAuthRateLimit_NilLimiterNoOp guards the defensive nil branch: a
// Server without a limiter wired (no construction path does this today, but
// the wrapper must not panic) passes requests through even in multi mode.
func TestPreAuthRateLimit_NilLimiterNoOp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	s := &Server{} // preAuthLimiter nil
	h := s.preAuthRateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil-limiter request got %d, want 200 (pass-through)", rec.Code)
	}
}
