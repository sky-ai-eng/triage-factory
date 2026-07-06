package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
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

	got, err := slackUsersInfo(context.Background(), srv.Client(), "xoxb-test", "U0MENTION1")
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

	_, err := slackUsersInfo(context.Background(), srv.Client(), "xoxb-test", "U0GONE0001")
	if err == nil {
		t.Fatal("slackUsersInfo with {ok:false} should return an error")
	}
}

// TestSlackUsersInfo_HTTPError covers a non-2xx transport-level failure
// (rate limit, upstream outage) — distinct from the {"ok":false}
// application-level convention.
func TestSlackUsersInfo_HTTPError(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := slackUsersInfo(context.Background(), srv.Client(), "xoxb-test", "U0THROTTL1")
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

	got, err := slackUsersInfo(context.Background(), srv.Client(), "xoxb-test", "U0BOT00001")
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

	got, err := slackConversationsInfo(context.Background(), srv.Client(), "xoxb-test", "C0MENTION1")
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

	_, err := slackConversationsInfo(context.Background(), srv.Client(), "xoxb-test", "C0GONE0001")
	if err == nil {
		t.Fatal("slackConversationsInfo with {ok:false} should return an error")
	}
}

// TestSlackConversationsInfo_HTTPError covers a non-2xx transport-level
// failure (rate limit, upstream outage) — distinct from the {"ok":false}
// application-level convention.
func TestSlackConversationsInfo_HTTPError(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := slackConversationsInfo(context.Background(), srv.Client(), "xoxb-test", "C0THROTTL1")
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
	got, truncated, err := slackConversationsList(context.Background(), srv.Client(), "xoxb-test")
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
	got, truncated, err := slackConversationsList(context.Background(), srv.Client(), "xoxb-test")
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
	got, truncated, err := slackConversationsList(context.Background(), srv.Client(), "xoxb-test")
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
