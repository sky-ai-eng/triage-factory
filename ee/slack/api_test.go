package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
