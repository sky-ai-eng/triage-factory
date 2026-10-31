package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// storedJiraSite reads the org's Jira site as stored.
func storedJiraSite(t *testing.T, s *Server) string {
	t.Helper()
	var site string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(base_url, '') FROM org_event_sources WHERE org_id = ? AND kind = 'jira'`,
		runmode.LocalDefaultOrgID).Scan(&site); err != nil {
		t.Fatalf("read jira site: %v", err)
	}
	return site
}

func connectJiraDC(t *testing.T, s *Server, url, pat string) int {
	t.Helper()
	return doJSON(t, s, http.MethodPut, jiraCredentialRoute(),
		map[string]any{"deployment": "data_center", "url": url, "pat": pat}).Code
}

// A bind stores the site in canonical form as the org's Jira site and nowhere
// else: nothing is saved beside the credential for a later read to disagree
// with.
func TestJiraConnect_StoresTheCanonicalSiteInSettingsOnly(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	stub := jiraMyselfStub(t, `{"accountId":"bot","displayName":"Bot"}`, nil)

	if code := connectJiraDC(t, s, " "+strings.ToUpper(stub.URL[:4])+stub.URL[4:]+"/ ", "pat-1"); code != http.StatusOK {
		t.Fatalf("connect = %d, want 200", code)
	}
	if got := storedJiraSite(t, s); got != stub.URL {
		t.Errorf("jira site = %q, want the canonical %q", got, stub.URL)
	}
	if got, err := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, "jira_url"); err != nil || got != "" {
		t.Errorf("a site was saved beside the credential: %q (err %v)", got, err)
	}
}

func TestJiraConnect_RefusesAURLThatIsNotHTTP(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPut, jiraCredentialRoute(),
		map[string]any{"deployment": "data_center", "url": "jira.example.com", "pat": "pat-1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if e := firstError(t, rec); e.Field != "url" {
		t.Errorf("error = %+v, want it on url", e)
	}
}

// A connected org rotates its credential on its own site, and a replacement
// naming another site is refused before anything is stored: the credential in
// place, and the site it works on, stay as they were.
func TestJiraConnect_AReplacementStaysOnTheConnectedSite(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	site := jiraMyselfStub(t, `{"accountId":"bot","displayName":"Bot"}`, nil)
	other := jiraMyselfStub(t, `{"accountId":"bot","displayName":"Bot"}`, nil)

	if code := connectJiraDC(t, s, site.URL, "pat-1"); code != http.StatusOK {
		t.Fatalf("connect = %d, want 200", code)
	}
	if code := connectJiraDC(t, s, site.URL, "pat-2"); code != http.StatusOK {
		t.Fatalf("rotate on the same site = %d, want 200", code)
	}

	rec := doJSON(t, s, http.MethodPut, jiraCredentialRoute(),
		map[string]any{"deployment": "data_center", "url": other.URL, "pat": "pat-3"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("replace on another site = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if e := firstError(t, rec); e.Field != "url" || !strings.Contains(e.Message, "disconnect") {
		t.Errorf("error = %+v, want it on url, saying to disconnect first", e)
	}
	if got := storedJiraSite(t, s); got != site.URL {
		t.Errorf("jira site = %q, want %q", got, site.URL)
	}
	creds, err := integrations.Load(t.Context(), s.secrets, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatal(err)
	}
	if creds.JiraPAT != "pat-2" {
		t.Errorf("jira pat = %q, want the rotated pat-2 kept", creds.JiraPAT)
	}

	// Disconnected, the org connects anywhere.
	if rec := doJSON(t, s, http.MethodDelete, jiraCredentialRoute(), nil); rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d", rec.Code)
	}
	if code := connectJiraDC(t, s, other.URL, "pat-3"); code != http.StatusOK {
		t.Fatalf("connect on the other site after disconnecting = %d, want 200", code)
	}
}

// The settings save moves or clears the Jira site only while no service
// credential is stored.
func TestOrgSettingsPatch_JiraSiteHoldsWhileACredentialIsStored(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	const site = "https://jira.example.com"
	seedSourceHost(t, s, eventsource.KindJira, site)
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{JiraPAT: "pat"}); err != nil {
		t.Fatal(err)
	}

	for name, value := range map[string]any{
		"move":  "https://other.example.com",
		"clear": nil,
	} {
		t.Run(name, func(t *testing.T) {
			rec := patchOrgSettings(t, s, map[string]any{"jira_base_url": value})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			if e := firstError(t, rec); e.Field != "jira_base_url" {
				t.Errorf("error = %+v, want it on jira_base_url", e)
			}
			if got := storedJiraSite(t, s); got != site {
				t.Errorf("jira site = %q, want %q", got, site)
			}
		})
	}

	// Re-sending the stored site is not a move.
	patchOrgSettingsOK(t, s, map[string]any{"jira_base_url": site})

	// With the credential gone the site moves.
	if err := integrations.ClearJira(t.Context(), s.secrets, runmode.LocalDefaultOrgID); err != nil {
		t.Fatal(err)
	}
	patchOrgSettingsOK(t, s, map[string]any{"jira_base_url": "https://other.example.com"})
	if got := storedJiraSite(t, s); got != "https://other.example.com" {
		t.Errorf("jira site = %q, want the moved site", got)
	}
}
