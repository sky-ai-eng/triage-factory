package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// Coverage for the credential half of the access change-log: every surface that
// binds, rotates, or destroys an org credential must leave a row, and no surface
// may invent one. The gap these close was user-visible — an admin connecting a
// GitHub App or rotating a PAT from Settings produced an empty audit view, which
// reads as "nobody touched our credentials" rather than "we don't record that".
//
// Local (SQLite) server: the write-points are mode-agnostic, and the change-log
// table is in both dialects' baseline, so the cheap rig proves the wiring. The
// EE *viewer* on top of these rows is exercised separately.

// credAuditRow is one recorded change, decoded far enough to assert on.
type credAuditRow struct {
	Action string
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	Name   string `json:"name"`
}

// credAuditRows reads every credential_set / credential_removed row for the
// local org, oldest-first, with detail_json flattened onto the row.
func credAuditRows(t *testing.T, s *Server) []credAuditRow {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `
		SELECT action, COALESCE(detail_json, '')
		  FROM access_change_log
		 WHERE org_id = ? AND action IN (?, ?)
		 ORDER BY created_at, rowid
	`, runmode.LocalDefaultOrgID, domain.AccessActionCredentialSet, domain.AccessActionCredentialRemoved)
	if err != nil {
		t.Fatalf("query access_change_log: %v", err)
	}
	defer rows.Close()
	var out []credAuditRow
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		row := credAuditRow{Action: action}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &row); err != nil {
				t.Fatalf("decode detail_json %q: %v", detail, err)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

// findCredAudit returns the single row with the given action+kind, failing if
// there isn't exactly one — "exactly one" is the property that matters for an
// audit log, in both directions.
func findCredAudit(t *testing.T, rows []credAuditRow, action, kind string) credAuditRow {
	t.Helper()
	var hits []credAuditRow
	for _, r := range rows {
		if r.Action == action && r.Kind == kind {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("rows matching (%s, %s) = %d, want exactly 1; all rows: %+v", action, kind, len(hits), rows)
	}
	return hits[0]
}

// githubUserStub stands up a GHES-shaped host whose /api/v3/user returns the
// given login — enough for auth.ValidateGitHub without a real network call.
func githubUserStub(t *testing.T, login string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": login})
		case "/api/v3/user/emails":
			writeGitHubPrimaryEmail(w, login+"@example.com")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeGitHubPrimaryEmail(w http.ResponseWriter, email string) {
	_ = json.NewEncoder(w).Encode([]map[string]any{{"email": email, "primary": true, "verified": true}})
}

// TestSetupWizard_AuditsBothOrgCredentials: the setup wizard binds a GitHub PAT
// and (optionally) a Jira credential. It used to do both in one fused call that
// recorded only the GitHub bind, so an org that connected Jira during setup had
// no record of it ever being bound. The wizard now drives the two per-credential
// routes, and this pins that the sequence it performs still lands both rows —
// the property the fused route was there to provide.
func TestSetupWizard_AuditsBothOrgCredentials(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	gh := githubUserStub(t, "acme-bot")
	jiraStub := jiraMyselfStub(t, `{"accountId":"org-bot","displayName":"Org Bot"}`, nil)

	if rec := bindOrgGitHubPAT(t, s, gh.URL, "ghp_test"); rec.Code != http.StatusOK {
		t.Fatalf("github pat bind = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, http.MethodPut, jiraCredentialRoute(), map[string]any{
		"deployment": "data_center", "url": jiraStub.URL, "pat": "jira_test",
	}); rec.Code != http.StatusOK {
		t.Fatalf("jira credential bind = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := patchOrgSettings(t, s, map[string]any{"github_clone_protocol": "https"}); rec.Code != http.StatusOK {
		t.Fatalf("clone protocol save = %d, body = %s", rec.Code, rec.Body.String())
	}

	rows := credAuditRows(t, s)
	ghRow := findCredAudit(t, rows, domain.AccessActionCredentialSet, domain.CredentialKindGitHubPAT)
	if ghRow.Host != gh.URL {
		t.Errorf("github row host = %q, want %q", ghRow.Host, gh.URL)
	}
	if ghRow.Name != "acme-bot" {
		t.Errorf("github row name = %q, want the validated login acme-bot", ghRow.Name)
	}
	findCredAudit(t, rows, domain.AccessActionCredentialSet, domain.CredentialKindJiraOrg)
}

// TestGitHubPATPut_AuditsSet: binding/rotating the org PAT through the
// credential resource records the bind with the @login the token authenticates
// as. This is the write-point that left no trace at all while it was a field on
// the bulk settings save.
func TestGitHubPATPut_AuditsSet(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	gh := githubUserStub(t, "rotated-bot")
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID,
		auth.Credentials{GitHubURL: gh.URL, GitHubPAT: "ghp_old"}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	rec := bindOrgGitHubPAT(t, s, gh.URL, "ghp_new")
	if rec.Code != http.StatusOK {
		t.Fatalf("pat bind = %d, body=%s", rec.Code, rec.Body.String())
	}

	row := findCredAudit(t, credAuditRows(t, s), domain.AccessActionCredentialSet, domain.CredentialKindGitHubPAT)
	if row.Name != "rotated-bot" {
		t.Errorf("name = %q, want the login the new PAT authenticates as", row.Name)
	}
	if row.Host != gh.URL {
		t.Errorf("host = %q, want %q", row.Host, gh.URL)
	}
	stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT)
	if stored != "ghp_new" {
		t.Errorf("stored PAT = %q, want the rotated one", stored)
	}
}

// TestGitHubPATPut_RequiresAToken: the token is the whole body, and it is
// required. Rejecting a blank one is what lets this route drop the "leave blank
// to keep current" ambiguity the bulk field carried — blank no longer means
// anything here, because you just don't call it. The host is not part of the
// body at all, so naming one is a decode error, not a value the route weighs.
func TestGitHubPATPut_RequiresAToken(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	for _, tc := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"no token", map[string]any{}, "pat"},
		{"blank token", map[string]any{"pat": "   "}, "pat"},
		// The host is org config with one writer. A caller that tries to name
		// one here is refused by strict decoding rather than quietly ignored,
		// so a client built against the old shape fails loudly instead of
		// binding against a host it thinks it chose.
		{"host in the body", map[string]any{"base_url": "https://github.com", "pat": "ghp_x"}, "base_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPut, patRoute(), tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); !strings.Contains(got, tc.field) {
				t.Errorf("body %s should name the offending field %q", got, tc.field)
			}
		})
	}
	if rows := credAuditRows(t, s); len(rows) != 0 {
		t.Errorf("rejected binds recorded %+v, want nothing", rows)
	}
}

// TestGitHubPATPut_AppRegisteredDuringValidation_409 pins the App-XOR-PAT gate
// against the read-then-act window. The handler checks for an App, then goes
// off to GitHub to validate the token — and that round-trip is the one place it
// is provably not holding the org's registration lock. Registering an App there
// used to be invisible to the rest of the request, so the bind would write a PAT
// underneath a live App.
//
// The validation stub doubles as the interleaving point, which makes the race
// deterministic rather than timing-dependent: the App appears exactly once,
// exactly while the token is in flight. What this asserts is the re-check inside
// the critical section — the advisory check before validation has already run
// and passed by then.
func TestGitHubPATPut_AppRegisteredDuringValidation_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	var (
		once    sync.Once
		mu      sync.Mutex
		seedErr error
	)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user/emails" {
			writeGitHubPrimaryEmail(w, "acme-bot@example.com")
			return
		}
		if r.URL.Path != "/api/v3/user" {
			http.NotFound(w, r)
			return
		}
		// Racing writer: an App registration commits mid-validation. Errors are
		// captured rather than t.Fatal'd — this runs on the server's goroutine.
		once.Do(func() {
			_, err := s.githubApps.CreateForOrg(context.Background(), domain.OrgGitHubApp{
				OrgID: runmode.LocalDefaultOrgID, AppID: "1", Slug: "tf-bot",
				ClientID: "Iv1.x", Active: true,
			})
			if err == nil {
				_, err = s.orgs.SetGitHubCredentialClass(context.Background(), runmode.LocalDefaultOrgID, domain.GitHubCredentialClassBYOApp)
			}
			mu.Lock()
			seedErr = err
			mu.Unlock()
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "acme-bot"})
	}))
	t.Cleanup(gh.Close)

	rec := bindOrgGitHubPAT(t, s, gh.URL, "ghp_new")

	mu.Lock()
	err := seedErr
	mu.Unlock()
	if err != nil {
		t.Fatalf("racing app registration failed to seed: %v", err)
	}

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — the re-check must see the App that landed during validation; body=%s",
			rec.Code, rec.Body.String())
	}
	if stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT); stored != "" {
		t.Errorf("stored PAT %q under a live App — App XOR PAT violated", stored)
	}
	if rows := credAuditRows(t, s); len(rows) != 0 {
		t.Errorf("a refused bind recorded %+v, want nothing", rows)
	}
}

// TestGitHubPATDelete_AuditsRemoval: the disconnect clears the credential and
// the host together, in one request, and records the removal against the host
// the token was bound to.
func TestGitHubPATDelete_AuditsRemoval(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	const host = "https://github.example.com"
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID,
		auth.Credentials{GitHubURL: host, GitHubPAT: "ghp_old"}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": host})

	rec := doJSON(t, s, http.MethodDelete, patRoute(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d, body=%s", rec.Code, rec.Body.String())
	}

	row := findCredAudit(t, credAuditRows(t, s), domain.AccessActionCredentialRemoved, domain.CredentialKindGitHubPAT)
	if row.Host != host {
		t.Errorf("host = %q, want the host the removed PAT was bound to (%q)", row.Host, host)
	}
	stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT)
	if stored != "" {
		t.Errorf("PAT still stored after disconnect: %q", stored)
	}
}

// TestGitHubPATDelete_Idempotent: disconnecting an org with nothing bound
// succeeds and records nothing. A removal row for a credential that was never
// there is a phantom revocation — worse in an audit log than a gap.
func TestGitHubPATDelete_Idempotent(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodDelete, patRoute(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rows := credAuditRows(t, s); len(rows) != 0 {
		t.Errorf("credential rows = %+v, want none (nothing was bound)", rows)
	}
}

// TestGitHubPATDelete_StagedApp_KeepsHost: a staged App coexists with a live
// PAT during a PAT→App switch, and it resolves against the SAME host. Clearing
// the host here would send a GHES org's App to github.com without an error —
// the resolver falls org_settings → github_url secret → github.com, and this
// disconnect clears the first two. So with any App registration present the
// unbind takes the token only.
func TestGitHubPATDelete_StagedApp_KeepsHost(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	const host = "https://github.example.com"
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID,
		auth.Credentials{GitHubURL: host, GitHubPAT: "ghp_live"}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": host})
	seedLocalApp(t, s, false) // staged: the PAT is still the live credential

	rec := doJSON(t, s, http.MethodDelete, patRoute(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Token gone, host intact — in org_settings AND in the secret the resolver
	// falls back to.
	if stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT); stored != "" {
		t.Errorf("PAT still stored after disconnect: %q", stored)
	}
	var settingsHost string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(base_url, '') FROM org_event_sources WHERE org_id = ? AND kind = 'github'`,
		runmode.LocalDefaultOrgID).Scan(&settingsHost); err != nil {
		t.Fatalf("read github base_url: %v", err)
	}
	if settingsHost != host {
		t.Errorf("github base_url = %q, want %q kept for the staged App", settingsHost, host)
	}
	if secretHost, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubURL); secretHost != host {
		t.Errorf("github_url secret = %q, want %q — it is the resolver's fallback", secretHost, host)
	}
	// The removal is still audited, against the host it was bound to.
	row := findCredAudit(t, credAuditRows(t, s), domain.AccessActionCredentialRemoved, domain.CredentialKindGitHubPAT)
	if row.Host != host {
		t.Errorf("audit host = %q, want %q", row.Host, host)
	}
}

// TestJiraCredentialDelete_AuditsRemoval: the Jira unbind clears the credential
// and the URL column in ONE server-side transaction. The frontend used to do
// this as two calls with a real "credentials were removed, but clearing the
// saved URL failed" half-state.
func TestJiraCredentialDelete_AuditsRemoval(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	const host = "https://jira.example.com"
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID,
		auth.Credentials{JiraURL: host, JiraPAT: "jira_old"}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	patchOrgSettingsOK(t, s, map[string]any{"jira_base_url": host})

	rec := doJSON(t, s, http.MethodDelete, jiraCredentialRoute(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d, body=%s", rec.Code, rec.Body.String())
	}

	findCredAudit(t, credAuditRows(t, s), domain.AccessActionCredentialRemoved, domain.CredentialKindJiraOrg)
	// Both halves gone, from the one call.
	stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyJiraPAT)
	if stored != "" {
		t.Errorf("Jira PAT still stored after disconnect: %q", stored)
	}
	var url string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(base_url, '') FROM org_event_sources WHERE org_id = ? AND kind = 'jira'`,
		runmode.LocalDefaultOrgID).Scan(&url); err != nil {
		t.Fatalf("read jira base_url: %v", err)
	}
	if url != "" {
		t.Errorf("jira_base_url = %q, want cleared by the same request", url)
	}
}

// TestDangerZoneClear_AuditsEveryBoundCredential: the danger-zone "clear all
// tokens" gesture used to be a bulk route that destroyed both org credentials
// with no discriminator and its own audit path. It is now the two
// per-credential DELETEs in sequence, and this pins that the sequence still
// records both removals — the whole reason the bulk route had to carry them.
func TestDangerZoneClear_AuditsEveryBoundCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: "https://github.com", GitHubPAT: "ghp_x",
		JiraURL: "https://jira.example.com", JiraPAT: "jira_x",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	if rec := doJSON(t, s, http.MethodDelete, patRoute(), nil); rec.Code != http.StatusOK {
		t.Fatalf("github disconnect = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, http.MethodDelete, jiraCredentialRoute(), nil); rec.Code != http.StatusOK {
		t.Fatalf("jira disconnect = %d, body=%s", rec.Code, rec.Body.String())
	}

	rows := credAuditRows(t, s)
	findCredAudit(t, rows, domain.AccessActionCredentialRemoved, domain.CredentialKindGitHubPAT)
	findCredAudit(t, rows, domain.AccessActionCredentialRemoved, domain.CredentialKindJiraOrg)
}

// TestOrgSettingsPatch_TouchesNoCredential: the config route is inert with
// respect to credentials in BOTH directions — it can't bind one and it can't
// revoke one. Clearing a base URL used to destroy the matching token as a side
// effect; now it clears a column, and disconnecting is an explicit DELETE.
func TestOrgSettingsPatch_TouchesNoCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	const host = "https://github.example.com"
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID,
		auth.Credentials{GitHubURL: host, GitHubPAT: "ghp_live"}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	// github_pat / jira_pat aren't fields on this route, and strict decoding
	// says so rather than accepting them silently — which is itself the
	// guarantee this test is about. Clear only the URLs.
	patchOrgSettingsOK(t, s, map[string]any{
		"github_base_url": nil, "jira_base_url": nil,
	})

	stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT)
	if stored != "ghp_live" {
		t.Errorf("stored PAT = %q, want it untouched — clearing a URL is not a disconnect", stored)
	}
	if rows := credAuditRows(t, s); len(rows) != 0 {
		t.Errorf("credential rows = %+v, want none (a config save is not a credential change)", rows)
	}
}

// TestAnthropicDelete_AuditsRemoval: switching back to system credentials
// revokes the org's stored LLM key. Only the bind was recorded, so the log
// showed a key being set and never unset.
func TestAnthropicDelete_AuditsRemoval(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)
	if rec := doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "sk-ant-seed"}); rec.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, http.MethodDelete, llmPath("anthropic"), nil); rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	rows := credAuditRows(t, s)
	findCredAudit(t, rows, domain.AccessActionCredentialSet, domain.CredentialKindAnthropicKey)
	findCredAudit(t, rows, domain.AccessActionCredentialRemoved, domain.CredentialKindAnthropicKey)
	// Neither call touches Bedrock — binding or removing one provider says
	// nothing about the other — so no Bedrock revocation may be logged.
	for _, r := range rows {
		if r.Kind == domain.CredentialKindBedrock {
			t.Errorf("unexpected Bedrock row %+v — Bedrock was never configured", r)
		}
	}
}

// TestGitHubIdentityPAT_AuditsIdentityBind: binding a personal GitHub identity
// stores no token, but "this TF user acts as @login" is an access fact the
// change-log has to carry — every review the user authorizes goes out under it.
func TestGitHubIdentityPAT_AuditsIdentityBind(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	gh := githubUserStub(t, "octocat")
	if rec := patchOrgSettings(t, s, map[string]any{"github_base_url": gh.URL}); rec.Code != http.StatusOK {
		t.Fatalf("seed base url = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, http.MethodPost,
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/identity/pat",
		map[string]any{"pat": "ghp_identity"})
	if rec.Code != http.StatusOK {
		t.Fatalf("identity status = %d, body = %s", rec.Code, rec.Body.String())
	}

	row := findCredAudit(t, credAuditRows(t, s), domain.AccessActionCredentialSet, domain.CredentialKindGitHubIdentity)
	if row.Name != "@octocat" {
		t.Errorf("name = %q, want @octocat", row.Name)
	}
}
