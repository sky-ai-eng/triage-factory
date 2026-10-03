package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestClassifyResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Class
	}{
		{"200", 200, `{"ok":true}`, OK},
		{"204", 204, "", OK},
		{"304 not modified", 304, "", OK},
		{"429", 429, `{"message":"slow down"}`, RateLimited},
		{"429 with HTML", 429, "<html>slow down</html>", RateLimited},
		{"408", 408, "", Transient},
		{"500", 500, `{"message":"boom"}`, Transient},
		{"502 proxy page", 502, "<html>bad gateway</html>", Transient},
		{"503", 503, "", Transient},
		{"401", 401, `{"message":"Bad credentials"}`, Auth},
		{"401 empty", 401, "", Auth},
		{"403 JSON", 403, `{"message":"Resource not accessible by integration"}`, Auth},
		{"403 JSON with whitespace", 403, "\n  {\"errorMessages\":[\"no\"]}\n", Auth},
		{"403 HTML", 403, "<!DOCTYPE html><html><body>Access denied</body></html>", Transient},
		{"403 empty", 403, "", Transient},
		{"403 JSON array", 403, `["no"]`, Transient},
		{"403 JSON null", 403, `null`, Transient},
		{"403 truncated JSON", 403, `{"message":"no`, Transient},
		{"400", 400, `{"message":"bad"}`, Rejected},
		{"404", 404, `{"message":"Not Found"}`, Rejected},
		{"409", 409, "", Rejected},
		{"422", 422, `{"message":"Validation Failed"}`, Rejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A Content-Type that disagrees with the body must not matter.
			h := http.Header{"Content-Type": []string{"application/json"}}
			if got := ClassifyResponse(tc.status, h, []byte(tc.body)); got != tc.want {
				t.Errorf("ClassifyResponse(%d, %q) = %q, want %q", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// transportErrors produces the failures http.Client.Do actually returns for
// the four ways a connection goes wrong. Refused and timeout are real; reset
// and DNS are built in the shapes the net package produces, since neither can
// be provoked reliably in a sandbox.
func transportErrors(t *testing.T) map[string]error {
	t.Helper()
	noProxy := &http.Transport{Proxy: nil}
	t.Cleanup(noProxy.CloseIdleConnections)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := ln.Addr().String()
	_ = ln.Close()
	_, refused := (&http.Client{Transport: noProxy}).Get("http://" + closedAddr + "/")

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	_, timeout := (&http.Client{Transport: noProxy, Timeout: 20 * time.Millisecond}).Get(slow.URL)

	reset := &url.Error{Op: "Get", URL: "https://api.github.com/x", Err: &net.OpError{
		Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET),
	}}
	dns := &url.Error{Op: "Get", URL: "https://jira.corp.example/x", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "jira.corp.example", IsNotFound: true},
	}}

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsSrv.Close)
	_, untrusted := (&http.Client{Transport: noProxy}).Get(tlsSrv.URL)
	trusting := tlsSrv.Client()
	trusting.Transport.(*http.Transport).Proxy = nil
	_, wrongHost := trusting.Get(strings.Replace(tlsSrv.URL, "127.0.0.1", "localhost", 1))

	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(plain.Close)
	_, notTLS := (&http.Client{Transport: noProxy}).Get(strings.Replace(plain.URL, "http://", "https://", 1))

	errs := map[string]error{
		"refused":               refused,
		"timeout":               timeout,
		"reset":                 reset,
		"dns":                   dns,
		"untrusted cert":        untrusted,
		"cert for another host": wrongHost,
		"server without TLS":    notTLS,
	}
	for name, e := range errs {
		if e == nil {
			t.Fatalf("%s: request unexpectedly succeeded", name)
		}
	}
	return errs
}

func TestClassifyTransport(t *testing.T) {
	for name, err := range transportErrors(t) {
		t.Run(name, func(t *testing.T) {
			got, ok := ClassifyTransport(context.Background(), err)
			if !ok || got != Transient {
				t.Errorf("ClassifyTransport(%v) = (%q, %v), want (transient, true)", err, got, ok)
			}
		})
	}

	t.Run("cancelled ctx is not counted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/", nil)
		_, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
		if got, ok := ClassifyTransport(ctx, err); ok {
			t.Errorf("ClassifyTransport after cancel = (%q, true), want not counted", got)
		}
	})

	t.Run("expired ctx deadline is not counted", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		if got, ok := ClassifyTransport(ctx, context.DeadlineExceeded); ok {
			t.Errorf("ClassifyTransport past the caller's deadline = (%q, true), want not counted", got)
		}
	})
}

type classifiedErr struct{ c Class }

func (e *classifiedErr) Error() string        { return "classified" }
func (e *classifiedErr) UpstreamClass() Class { return e.c }

func TestClassOf(t *testing.T) {
	for name, err := range transportErrors(t) {
		t.Run(name, func(t *testing.T) {
			wrapped := fmt.Errorf("request /x: %w", err)
			if got, ok := ClassOf(wrapped); !ok || got != Transient {
				t.Errorf("ClassOf(%v) = (%q, %v), want (transient, true)", wrapped, got, ok)
			}
		})
	}

	cases := []struct {
		name   string
		err    error
		want   Class
		wantOK bool
	}{
		{"classified", fmt.Errorf("wrap: %w", &classifiedErr{Auth}), Auth, true},
		{"bare net error", &net.OpError{Op: "dial", Err: errors.New("refused")}, Transient, true},
		{"cancellation", &url.Error{Op: "Get", URL: "u", Err: context.Canceled}, "", false},
		{"local failure", errors.New("parse response: unexpected end of JSON input"), "", false},
		{"nil", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ClassOf(tc.err)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("ClassOf = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		c          Class
		idempotent bool
		want       bool
	}{
		{RateLimited, false, true},
		{RateLimited, true, true},
		{Transient, true, true},
		{Transient, false, false},
		{Auth, true, false},
		{Rejected, true, false},
		{OK, true, false},
	}
	for _, tc := range cases {
		if got := Retryable(tc.c, tc.idempotent); got != tc.want {
			t.Errorf("Retryable(%q, idempotent=%v) = %v, want %v", tc.c, tc.idempotent, got, tc.want)
		}
	}
}

func TestRetryableResponse(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		c          Class
		idempotent bool
		want       bool
	}{
		{"503 on a GET", 503, Transient, true, true},
		{"503 on a mutation", 503, Transient, false, false},
		{"408 on a GET", 408, Transient, true, true},
		{"non-JSON 403 on a GET", 403, Transient, true, false},
		{"rate-limit 403", 403, RateLimited, false, true},
		{"429", 429, RateLimited, false, true},
		{"JSON 403", 403, Auth, true, false},
		{"404", 404, Rejected, true, false},
	}
	for _, tc := range cases {
		if got := RetryableResponse(tc.status, tc.c, tc.idempotent); got != tc.want {
			t.Errorf("%s: RetryableResponse = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRetryableTransport: a connection that was refused, reset or not
// resolved is worth another attempt; a TLS failure recurs on every attempt
// and a timeout has already spent the client's budget, so neither is.
func TestRetryableTransport(t *testing.T) {
	want := map[string]bool{
		"refused":               true,
		"reset":                 true,
		"dns":                   true,
		"timeout":               false,
		"untrusted cert":        false,
		"cert for another host": false,
		"server without TLS":    false,
	}
	errs := transportErrors(t)
	if len(errs) != len(want) {
		t.Fatalf("transportErrors has %d cases, the table %d", len(errs), len(want))
	}
	for name, err := range errs {
		if got := RetryableTransport(err, true); got != want[name] {
			t.Errorf("%s: RetryableTransport(%v) = %v, want %v", name, err, got, want[name])
		}
		if RetryableTransport(err, false) {
			t.Errorf("%s: a non-idempotent request must never be retried after a transport failure", name)
		}
	}
}

// TestRetryAfter covers both header forms, and pins that a non-positive or
// past value is reported absent: honoring it as "wait zero" would have a
// client spin against the upstream on every bounded retry.
func TestRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		wantOK bool
		approx time.Duration
	}{
		{"absent", "", false, 0},
		{"seconds", "5", true, 5 * time.Second},
		{"zero", "0", false, 0},
		{"negative", "-3", false, 0},
		{"http-date-future", time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat), true, 10 * time.Second},
		{"http-date-past", time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat), false, 0},
		{"junk", "soon", false, 0},
		{"a week is reported as the bound", "604800", true, maxRetryAfter},
		{"too large to convert is reported as the bound", "9999999999", true, maxRetryAfter},
		{"http-date past the bound", time.Now().Add(400 * 24 * time.Hour).UTC().Format(http.TimeFormat), true, maxRetryAfter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			d, ok := RetryAfter(h)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && (d < tc.approx-2*time.Second || d > tc.approx+time.Second) {
				t.Errorf("duration = %v, want ~%v", d, tc.approx)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	base, max := time.Second, 30*time.Second
	for attempt, want := range map[int]time.Duration{
		0:   time.Second,
		1:   time.Second,
		2:   2 * time.Second,
		3:   4 * time.Second,
		5:   16 * time.Second,
		6:   30 * time.Second,
		34:  30 * time.Second,
		35:  30 * time.Second,
		40:  30 * time.Second,
		62:  30 * time.Second,
		63:  30 * time.Second,
		100: 30 * time.Second,
	} {
		if got := Backoff(attempt, base, max); got != want {
			t.Errorf("Backoff(%d, 1s, 30s) = %v, want %v", attempt, got, want)
		}
	}
	if got := Backoff(1, time.Minute, time.Second); got != time.Second {
		t.Errorf("a base over the cap = %v, want the cap", got)
	}
	// With the largest cap, every attempt is either an exact power of two
	// times base or the cap: a shift that overflowed would land below it.
	for attempt := 1; attempt <= 100; attempt++ {
		got := Backoff(attempt, time.Second, math.MaxInt64)
		if got <= 0 || (got != math.MaxInt64 && got != time.Second<<uint(attempt-1)) {
			t.Fatalf("Backoff(%d, 1s, max) = %v, an overflowed value", attempt, got)
		}
	}
}

func TestSleep(t *testing.T) {
	if err := Sleep(context.Background(), 0); err != nil {
		t.Errorf("Sleep(0) = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on a cancelled ctx = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Error("Sleep did not return promptly on a cancelled ctx")
	}
}

// endless yields bytes forever, counting how many it handed out.
type endless struct{ n int }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.n += len(p)
	return len(p), nil
}

func TestReadErrorBody_StopsAtCap(t *testing.T) {
	src := &endless{}
	got, err := ReadErrorBody(src)
	if err != nil {
		t.Fatalf("ReadErrorBody: %v", err)
	}
	if len(got) != MaxErrorBody {
		t.Errorf("read %d bytes, want %d", len(got), MaxErrorBody)
	}
	if src.n > MaxErrorBody+32<<10 {
		t.Errorf("pulled %d bytes from the source, want it to stop near the %d cap", src.n, MaxErrorBody)
	}

	small, err := ReadErrorBody(strings.NewReader("short"))
	if err != nil || string(small) != "short" {
		t.Errorf("ReadErrorBody(short) = (%q, %v)", small, err)
	}

	_, err = ReadErrorBody(io.MultiReader(strings.NewReader("ab"), errReader{}))
	if err == nil {
		t.Error("a read error must surface")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestExcerpt(t *testing.T) {
	long := strings.Repeat("é", 300)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"GitHub message", `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`, "Bad credentials"},
		{"GitHub validation with an error message", `{"message":"Validation Failed","errors":[{"code":"custom","message":"No commits"}]}`, "Validation Failed: No commits"},
		{"GitHub validation with a field error", `{"message":"Validation Failed","errors":[{"resource":"Issue","field":"title","code":"missing_field"}],"documentation_url":"https://docs.github.com"}`, "Validation Failed: missing_field field 'title'"},
		{"GitHub validation with a code only", `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"already_exists"}]}`, "Validation Failed: already_exists"},
		{"detail already in the message", `{"message":"No commits between main and x","errors":[{"message":"No commits between main and x"}]}`, "No commits between main and x"},
		{"Jira errorMessages with field errors", `{"errorMessages":["Issue could not be updated."],"errors":{"priority":"bad priority"}}`, "Issue could not be updated.: bad priority"},
		{"Jira errorMessages", `{"errorMessages":["Issue does not exist or you do not have permission to see it."],"errors":{}}`, "Issue does not exist or you do not have permission to see it."},
		{"Jira errors map, first in document order", `{"errorMessages":[],"errors":{"summary":"You must specify a summary.","priority":"bad priority"}}`, "You must specify a summary."},
		{"errors array of strings", `{"errors":["first","second"]}`, "first"},
		{"errors array of objects", `{"errors":[{"message":"thing failed"}]}`, "thing failed"},
		{"Slack error", `{"ok":false,"error":"invalid_auth"}`, "invalid_auth"},
		{"OAuth error with description", `{"error":"invalid_grant","error_description":"Invalid login credentials"}`, "Invalid login credentials"},
		{"OAuth error without description", `{"error":"invalid_grant"}`, "invalid_grant"},
		{"GoTrue msg", `{"code":400,"error_code":"validation_failed","msg":"metadata_url is invalid"}`, "metadata_url is invalid"},
		{"whitespace is collapsed", `{"message":"line one\n\tline two"}`, "line one line two"},
		{"control characters are dropped", `{"message":"\u001b[31mred\u001b[0m alert\u0007 \u009b2Jdone"}`, "[31mred[0m alert 2Jdone"},
		{"oversized JSON message", `{"message":"` + long + `"}`, strings.Repeat("é", 200) + "…"},
		{"empty message falls through", `{"message":"","error":"ratelimited"}`, "ratelimited"},
		{"no known key", `{"detail":"something"}`, "JSON body with no error message, 22 bytes"},
		{"HTML page", "<!DOCTYPE html><html><body>Access denied</body></html>", "non-JSON body, 54 bytes"},
		{"JSON array", `["no"]`, "non-JSON body, 6 bytes"},
		{"empty", "", "empty body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Excerpt([]byte(tc.body)); got != tc.want {
				t.Errorf("Excerpt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTally_CountsUnderConcurrency runs with -race in CI: tracker discovery
// records from many goroutines into one cycle's tally.
func TestTally_CountsUnderConcurrency(t *testing.T) {
	ctx, tally := WithTally(context.Background())
	const workers, each = 16, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c := OK
				if i%5 == 0 {
					c = Transient
				}
				Record(ctx, GitHub, "org-1", c)
				_ = tally.Count(OK)
			}
		}(w)
	}
	wg.Wait()

	if got := tally.Attempts(); got != workers*each {
		t.Errorf("Attempts = %d, want %d", got, workers*each)
	}
	if got := tally.Count(Transient); got != workers*each/5 {
		t.Errorf("Count(transient) = %d, want %d", got, workers*each/5)
	}
	if got := tally.Count(OK); got != workers*each*4/5 {
		t.Errorf("Count(ok) = %d, want %d", got, workers*each*4/5)
	}
	if got := tally.Count(Auth); got != 0 {
		t.Errorf("Count(auth) = %d, want 0", got)
	}
}

func TestTally_ScopedToItsContext(t *testing.T) {
	outer, outerTally := WithTally(context.Background())
	inner, innerTally := WithTally(outer)
	Record(outer, Jira, "", OK)
	Record(inner, Jira, "", Auth)
	Record(context.Background(), Jira, "", OK)
	RecordRetry(inner, Jira, "", Transient)

	if outerTally.Attempts() != 1 || outerTally.Count(OK) != 1 {
		t.Errorf("outer tally = %d attempts, want only its own one", outerTally.Attempts())
	}
	if innerTally.Attempts() != 1 || innerTally.Count(Auth) != 1 {
		t.Errorf("inner tally = %d attempts, want only its own one", innerTally.Attempts())
	}
}
