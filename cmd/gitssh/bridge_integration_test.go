package gitssh

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// These tests are the point of the whole dispatcher: an SSH-shaped session
// reaching the REAL proxy, not a stand-in, so what they assert is that the
// credential swap, the ref gate and the outcome capture apply to it exactly as
// they apply to an HTTPS remote. Nothing in the proxy is told which transport
// the request came from, because nothing in it can be.

// fakeUpstream stands in for GitHub. It records the credential it was handed
// so a test can prove the run's placeholder never reached it.
type fakeUpstream struct {
	advertisement string
	response      string

	auth        string
	gitProtocol string
	body        []byte
}

func (u *fakeUpstream) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.auth = r.Header.Get("Authorization")
		u.gitProtocol = r.Header.Get("Git-Protocol")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, u.advertisement)
			return
		}
		buf := &bytes.Buffer{}
		_, _ = buf.ReadFrom(r.Body)
		u.body = buf.Bytes()
		fmt.Fprint(w, u.response)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startProxy runs a real gitproxy in front of upstream, gated exactly as a
// managed local run gates it, and returns its address plus the channels the
// test observes its capture and audit hooks through.
func startProxy(t *testing.T, upstream string, decision gitproxy.Decision) (addr string, pushes chan gitproxy.PushedRef, denials chan gitproxy.DeniedGitOp) {
	t.Helper()
	pushes = make(chan gitproxy.PushedRef, 4)
	denials = make(chan gitproxy.DeniedGitOp, 4)
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "real-upstream-token"}, nil
		},
		Upstream:      upstream,
		IncomingToken: "run-token",
		Authorize: func(context.Context, string, string) (gitproxy.Decision, error) {
			return decision, nil
		},
		RecordPush:   func(_ context.Context, p gitproxy.PushedRef) { pushes <- p },
		RecordDenial: func(_ context.Context, d gitproxy.DeniedGitOp) { denials <- d },
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	front := httptest.NewServer(srv.Handler())
	t.Cleanup(front.Close)
	return front.URL, pushes, denials
}

func TestBridgePush_ThroughRealProxy_LandsAsManagedIdentityAndIsRecorded(t *testing.T) {
	upstream := &fakeUpstream{
		advertisement: pkt("# service=git-receive-pack\n") + flush +
			pkt("de1e7e5 refs/heads/main\x00report-status side-band-64k\n") + flush,
		response: pkt("unpack ok\n") + pkt("ok refs/heads/feature\n") + flush,
	}
	upstreamSrv := upstream.start(t)
	proxyURL, pushes, _ := startProxy(t, upstreamSrv.URL, gitproxy.Decision{
		Allowed:       true,
		AllowedRefs:   []string{"refs/heads/feature"},
		ProtectedRefs: []string{"refs/heads/main"},
	})

	commands := pkt("0000000000000000000000000000000000000000 abc123 refs/heads/feature\x00report-status\n") + flush
	pack := "PACK\x00\x00\x00\x02fake"
	var stdout bytes.Buffer

	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: proxyURL, proxyToken: "run-token"}
	if err := bridge(cfg, "git-receive-pack '/acme/widgets.git'", strings.NewReader(commands+pack), &stdout); err != nil {
		t.Fatalf("bridge: %v", err)
	}

	if !strings.Contains(stdout.String(), "ok refs/heads/feature") {
		t.Fatalf("stdout = %q, want the upstream's report-status relayed back to git", stdout.String())
	}
	if string(upstream.body) != commands+pack {
		t.Fatalf("upstream body = %q, want the commands and pack unchanged", upstream.body)
	}
	// The push authenticated as the org's managed identity, and the run's own
	// placeholder — the thing an SSH-shaped remote used to bypass entirely —
	// never left the host.
	if !strings.HasPrefix(upstream.auth, "Basic ") || strings.Contains(upstream.auth, "run-token") {
		t.Fatalf("upstream Authorization = %q, want the swapped-in managed credential", upstream.auth)
	}

	select {
	case got := <-pushes:
		if got.Ref != "refs/heads/feature" || got.NewSHA != "abc123" || !got.Created || !got.Succeeded() {
			t.Fatalf("captured push = %+v, want the created feature ref with the upstream's success", got)
		}
		if got.Repo != "acme/widgets" {
			t.Fatalf("captured repo = %q, want acme/widgets", got.Repo)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy recorded no push for a bridged SSH session")
	}
}

func TestBridgePush_ThroughRealProxy_RefusesProtectedRef(t *testing.T) {
	upstream := &fakeUpstream{
		advertisement: pkt("de1e7e5 refs/heads/main\x00report-status\n") + flush,
		response:      pkt("unpack ok\n") + flush,
	}
	upstreamSrv := upstream.start(t)
	proxyURL, pushes, denials := startProxy(t, upstreamSrv.URL, gitproxy.Decision{
		Allowed:       true,
		AllowedRefs:   []string{"refs/heads/feature"},
		ProtectedRefs: []string{"refs/heads/main"},
	})

	commands := pkt("de1e7e5 abc123 refs/heads/main\x00report-status\n") + flush
	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: proxyURL, proxyToken: "run-token"}
	err := bridge(cfg, "git-receive-pack '/acme/widgets.git'", strings.NewReader(commands+"PACK"), &bytes.Buffer{})
	if err == nil {
		t.Fatal("a base-branch push over the bridge succeeded, want the ref gate to refuse it")
	}

	select {
	case got := <-denials:
		if got.Ref != "refs/heads/main" || got.Op != "push" {
			t.Fatalf("audit record = %+v, want the refused base-branch push", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy wrote no audit record for a refused bridged push")
	}
	select {
	case got := <-pushes:
		t.Fatalf("a refused push produced a capture record %+v, want none", got)
	default:
	}
	if upstream.body != nil {
		t.Fatalf("upstream saw a body %q for a push the gate refused", upstream.body)
	}
}

func TestBridgeFetch_ThroughRealProxy_CarriesProtocolV2(t *testing.T) {
	t.Setenv("GIT_PROTOCOL", "version=2")

	upstream := &fakeUpstream{
		advertisement: pkt("version 2\n") + pkt("ls-refs=unborn\n") + flush,
		response:      pkt("abc123 refs/heads/main\n") + flush,
	}
	upstreamSrv := upstream.start(t)
	proxyURL, _, _ := startProxy(t, upstreamSrv.URL, gitproxy.Decision{Allowed: true})

	lsRefs := pkt("command=ls-refs\n") + "0001" + pkt("peel\n") + flush
	var stdout bytes.Buffer

	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: proxyURL, proxyToken: "run-token"}
	if err := bridge(cfg, "git-upload-pack '/acme/widgets.git'", strings.NewReader(lsRefs+flush), &stdout); err != nil {
		t.Fatalf("bridge: %v", err)
	}

	if !strings.Contains(stdout.String(), "refs/heads/main") {
		t.Fatalf("stdout = %q, want the ls-refs answer relayed to git", stdout.String())
	}
	if upstream.gitProtocol != "version=2" {
		t.Fatalf("upstream Git-Protocol = %q, want version=2", upstream.gitProtocol)
	}
	if strings.Contains(upstream.auth, "run-token") {
		t.Fatalf("upstream Authorization = %q, want the swapped-in managed credential", upstream.auth)
	}
}
