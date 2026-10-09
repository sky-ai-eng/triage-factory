package jira

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestPut400_ReturnsStatusError: a PUT failure is the same typed error a GET
// failure is, so callers can read its status and body.
func TestPut400_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorMessages":[],"errors":{"priority":"Priority name 'Urgent' is not valid"}}`))
	}))
	defer srv.Close()

	err := testClient(srv.URL).SetPriority(context.Background(), "SKY-1", "Urgent")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *StatusError", err, err)
	}
	if se.Method != "PUT" || se.Status != http.StatusBadRequest {
		t.Errorf("StatusError = %s %d, want PUT 400", se.Method, se.Status)
	}
	if se.Class != upstream.Rejected {
		t.Errorf("Class = %q, want rejected", se.Class)
	}
	if !strings.HasSuffix(err.Error(), "returned 400: Priority name 'Urgent' is not valid") {
		t.Errorf("Error() = %q, want the excerpt of Jira's errors map", err.Error())
	}
}

// TestPost400_ReturnsStatusError covers the two POST helpers.
func TestPost400_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorMessages":["Transition id '9' is not valid for this issue."],"errors":{}}`))
	}))
	defer srv.Close()
	c := testClient(srv.URL)

	err := c.doTransition(context.Background(), "SKY-1", "9")
	var se *StatusError
	if !errors.As(err, &se) || se.Method != "POST" || se.Status != http.StatusBadRequest {
		t.Fatalf("post: err = %v, want a POST 400 *StatusError", err)
	}
	if !strings.HasSuffix(err.Error(), "returned 400: Transition id '9' is not valid for this issue.") {
		t.Errorf("post: Error() = %q", err.Error())
	}

	_, err = c.AddComment(context.Background(), "SKY-1", "hello")
	if !errors.As(err, &se) || se.Method != "POST" {
		t.Fatalf("postJSON: err = %v, want a POST *StatusError", err)
	}
}

// epicServer rejects the first parent write with rejectBody, serves the
// Epic Link field from /field, and accepts everything after.
func epicServer(t *testing.T, rejectBody string) (*httptest.Server, *[]string) {
	t.Helper()
	var writes int32
	var payloads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/field") {
			_, _ = w.Write([]byte(`[{"id":"summary","schema":{}},{"id":"customfield_10100","schema":{"custom":"com.pyxis.greenhopper.jira:gh-epic-link"}}]`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		payloads = append(payloads, string(b))
		if atomic.AddInt32(&writes, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(rejectBody))
			return
		}
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"10002","key":"SKY-2"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, &payloads
}

// TestSetParent_EpicFallbackReadsBody: the Epic Link fallback triggers on
// Jira's error body, read from StatusError.Body, because the message carries
// only an excerpt of it.
func TestSetParent_EpicFallbackReadsBody(t *testing.T) {
	srv, payloads := epicServer(t, `{"errorMessages":[],"errors":{"customfield_10100":"gh.epic.error.not.found"}}`)

	if err := testClient(srv.URL).SetParent(context.Background(), "SKY-1", "SKY-9"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if len(*payloads) != 2 {
		t.Fatalf("writes = %d, want the parent write then the Epic Link write", len(*payloads))
	}
	var second struct {
		Fields map[string]any `json:"fields"`
	}
	if err := json.Unmarshal([]byte((*payloads)[1]), &second); err != nil {
		t.Fatalf("decode second write: %v", err)
	}
	if second.Fields["customfield_10100"] != "SKY-9" {
		t.Errorf("second write fields = %v, want the Epic Link field set to SKY-9", second.Fields)
	}
}

// TestCreateIssue_EpicFallbackReadsBody is the same fallback on create.
func TestCreateIssue_EpicFallbackReadsBody(t *testing.T) {
	srv, payloads := epicServer(t, `{"errorMessages":["gh.epic.error.not.supported"],"errors":{}}`)

	created, err := testClient(srv.URL).CreateIssue(context.Background(), "SKY", "Story", "s", "", "SKY-9", "")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if created.Key != "SKY-2" || created.ID != "10002" || len(*payloads) != 2 {
		t.Errorf("created = %+v after %d writes, want SKY-2 (10002) after 2", created, len(*payloads))
	}
}

// TestSetParent_UnrelatedFailureDoesNotFallBack: a failure whose body says
// nothing about the parent is returned as-is.
func TestSetParent_UnrelatedFailureDoesNotFallBack(t *testing.T) {
	srv, payloads := epicServer(t, `{"errorMessages":["You do not have permission to edit issues in this project."],"errors":{}}`)

	err := testClient(srv.URL).SetParent(context.Background(), "SKY-1", "SKY-9")
	if err == nil {
		t.Fatal("SetParent succeeded, want the first write's error")
	}
	if len(*payloads) != 1 {
		t.Errorf("writes = %d, want 1", len(*payloads))
	}
}

// TestErrorMessages_CarryAtMost200CharsOfBody: whatever the body, the message
// carries at most 200 characters of it — a long Jira message is truncated,
// and a proxy page contributes only its size.
func TestErrorMessages_CarryAtMost200CharsOfBody(t *testing.T) {
	long := strings.Repeat("x", 1000)
	cases := []struct {
		name string
		body string
	}{
		{"long JSON message", `{"errorMessages":["` + long + `"]}`},
		{"HTML page", "<html><body>" + long + "</body></html>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := testClient(srv.URL)

			for _, err := range []error{
				func() error { _, err := c.get(context.Background(), srv.URL+"/x"); return err }(),
				c.put(context.Background(), srv.URL+"/x", map[string]any{}),
				c.post(context.Background(), srv.URL+"/x", map[string]any{}),
			} {
				var se *StatusError
				if !errors.As(err, &se) {
					t.Fatalf("err = %v, want *StatusError", err)
				}
				if se.Body != tc.body {
					t.Errorf("Body = %d bytes, want the whole %d-byte body kept", len(se.Body), len(tc.body))
				}
				prefix := se.Method + " " + se.URL + " returned 400: "
				excerpt := strings.TrimPrefix(err.Error(), prefix)
				if n := strings.Count(excerpt, "x"); n > 200 {
					t.Errorf("message carries %d characters of body: %q", n, err.Error())
				}
				if strings.Contains(excerpt, "<") {
					t.Errorf("message carries markup: %q", err.Error())
				}
			}
		})
	}
}

// TestDoRequest_RecordsEveryAttempt: the retried 503 and the final 200 are
// both counted, under the org the resolver gave the client.
func TestDoRequest_RecordsEveryAttempt(t *testing.T) {
	shortBackoff(t)
	srv, _ := retryServer(t, nil, http.StatusServiceUnavailable)
	c := NewClient(DataCenterPAT(srv.URL, "tok")).WithOrg("org-1")

	ctx, tally := upstream.WithTally(context.Background())
	if _, err := c.get(ctx, srv.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	if tally.Attempts() != 2 || tally.Count(upstream.Transient) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally = %d attempts, %d transient, %d ok; want 2, 1, 1",
			tally.Attempts(), tally.Count(upstream.Transient), tally.Count(upstream.OK))
	}
}

// truncated answers 200 with a Content-Length longer than the body it writes,
// then drops the connection, so the client's body read fails after the
// headers arrived.
func truncated(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "1000")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"issues":[{"ke`))
	w.(http.Flusher).Flush()
	panic(http.ErrAbortHandler)
}

// TestDoRequest_TruncatedBodyRetriedLikeATransportFailure: a GET whose body
// breaks off mid-read is retried as a dropped connection is, and both
// attempts are counted.
func TestDoRequest_TruncatedBodyRetriedLikeATransportFailure(t *testing.T) {
	shortBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			truncated(w)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	ctx, tally := upstream.WithTally(context.Background())
	body, err := testClient(srv.URL).get(ctx, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(body) != `{"ok":true}` || atomic.LoadInt32(&calls) != 2 {
		t.Errorf("body = %s after %d attempts, want ok after 2", body, atomic.LoadInt32(&calls))
	}
	if tally.Count(upstream.Transient) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally: %d transient, %d ok; want 1, 1", tally.Count(upstream.Transient), tally.Count(upstream.OK))
	}
}

// TestDoRequest_TruncatedBodyMutationNotRetried: a mutation whose response
// breaks off is returned after one attempt, since Jira may have applied it.
func TestDoRequest_TruncatedBodyMutationNotRetried(t *testing.T) {
	shortBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		truncated(w)
	}))
	t.Cleanup(srv.Close)

	err := testClient(srv.URL).put(context.Background(), srv.URL, map[string]string{"k": "v"})
	var te *upstream.TransportError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want a *upstream.TransportError", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("attempts = %d, want 1: the write may have applied", got)
	}
}

// TestDoRequest_HTML403IsNotRetried: a 403 that is not Jira's own JSON (a
// proxy's page, or Data Center's login lockout) is Transient for counting,
// but it does not clear within a backoff, so even a GET returns it at once.
func TestDoRequest_HTML403IsNotRetried(t *testing.T) {
	shortBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><body>CAPTCHA required</body></html>"))
	}))
	defer srv.Close()

	ctx, tally := upstream.WithTally(context.Background())
	_, err := testClient(srv.URL).get(ctx, srv.URL)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.Class != upstream.Transient {
		t.Fatalf("err = %v, want a transient 403 *StatusError", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
	if tally.Count(upstream.Transient) != 1 {
		t.Errorf("tally: %d transient, want 1", tally.Count(upstream.Transient))
	}
}
