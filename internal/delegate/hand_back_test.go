package delegate

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// closedLoopbackAddr is a loopback address nothing listens on, so a
// connection to it is refused at once.
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// Each cause is a real failure of something on this host, wrapped the way the
// bring-up path wraps it. Every one carries a net.Error or a deadline, which
// is also what a request to an upstream fails with, and none of them is an
// upstream being unavailable: retrying them for four hours and then telling a
// person a service was unreachable would hide a fault that fails the same way
// on every attempt.
func TestUpstreamSetupFailure_ALocalFaultIsNotAnOutage(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "toolhost.sock")
	held, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	_, bindErr := net.Listen("unix", sockPath)
	if bindErr == nil {
		t.Fatal("a second bind of a held socket succeeded")
	}

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	<-expired.Done()

	_, dialErr := net.Dial("tcp", closedLoopbackAddr(t))
	if dialErr == nil {
		t.Fatal("dial to a closed port succeeded")
	}

	_, httpErr := http.Get("http://" + closedLoopbackAddr(t) + "/v1/launch")
	if httpErr == nil {
		t.Fatal("request to a closed port succeeded")
	}

	for name, cause := range map[string]error{
		"tool host socket bind": fmt.Errorf("launch tool host: %w",
			fmt.Errorf("agentproc: bind tool host socket %s: %w", sockPath, bindErr)),
		"opening turn past its deadline":                    fmt.Errorf("mint opening turn: %w", expired.Err()),
		"database dial":                                     fmt.Errorf("resolve model for claim: %w", dialErr),
		"an HTTP request no client marked as an upstream's": fmt.Errorf("set up run network: %w", httpErr),
	} {
		if upstreamSetupFailure(cause) {
			t.Errorf("%s: upstreamSetupFailure(%v) = true, want false", name, cause)
		}
	}
}

// The upstream failures a setup step really produces, built by the code that
// produces them: the GitHub client's answer to a pull-request read GitHub
// refused with a 503, and git's answer to a fetch that could not connect.
func TestUpstreamSetupFailure_AnUnreachableUpstreamCounts(t *testing.T) {
	ghclient.SetTransientBackoffForTest(t, time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"Service Unavailable"}`)
	}))
	t.Cleanup(server.Close)
	_, prErr := ghclient.NewClient(server.URL, "test-token").GetPR(context.Background(), "o", "r", 7, false)
	if prErr == nil {
		t.Fatal("GetPR against a 503 succeeded")
	}

	paths.SetForTest(t, t.TempDir())
	_, cloneErr := worktree.CreateForPR(context.Background(), "o", "r",
		"http://"+closedLoopbackAddr(t)+"/o/r.git", "", "feature", 7, "task-1")
	if cloneErr == nil {
		t.Fatal("clone from a closed port succeeded")
	}

	_, dropped := http.Get("http://" + closedLoopbackAddr(t) + "/repos/o/r/pulls/7")
	if dropped == nil {
		t.Fatal("request to a closed port succeeded")
	}

	for name, cause := range map[string]error{
		"pull-request read answered 503": fmt.Errorf("failed to fetch PR: %w", prErr),
		"git fetch could not connect":    fmt.Errorf("failed to create worktree: %w", cloneErr),
		"pull-request read its client marked as a transport failure": fmt.Errorf("failed to fetch PR: %w",
			fmt.Errorf("request /repos/o/r/pulls/7: %w", &upstream.TransportError{Err: dropped})),
	} {
		if !upstreamSetupFailure(cause) {
			t.Errorf("%s: upstreamSetupFailure(%v) = false, want true", name, cause)
		}
	}
}
