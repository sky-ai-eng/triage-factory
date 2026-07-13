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
	// start proxies; while serving, the sidecar calls the orchestrator back to
	// authorize a repo. Exercises both directions on one connection.
	authHandler := HandlerFunc(func(_ context.Context, kind Kind, _ json.RawMessage) (any, error) {
		if kind != KindAuthorizeRepo {
			return nil, errors.New("wrong kind")
		}
		return AuthorizeRepoResult{Allowed: true, AllowedRefs: []string{"refs/heads/feature"}}, nil
	})
	var b *Conn
	sidecarHandler := HandlerFunc(func(ctx context.Context, kind Kind, _ json.RawMessage) (any, error) {
		if kind != KindStartProxies {
			return nil, errors.New("wrong kind")
		}
		// The sidecar calls the orchestrator back over its OWN connection.
		var dec AuthorizeRepoResult
		if err := b.Call(ctx, KindAuthorizeRepo, AuthorizeRepoBody{Owner: "o", Repo: "r"}, &dec); err != nil {
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
