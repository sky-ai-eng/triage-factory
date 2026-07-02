package slack

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildSlackManifest_Socket: socket_mode_enabled true, bot_events carries
// exactly app_mention, no request_url, the full scope list, and the org name
// folded into display_information.name.
func TestBuildSlackManifest_Socket(t *testing.T) {
	m := buildSlackManifest("Acme", transportSocket, "https://tf.acme.example", "org-123")

	if m.DisplayInformation.Name != "Triage Factory (Acme)" {
		t.Errorf("name = %q, want %q", m.DisplayInformation.Name, "Triage Factory (Acme)")
	}
	if m.Settings.SocketModeEnabled == nil || !*m.Settings.SocketModeEnabled {
		t.Errorf("socket_mode_enabled = %v, want true", m.Settings.SocketModeEnabled)
	}
	if m.Settings.EventSubscriptions.RequestURL != "" {
		t.Errorf("request_url = %q, want empty for socket transport", m.Settings.EventSubscriptions.RequestURL)
	}
	if got := m.Settings.EventSubscriptions.BotEvents; len(got) != 1 || got[0] != "app_mention" {
		t.Errorf("bot_events = %v, want [app_mention] only", got)
	}
	assertFullScopeSet(t, m.OAuthConfig.Scopes.Bot)

	// Round-trips through JSON with no "request_url" key present at all —
	// omitempty, not an empty string in the wire payload.
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "request_url") {
		t.Errorf("socket manifest JSON unexpectedly carries request_url: %s", raw)
	}
	if !strings.Contains(string(raw), `"socket_mode_enabled":true`) {
		t.Errorf("socket manifest JSON missing socket_mode_enabled:true: %s", raw)
	}
}

// TestBuildSlackManifest_EventsAPI: request_url carries the publicURL +
// per-org webhook path, socket_mode_enabled is entirely absent from the
// wire JSON (omitempty on a nil pointer), bot_events still exactly
// [app_mention].
func TestBuildSlackManifest_EventsAPI(t *testing.T) {
	m := buildSlackManifest("Acme", transportEventsAPI, "https://tf.acme.example", "org-123")

	wantURL := "https://tf.acme.example/api/webhooks/slack/org-123"
	if m.Settings.EventSubscriptions.RequestURL != wantURL {
		t.Errorf("request_url = %q, want %q", m.Settings.EventSubscriptions.RequestURL, wantURL)
	}
	if m.Settings.SocketModeEnabled != nil {
		t.Errorf("socket_mode_enabled = %v, want nil (omitted) for events_api transport", *m.Settings.SocketModeEnabled)
	}
	if got := m.Settings.EventSubscriptions.BotEvents; len(got) != 1 || got[0] != "app_mention" {
		t.Errorf("bot_events = %v, want [app_mention] only", got)
	}
	assertFullScopeSet(t, m.OAuthConfig.Scopes.Bot)

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "socket_mode_enabled") {
		t.Errorf("events_api manifest JSON unexpectedly carries socket_mode_enabled: %s", raw)
	}
}

func assertFullScopeSet(t *testing.T, got []string) {
	t.Helper()
	want := []string{
		"app_mentions:read", "chat:write", "reactions:write", "users:read", "users:read.email",
		"channels:history", "groups:history", "files:read", "files:write",
	}
	if len(got) != len(want) {
		t.Fatalf("scopes = %v (%d); want %d scopes", got, len(got), len(want))
	}
	wantSet := map[string]bool{}
	for _, s := range want {
		wantSet[s] = true
	}
	for _, s := range got {
		if !wantSet[s] {
			t.Errorf("unexpected scope %q in manifest", s)
		}
	}
}

// TestBuildSlackManifest_NoOrgName_FallsBackToBareName covers the defensive
// branch for an empty org display name.
func TestBuildSlackManifest_NoOrgName_FallsBackToBareName(t *testing.T) {
	m := buildSlackManifest("", transportSocket, "https://tf.example", "org-1")
	if m.DisplayInformation.Name != "Triage Factory" {
		t.Errorf("name = %q, want bare \"Triage Factory\" for an empty org name", m.DisplayInformation.Name)
	}
}

// TestHandleManifest_EventsAPI_NoPublicURL_409 (the real handler, wired
// through adminGate with a live authz.Checker) lives in the pgtest-backed
// suite — workspaces_pg_test.go — alongside the other admin-gated handler
// behavior, since adminGate needs a real org-membership read.
