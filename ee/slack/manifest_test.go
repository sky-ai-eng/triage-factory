package slack

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBuildSlackManifest_Socket: socket_mode_enabled true, bot_events carries
// the mention + engaged-thread message subscriptions, no request_url, the full
// scope list, and the org name folded into display_information.name.
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
	assertBotEvents(t, m.Settings.EventSubscriptions.BotEvents)
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
// wire JSON (omitempty on a nil pointer), bot_events carries the mention +
// engaged-thread message subscriptions.
func TestBuildSlackManifest_EventsAPI(t *testing.T) {
	m := buildSlackManifest("Acme", transportEventsAPI, "https://tf.acme.example", "org-123")

	wantURL := "https://tf.acme.example/api/webhooks/slack/org-123"
	if m.Settings.EventSubscriptions.RequestURL != wantURL {
		t.Errorf("request_url = %q, want %q", m.Settings.EventSubscriptions.RequestURL, wantURL)
	}
	if m.Settings.SocketModeEnabled != nil {
		t.Errorf("socket_mode_enabled = %v, want nil (omitted) for events_api transport", *m.Settings.SocketModeEnabled)
	}
	assertBotEvents(t, m.Settings.EventSubscriptions.BotEvents)
	assertFullScopeSet(t, m.OAuthConfig.Scopes.Bot)

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "socket_mode_enabled") {
		t.Errorf("events_api manifest JSON unexpectedly carries socket_mode_enabled: %s", raw)
	}
}

// assertBotEvents pins the exact bot_events set: the explicit @-mention, the
// two engaged-thread message subscriptions (public + private channels),
// channel_id_changed, which moves a channel's rows when Slack gives it a new
// id, and app_uninstalled and tokens_revoked, which remove a workspace's
// connection when its bot token dies. message.im / message.mpim are
// deliberately absent — DM support is a separate future ticket, and
// un-subscribed message types must never reach the binary.
func assertBotEvents(t *testing.T, got []string) {
	t.Helper()
	want := []string{"app_mention", "message.channels", "message.groups", "channel_id_changed", "app_uninstalled", "tokens_revoked"}
	if len(got) != len(want) {
		t.Fatalf("bot_events = %v (%d); want %v", got, len(got), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("bot_events[%d] = %q; want %q", i, got[i], w)
		}
	}
}

func assertFullScopeSet(t *testing.T, got []string) {
	t.Helper()
	want := []string{
		"app_mentions:read", "chat:write", "reactions:write", "users:read", "users:read.email",
		"channels:history", "groups:history", "channels:read", "groups:read", "channels:join",
		"files:read", "files:write", "links:read", "links:write",
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

// TestBuildSlackManifest_DisplayCopyWithinSlackLimits: Slack refuses to
// create an app from a manifest whose description runs past 140 characters
// or whose long_description falls outside 175–4000, and it counts characters,
// not bytes (the long description carries multi-byte bullets and arrows).
func TestBuildSlackManifest_DisplayCopyWithinSlackLimits(t *testing.T) {
	d := buildSlackManifest("Acme", transportSocket, "https://tf.acme.example", "org-123").DisplayInformation

	if n := utf8.RuneCountInString(d.Description); n == 0 || n > 140 {
		t.Errorf("description is %d characters; Slack allows 1–140", n)
	}
	if n := utf8.RuneCountInString(d.LongDescription); n < 175 || n > 4000 {
		t.Errorf("long_description is %d characters; Slack allows 175–4000", n)
	}
	if !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(d.BackgroundColor) {
		t.Errorf("background_color = %q, want a #rrggbb hex color", d.BackgroundColor)
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

// deploymentManifestPlaceholderURL is the public URL the published manifest
// files carry, which the operator replaces with their own.
const deploymentManifestPlaceholderURL = "https://YOUR-TF-PUBLIC-URL"

// deploymentManifestFiles are the files an operator creates the deployment's
// Slack app from, one per transport, beside the page that explains them.
var deploymentManifestFiles = []struct{ transport, path string }{
	{transportEventsAPI, "../../docs/users/self-hosting/deployment-integrations/slack-app-manifest.events-api.json"},
	{transportSocket, "../../docs/users/self-hosting/deployment-integrations/slack-app-manifest.socket.json"},
}

// renderDeploymentManifestFile is a manifest file's exact contents: indented
// for a person to read, with the long description's arrows and bullets left
// unescaped, and a trailing newline.
func renderDeploymentManifestFile(t *testing.T, transport string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(buildDeploymentSlackManifest(transport, deploymentManifestPlaceholderURL)); err != nil {
		t.Fatalf("encode the %s manifest: %v", transport, err)
	}
	return buf.Bytes()
}

// TestDeploymentSlackManifestFiles: each published manifest file is the
// builder's output, byte for byte, so a change to the scopes or the events
// reaches an operator the next time they create the app. With
// TF_UPDATE_SLACK_MANIFESTS=1 the test writes the files instead.
func TestDeploymentSlackManifestFiles(t *testing.T) {
	update := os.Getenv("TF_UPDATE_SLACK_MANIFESTS") == "1"
	for _, f := range deploymentManifestFiles {
		want := renderDeploymentManifestFile(t, f.transport)
		if update {
			if err := os.WriteFile(f.path, want, 0o644); err != nil {
				t.Fatalf("write %s: %v", f.path, err)
			}
			continue
		}
		got, err := os.ReadFile(f.path)
		if err != nil {
			t.Errorf("read %s: %v", f.path, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is not buildDeploymentSlackManifest(%q, %q). Regenerate the files with\n"+
				"\tTF_UPDATE_SLACK_MANIFESTS=1 go test ./ee/slack -run TestDeploymentSlackManifestFiles\n"+
				"and commit them.\ngot:\n%s\nwant:\n%s",
				filepath.Base(f.path), f.transport, deploymentManifestPlaceholderURL, got, want)
		}
	}
}

// TestBuildDeploymentSlackManifest: the deployment's manifest names no org,
// registers the install callback, and for the Events API points at the
// org-free webhook; everything else matches an org's own manifest for the
// same transport.
func TestBuildDeploymentSlackManifest(t *testing.T) {
	const base = "https://tf.example"
	for _, tc := range []struct {
		transport      string
		wantRequestURL string
	}{
		{transportEventsAPI, base + "/api/webhooks/slack"},
		{transportSocket, ""},
	} {
		t.Run(tc.transport, func(t *testing.T) {
			m := buildDeploymentSlackManifest(tc.transport, base)

			if m.DisplayInformation.Name != "Triage Factory" {
				t.Errorf("name = %q, want %q", m.DisplayInformation.Name, "Triage Factory")
			}
			wantRedirect := []string{base + "/api/slack/install/callback"}
			if !reflect.DeepEqual(m.OAuthConfig.RedirectURLs, wantRedirect) {
				t.Errorf("redirect_urls = %v, want %v", m.OAuthConfig.RedirectURLs, wantRedirect)
			}
			if got := m.Settings.EventSubscriptions.RequestURL; got != tc.wantRequestURL {
				t.Errorf("request_url = %q, want %q", got, tc.wantRequestURL)
			}
			if m.Settings.OrgDeployEnabled {
				t.Error("org_deploy_enabled = true; the deployment app refuses Enterprise Grid org-wide installs")
			}
			if m.Settings.TokenRotationEnabled {
				t.Error("token_rotation_enabled = true; TF never refreshes a bot token")
			}

			// The rest is the org manifest's, with the fields above set to
			// what it has for the same transport.
			org := buildSlackManifest("Acme", tc.transport, base, "org-123")
			m.DisplayInformation.Name = org.DisplayInformation.Name
			m.OAuthConfig.RedirectURLs = nil
			m.Settings.EventSubscriptions.RequestURL = org.Settings.EventSubscriptions.RequestURL
			if !reflect.DeepEqual(m, org) {
				t.Errorf("past its name, redirect URL and request URL, the deployment manifest differs from an org's:\n got %+v\nwant %+v", m, org)
			}
		})
	}
}

// TestBuildSlackManifest_NoRedirectURLs: an org's own app is connected with
// pasted tokens, never through OAuth, so its manifest registers no redirect.
func TestBuildSlackManifest_NoRedirectURLs(t *testing.T) {
	raw, err := json.Marshal(buildSlackManifest("Acme", transportEventsAPI, "https://tf.example", "org-123"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "redirect_urls") {
		t.Errorf("org manifest JSON carries redirect_urls: %s", raw)
	}
}

// TestDeploymentManifestURLsAreMounted: the request URL and the redirect URL
// the deployment manifest registers are routes the extension mounts, so an
// app created from the published file reaches TF.
func TestDeploymentManifestURLsAreMounted(t *testing.T) {
	mnt := muxMounter{mux: http.NewServeMux()}
	mountRoutes(mnt, &workspacesHandler{}, &deploymentAppHandler{}, &installHandler{}, &channelsHandler{}, &webhookHandler{})

	m := buildDeploymentSlackManifest(transportEventsAPI, "https://tf.example")
	for _, tc := range []struct{ method, rawURL string }{
		{http.MethodPost, m.Settings.EventSubscriptions.RequestURL},
		{http.MethodGet, m.OAuthConfig.RedirectURLs[0]},
	} {
		u, err := url.Parse(tc.rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.rawURL, err)
		}
		rec := httptest.NewRecorder()
		mnt.mux.ServeHTTP(rec, httptest.NewRequest(tc.method, u.Path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d; the manifest registers a URL no route serves", tc.method, u.Path, rec.Code)
		}
	}
}
