package agentloop

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
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

// streamWithRetry makes one provider call, retrying only transient classes
// (rate limits, 5xx, network timeouts) with bounded exponential backoff.
// Exhaustion returns the last error; the caller fails the conversation.
func (e *Engine) streamWithRetry(ctx context.Context, client Provider, req inference.Request) (*inference.Completion, error) {
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
		sleep = sleepCtx
	}

	delay := p.BaseDelay
	var lastErr error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		completion, err := client.Stream(ctx, req)
		if err == nil {
			return completion, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isTransient(err) || attempt == p.MaxAttempts {
			break
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

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// transientMarkers are the substrings that classify a provider error as
// worth retrying. The neutral layer flattens provider errors to a message
// string, so this is substring classification rather than status-code
// inspection — deliberately conservative: an unclassified error is treated
// as permanent and fails the run rather than burning the retry budget on
// something that will never succeed (a bad key, an unknown model).
var transientMarkers = []string{
	"rate limit",
	"rate_limit",
	"overloaded",
	"internal server error",
	"bad gateway",
	"service unavailable",
	"gateway timeout",
	"timeout",
	"timed out",
	"connection reset",
	"connection refused",
}

// eofPattern matches a standalone "eof" token (io.EOF renders as "EOF",
// io.ErrUnexpectedEOF as "unexpected EOF") without matching it embedded in a
// longer run of characters — a base64 blob or a path fragment that happens to
// contain "eof" has word characters on at least one side, so \b excludes it.
// Kept out of transientMarkers because that list is plain substring
// matching; this one needs the boundary check.
var eofPattern = regexp.MustCompile(`\beof\b`)

// numericTransientMarkers are the retryable HTTP status codes, matched as
// standalone numeric tokens rather than raw substrings — the same
// embedded-token hazard the eof pattern guards against, in digit form: a
// request or trace id in a permanent error's text can coincidentally
// contain "500", and a spurious retry on a permanent error burns the whole
// backoff budget on something that will never succeed.
var numericTransientMarkers = []string{"429", "500", "502", "503", "504"}

func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	if eofPattern.MatchString(msg) {
		return true
	}
	for _, m := range transientMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	for _, code := range numericTransientMarkers {
		if containsNumericToken(msg, code) {
			return true
		}
	}
	return false
}

// containsNumericToken reports whether code occurs in msg with no letter or
// digit on either side — "HTTP 500" and "(500)" match, "req_a5003b" and
// "15000ms" do not.
func containsNumericToken(msg, code string) bool {
	for i := 0; ; {
		j := strings.Index(msg[i:], code)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(code)
		beforeOK := j == 0 || !isAlnumByte(msg[j-1])
		afterOK := end >= len(msg) || !isAlnumByte(msg[end])
		if beforeOK && afterOK {
			return true
		}
		i = j + 1
	}
}

func isAlnumByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
