package github

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clientAgainst wires a Client at a specific test server's base URL. The
// stock NewClient hardcodes github.com → api.github.com rewriting, which
// we don't want in tests — we point directly at the httptest server.
func clientAgainst(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		pat:     "test-token",
		http:    http.DefaultClient,
	}
}

func TestDownloadArtifact_SuccessfulDownload(t *testing.T) {
	payload := []byte("hello, world — this pretends to be a zip archive")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing or wrong Authorization header: %q", got)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	var dst bytes.Buffer
	n, err := c.DownloadArtifact(context.Background(), "/anywhere", &dst, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("bytes written = %d, want %d", n, len(payload))
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Errorf("body mismatch: got %q, want %q", dst.String(), string(payload))
	}
}

// TestDownloadArtifact_FollowsRedirect simulates GitHub's actual flow: the
// logs endpoint returns a 302 to a signed URL, and we should transparently
// follow to the second server and stream its body.
//
// Note on the cross-origin auth-strip behavior: Go's stdlib strips
// Authorization/Cookie headers on redirects to a different host (not a
// subdomain). In production that happens when api.github.com redirects to
// pipelines.actions.githubusercontent.com — different hosts, header gets
// stripped, signed URL accepts the anonymous request, everything works.
//
// We cannot reproduce that in a unit test: httptest.NewServer always binds
// to 127.0.0.1 with a fresh port, so two test servers share a hostname and
// stdlib considers them same-origin. The assertion we'd *want* to make —
// "signed URL receives no Authorization header" — would pass in prod and
// fail in test purely because of loopback semantics. Relying on the stdlib
// documentation for that guarantee; this test just verifies the
// redirect-follow path works and the final body is returned correctly.
func TestDownloadArtifact_FollowsRedirect(t *testing.T) {
	payload := []byte("signed URL body content")

	// Signed-URL server. Accepts any request (in prod, this is where the
	// stripped-auth request lands; in test, stdlib forwards the Bearer
	// token because both servers are same-host, and that's fine for the
	// assertion we can actually make).
	signedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(signedSrv.Close)

	// Primary server. Returns a 302 to the signed URL.
	primarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("primary should see Bearer token, got %q", got)
		}
		http.Redirect(w, r, signedSrv.URL+"/signed-blob", http.StatusFound)
	}))
	t.Cleanup(primarySrv.Close)

	c := clientAgainst(primarySrv.URL)
	var dst bytes.Buffer
	n, err := c.DownloadArtifact(context.Background(), "/repos/foo/bar/actions/runs/42/logs", &dst, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("bytes = %d, want %d", n, len(payload))
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Errorf("body mismatch: got %q, want %q", dst.String(), string(payload))
	}
}

// TestDownloadArtifact_ContentLengthExceedsCap verifies the pre-flight cap
// check — we should refuse to read a single byte when the server advertises
// a Content-Length larger than our cap. This is the fast path; the
// runtime check below catches servers that lie or omit the header.
func TestDownloadArtifact_ContentLengthExceedsCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Claim 10 KB; we don't care what body we send because the cap
		// check should fire before we touch it.
		w.Header().Set("Content-Length", "10240")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("A"), 10240))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	var dst bytes.Buffer
	_, err := c.DownloadArtifact(context.Background(), "/whatever", &dst, 1024)
	if err == nil {
		t.Fatal("expected cap-exceeded error, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should mention size, got: %v", err)
	}
}

// TestDownloadArtifact_StreamOverflowWithoutContentLength covers the
// belt-and-suspenders runtime cap: if Content-Length is missing (or wrong),
// io.LimitReader catches content that streams past the cap.
func TestDownloadArtifact_StreamOverflowWithoutContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Chunked transfer — no Content-Length advertised.
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 10; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("B"), 512))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	var dst bytes.Buffer
	_, err := c.DownloadArtifact(context.Background(), "/whatever", &dst, 1024)
	if err == nil {
		t.Fatal("expected runtime cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should mention size, got: %v", err)
	}
}

// countingTransport is a test RoundTripper that delegates to another
// transport and counts the requests it sees. Used to verify that
// DownloadArtifact actually uses the Transport configured on c.http
// rather than constructing a fresh http.Client that ignores it.
type countingTransport struct {
	inner http.RoundTripper
	calls int
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls++
	return c.inner.RoundTrip(req)
}

// TestDownloadArtifact_InheritsClientTransport is the regression guard
// for the "fresh http.Client ignores c.http configuration" bug. If
// DownloadArtifact creates its own http.Client without cloning c.http,
// any custom Transport attached to c.http (corporate proxy, GHES root
// CA bundle, etc.) would be silently dropped — downloads would work in
// dev but break in production. The test installs a counting Transport
// on c.http and verifies the download path routes through it.
func TestDownloadArtifact_InheritsClientTransport(t *testing.T) {
	payload := []byte("inherited transport body")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	counter := &countingTransport{inner: http.DefaultTransport}
	c := &Client{
		baseURL: srv.URL,
		pat:     "test-token",
		http:    &http.Client{Transport: counter},
	}

	var dst bytes.Buffer
	if _, err := c.DownloadArtifact(context.Background(), "/anywhere", &dst, 1024); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if counter.calls != 1 {
		t.Errorf("expected 1 RoundTrip through the custom transport, got %d — DownloadArtifact is not inheriting c.http.Transport", counter.calls)
	}
}

// TestDownloadArtifact_OverridesTimeoutWithoutMutatingClient verifies
// that the shallow-copy approach doesn't mutate the shared client. If
// DownloadArtifact ever regressed to setting Timeout on c.http directly
// (instead of on a local copy), the original client's timeout would
// leak to subsequent callers — observably as "my next 30-second API
// call now has a 15-minute timeout."
func TestDownloadArtifact_OverridesTimeoutWithoutMutatingClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data"))
	}))
	t.Cleanup(srv.Close)

	shared := &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   30 * time.Second,
	}
	c := &Client{baseURL: srv.URL, pat: "test-token", http: shared}

	var dst bytes.Buffer
	if _, err := c.DownloadArtifact(context.Background(), "/anywhere", &dst, 1024); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if shared.Timeout != 30*time.Second {
		t.Errorf("shared client Timeout was mutated: got %v, want 30s", shared.Timeout)
	}
}

func TestDownloadArtifact_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	var dst bytes.Buffer
	_, err := c.DownloadArtifact(context.Background(), "/missing", &dst, 1024)
	if err == nil {
		t.Fatal("expected 404 error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should include status code, got: %v", err)
	}
}
