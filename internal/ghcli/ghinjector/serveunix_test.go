package ghinjector_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghinjector"
)

func newUnixTestServer(t *testing.T) *ghinjector.Server {
	t.Helper()
	srv, err := ghinjector.New(ghinjector.Config{
		Upstream:    "https://api.github.com",
		TokenSource: func(context.Context) (string, error) { return "ghs_real", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// ServeUnix serves plain HTTP, so it takes only a listener whose transport is
// private by construction; a TCP one would put the handler on the network
// without TLS.
func TestServeUnix_RefusesANonUnixListener(t *testing.T) {
	srv := newUnixTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := srv.ServeUnix(ln); err == nil {
		t.Fatal("ServeUnix accepted a TCP listener, want a refusal")
	}
}

// Shutdown closes the unix listener as well as the TLS one, so a run whose
// channel is gone gets connection-refused on the socket.
func TestShutdown_ClosesTheUnixListener(t *testing.T) {
	srv := newUnixTestServer(t)
	// Short, so the path fits the platform's socket limit.
	dir, err := os.MkdirTemp("", "tfgh")
	if err != nil {
		t.Fatalf("make dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "gh.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := srv.ServeUnix(ln); err != nil {
		t.Fatalf("ServeUnix: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if conn, err := net.Dial("unix", path); err == nil {
		_ = conn.Close()
		t.Error("the socket still accepts connections after Shutdown")
	}
}
