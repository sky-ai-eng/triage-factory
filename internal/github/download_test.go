package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
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

	// Typed error: callers (e.g., the download-logs fallback path that
	// needs to distinguish "run not finished yet" from real errors) must
	// be able to discriminate status codes via errors.As. Regression guard
	// for a previous regression where DownloadArtifact wrapped the status
	// in fmt.Errorf and forced callers to string-match for 404.
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected error to be *HTTPError, got %T: %v", err, err)
	}
	if he.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", he.StatusCode, http.StatusNotFound)
	}
}

// TestDownloadArtifact_TruncatedBody: a download whose body breaks off is not
// retried, since part of it is already in dst, but the error classifies as
// the transport failure it is, and the attempt is counted as one.
func TestDownloadArtifact_TruncatedBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		truncated(w, http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, tally := upstream.WithTally(context.Background())
	var dst bytes.Buffer
	_, err := clientAgainst(srv.URL).DownloadArtifact(ctx, "/logs", &dst, 1<<20)
	if class, ok := upstream.ClassOf(err); !ok || class != upstream.Transient {
		t.Fatalf("ClassOf(%v) = (%q, %v), want transient", err, class, ok)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1: the body was already being written to dst", got)
	}
	if tally.Attempts() != 1 || tally.Count(upstream.Transient) != 1 {
		t.Errorf("tally = %d attempts, %d transient; want 1, 1", tally.Attempts(), tally.Count(upstream.Transient))
	}
}

// failingWriter refuses every write, standing in for a full disk.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// TestDownloadArtifact_CountedOnceAsOK: a download GitHub served in full is
// one ok attempt, whether it was read to its end, refused by the size cap
// before a byte was read, or abandoned because dst failed. A failed write is
// the caller's fault, so its error does not classify as an upstream one.
func TestDownloadArtifact_CountedOnceAsOK(t *testing.T) {
	payload := []byte("pretend this is a zip archive")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name     string
		dst      io.Writer
		maxBytes int64
		wantErr  bool
	}{
		{name: "read to the end", dst: &bytes.Buffer{}, maxBytes: 1024},
		{name: "over the cap", dst: &bytes.Buffer{}, maxBytes: 4, wantErr: true},
		{name: "dst write fails", dst: failingWriter{}, maxBytes: 1024, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, tally := upstream.WithTally(context.Background())
			_, err := clientAgainst(srv.URL).DownloadArtifact(ctx, "/logs", tc.dst, tc.maxBytes)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if class, ok := upstream.ClassOf(err); ok {
				t.Errorf("ClassOf(%v) = %q, want no upstream class", err, class)
			}
			if tally.Attempts() != 1 || tally.Count(upstream.OK) != 1 {
				t.Errorf("tally = %d attempts, %d ok; want 1, 1", tally.Attempts(), tally.Count(upstream.OK))
			}
		})
	}
}
