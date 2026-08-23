package systemllm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// providerBreakerBaseDelay is the first cooldown a transient failure opens
// for a provider (see providerBreaker). Doubles per consecutive failure,
// capped at providerBreakerMaxDelay.
const providerBreakerBaseDelay = 2 * time.Second

// providerBreakerMaxDelay caps how long a provider stays in cooldown after
// repeated transient failures. Short relative to internal/github's
// rate-limit cap (5min): an Anthropic-side overload is typically a
// boot-time transient that clears in seconds, and every completeDirect
// caller already falls back to "retry next poll cycle" on any error, so a
// long cooldown just delays that existing fallback rather than protecting
// anything further.
const providerBreakerMaxDelay = 30 * time.Second

// providerBreakerMaxDoublings bounds the exponential ramp's shift so a long
// run of consecutive failures can't overflow it before the cap kicks in.
const providerBreakerMaxDoublings = 4

// ErrProviderBackoff is returned by completeDirect, without attempting a
// call, when a prior call to the same upstream provider (see providerKey)
// recently failed with a transient error and the cooldown window it opened
// hasn't elapsed yet.
//
// Every completeDirect caller already treats any returned error the same
// way — leave the task/repo/entity as-is so the next poll cycle retries —
// so this isn't a new failure mode callers need to handle, just a cheaper
// way to arrive at the existing one: instead of every concurrent batch
// independently rediscovering "the provider is still overloaded" via its
// own network round-trip (and the SDK's own retries), the first failure
// tells every other caller sharing that provider to skip straight to the
// fallback.
type ErrProviderBackoff struct {
	Provider string
	ResumeAt time.Time
}

func (e *ErrProviderBackoff) Error() string {
	return fmt.Sprintf("systemllm: provider %q in backoff, resume at %s", e.Provider, e.ResumeAt.Format(time.RFC3339))
}

// IsProviderBackoff reports whether err is (or wraps) an ErrProviderBackoff
// — callers use this to distinguish an anticipated, self-healing skip from
// a genuine failure worth logging loudly.
func IsProviderBackoff(err error) bool {
	var e *ErrProviderBackoff
	return errors.As(err, &e)
}

// providerBreaker tracks a transient-failure cooldown per upstream provider
// (see providerKey), so scorer/profiler batches racing at boot —
// or any time several land together — share one signal instead of each
// independently rediscovering an overloaded endpoint. One instance lives on
// the shared *Recorder (see NewRecorder), which is itself a single
// process-wide instance handed to all three jobs, so the state is naturally
// shared across every completeDirect call regardless of which job or org
// made it.
//
// In-memory only, mirroring internal/github's rateLimitRegistry: a restart
// loses it and the next failure repopulates it — acceptable for a soft
// signal that self-heals. A nil *providerBreaker is a safe no-op (never
// gates, never records), matching internal/syslimit's nil-is-unlimited
// contract, though in practice NewRecorder always constructs one.
type providerBreaker struct {
	mu    sync.Mutex
	state map[string]*providerCooldown
}

type providerCooldown struct {
	until    time.Time
	failures int
}

func newProviderBreaker() *providerBreaker {
	return &providerBreaker{state: make(map[string]*providerCooldown)}
}

// check returns a non-nil *ErrProviderBackoff if provider is currently in a
// cooldown window. It never itself makes or waits on a network call — a
// miss (no cooldown, or an expired one) falls straight through to the
// caller making a real request.
func (b *providerBreaker) check(provider string) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.state[provider]
	if !ok || !time.Now().Before(c.until) {
		return nil
	}
	return &ErrProviderBackoff{Provider: provider, ResumeAt: c.until}
}

// recordResult updates provider's cooldown state from the outcome of a call
// that was actually attempted (never called for a check-short-circuited
// call — there's nothing new to learn from a call that didn't happen).
// transientFailure opens or extends the cooldown with exponential backoff;
// any other outcome (success, or a permanent error like a bad request)
// clears it — holding a stale cooldown from an unrelated error would
// incorrectly gate later, likely-to-succeed calls.
func (b *providerBreaker) recordResult(provider string, transientFailure bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !transientFailure {
		delete(b.state, provider)
		return
	}
	c, ok := b.state[provider]
	if !ok {
		c = &providerCooldown{}
		b.state[provider] = c
	}
	// Clamp failures itself (not just the doublings derived from it) so an
	// extended outage doesn't leave an ever-growing counter behind — the
	// delay it produces is already capped, but an unbounded failures count
	// would be a misleading number for anyone who later logs or exposes it
	// directly for observability.
	if c.failures < providerBreakerMaxDoublings+1 {
		c.failures++
	}
	delay := providerBreakerBaseDelay * time.Duration(1<<uint(c.failures-1))
	if delay > providerBreakerMaxDelay {
		delay = providerBreakerMaxDelay
	}
	c.until = time.Now().Add(delay)
}

// isTransientFailure reports whether err reflects an upstream condition
// worth backing off on: an overloaded/rate-limited/5xx API response, or a
// network-level failure that never got a response at all — an unreachable
// endpoint looks identical to an overloaded one from the caller's side.
//
// Everything else does NOT trip the breaker: a caller-cancelled or
// deadline-exceeded ctx is not a provider signal; a 4xx client error (bad
// request, auth, not found) is a permanent misconfiguration no cooldown
// will fix; a context overflow is a deterministic rejection of this exact
// prompt and says nothing about provider health; and an unrecognized error
// is equally not something a cooldown fixes — bucketing every unclassified
// error as transient would silently downgrade a genuine, recurring bug to a
// quiet "retrying next cycle" log line instead of surfacing it.
//
// Classification is on the error text because that is the shape the neutral
// layer produces: internal/inference flattens a provider failure into a
// message, deliberately rendering the status code as "(HTTP %d)" and the
// wrapped cause alongside it, precisely so a caller can sort transient from
// permanent. Nothing structured survives to match on — the transport error
// underneath is rendered, not wrapped — which is also why the whole cooldown
// matters more than it used to: bifrost's default retry count is zero, so
// the first failure here is the first failure, not the tail of a retry
// budget already spent inside a client.
func isTransientFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, inference.ErrContextOverflow) {
		return false
	}
	// A rendered status is the authoritative signal and settles the question
	// either way: an error carrying one is classified on it alone, so a 400
	// whose body happens to quote "connection reset" stays permanent.
	if status, ok := inference.RenderedStatus(err); ok {
		return status == 408 || status == 409 || status == 429 || status >= 500
	}
	msg := strings.ToLower(err.Error())
	for _, m := range transportFailureMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// transportFailureMarkers classify a failure that never reached the provider
// (dial, DNS, TLS, timeout, a reset mid-stream) and so carries no status to
// render. Deliberately narrower than the retry classifier in
// internal/agentloop: that one decides whether to try again immediately, this
// one opens a cooldown that gates every other org sharing the endpoint, so an
// ambiguous match costs more here than a missed one.
var transportFailureMarkers = []string{
	"connection refused",
	"connection reset",
	"no such host",
	"i/o timeout",
	"tls handshake timeout",
	"timeout awaiting response",
	"context deadline exceeded",
	"network is unreachable",
	"no route to host",
	"broken pipe",
}
