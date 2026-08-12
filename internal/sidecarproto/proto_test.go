package sidecarproto

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// pipeConns wires two Conns over an in-memory duplex pipe, each with its own
// handler, and returns them plus a cleanup.
func pipeConns(t *testing.T, aHandler, bHandler Handler) (a, b *Conn) {
	t.Helper()
	ca, cb := net.Pipe()
	a = New(ca, aHandler)
	b = New(cb, bHandler)
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
		_ = ca.Close()
		_ = cb.Close()
	})
	return a, b
}

func TestConn_CallRoundTrip(t *testing.T) {
	// b serves StartProxies by echoing an env entry derived from the request.
	server := HandlerFunc(func(_ context.Context, kind Kind, body json.RawMessage) (any, error) {
		if kind != KindStartProxies {
			t.Errorf("unexpected kind %q", kind)
		}
		var req StartProxiesBody
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return StartProxiesResult{Env: []string{"HOST=" + req.HostVethIP}}, nil
	})
	client, _ := pipeConns(t, nil, server)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var res StartProxiesResult
	if err := client.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.7.1"}, &res); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res.Env) != 1 || res.Env[0] != "HOST=10.42.7.1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestConn_RemoteError(t *testing.T) {
	server := HandlerFunc(func(_ context.Context, _ Kind, _ json.RawMessage) (any, error) {
		return nil, errors.New("unseal failed")
	})
	client, _ := pipeConns(t, nil, server)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := client.Call(ctx, KindSealedBundle, SealedBundleBody{Sealed: []byte{1, 2, 3}}, nil)
	if err == nil {
		t.Fatal("expected remote error")
	}
	if got := err.Error(); got == "" || !contains(got, "unseal failed") {
		t.Fatalf("expected wrapped remote error, got %q", got)
	}
}

func TestConn_Bidirectional(t *testing.T) {
	// The classic sidecar shape: the orchestrator (a) asks the sidecar (b) to
	// start proxies; while serving, the sidecar relays a call back to the
	// orchestrator (the generic KindRelayCall envelope — a git-push authz
	// decision). Exercises both directions on one connection.
	authHandler := HandlerFunc(func(_ context.Context, kind Kind, body json.RawMessage) (any, error) {
		if kind != KindRelayCall {
			return nil, errors.New("wrong kind")
		}
		var env RelayCallBody
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.Namespace != "core" || env.Op != "authorize_repo" {
			return nil, errors.New("wrong relay op")
		}
		return json.RawMessage(`{"allowed":true,"allowed_refs":["refs/heads/feature"]}`), nil
	})
	var b *Conn
	sidecarHandler := HandlerFunc(func(ctx context.Context, kind Kind, _ json.RawMessage) (any, error) {
		if kind != KindStartProxies {
			return nil, errors.New("wrong kind")
		}
		// The sidecar relays a call to the orchestrator over its OWN connection.
		var dec struct {
			Allowed bool `json:"allowed"`
		}
		if err := b.Call(ctx, KindRelayCall, RelayCallBody{
			Namespace: "core", Op: "authorize_repo", Args: json.RawMessage(`{"owner":"o","repo":"r"}`),
		}, &dec); err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, errors.New("expected allow")
		}
		return StartProxiesResult{Env: []string{"OK=1"}}, nil
	})
	var a *Conn
	a, b = pipeConns(t, authHandler, sidecarHandler)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var res StartProxiesResult
	if err := a.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, &res); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res.Env) != 1 || res.Env[0] != "OK=1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestConn_Notify(t *testing.T) {
	got := make(chan HelloBody, 1)
	server := HandlerFunc(func(_ context.Context, kind Kind, body json.RawMessage) (any, error) {
		if kind == KindHello {
			var h HelloBody
			_ = json.Unmarshal(body, &h)
			got <- h
		}
		return nil, nil
	})
	client, _ := pipeConns(t, nil, server)

	if err := client.Notify(KindHello, HelloBody{PubKey: "abc"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	select {
	case h := <-got:
		if h.PubKey != "abc" {
			t.Fatalf("unexpected hello: %+v", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notify not delivered")
	}
}

func TestConn_CallAfterCloseFails(t *testing.T) {
	client, _ := pipeConns(t, nil, nil)
	_ = client.Close()
	err := client.Call(context.Background(), KindStartProxies, StartProxiesBody{}, nil)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

// TestConn_CallInterruptedByCloseFails covers the in-flight case: the conn
// shuts down while a Call is blocked waiting for its reply. shutdown() closes
// the pending channel, and the Call must report that as a failure — never read
// the zero-value Frame off the closed channel as an empty success.
func TestConn_CallInterruptedByCloseFails(t *testing.T) {
	// The server never answers, so the Call blocks on its pending channel until
	// we tear the connection down underneath it.
	blocked := HandlerFunc(func(ctx context.Context, _ Kind, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client, _ := pipeConns(t, nil, blocked)

	errc := make(chan error, 1)
	go func() {
		errc <- client.Call(context.Background(), KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, nil)
	}()

	// Give the Call time to register its pending entry and block, then shut the
	// conn down out from under it.
	time.Sleep(50 * time.Millisecond)
	_ = client.Close()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Call returned nil on a mid-flight shutdown — a closed pending channel must not read as success")
		}
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("expected ErrClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after the conn was closed mid-flight")
	}
}

// panicPayload is the giveaway a panic value would carry across the wire if
// either guard formatted it into an error. Panic values are arbitrary runtime
// state — a formatted request body, a struct that happened to hold a credential
// — and the peer's error string is logged and persisted, so the tests below
// assert this string is nowhere in what the caller receives.
const panicPayload = "token-abc123-must-not-cross-the-wire"

// TestConn_PanickingHandler_AnswersAndKeepsServing pins all three halves of the
// dispatch guard. Containment: the panic doesn't take the process (without the
// recover this test kills the binary, which is what it would do to the sidecar
// and every proxy in it). Correctness: the caller gets an error response
// promptly instead of blocking to its own deadline — a panicking handler used
// to turn an immediate failure into a timeout. Discretion: the response names
// the failure without carrying the panic value.
func TestConn_PanickingHandler_AnswersAndKeepsServing(t *testing.T) {
	server := HandlerFunc(func(_ context.Context, kind Kind, _ json.RawMessage) (any, error) {
		if kind == KindSealedBundle {
			panic("handler exploded: " + panicPayload)
		}
		return StartProxiesResult{Env: []string{"OK=1"}}, nil
	})
	client, _ := pipeConns(t, nil, server)

	// A deadline far longer than the answer should take, so a timeout here
	// means the caller was left blocking rather than answered.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	answered := make(chan error, 1)
	go func() {
		answered <- client.Call(ctx, KindSealedBundle, SealedBundleBody{Sealed: []byte{1, 2, 3}}, nil)
	}()
	select {
	case err := <-answered:
		if err == nil {
			t.Fatal("expected the panicking handler to answer with an error")
		}
		if !contains(err.Error(), "panicked") {
			t.Fatalf("error = %q, want it to name the internal failure", err)
		}
		if contains(err.Error(), panicPayload) {
			t.Fatalf("the panic value crossed the wire in %q — a response is an error string the peer logs and persists", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller was left blocking on a request whose handler panicked")
	}

	// The connection is still usable: one dead request is one dead request.
	var res StartProxiesResult
	if err := client.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, &res); err != nil {
		t.Fatalf("next call after the panic: %v", err)
	}
	if len(res.Env) != 1 || res.Env[0] != "OK=1" {
		t.Fatalf("unexpected result after the panic: %+v", res)
	}
}

// panickingStream is a duplex stream whose Read panics — the shape of a
// decoder blowing up on an inbound frame. The read blocks until the first
// Write, so the panic lands after a Call has registered its pending entry and
// the test is exercising the in-flight case rather than a race.
type panickingStream struct {
	release   chan struct{}
	once      sync.Once
	firstRead chan struct{}
}

func newPanickingStream() *panickingStream {
	return &panickingStream{release: make(chan struct{}), firstRead: make(chan struct{}, 1)}
}

func (p *panickingStream) Read([]byte) (int, error) {
	select {
	case p.firstRead <- struct{}{}:
	default:
	}
	<-p.release
	panic("reader exploded: " + panicPayload)
}

func (p *panickingStream) Write(b []byte) (int, error) {
	p.once.Do(func() { close(p.release) })
	return len(b), nil
}

func (p *panickingStream) Close() error { return nil }

// TestConn_PanickingReadLoop_CollapsesConnWithoutLeakingTheValue covers the
// other raw goroutine. A panic in the reader must collapse the connection
// rather than the process — every in-flight Call fails with the shutdown cause,
// which is an outcome callers already handle — and that cause must not carry
// the panic value, because unlike a response it is handed to EVERY pending Call
// at once.
func TestConn_PanickingReadLoop_CollapsesConnWithoutLeakingTheValue(t *testing.T) {
	stream := newPanickingStream()
	c := New(stream, nil)
	t.Cleanup(func() { _ = c.Close() })

	// Wait for the read loop to be parked in Read, so the Call below is the
	// in-flight case: its own Write is what releases the panic.
	select {
	case <-stream.firstRead:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop never reached its first Read")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Call(ctx, KindStartProxies, StartProxiesBody{HostVethIP: "10.42.1.1"}, nil)
	if err == nil {
		t.Fatal("Call returned nil after the read loop panicked")
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("error = %v, want it to wrap ErrClosed (the defined collapse outcome)", err)
	}
	if contains(err.Error(), panicPayload) {
		t.Fatalf("the panic value reached a pending Call in %q — this cause fans out to every caller at once", err)
	}

	// The conn is properly collapsed, not merely wedged: Done is closed and
	// subsequent Calls fail fast instead of blocking on a dead reader.
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was not closed after the read loop panicked")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
