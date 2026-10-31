package reachability

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestProbe_ReachableAnyStatus proves the verdict is "host answered", not
// "host answered 2xx": a 401/403/404/5xx is still Reachable, because the URL
// stage is about network reach, not auth or health.
func TestProbe_ReachableAnyStatus(t *testing.T) {
	for _, code := range []int{200, 401, 403, 404, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		res := Probe(context.Background(), srv.URL)
		srv.Close()

		if !res.Reachable {
			t.Errorf("status %d: Reachable=false, want true (host answered)", code)
		}
		if res.Status != code {
			t.Errorf("status %d: Result.Status = %d, want %d", code, res.Status, code)
		}
		if res.Reason != "" {
			t.Errorf("status %d: Reason = %q, want empty on a reachable host", code, res.Reason)
		}
	}
}

// TestProbe_DoesNotFollowRedirect proves a 3xx is the verdict itself: the host
// answered (reachable), and the probe does NOT chase the Location into another
// (possibly internal) host.
func TestProbe_DoesNotFollowRedirect(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound) // 302
	}))
	defer srv.Close()

	res := Probe(context.Background(), srv.URL)
	if !res.Reachable {
		t.Errorf("Reachable=false on a 302, want true (host answered)")
	}
	if res.Status != http.StatusFound {
		t.Errorf("Status = %d, want 302 (redirect not followed)", res.Status)
	}
	if followed {
		t.Errorf("probe followed the redirect to /elsewhere; it must not")
	}
}

// TestProbe_UnreachableConnRefused points at a server that has been closed, so
// nothing listens — a pure-loopback connection refused, no DNS needed.
func TestProbe_UnreachableConnRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	res := Probe(context.Background(), addr)
	if res.Reachable {
		t.Fatalf("closed server: Reachable=true, want false")
	}
	if res.Status != 0 {
		t.Errorf("unreachable: Status = %d, want 0", res.Status)
	}
	if res.Reason == "" {
		t.Errorf("unreachable: Reason empty, want a non-empty cause")
	}
	if res.ProbedURL != addr {
		t.Errorf("ProbedURL = %q, want %q (echoed even when unreachable)", res.ProbedURL, addr)
	}
}

// TestProbe_InvalidURL: a value that can't be turned into a request is bad
// input, reported as not reachable with an "invalid URL" reason (the handler
// upstream turns malformed input into a 400 before reaching here).
func TestProbe_InvalidURL(t *testing.T) {
	res := Probe(context.Background(), "://nope")
	if res.Reachable {
		t.Fatalf("invalid URL: Reachable=true, want false")
	}
	if res.Reason != "invalid URL" {
		t.Errorf("invalid URL: Reason = %q, want %q", res.Reason, "invalid URL")
	}
}

// TestClassify exercises the reason buckets with synthetic errors so the DNS /
// timeout / generic branches are covered without depending on the network.
func TestClassify(t *testing.T) {
	dns := &url.Error{Op: "Get", URL: "https://x", Err: &net.DNSError{IsNotFound: true, Name: "x"}}
	if got := classify(dns); got != "could not resolve host" {
		t.Errorf("DNS error classified %q, want %q", got, "could not resolve host")
	}

	// A resolver timeout is still a name-resolution failure — DNS is checked
	// before the general timeout bucket, so this must NOT read "timed out"
	// even though *net.DNSError also satisfies net.Error with Timeout()==true.
	dnsTimeout := &url.Error{Op: "Get", URL: "https://x", Err: &net.DNSError{IsTimeout: true, Name: "x"}}
	if got := classify(dnsTimeout); got != "could not resolve host" {
		t.Errorf("DNS timeout classified %q, want %q", got, "could not resolve host")
	}

	to := &url.Error{Op: "Get", URL: "https://x", Err: timeoutErr{}}
	if got := classify(to); !strings.Contains(got, "timed out") {
		t.Errorf("timeout error classified %q, want it to mention 'timed out'", got)
	}

	refused := &url.Error{Op: "Get", URL: "https://x", Err: errors.New("connect: connection refused")}
	if got := classify(refused); got != "connect: connection refused" {
		t.Errorf("generic error classified %q, want the bare unwrapped cause", got)
	}
}

// timeoutErr is a net.Error whose Timeout() reports true — the shape a
// client-timeout or context-deadline transport failure presents as.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }
