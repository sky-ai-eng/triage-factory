package upstream

import (
	"context"
	"sync"
)

// failFastKey is the context key of a fail-fast scope.
type failFastKey struct{}

// failFast is the set of hosts a scope has given up retrying against. It is
// safe for concurrent use, because a poll cycle fans its requests out across
// goroutines.
type failFast struct {
	mu   sync.Mutex
	down map[string]bool
}

// WithFailFast returns a context that opens a fail-fast scope. Under it, once
// one request to a host has ended in a transient failure, after whatever
// retries its client allowed, every later request to that host gets a single
// attempt: no retry, no backoff and no Retry-After wait.
//
// It is for a caller that will try again on its own schedule and has already
// learned what it needs from the first failure: one org's poll cycle, which
// records the connection as lost from that failure and polls again next
// cycle. Without it, every request in the cycle waits out the whole retry
// ladder against a host that is not answering, so a cycle over many repos
// takes one ladder per repo and delays every org polled after it. A caller
// whose request is the work itself (an agent's exec verb) does not open a
// scope, and keeps every retry.
//
// The scope is per host, so it never stops retries against a host that has
// not failed. A scope already on ctx is replaced for the derived context.
func WithFailFast(ctx context.Context) context.Context {
	return context.WithValue(ctx, failFastKey{}, &failFast{down: map[string]bool{}})
}

func failFastFrom(ctx context.Context) *failFast {
	if ctx == nil {
		return nil
	}
	f, _ := ctx.Value(failFastKey{}).(*failFast)
	return f
}

// MarkUnreachable records that a request to host ended in a transient
// failure and will not be retried. It does nothing outside a fail-fast scope.
func MarkUnreachable(ctx context.Context, host string) {
	f := failFastFrom(ctx)
	if f == nil {
		return
	}
	f.mu.Lock()
	f.down[host] = true
	f.mu.Unlock()
}

// Unreachable reports whether ctx is a fail-fast scope in which a request to
// host has already ended in a transient failure, so a client sends its
// request to host once and does not retry it.
func Unreachable(ctx context.Context, host string) bool {
	f := failFastFrom(ctx)
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.down[host]
}
