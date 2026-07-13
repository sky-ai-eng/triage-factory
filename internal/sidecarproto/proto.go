// Package sidecarproto is the supervision wire-protocol spoken over the
// single duplex stdio socket the cap-broker wires between the orchestrator
// and a per-run credential sidecar (internal/sandbox.LaunchSidecar). It is
// how the orchestrator — which holds no run credential and no unseal key —
// drives the sidecar that does: the sidecar publishes its per-run public
// key, the orchestrator relays the opaque sealed bundle down and the
// non-secret proxy env back up, and the git proxy's DB-backed authorize
// decision is relayed the other way (sidecar → orchestrator) so no database
// handle ever enters the capless sidecar.
//
// The transport is one unix socket carrying newline-delimited JSON frames
// in both directions, so the protocol is a small symmetric duplex RPC:
// either side may open a Call (request awaiting a matched response) or fire
// a Notify (one-way), and either side dispatches inbound Calls to a
// Handler. That symmetry is load-bearing — the orchestrator calls the
// sidecar (relay a bundle, start proxies) and the sidecar calls the
// orchestrator back (authorize this push, record this denial) over the same
// connection.
//
// Deliberately dependency-free (stdlib only) so both cmd/runsidecar and the
// orchestrator side can import it without dragging in sandbox/agentproc.
package sidecarproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Kind names a frame's message type. Requests and their responses share a
// Kind; the id + IsResponse flag distinguish a reply from a fresh request.
type Kind string

const (
	// KindHello is the sidecar's unsolicited opening frame carrying its
	// freshly-minted per-run X25519 public key. It is a Notify (id 0), sent
	// exactly once before the sidecar processes any request, so the
	// orchestrator can publish the key upward and drive the seal before it
	// asks the sidecar to do anything.
	KindHello Kind = "hello"

	// KindSealedBundle relays one run's opaque sealed credential bundle from
	// the orchestrator (which read it out of run_credentials but cannot open
	// it) to the sidecar (which holds the private key). Repeatable: the
	// orchestrator re-sends whenever the brain re-seals (refresh sweep), and
	// the sidecar's proxies pick up the newer material. A Call so the sidecar
	// can report an unseal failure back.
	KindSealedBundle Kind = "sealed_bundle"

	// KindStartProxies asks the sidecar to bind this run's credential proxies
	// on the host-side veth IP (now that the broker has configured the netns)
	// and return the non-secret sandbox env naming them. A Call.
	KindStartProxies Kind = "start_proxies"

	// KindRelayCall is the single sidecar → orchestrator relay envelope for a
	// request/response op: the sidecar (which holds no DB handle and no
	// secret store) asks the orchestrator to serve one org-bound, non-secret
	// policy/data or authz decision, named by (namespace, op) in the body.
	// The response is the op's marshaled result (or a remote error). Every
	// DB-backed read + authz the relocated agenthost needs — the git proxy's
	// push authorization, the exec verb-trace reads, a provider's policy
	// lookup — rides this one Kind; identity is bound orchestrator-side from
	// the supervised run's RunInfo, so the body carries no org id and a
	// sidecar cannot address another org's data.
	KindRelayCall Kind = "relay_call"

	// KindRelayNotify is the fire-and-forget twin of KindRelayCall for
	// audit-record ops that must never block the external write they follow —
	// a denied git op, a completed push, an external-write record. Same
	// (namespace, op) envelope; no response awaited. The orchestrator serves
	// it best-effort against its stores (the write already landed upstream);
	// a dropped notify costs at most one audit row, never the agent's action.
	KindRelayNotify Kind = "relay_notify"
)

// Frame is one newline-delimited JSON message. Body is the Kind-specific
// payload, decoded by the receiver into the matching typed struct below.
type Frame struct {
	Kind Kind `json:"kind"`
	// ID correlates a response to its request. 0 marks a Notify (one-way, no
	// response expected). Non-zero ids are minted per outbound Call by the
	// sending side and echoed on the response.
	ID uint64 `json:"id,omitempty"`
	// IsResponse marks this frame as the reply to an earlier Call of the same
	// id, rather than a fresh inbound request. Without it a request and its
	// response are ambiguous (both carry the same Kind + id).
	IsResponse bool `json:"resp,omitempty"`
	// Err, when non-empty on a response, carries the handler's error text;
	// Body is then empty. Transport for a remote failure — Call returns it
	// wrapped so a caller distinguishes a remote error from a local one.
	Err  string          `json:"err,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Handler dispatches an inbound Call (a request the peer opened). It returns
// the response body to marshal, or an error whose text is relayed back to
// the caller as the response Err. A nil Handler rejects every inbound Call
// with ErrNoHandler — correct for a side that only makes Calls and never
// serves them. Notifies are delivered here too, with a nil expectation on
// the return (the transport discards a Notify's response); a Handler that
// only cares about Notifies can return (nil, nil).
type Handler interface {
	Handle(ctx context.Context, kind Kind, body json.RawMessage) (respBody any, err error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, kind Kind, body json.RawMessage) (any, error)

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, kind Kind, body json.RawMessage) (any, error) {
	return f(ctx, kind, body)
}

// ErrNoHandler is the response error when a Call arrives on a Conn with no
// Handler wired.
var ErrNoHandler = errors.New("sidecarproto: no handler for inbound request")

// ErrClosed is returned by Call once the Conn's read loop has stopped
// (peer hung up, or Close was called) — every in-flight and subsequent Call
// fails with it rather than blocking forever.
var ErrClosed = errors.New("sidecarproto: connection closed")

// Conn is one end of the duplex supervision channel. Construct with New,
// then use Call/Notify to talk to the peer; inbound requests are dispatched
// to the Handler passed to New. Safe for concurrent use.
type Conn struct {
	w   *json.Encoder
	rwc io.Closer

	// wmu serializes frame writes — json.Encoder is not safe for concurrent
	// Encode, and a torn frame would desync the newline framing.
	wmu sync.Mutex

	handler Handler

	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan Frame
	closed   bool
	closeErr error

	// done closes exactly once when the conn shuts down (peer hung up, read/
	// write error, or Close). Lets a sidecar block on the channel's life
	// without polling.
	done      chan struct{}
	closeOnce sync.Once
}

// New wraps a duplex byte stream (the stdio socket) as a Conn and starts its
// read loop. handler dispatches inbound Calls/Notifies; pass nil for a side
// that only originates Calls. The caller owns rwc's lifetime beyond Close.
func New(rwc io.ReadWriteCloser, handler Handler) *Conn {
	c := &Conn{
		w:       json.NewEncoder(rwc),
		rwc:     rwc,
		handler: handler,
		pending: make(map[uint64]chan Frame),
		done:    make(chan struct{}),
	}
	go c.readLoop(bufio.NewReader(rwc))
	return c
}

// Done returns a channel that closes when the connection shuts down for any
// reason — the peer hung up, a read/write failed, or Close was called. A
// sidecar blocks on it to run for the life of its supervision channel.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns the cause the conn shut down with, or nil if it is still open.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Call sends a request of the given kind and blocks until the peer's
// response, ctx cancellation, or the connection closing. reqBody is marshaled
// as the request payload; respBody, when non-nil, is unmarshaled from the
// response. A remote handler error is returned wrapped (never as respBody).
func (c *Conn) Call(ctx context.Context, kind Kind, reqBody any, respBody any) error {
	payload, err := marshalBody(reqBody)
	if err != nil {
		return fmt.Errorf("sidecarproto: marshal %s request: %w", kind, err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.closeErr
	}
	c.nextID++
	id := c.nextID
	ch := make(chan Frame, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	// Ensure the pending entry is reclaimed on every exit (response, ctx,
	// close) so a cancelled Call doesn't leak its channel.
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.writeFrame(Frame{Kind: kind, ID: id, Body: payload}); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			// shutdown() closed this pending channel instead of delivering a
			// reply — the conn died (peer hung up, an I/O error, or Close)
			// while the Call was in flight. Surface the shutdown cause; a
			// zero-value Frame here is NOT an empty success.
			c.mu.Lock()
			err := c.closeErr
			c.mu.Unlock()
			if err == nil {
				err = ErrClosed
			}
			return err
		}
		if resp.Err != "" {
			return fmt.Errorf("sidecarproto: remote %s: %s", kind, resp.Err)
		}
		if respBody != nil && len(resp.Body) > 0 {
			if err := json.Unmarshal(resp.Body, respBody); err != nil {
				return fmt.Errorf("sidecarproto: unmarshal %s response: %w", kind, err)
			}
		}
		return nil
	}
}

// Notify sends a one-way message (no response awaited). Use for best-effort
// signals — the hello key publish, a denial audit record — where the sender
// doesn't need to sequence on delivery.
func (c *Conn) Notify(kind Kind, body any) error {
	payload, err := marshalBody(body)
	if err != nil {
		return fmt.Errorf("sidecarproto: marshal %s notify: %w", kind, err)
	}
	return c.writeFrame(Frame{Kind: kind, Body: payload})
}

// Close marks the conn closed, fails every pending and subsequent Call with
// ErrClosed, and closes Done. It does NOT close the underlying stream — the
// caller owns the socket's lifecycle (e.g. the broker's sidecarHandle) — and
// therefore does NOT on its own stop the read-loop goroutine: that goroutine
// stays blocked in Decode until the stream's owner/peer closes it, at which
// point Decode errors and the loop exits via a second (idempotent) shutdown.
// So a caller reasoning about goroutine lifetime must close rwc, not just call
// Close. Idempotent.
func (c *Conn) Close() error {
	c.shutdown(ErrClosed)
	return nil
}

func (c *Conn) shutdown(cause error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.closeErr = cause
		for id, ch := range c.pending {
			// Non-blocking: the buffered response channel may already hold a
			// value the Call goroutine hasn't drained; either way it wakes.
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *Conn) writeFrame(f Frame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	closed := c.closed
	closeErr := c.closeErr
	c.mu.Unlock()
	if closed {
		return closeErr
	}
	if err := c.w.Encode(f); err != nil {
		// A write failure means the peer is gone — collapse the conn so
		// in-flight Calls unblock rather than hang on a dead socket.
		c.shutdown(fmt.Errorf("%w: write: %v", ErrClosed, err))
		return c.closeErr
	}
	return nil
}

func (c *Conn) readLoop(r *bufio.Reader) {
	dec := json.NewDecoder(r)
	for {
		var f Frame
		if err := dec.Decode(&f); err != nil {
			cause := ErrClosed
			if err != io.EOF {
				cause = fmt.Errorf("%w: read: %v", ErrClosed, err)
			}
			c.shutdown(cause)
			return
		}
		if f.IsResponse {
			c.deliverResponse(f)
			continue
		}
		c.dispatchRequest(f)
	}
}

func (c *Conn) deliverResponse(f Frame) {
	c.mu.Lock()
	ch, ok := c.pending[f.ID]
	if ok {
		delete(c.pending, f.ID)
	}
	c.mu.Unlock()
	if ok {
		ch <- f
	}
}

func (c *Conn) dispatchRequest(f Frame) {
	// One goroutine per inbound request: a slow authorize callback must not
	// head-of-line-block the reader from delivering the response to a Call
	// this side has open. The control channel's request rate is low.
	go func() {
		respBody, err := c.serve(f)
		if f.ID == 0 {
			// Notify — no response frame.
			return
		}
		resp := Frame{Kind: f.Kind, ID: f.ID, IsResponse: true}
		if err != nil {
			resp.Err = err.Error()
		} else if payload, mErr := marshalBody(respBody); mErr != nil {
			resp.Err = fmt.Sprintf("marshal response: %v", mErr)
		} else {
			resp.Body = payload
		}
		_ = c.writeFrame(resp)
	}()
}

func (c *Conn) serve(f Frame) (any, error) {
	if c.handler == nil {
		return nil, ErrNoHandler
	}
	return c.handler.Handle(context.Background(), f.Kind, f.Body)
}

// marshalBody encodes a request/response payload to json.RawMessage. A nil
// body encodes as an absent Body (not "null").
func marshalBody(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(v)
}
