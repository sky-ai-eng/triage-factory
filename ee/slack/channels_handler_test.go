package slack

import (
	"net/http"
	"net/http/httptest"
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestChannelsHandler_LocalMode404 mirrors TestHandlers_LocalMode404: every
// channels route 404s in local mode before touching any dependency — the
// Slack store is Postgres-only, so slackEntitlementGate must reject before a
// zero-value handler (nil tx/stores/az/client) ever gets used.
func TestChannelsHandler_LocalMode404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	h := &channelsHandler{}

	cases := []struct {
		name   string
		method string
		path   string
		fn     func(http.ResponseWriter, *http.Request)
	}{
		{"list", http.MethodGet, "/api/slack/teams/default/channels", h.handleList},
		{"put", http.MethodPut, "/api/slack/teams/default/channels", h.handlePut},
		{"primary", http.MethodPost, "/api/slack/channels/C123/primary", h.handlePrimary},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.SetPathValue("team_id", "default")
			req.SetPathValue("channel_id", "C123")
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d; want 404 in local mode", tc.name, rec.Code)
			}
		})
	}
}

// TestDiffChannelIDs pins the added/removed computation diffChannelIDs feeds
// to ReconcilePrimariesSystem's touched-channel set and the auto-join pass.
func TestDiffChannelIDs(t *testing.T) {
	prior := []slackstore.TeamChannel{{ChannelID: "C1"}, {ChannelID: "C2"}}
	added, removed := diffChannelIDs(prior, []string{"C2", "C3"})
	if len(added) != 1 || added[0] != "C3" {
		t.Errorf("added = %v; want [C3]", added)
	}
	if len(removed) != 1 || removed[0] != "C1" {
		t.Errorf("removed = %v; want [C1]", removed)
	}
}

// TestNormalizeChannelIDs pins the blank-drop + dedup + order-preserving
// contract diffChannelIDs and the first-track check rely on.
func TestNormalizeChannelIDs(t *testing.T) {
	got := normalizeChannelIDs([]string{"C1", "", "C2", "C1", "C3"})
	want := []string{"C1", "C2", "C3"}
	if len(got) != len(want) {
		t.Fatalf("normalizeChannelIDs = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeChannelIDs = %v; want %v", got, want)
		}
	}
}
