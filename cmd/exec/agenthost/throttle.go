package agenthost

import (
	"context"
	"os"
	"strconv"
	"strings"
)

// envMaxUpstreamConcurrency overrides the per-upstream in-flight cap. A value
// <= 0 disables the gate entirely.
const envMaxUpstreamConcurrency = "TF_AGENTHOST_MAX_UPSTREAM_CONCURRENCY"

// defaultMaxUpstreamConcurrency bounds how many upstream (Jira / GitHub) REST
// calls one run's agent may have in flight at once, per upstream.
//
// Every delegated run has its own agenthost daemon (one socket per run is the
// isolation boundary), so this is a per-run governor: it caps a single agent's
// parallel tool-call fan-out against the org bot identity all runs share. It is
// deliberately generous — normal agent parallelism stays well under it, so it
// bounds a pathological burst rather than throttling ordinary work. The
// load-bearing protection against fleet-wide pressure on the shared per-account
// rate limit is the jira/github clients' 429/Retry-After backoff, which every
// run applies independently; this cap just keeps any one run from opening an
// unbounded number of simultaneous connections to the upstream.
const defaultMaxUpstreamConcurrency = 8

// upstreamKind identifies which shared upstream a dispatch method calls, for
// concurrency accounting. Jira and GitHub are gated separately so a burst
// against one can't starve calls to the other (independent accounts, limits).
type upstreamKind int

const (
	upstreamNone upstreamKind = iota
	upstreamJira
	upstreamGitHub
)

// upstreamForMethod classifies a dispatch method by the upstream it hits. The
// method constants are namespaced by prefix ("Jira*" / "Github*"), so a new
// verb is classified automatically; every other method (DB/core ops, git
// checkout via the git proxy) is upstreamNone and never gated here.
func upstreamForMethod(method string) upstreamKind {
	switch {
	case strings.HasPrefix(method, "Jira"):
		return upstreamJira
	case strings.HasPrefix(method, "Github"):
		return upstreamGitHub
	default:
		return upstreamNone
	}
}

// upstreamThrottle is a per-upstream concurrency gate shared by every
// connection one Server handles. A nil *upstreamThrottle, or one built with a
// non-positive limit, is a no-op — acquire returns instantly with a no-op
// release — so a Server constructed without a gate (a bare test fixture, or an
// operator who set the limit to 0) simply doesn't throttle.
type upstreamThrottle struct {
	jira   chan struct{}
	github chan struct{}
}

func newUpstreamThrottle(limit int) *upstreamThrottle {
	if limit <= 0 {
		return &upstreamThrottle{}
	}
	return &upstreamThrottle{
		jira:   make(chan struct{}, limit),
		github: make(chan struct{}, limit),
	}
}

func (t *upstreamThrottle) slots(up upstreamKind) chan struct{} {
	if t == nil {
		return nil
	}
	switch up {
	case upstreamJira:
		return t.jira
	case upstreamGitHub:
		return t.github
	default:
		return nil
	}
}

// acquire blocks until a slot for up is free or ctx is done, whichever first.
// It returns a release func that frees the slot (safe to call exactly once,
// e.g. via defer). For upstreamNone, a nil gate, or a disabled gate it acquires
// instantly and release is a no-op. When ctx is done first it returns ctx.Err()
// and a nil release, so the dispatch surfaces the deadline instead of running
// the upstream call — the graceful-degradation path under sustained pressure.
func (t *upstreamThrottle) acquire(ctx context.Context, up upstreamKind) (func(), error) {
	ch := t.slots(up)
	if ch == nil {
		return func() {}, nil
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// maxUpstreamConcurrencyFromEnv resolves the per-upstream cap from the
// environment, falling back to the default when unset and warning (then falling
// back) on an unparsable value. A parsed value <= 0 is returned as-is and
// disables the gate.
func maxUpstreamConcurrencyFromEnv() int {
	raw := strings.TrimSpace(os.Getenv(envMaxUpstreamConcurrency))
	if raw == "" {
		return defaultMaxUpstreamConcurrency
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		agenthostLog.Warn("invalid "+envMaxUpstreamConcurrency+", using default",
			"value", raw, "default", defaultMaxUpstreamConcurrency)
		return defaultMaxUpstreamConcurrency
	}
	return n
}
