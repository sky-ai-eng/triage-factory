package upstream

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// failFastKey is the context key of a fail-fast scope.
type failFastKey struct{}

// failFast is the set of hosts a scope has given up retrying against, and the
// subset it has stopped sending to. It is safe for concurrent use, because a
// poll cycle fans its requests out across goroutines.
type failFast struct {
	mu   sync.Mutex
	down map[string]bool
	// silent holds the hosts a request timed out against with no answer from
	// the host since that request was sent.
	silent map[string]bool
	// answered is when each host last answered a request in the scope.
	answered map[string]time.Time
}

// ErrHostSilent is the error a client returns, inside a TransportError, for a
// request it did not send because its host is silent in the fail-fast scope
// (Silent).
var ErrHostSilent = errors.New("not sent: an earlier request to this host timed out in this poll cycle")

// WithFailFast returns a context that opens a fail-fast scope. Under it, once
// one request to a host has ended in a transient failure, after whatever
// retries its client allowed, every later request to that host gets a single
// attempt: no retry, no backoff and no Retry-After wait. Once a request to a
// host has timed out, later requests to it are not sent at all (Silent).
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
// The single attempt is kept because it is cheap for most transient failures:
// a refused connection or a 5xx comes back in milliseconds, and a host that
// recovers partway through the cycle answers it. A timeout is the exception,
// since the attempt is the client's whole time budget, so a cycle over many
// repos would again wait once per repo.
//
// The scope is per host, so it never stops retries against a host that has
// not failed. A scope already on ctx is replaced for the derived context.
func WithFailFast(ctx context.Context) context.Context {
	return context.WithValue(ctx, failFastKey{}, &failFast{
		down:     map[string]bool{},
		silent:   map[string]bool{},
		answered: map[string]time.Time{},
	})
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

// MarkTransportFailure is MarkUnreachable for a request, sent at sent, that
// got no response at all: err is the error from http.Client.Do. A timeout
// also makes host silent, unless the host has answered another request since
// this one was sent. That answer shows the host was serving while this
// request hung, so the timeout was about the request rather than the host.
func MarkTransportFailure(ctx context.Context, host string, sent time.Time, err error) {
	f := failFastFrom(ctx)
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[host] = true
	if timedOut(err) && !f.answered[host].After(sent) {
		f.silent[host] = true
	}
}

// MarkAnswered records that host returned a response, of any status. A host
// that answers is not silent, whatever timed out against it before.
func MarkAnswered(ctx context.Context, host string) {
	f := failFastFrom(ctx)
	if f == nil {
		return
	}
	f.mu.Lock()
	f.answered[host] = time.Now()
	delete(f.silent, host)
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

// Silent reports whether ctx is a fail-fast scope in which a request to host
// timed out and nothing from host has answered since, so a client does not
// send its request to host at all and returns ErrHostSilent in a
// TransportError instead. The request is not counted: it never reached the
// upstream, and the timeout that made the host silent was counted already.
func Silent(ctx context.Context, host string) bool {
	f := failFastFrom(ctx)
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.silent[host]
}

// timedOut reports whether err is a timeout from http.Client.Do.
func timedOut(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
