package agentloop

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// RetryPolicy bounds the same-provider-same-model retry. There is no
// fallback of any kind — not a different model, not a different provider,
// not a degraded request. A run that cannot reach the model it was given
// fails; silently substituting a different model would make the transcript a
// lie about what produced it, and the cost ledger a lie about what was
// bought.
type RetryPolicy struct {
	// MaxAttempts counts the first try. Zero uses defaultMaxAttempts.
	MaxAttempts int
	// BaseDelay is the first backoff; each retry doubles it. Zero uses
	// defaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps the doubling. Zero uses defaultMaxDelay.
	MaxDelay time.Duration
	// Sleep is the wait function, injectable so tests don't sleep. nil uses
	// a context-aware timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

const (
	defaultMaxAttempts = 5
	defaultBaseDelay   = time.Second
	defaultMaxDelay    = 30 * time.Second
)

// streamWithRetry makes one provider call, retrying only the classes that say
// the provider could not serve it right now (inference.Classify: a rate
// limit, a 5xx, a transport failure) with bounded exponential backoff.
// Exhaustion returns the last error; the caller fails the conversation.
//
// Every attempt is counted against orgID, and every retry decision beside
// it, so a provider outage shows in the upstream counters rather than only in
// the runs it fails.
//
// Each attempt is its own operation for Activity, so a run of retries never
// shares one deadline, and every chunk the stream delivers extends the
// attempt's deadline: a first byte has the provider bound, and so does each
// byte after the one before it.
func (e *Engine) streamWithRetry(ctx context.Context, orgID string, client Provider, req inference.Request) (*inference.Completion, error) {
	p := e.Retry
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultMaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaultBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = defaultMaxDelay
	}
	sleep := p.Sleep
	if sleep == nil {
		sleep = upstream.Sleep
	}

	if e.Activity != nil {
		bound := e.ActivityBounds.Provider
		req.OnChunk = func() { e.Activity.Progress(bound) }
	}

	name, named := inference.UpstreamName(req.Provider)
	delay := p.BaseDelay
	var lastErr error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		end := e.beginActivity("provider", e.ActivityBounds.Provider)
		completion, err := client.Stream(ctx, req)
		end()
		class, counted := inference.Classify(ctx, err)
		if counted && named {
			upstream.Record(ctx, name, orgID, class)
		}
		if err == nil {
			return completion, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if (class != upstream.Transient && class != upstream.RateLimited) || attempt == p.MaxAttempts {
			break
		}
		if named {
			upstream.RecordRetry(ctx, name, orgID, class)
		}
		e.warn("provider call failed; retrying same provider and model",
			"model", req.Model, "attempt", attempt, "delay", delay, "error", err)
		if serr := sleep(ctx, delay); serr != nil {
			return nil, serr
		}
		if delay *= 2; delay > p.MaxDelay {
			delay = p.MaxDelay
		}
	}
	return nil, lastErr
}
