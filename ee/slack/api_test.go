package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// withFakeSlackAPI points slackAPIBase at a local httptest server for the
// duration of the test, restoring the original on cleanup. Same swap-and-
// restore seam workspaces_pg_test.go uses.
func withFakeSlackAPI(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	orig := slackAPIBase
	slackAPIBase = srv.URL
	t.Cleanup(func() { slackAPIBase = orig })
	return srv
}

// TestSlackUsersInfo_GoldenDecode covers the happy path: the user id lands
// in the query string, the bot token in the Authorization header, and the
// profile fields decode from their nested users.info shape.
func TestSlackUsersInfo_GoldenDecode(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("user"); got != "U0MENTION1" {
			t.Errorf("user query param = %q; want U0MENTION1", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Errorf("Authorization header = %q; want Bearer xoxb-test", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"user": map[string]any{
				"is_bot":  false,
				"deleted": false,
				"profile": map[string]any{
					"email":        "ada@example.com",
					"real_name":    "Ada Lovelace",
					"display_name": "ada",
				},
			},
		})
	})

	got, err := slackUsersInfo(context.Background(), srv.Client(), "org-1", "xoxb-test", "U0MENTION1")
	if err != nil {
		t.Fatalf("slackUsersInfo: %v", err)
	}
	if got.Email != "ada@example.com" || got.RealName != "Ada Lovelace" || got.DisplayName != "ada" {
		t.Errorf("got = %+v; want the seeded profile", got)
	}
	if got.IsBot || got.Deleted {
		t.Errorf("got = %+v; want IsBot=false Deleted=false", got)
	}
}

// TestSlackUsersInfo_NotOk covers Slack's {"ok":false} error convention —
// a 200 response that is still a failure.
func TestSlackUsersInfo_NotOk(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
	})

	_, err := slackUsersInfo(context.Background(), srv.Client(), "org-1", "xoxb-test", "U0GONE0001")
	if err == nil {
		t.Fatal("slackUsersInfo with {ok:false} should return an error")
	}
}

// TestSlackUsersInfo_HTTPError covers a non-2xx transport-level failure
// (rate limit, upstream outage) — distinct from the {"ok":false}
// application-level convention.
func TestSlackUsersInfo_HTTPError(t *testing.T) {
	recordSlackWaits(t)
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := slackUsersInfo(context.Background(), srv.Client(), "org-1", "xoxb-test", "U0THROTTL1")
	if err == nil {
		t.Fatal("slackUsersInfo with HTTP 429 should return an error")
	}
}

// TestSlackUsersInfo_BotAndDeletedFlags confirms is_bot/deleted decode even
// when the profile carries no email — the resolver's short-circuit inputs.
func TestSlackUsersInfo_BotAndDeletedFlags(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"user": map[string]any{
				"is_bot":  true,
				"deleted": true,
				"profile": map[string]any{},
			},
		})
	})

	got, err := slackUsersInfo(context.Background(), srv.Client(), "org-1", "xoxb-test", "U0BOT00001")
	if err != nil {
		t.Fatalf("slackUsersInfo: %v", err)
	}
	if !got.IsBot || !got.Deleted {
		t.Errorf("got = %+v; want IsBot=true Deleted=true", got)
	}
	if got.Email != "" {
		t.Errorf("Email = %q; want empty", got.Email)
	}
}

// TestSlackConversationsInfo_GoldenDecode covers the happy path: the
// channel id lands in the query string, the bot token in the Authorization
// header, and the name decodes from its nested conversations.info shape.
func TestSlackConversationsInfo_GoldenDecode(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("channel"); got != "C0MENTION1" {
			t.Errorf("channel query param = %q; want C0MENTION1", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Errorf("Authorization header = %q; want Bearer xoxb-test", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": map[string]any{"name": "general"},
		})
	})

	got, err := slackConversationsInfo(context.Background(), srv.Client(), "org-1", "xoxb-test", "C0MENTION1")
	if err != nil {
		t.Fatalf("slackConversationsInfo: %v", err)
	}
	if got.Name != "general" {
		t.Errorf("Name = %q; want general", got.Name)
	}
}

// TestSlackConversationsInfo_NotOk covers Slack's {"ok":false} error
// convention — a 200 response that is still a failure.
func TestSlackConversationsInfo_NotOk(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
	})

	_, err := slackConversationsInfo(context.Background(), srv.Client(), "org-1", "xoxb-test", "C0GONE0001")
	if err == nil {
		t.Fatal("slackConversationsInfo with {ok:false} should return an error")
	}
}

// TestSlackConversationsInfo_HTTPError covers a non-2xx transport-level
// failure (rate limit, upstream outage) — distinct from the {"ok":false}
// application-level convention.
func TestSlackConversationsInfo_HTTPError(t *testing.T) {
	recordSlackWaits(t)
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := slackConversationsInfo(context.Background(), srv.Client(), "org-1", "xoxb-test", "C0THROTTL1")
	if err == nil {
		t.Fatal("slackConversationsInfo with HTTP 429 should return an error")
	}
}

// withLoweredCap temporarily lowers slackConversationsListCap for a test,
// restoring the original on cleanup — mirrors withFakeSlackAPI's swap-and-
// restore seam so a truncation test doesn't need to generate a thousand
// fake channels.
func withLoweredCap(t *testing.T, n int) {
	t.Helper()
	orig := slackConversationsListCap
	slackConversationsListCap = n
	t.Cleanup(func() { slackConversationsListCap = orig })
}

// fakePaginatedChannels serves conversations.list/users.conversations
// across pageSize-sized pages, cursoring by an opaque integer offset, for
// exactly total channels.
func fakePaginatedChannels(t *testing.T, total, pageSize int) *httptest.Server {
	t.Helper()
	return withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			var e error
			offset, e = strconv.Atoi(c)
			if e != nil {
				t.Fatalf("bad test cursor %q: %v", c, e)
			}
		}
		end := offset + pageSize
		if end > total {
			end = total
		}
		channels := make([]map[string]any, 0, end-offset)
		for i := offset; i < end; i++ {
			channels = append(channels, map[string]any{"id": fmt.Sprintf("C%04d", i), "name": "chan", "is_private": false})
		}
		next := ""
		if end < total {
			next = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channels": channels,
			"response_metadata": map[string]any{"next_cursor": next},
		})
	})
}

// TestSlackConversationsList_PaginatesAcrossPages: fewer channels than the
// cap, spread across several pages — every page is fetched and nothing is
// reported truncated.
func TestSlackConversationsList_PaginatesAcrossPages(t *testing.T) {
	srv := fakePaginatedChannels(t, 25, 10)
	got, truncated, err := slackConversationsList(context.Background(), srv.Client(), "org-1", "xoxb-test")
	if err != nil {
		t.Fatalf("slackConversationsList: %v", err)
	}
	if len(got) != 25 {
		t.Errorf("len(got) = %d; want 25 (all pages fetched)", len(got))
	}
	if truncated {
		t.Errorf("truncated = true; want false (well under the cap)")
	}
}

// TestSlackConversationsList_CapTruncates: more channels available than the
// (lowered) cap — the result is capped AND truncated=true, since Slack still
// had a next_cursor when the cap was hit.
func TestSlackConversationsList_CapTruncates(t *testing.T) {
	withLoweredCap(t, 15)
	srv := fakePaginatedChannels(t, 40, 10)
	got, truncated, err := slackConversationsList(context.Background(), srv.Client(), "org-1", "xoxb-test")
	if err != nil {
		t.Fatalf("slackConversationsList: %v", err)
	}
	if len(got) != 15 {
		t.Errorf("len(got) = %d; want 15 (capped)", len(got))
	}
	if !truncated {
		t.Errorf("truncated = false; want true (more pages existed beyond the cap)")
	}
}

// TestSlackConversationsList_CapExactlyMatchesTotal_NotTruncated: the cap
// happens to land exactly on the last channel of the last page — the
// universe was NOT actually cut short, so truncated must be false even
// though the loop stopped at the cap.
func TestSlackConversationsList_CapExactlyMatchesTotal_NotTruncated(t *testing.T) {
	withLoweredCap(t, 20)
	srv := fakePaginatedChannels(t, 20, 10)
	got, truncated, err := slackConversationsList(context.Background(), srv.Client(), "org-1", "xoxb-test")
	if err != nil {
		t.Fatalf("slackConversationsList: %v", err)
	}
	if len(got) != 20 {
		t.Errorf("len(got) = %d; want 20", len(got))
	}
	if truncated {
		t.Errorf("truncated = true; want false (the cap coincided with the actual end of data)")
	}
}

// recordSlackWaits replaces slackWait for the test with one that records
// each requested wait and returns at once (or with ctx's error), so a 429
// retry costs no real time. It returns an accessor for the recorded waits.
func recordSlackWaits(t *testing.T) func() []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	orig := slackWait
	slackWait = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { slackWait = orig })
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), waits...)
	}
}

// newSlackTestRequest builds a GET against the fake server's some.method.
func newSlackTestRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slackAPIBase+"/some.method", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	return req
}

// TestDoSlackJSON_429ThenSuccess_WaitsRetryAfterAndRetriesOnce covers the
// single retry: a 429 asking for 5s is waited out for exactly that long,
// the request is sent a second time, and the success body decodes. Each
// attempt is counted under its own class.
func TestDoSlackJSON_429ThenSuccess_WaitsRetryAfterAndRetriesOnce(t *testing.T) {
	waits := recordSlackWaits(t)
	var hits int32
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "value": "x"})
	})

	ctx, tally := upstream.WithTally(context.Background())
	var out struct {
		OK    bool   `json:"ok"`
		Value string `json:"value"`
	}
	if err := doSlackJSON(ctx, srv.Client(), "org-1", newSlackTestRequest(t, ctx), &out); err != nil {
		t.Fatalf("doSlackJSON: %v", err)
	}
	if !out.OK || out.Value != "x" {
		t.Errorf("out = %+v; want the post-retry success body decoded", out)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d; want 2 (one 429, one successful retry)", got)
	}
	if got := waits(); len(got) != 1 || got[0] != 5*time.Second {
		t.Errorf("waits = %v; want exactly one 5s wait", got)
	}
	if tally.Attempts() != 2 || tally.Count(upstream.RateLimited) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally: attempts=%d rate_limited=%d ok=%d; want 2/1/1",
			tally.Attempts(), tally.Count(upstream.RateLimited), tally.Count(upstream.OK))
	}
}

// TestDoSlackJSON_429AtCap_Retries pins the boundary: a wait of exactly
// slackRetryAfterCap is still waited out and retried.
func TestDoSlackJSON_429AtCap_Retries(t *testing.T) {
	waits := recordSlackWaits(t)
	var hits int32
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", strconv.Itoa(int(slackRetryAfterCap/time.Second)))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	var out struct {
		OK bool `json:"ok"`
	}
	if err := doSlackJSON(context.Background(), srv.Client(), "org-1", newSlackTestRequest(t, context.Background()), &out); err != nil {
		t.Fatalf("doSlackJSON: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d; want 2", got)
	}
	if got := waits(); len(got) != 1 || got[0] != slackRetryAfterCap {
		t.Errorf("waits = %v; want exactly one %s wait", got, slackRetryAfterCap)
	}
}

// TestDoSlackJSON_429OverCap_ReturnsWithoutWaitOrRetry covers a 429 asking
// for longer than slackRetryAfterCap: the typed rate-limit error comes back
// after the first attempt, carrying Slack's full wait, with no wait and no
// second request.
func TestDoSlackJSON_429OverCap_ReturnsWithoutWaitOrRetry(t *testing.T) {
	waits := recordSlackWaits(t)
	var hits int32
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	ctx, tally := upstream.WithTally(context.Background())
	var out struct {
		OK bool `json:"ok"`
	}
	start := time.Now()
	err := doSlackJSON(ctx, srv.Client(), "org-1", newSlackTestRequest(t, ctx), &out)
	elapsed := time.Since(start)

	var rl *slackRateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v (%T); want a *slackRateLimitError", err, err)
	}
	if rl.retryAfter != 60*time.Second {
		t.Errorf("retryAfter = %s; want 60s (Slack's own wait, uncapped)", rl.retryAfter)
	}
	if class, ok := upstream.ClassOf(err); !ok || class != upstream.RateLimited {
		t.Errorf("ClassOf(err) = %q, %v; want rate_limited, true", class, ok)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d; want 1 (no retry past the cap)", got)
	}
	if got := waits(); len(got) != 0 {
		t.Errorf("waits = %v; want none", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("doSlackJSON took %s; want an immediate return", elapsed)
	}
	if tally.Attempts() != 1 || tally.Count(upstream.RateLimited) != 1 {
		t.Errorf("tally: attempts=%d rate_limited=%d; want 1/1", tally.Attempts(), tally.Count(upstream.RateLimited))
	}
}

// TestDoSlackJSON_DoublePermanent429_ReturnsTypedError covers the "no
// general retry loop" contract: a second consecutive 429 (after the single
// retry) surfaces as a typed *slackRateLimitError, and doSlackJSON sends
// the request exactly twice — never a third time. Neither response carries
// a Retry-After, so the wait is slackRetryAfterDefault.
func TestDoSlackJSON_DoublePermanent429_ReturnsTypedError(t *testing.T) {
	waits := recordSlackWaits(t)
	var hits int32
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	})

	var out struct {
		OK bool `json:"ok"`
	}
	err := doSlackJSON(context.Background(), srv.Client(), "org-1", newSlackTestRequest(t, context.Background()), &out)
	if err == nil {
		t.Fatal("doSlackJSON with two consecutive 429s should return an error")
	}
	if !isSlackRateLimitError(err) {
		t.Errorf("err = %v (%T); want a *slackRateLimitError", err, err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d; want 2 (initial attempt + single retry, no further looping)", got)
	}
	if got := waits(); len(got) != 1 || got[0] != slackRetryAfterDefault {
		t.Errorf("waits = %v; want exactly one %s wait", got, slackRetryAfterDefault)
	}
}

// TestDoSlackJSON_NonOK_ErrorCarriesExcerptNotBody pins that a non-200's
// error names the status and an excerpt of the body, never the body
// itself: a proxy's HTML page contributes only its size, a JSON error body
// only its error message. Neither status is retried.
func TestDoSlackJSON_NonOK_ErrorCarriesExcerptNotBody(t *testing.T) {
	const html = "<html><body>502 Bad Gateway</body></html>"
	const jsonBody = `{"ok":false,"error":"x"}`
	cases := []struct {
		name    string
		status  int
		body    string
		want    string
		notWant string
		class   upstream.Class
	}{
		{"html body", http.StatusBadGateway, html,
			fmt.Sprintf("slack api: http 502: non-JSON body, %d bytes", len(html)), "<html>", upstream.Transient},
		{"json body", http.StatusInternalServerError, jsonBody,
			"slack api: http 500: x", `{"ok"`, upstream.Transient},
		{"json 403", http.StatusForbidden, jsonBody,
			"slack api: http 403: x", `{"ok"`, upstream.Auth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			ctx, tally := upstream.WithTally(context.Background())
			var out struct {
				OK bool `json:"ok"`
			}
			err := doSlackJSON(ctx, srv.Client(), "org-1", newSlackTestRequest(t, ctx), &out)
			if err == nil {
				t.Fatal("doSlackJSON with a non-200 should return an error")
			}
			if err.Error() != tc.want {
				t.Errorf("err = %q; want %q", err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("err = %q; carries raw body bytes %q", err.Error(), tc.notWant)
			}
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Errorf("hits = %d; want 1 (no retry)", got)
			}
			if tally.Attempts() != 1 || tally.Count(tc.class) != 1 {
				t.Errorf("tally: attempts=%d %s=%d; want 1/1", tally.Attempts(), tc.class, tally.Count(tc.class))
			}
		})
	}
}

// TestDoSlackJSON_TransportFailure_CountedOnceNotRetried covers a request
// that never reaches Slack: one transient attempt is counted and nothing is
// retried.
func TestDoSlackJSON_TransportFailure_CountedOnceNotRetried(t *testing.T) {
	waits := recordSlackWaits(t)
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	client := srv.Client()
	srv.Close()

	ctx, tally := upstream.WithTally(context.Background())
	var out struct {
		OK bool `json:"ok"`
	}
	err := doSlackJSON(ctx, client, "org-1", newSlackTestRequest(t, ctx), &out)
	if err == nil {
		t.Fatal("doSlackJSON against a closed server should return an error")
	}
	if tally.Attempts() != 1 || tally.Count(upstream.Transient) != 1 {
		t.Errorf("tally: attempts=%d transient=%d; want 1/1", tally.Attempts(), tally.Count(upstream.Transient))
	}
	if got := waits(); len(got) != 0 {
		t.Errorf("waits = %v; want none", got)
	}
}

// TestDoSlackJSON_LargePageDecodes: a page far larger than an error body is
// read whole. A page of 200 conversations easily passes 64 KiB.
func TestDoSlackJSON_LargePageDecodes(t *testing.T) {
	topic := strings.Repeat("t", 1000)
	var page strings.Builder
	page.WriteString(`{"ok":true,"channels":[`)
	for i := 0; i < 200; i++ {
		if i > 0 {
			page.WriteString(",")
		}
		fmt.Fprintf(&page, `{"id":"C%d","topic":{"value":%q}}`, i, topic)
	}
	page.WriteString(`]}`)
	if page.Len() <= upstream.MaxErrorBody {
		t.Fatalf("page is %d bytes, want one larger than an error body", page.Len())
	}
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page.String()))
	})

	var out struct {
		OK       bool `json:"ok"`
		Channels []struct {
			ID string `json:"id"`
		} `json:"channels"`
	}
	if err := doSlackJSON(context.Background(), srv.Client(), "org-1", newSlackTestRequest(t, context.Background()), &out); err != nil {
		t.Fatalf("doSlackJSON: %v", err)
	}
	if len(out.Channels) != 200 {
		t.Errorf("decoded %d channels, want 200", len(out.Channels))
	}
}

// TestDoSlackJSON_CanceledContext_NotCounted pins that a request abandoned
// because its caller's ctx ended is not recorded as an upstream outcome.
func TestDoSlackJSON_CanceledContext_NotCounted(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	ctx, tally := upstream.WithTally(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := doSlackJSON(ctx, srv.Client(), "org-1", newSlackTestRequest(t, ctx), &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if got := tally.Attempts(); got != 0 {
		t.Errorf("tally attempts = %d; want 0", got)
	}
}

// TestDoSlackJSON_429_RespectsContextCancellation pins that the Retry-After
// wait is bounded by ctx, not just slackRetryAfterCap: a caller with a
// short-lived context gets its own error back promptly rather than being
// held hostage by Slack's advertised wait, and the retry request is never
// sent once ctx has already expired.
func TestDoSlackJSON_429_RespectsContextCancellation(t *testing.T) {
	var hits int32
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var out struct {
		OK bool `json:"ok"`
	}
	start := time.Now()
	err := doSlackJSON(ctx, srv.Client(), "org-1", newSlackTestRequest(t, ctx), &out)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("doSlackJSON should fail once ctx is canceled mid-wait")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want context.DeadlineExceeded", err)
	}
	if elapsed > 4*time.Second {
		t.Errorf("doSlackJSON took %s; want a prompt return once ctx expired, not the full 5s Retry-After wait", elapsed)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d; want 1 (ctx expired before the retry request was ever sent)", got)
	}
}

// TestSlackConversationsJoin_429ThenSuccess_BodyPreservedOnRetry pins that a
// POST wrapper's form body survives the 429 retry — cloneSlackRequest must
// re-derive the already-drained body via GetBody rather than resending an
// empty one.
func TestSlackConversationsJoin_429ThenSuccess_BodyPreservedOnRetry(t *testing.T) {
	recordSlackWaits(t)
	var hits int32
	var gotChannels []string
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		gotChannels = append(gotChannels, r.FormValue("channel"))
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	if err := slackConversationsJoin(context.Background(), srv.Client(), "org-1", "xoxb-test", "C0RETRY01"); err != nil {
		t.Fatalf("slackConversationsJoin: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits = %d; want 2", got)
	}
	for i, ch := range gotChannels {
		if ch != "C0RETRY01" {
			t.Errorf("attempt %d channel = %q; want C0RETRY01 (body must survive the retry)", i+1, ch)
		}
	}
}
