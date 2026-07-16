package systemllm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
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
// (see providerKey), so scorer/profiler/classifier batches racing at boot —
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
	c.failures++
	doublings := c.failures - 1
	if doublings > providerBreakerMaxDoublings {
		doublings = providerBreakerMaxDoublings
	}
	delay := providerBreakerBaseDelay * time.Duration(1<<uint(doublings))
	if delay > providerBreakerMaxDelay {
		delay = providerBreakerMaxDelay
	}
	c.until = time.Now().Add(delay)
}

// isTransientFailure reports whether err reflects an upstream condition
// worth backing off on: an overloaded/rate-limited/5xx API response
// (mirrors the SDK's own retry classifier, internal/requestconfig — by the
// time completeDirect sees callErr, the SDK has already exhausted its own
// retries for exactly this class), or a network-level failure that never
// got a response at all — the SDK's request executor returns the
// underlying http.Client.Do error verbatim when even its own retries never
// got a response, which net/http surfaces as a *url.Error wrapping the
// dial/DNS/TLS/timeout failure; that satisfies net.Error, and an
// unreachable endpoint looks identical to an overloaded one from the
// caller's side.
//
// Everything else does NOT trip the breaker: a caller-cancelled or
// deadline-exceeded ctx is not a provider signal; a 4xx client error (bad
// request, auth, not found) is a permanent misconfiguration no cooldown
// will fix; and an unrecognized local/SDK error (a request body that
// couldn't be replayed for retry, a malformed error response the SDK
// couldn't parse into apierror.Error, ...) is equally not something a
// cooldown fixes — bucketing every unclassified error as transient would
// silently downgrade a genuine, recurring bug to a quiet "retrying next
// cycle" log line instead of surfacing it.
func isTransientFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		sc := apiErr.StatusCode
		return sc == 408 || sc == 409 || sc == 429 || sc >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
