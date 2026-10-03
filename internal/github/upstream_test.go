package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestGet_RetriesTransientStatusThenSucceeds: a GET that meets a 503 is
// retried, and the retry's 200 is the call's result. Both attempts are
// counted, with the classes they ended in.
func TestGet_RetriesTransientStatusThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("<html>upstream unavailable</html>"))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx, tally := upstream.WithTally(context.Background())
	data, err := clientAgainst(srv.URL).Get(ctx, "/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("body = %q, want the retry's", data)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream requests = %d, want 2", got)
	}
	if tally.Attempts() != 2 || tally.Count(upstream.Transient) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally = %d attempts, %d transient, %d ok; want 2, 1, 1",
			tally.Attempts(), tally.Count(upstream.Transient), tally.Count(upstream.OK))
	}
}

// failFirstTransport fails its first round trip the way a dropped connection
// does, then delegates.
type failFirstTransport struct {
	calls int32
	next  http.RoundTripper
}

func (f *failFirstTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&f.calls, 1) == 1 {
		return nil, errors.New("read: connection reset by peer")
	}
	return f.next.RoundTrip(r)
}

// TestGet_RetriesTransportErrorThenSucceeds: a GET whose first attempt dies
// in transport is retried.
func TestGet_RetriesTransportErrorThenSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tr := &failFirstTransport{next: http.DefaultTransport}
	c := clientAgainst(srv.URL)
	c.http = &http.Client{Transport: tr}

	ctx, tally := upstream.WithTally(context.Background())
	if _, err := c.Get(ctx, "/x"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := atomic.LoadInt32(&tr.calls); got != 2 {
		t.Errorf("round trips = %d, want 2", got)
	}
	if tally.Count(upstream.Transient) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally: %d transient, %d ok; want 1, 1", tally.Count(upstream.Transient), tally.Count(upstream.OK))
	}
}

// TestGet_TransientRetriesAreBounded: a GET that only ever meets 503 stops
// after the initial attempt plus maxRateLimitRetries retries and returns the
// last response as a Transient *HTTPError.
func TestGet_TransientRetriesAreBounded(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := clientAgainst(srv.URL).Get(context.Background(), "/x")
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusBadGateway {
		t.Fatalf("err = %v, want a 502 *HTTPError", err)
	}
	if he.Class != upstream.Transient {
		t.Errorf("Class = %q, want transient", he.Class)
	}
	if got, want := atomic.LoadInt32(&calls), int32(1+maxRateLimitRetries); got != want {
		t.Errorf("upstream requests = %d, want %d", got, want)
	}
}

// TestMutation_TransientIsNotRetried: a 503 on a POST may have followed the
// write, so it is surfaced after one attempt.
func TestMutation_TransientIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := clientAgainst(srv.URL).Post(context.Background(), "/repos/o/r/issues/1/comments", map[string]any{"body": "x"})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503 *HTTPError", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}

// TestHTML403_IsTransientAndRendersSizeOnly is the GHES-behind-a-proxy case:
// a 403 whose body is an HTML page came from the proxy, not GitHub. It is
// classified Transient (and retried, being a GET), and the message carries
// the page's size, never its markup.
func TestHTML403_IsTransientAndRendersSizeOnly(t *testing.T) {
	page := "<!DOCTYPE html><html><body><h1>Access denied</h1><p>Connect to the corporate VPN.</p></body></html>"
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	_, err := clientAgainst(srv.URL).Get(context.Background(), "/repos/o/r/pulls")
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *HTTPError", err)
	}
	if he.Class != upstream.Transient {
		t.Errorf("Class = %q, want transient", he.Class)
	}
	if c, ok := upstream.ClassOf(err); !ok || c != upstream.Transient {
		t.Errorf("ClassOf = (%q, %v), want (transient, true)", c, ok)
	}
	want := "GET /repos/o/r/pulls returned 403: non-JSON body, " + strconv.Itoa(len(page)) + " bytes"
	if he.Error() != want {
		t.Errorf("Error() = %q, want %q", he.Error(), want)
	}
	if strings.Contains(he.Error(), "<") {
		t.Errorf("message carries markup: %q", he.Error())
	}
	if he.Body != page {
		t.Errorf("Body = %q, want the page kept for callers that parse it", he.Body)
	}
	if got, want := atomic.LoadInt32(&calls), int32(1+maxRateLimitRetries); got != want {
		t.Errorf("upstream requests = %d, want %d", got, want)
	}
}

// TestJSON403_IsAuthAndNotRetried: a 403 GitHub itself wrote, with no
// rate-limit signal, is a permissions failure.
func TestJSON403_IsAuthAndNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()

	_, err := clientAgainst(srv.URL).Get(context.Background(), "/x")
	var he *HTTPError
	if !errors.As(err, &he) || he.Class != upstream.Auth {
		t.Fatalf("err = %v, want an auth-class *HTTPError", err)
	}
	if want := "GET /x returned 403: Resource not accessible by integration"; he.Error() != want {
		t.Errorf("Error() = %q, want %q", he.Error(), want)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}

// TestLiftValidationErr_ReadsTypedBody: the 422 envelope is lifted from
// HTTPError.Body, since the message no longer carries the body.
func TestLiftValidationErr_ReadsTypedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"No commits between main and feature/x"},{"resource":"PullRequest","code":"invalid","field":"base"}]}`))
	}))
	defer srv.Close()

	_, err := clientAgainst(srv.URL).Post(context.Background(), "/repos/o/r/pulls", map[string]any{})
	got := liftValidationErr(err)
	if want := "Validation Failed: No commits between main and feature/x: invalid field 'base'"; got == nil || got.Error() != want {
		t.Errorf("liftValidationErr = %v, want %q", got, want)
	}

	plain := errors.New("request /repos/o/r/pulls: dial tcp: connection refused")
	if got := liftValidationErr(plain); got != plain {
		t.Errorf("a non-HTTP error must pass through unchanged, got %v", got)
	}
}

// TestGetFileContent_404IsAbsent and TestDismissReview_422 pin the two
// status checks that read StatusCode rather than the message.
func TestGetFileContent_404IsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	got, err := clientAgainst(srv.URL).GetFileContent(context.Background(), "o", "r", "CLAUDE.md")
	if err != nil || got != "" {
		t.Errorf("GetFileContent = (%q, %v), want (\"\", nil) for a 404", got, err)
	}
}

func TestDismissReview_422(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Can not dismiss a commented pull request review"}`))
	}))
	defer srv.Close()

	err := clientAgainst(srv.URL).DismissReview(context.Background(), "o", "r", 1, 2, "stale")
	if err == nil || !strings.Contains(err.Error(), "only APPROVED or CHANGES_REQUESTED") {
		t.Errorf("DismissReview = %v, want the commented-review explanation", err)
	}
}
