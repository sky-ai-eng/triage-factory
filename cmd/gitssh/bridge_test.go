package gitssh

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pkt frames one pkt-line the way git does: four hex digits counting
// themselves, then the payload.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

const flush = "0000"

// recordingProxy stands in for the run's git proxy. It answers the two
// smart-HTTP shapes the bridge uses and records what it was asked, so a test
// can assert the bridge produced the same request an HTTPS remote would have.
type recordingProxy struct {
	t *testing.T

	advertisement string
	advertStatus  int

	rpcResponses []string
	rpcStatus    int

	rpcBodies    [][]byte
	rpcProtocols []string
	advertQuery  string
	advertPath   string
	rpcPaths     []string
	passwords    []string
}

func (p *recordingProxy) start() *httptest.Server {
	p.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, _ := r.BasicAuth()
		p.passwords = append(p.passwords, password)
		if r.Method == http.MethodGet {
			p.advertPath = r.URL.Path
			p.advertQuery = r.URL.RawQuery
			if p.advertStatus != 0 && p.advertStatus != http.StatusOK {
				w.WriteHeader(p.advertStatus)
				fmt.Fprint(w, "gitproxy: repo not authorized for this run")
				return
			}
			fmt.Fprint(w, p.advertisement)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			p.t.Errorf("read request body: %v", err)
		}
		p.rpcBodies = append(p.rpcBodies, body)
		p.rpcPaths = append(p.rpcPaths, r.URL.Path)
		p.rpcProtocols = append(p.rpcProtocols, r.Header.Get("Git-Protocol"))
		if p.rpcStatus != 0 && p.rpcStatus != http.StatusOK {
			w.WriteHeader(p.rpcStatus)
			fmt.Fprint(w, "gitproxy: refs/heads/main is the base branch of acme/widgets and is protected")
			return
		}
		i := len(p.rpcBodies) - 1
		if i < len(p.rpcResponses) {
			fmt.Fprint(w, p.rpcResponses[i])
		}
	}))
	p.t.Cleanup(srv.Close)
	return srv
}

// A push carries its ref-update commands and its packfile as one request body,
// and the report-status comes back on the response — so what the proxy sees is
// byte-for-byte what it would see from an HTTPS remote, which is what makes
// the ref gate and the outcome capture apply unchanged.
func TestBridgePush_RelaysAdvertisementCommandsAndPack(t *testing.T) {
	advert := pkt("de1e7e5 refs/heads/main\x00report-status side-band-64k agent=git/2.44\n") + flush
	report := pkt("unpack ok\n") + pkt("ok refs/heads/feature\n") + flush
	proxy := &recordingProxy{
		t:             t,
		advertisement: pkt("# service=git-receive-pack\n") + flush + advert,
		rpcResponses:  []string{report},
	}
	srv := proxy.start()

	commands := pkt("0000000000000000000000000000000000000000 abc123 refs/heads/feature\x00report-status side-band-64k\n") + flush
	pack := "PACK\x00\x00\x00\x02fake-pack-bytes"
	var stdout bytes.Buffer

	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: srv.URL, proxyToken: "run-token"}
	if err := bridge(cfg, "git-receive-pack '/acme/widgets.git'", strings.NewReader(commands+pack), &stdout); err != nil {
		t.Fatalf("bridge: %v", err)
	}

	// git's ssh transport never sees the "# service=" header smart HTTP puts
	// in front of the advertisement.
	if stdout.String() != advert+report {
		t.Fatalf("stdout = %q, want %q", stdout.String(), advert+report)
	}
	if len(proxy.rpcBodies) != 1 {
		t.Fatalf("proxy saw %d service requests, want 1", len(proxy.rpcBodies))
	}
	if got := string(proxy.rpcBodies[0]); got != commands+pack {
		t.Fatalf("push body = %q, want %q", got, commands+pack)
	}
	if proxy.advertPath != "/acme/widgets.git/info/refs" || proxy.advertQuery != "service=git-receive-pack" {
		t.Fatalf("advertisement request = %s?%s", proxy.advertPath, proxy.advertQuery)
	}
	if proxy.rpcPaths[0] != "/acme/widgets.git/git-receive-pack" {
		t.Fatalf("push path = %s", proxy.rpcPaths[0])
	}
	for _, p := range proxy.passwords {
		if p != "run-token" {
			t.Fatalf("proxy saw password %q, want the run placeholder", p)
		}
	}
	// A push is protocol v0 — git downgrades it itself, since v2 has no push.
	if proxy.rpcProtocols[0] != "" {
		t.Fatalf("push announced Git-Protocol %q, want none", proxy.rpcProtocols[0])
	}
}

// A push that only deletes refs carries no packfile, and git holds its end of
// the pipe open waiting for the report — so the request has to be delimited by
// the command block itself. Reading to EOF here would deadlock the push.
func TestBridgePush_DeleteOnlyDoesNotWaitForEOF(t *testing.T) {
	advert := pkt("de1e7e5 refs/heads/stale\x00report-status\n") + flush
	proxy := &recordingProxy{
		t:             t,
		advertisement: advert,
		rpcResponses:  []string{pkt("unpack ok\n") + flush},
	}
	srv := proxy.start()

	commands := pkt("abc123 0000000000000000000000000000000000000000 refs/heads/stale\x00report-status\n") + flush

	// A pipe that is written but never closed is what git's side looks like
	// here: any read past the command block blocks forever.
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte(commands)) }()
	t.Cleanup(func() { _ = pw.Close() })

	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: srv.URL, proxyToken: "run-token"}
	done := make(chan error, 1)
	go func() {
		done <- bridge(cfg, "git-receive-pack '/acme/widgets.git'", pr, &bytes.Buffer{})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bridge: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bridge blocked waiting for a packfile a delete-only push never sends")
	}

	if len(proxy.rpcBodies) != 1 || string(proxy.rpcBodies[0]) != commands {
		t.Fatalf("push body = %q, want the command block alone", proxy.rpcBodies)
	}
}

// Under protocol v2 each of the client's commands is one stateless round, so
// the bridge maps command block to POST and response to stdout, in order,
// until the client says it has no more.
func TestBridgeFetch_RoundTripsProtocolV2(t *testing.T) {
	t.Setenv("GIT_PROTOCOL", "version=2")

	advert := pkt("version 2\n") + pkt("agent=git/2.44\n") + pkt("ls-refs=unborn\n") + pkt("fetch=shallow\n") + flush
	lsRefsResp := pkt("abc123 refs/heads/main\n") + flush
	fetchResp := pkt("packfile\n") + pkt("\x01PACK-bytes") + flush
	proxy := &recordingProxy{
		t:             t,
		advertisement: advert,
		rpcResponses:  []string{lsRefsResp, fetchResp},
	}
	srv := proxy.start()

	lsRefs := pkt("command=ls-refs\n") + pkt("agent=git/2.44\n") + "0001" + pkt("peel\n") + flush
	fetchCmd := pkt("command=fetch\n") + pkt("agent=git/2.44\n") + "0001" + pkt("want abc123\n") + pkt("done\n") + flush
	var stdout bytes.Buffer

	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: srv.URL, proxyToken: "run-token"}
	// The trailing bare flush is how a v2 client closes a session.
	stdin := strings.NewReader(lsRefs + fetchCmd + flush)
	if err := bridge(cfg, "git-upload-pack '/acme/widgets.git'", stdin, &stdout); err != nil {
		t.Fatalf("bridge: %v", err)
	}

	if stdout.String() != advert+lsRefsResp+fetchResp {
		t.Fatalf("stdout = %q, want the advertisement then both responses", stdout.String())
	}
	if len(proxy.rpcBodies) != 2 {
		t.Fatalf("proxy saw %d commands, want 2", len(proxy.rpcBodies))
	}
	if string(proxy.rpcBodies[0]) != lsRefs || string(proxy.rpcBodies[1]) != fetchCmd {
		t.Fatalf("command bodies = %q, want them relayed verbatim", proxy.rpcBodies)
	}
	// The header is smart HTTP's spelling of the GIT_PROTOCOL variable ssh
	// would have carried; without it the server answers in v0.
	for _, p := range proxy.rpcProtocols {
		if p != "version=2" {
			t.Fatalf("command announced Git-Protocol %q, want version=2", p)
		}
	}
}

func TestBridgeFetch_RefusesWithoutProtocolV2(t *testing.T) {
	t.Setenv("GIT_PROTOCOL", "")
	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: "http://127.0.0.1:1", proxyToken: "tok"}
	err := bridge(cfg, "git-upload-pack '/acme/widgets.git'", strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "protocol.version=2") {
		t.Fatalf("error = %v, want a refusal naming the required protocol version", err)
	}
}

func TestBridgeFetch_RefusesV0Advertisement(t *testing.T) {
	t.Setenv("GIT_PROTOCOL", "version=2")
	proxy := &recordingProxy{t: t, advertisement: pkt("abc123 refs/heads/main\x00agent=git/2.44\n") + flush}
	srv := proxy.start()

	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: srv.URL, proxyToken: "tok"}
	err := bridge(cfg, "git-upload-pack '/acme/widgets.git'", strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "v0 advertisement") {
		t.Fatalf("error = %v, want the version mismatch named", err)
	}
}

// The ref gate refuses a protected-branch push at the proxy, and its
// explanation is the only thing that tells the agent why. git prints our
// stderr, so the denial has to survive as the error text.
func TestBridgePush_SurfacesRefGateDenial(t *testing.T) {
	advert := pkt("de1e7e5 refs/heads/main\x00report-status\n") + flush
	proxy := &recordingProxy{t: t, advertisement: advert, rpcStatus: http.StatusForbidden}
	srv := proxy.start()

	commands := pkt("de1e7e5 abc123 refs/heads/main\x00report-status\n") + flush
	var stdout bytes.Buffer

	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: srv.URL, proxyToken: "tok"}
	err := bridge(cfg, "git-receive-pack '/acme/widgets.git'", strings.NewReader(commands+"PACK"), &stdout)
	if err == nil {
		t.Fatal("bridge reported success for a push the ref gate refused")
	}
	if !strings.Contains(err.Error(), "is protected") {
		t.Fatalf("error = %v, want the gate's own explanation", err)
	}
	if stdout.String() != advert {
		t.Fatalf("stdout = %q, want only the advertisement (no report-status for a refused push)", stdout.String())
	}
}

func TestBridge_SurfacesRepoDenialOnAdvertisement(t *testing.T) {
	proxy := &recordingProxy{t: t, advertStatus: http.StatusForbidden}
	srv := proxy.start()

	cfg := bridgeConfig{upstreamHost: "ghe.example.com", proxyURL: srv.URL, proxyToken: "tok"}
	err := bridge(cfg, "git-receive-pack '/acme/widgets.git'", strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not authorized for this run") {
		t.Fatalf("error = %v, want the proxy's denial relayed", err)
	}
}
