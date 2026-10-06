package pgtest

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// Every test binary of one `go test` invocation shares a single session
// reaper (Ryuk), and that reaper removes every container carrying the
// session's labels once no binary has been connected to it for its
// reconnection timeout. A connected binary is what keeps its containers
// alive. testcontainers-go connects each container it creates, but it runs
// the handshake on a goroutine and only logs a failure, and a reaper that
// has begun its final prune still accepts the TCP connection — it closes it
// without answering. A container created in that window (seconds long: the
// prune removes, one at a time, the container every finished binary left
// running) carries the session's labels with no live connection behind
// it, and the next reaper the session starts removes it the first time its
// own clients drop to zero, which from this binary's side is a container
// dying mid-suite. A revive boots its replacement while the reaper that
// removed the old one is still pruning, so without holdReaper the
// replacement lands in the same window.
//
// holdReaper takes the process out of that race: before the first
// container boots, it registers with the session's reaper itself, waits
// for the acknowledgement, and holds the connection until the process
// exits. A reaper that acknowledged a connection counts it as a client,
// and a reaper with a client does not prune, so nothing this process
// starts is removed while it runs. Once it exits, the reaper removes those
// containers as it would have anyway.

var (
	reaperMu sync.Mutex
	// reaperConn is never closed. The process exiting closes it, which is
	// the signal the reaper waits for; holding the reference also keeps
	// the runtime's finalizer from closing the descriptor first.
	reaperConn net.Conn
)

// reaperRetryInterval is the wait after a reaper refuses a registration.
// A refusing reaper is in its final prune and exits when that finishes,
// after which the next request starts a replacement.
const reaperRetryInterval = time.Second

// reaperAckTimeout bounds one registration exchange. A live reaper
// answers in milliseconds; this only stops a connection nobody answers
// from consuming the whole boot deadline.
const reaperAckTimeout = 10 * time.Second

// reaperAck is the reaper's reply to a filter line it accepted.
const reaperAck = "ACK\n"

// holdReaper registers this process with the session reaper, once per
// process, retrying until a reaper acknowledges or ctx ends. It is a
// no-op when the reaper is disabled, since nothing then removes
// containers on the session's behalf.
func holdReaper(ctx context.Context) error {
	reaperMu.Lock()
	defer reaperMu.Unlock()
	if reaperConn != nil || testcontainers.ReadConfig().Config.RyukDisabled {
		return nil
	}

	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return fmt.Errorf("docker provider: %w", err)
	}
	defer func() { _ = provider.Close() }()

	for {
		// NewReaper is deprecated as a way to create a reaper. What it
		// does here is return the session's running reaper, starting one
		// when there is none, and it is the only exported way to learn
		// the reaper's endpoint.
		r, err := testcontainers.NewReaper(ctx, testcontainers.SessionID(), provider, "") //nolint:staticcheck // see above
		if err == nil {
			var conn net.Conn
			if conn, err = registerWithReaper(ctx, r.Endpoint); err == nil {
				reaperConn = conn
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("register with the session reaper: %w (last attempt: %v)", ctx.Err(), err)
		case <-time.After(reaperRetryInterval):
		}
	}
}

// registerWithReaper sends the reaper one line of label filters and
// waits for its acknowledgement. The acknowledgement is the part that
// matters: a reaper sends it only for a connection it has counted as a
// client, and one in its final prune closes the connection without it.
func registerWithReaper(ctx context.Context, endpoint string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("dial reaper %s: %w", endpoint, err)
	}
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}

	if err := conn.SetDeadline(time.Now().Add(reaperAckTimeout)); err != nil {
		return fail(fmt.Errorf("set registration deadline: %w", err))
	}
	if _, err := io.WriteString(conn, reaperFilter(testcontainers.GenericLabels())); err != nil {
		return fail(fmt.Errorf("send filter to reaper %s: %w", endpoint, err))
	}
	ack := make([]byte, len(reaperAck))
	if _, err := io.ReadFull(conn, ack); err != nil {
		return fail(fmt.Errorf("reaper %s did not acknowledge: %w", endpoint, err))
	}
	if string(ack) != reaperAck {
		return fail(fmt.Errorf("reaper %s answered %q, want %q", endpoint, ack, reaperAck))
	}
	// The connection is held for the life of the process from here, so
	// the deadline that bounded the exchange must not outlive it.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(fmt.Errorf("clear registration deadline: %w", err))
	}
	return conn, nil
}

// reaperFilter renders labels as the reaper's filter line: one
// label=key=value term per label, joined by "&", newline-terminated.
// The terms are sorted so the line is stable across calls.
func reaperFilter(labels map[string]string) string {
	terms := make([]string, 0, len(labels))
	for k, v := range labels {
		terms = append(terms, "label="+k+"="+v)
	}
	sort.Strings(terms)
	return strings.Join(terms, "&") + "\n"
}
