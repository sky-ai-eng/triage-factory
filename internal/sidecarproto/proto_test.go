package sidecarproto

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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

// TestConn_PanickingHandler_AnswersAndKeepsServing pins both halves of the
// dispatch guard. Containment: the panic doesn't take the process (without the
// recover this test kills the binary, which is what it would do to the sidecar
// and every proxy in it). Correctness: the caller gets an error response
// promptly instead of blocking to its own deadline — a panicking handler used
// to turn an immediate failure into a timeout.
func TestConn_PanickingHandler_AnswersAndKeepsServing(t *testing.T) {
	server := HandlerFunc(func(_ context.Context, kind Kind, _ json.RawMessage) (any, error) {
		if kind == KindSealedBundle {
			panic("handler exploded")
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
