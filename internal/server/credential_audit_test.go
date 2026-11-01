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
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// The credential half of the access change-log: every surface that binds,
// rotates, or destroys an org credential leaves a row, and no surface invents
// one. The write-points are mode-agnostic and the change-log table is in both
// dialects' baseline, so the local (SQLite) server proves the wiring.

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

// TestGitHubPATPut_RequiresAToken: the token is the whole body and it is
// required, so a missing or blank one is a 400 naming pat. The host is not part
// of the body, so naming one is a strict-decode error.
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

// TestGitHubPATPut_AppRegisteredDuringValidation_409: an App registered while
// the token is being validated is seen by the re-check inside the critical
// section, so the bind is a 409 and stores no PAT. The validation stub registers
// the App, which makes the race deterministic.
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

// TestGitHubPATDelete_StagedApp_KeepsHost: a staged App resolves against the
// org's GitHub URL, so while one is registered the PAT unbind takes the token
// only, leaves the URL, and audits the removal against that host.
func TestGitHubPATDelete_StagedApp_KeepsHost(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	const host = "https://github.example.com"
	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": host})
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID,
		auth.Credentials{GitHubPAT: "ghp_live"}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	seedLocalApp(t, s, false) // staged: the PAT is still the live credential

	rec := doJSON(t, s, http.MethodDelete, patRoute(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d, body=%s", rec.Code, rec.Body.String())
	}

	if stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT); stored != "" {
		t.Errorf("PAT still stored after disconnect: %q", stored)
	}
	if got := sourceHost(t, s, eventsource.KindGitHub); got != host {
		t.Errorf("github base_url = %q, want %q kept for the staged App", got, host)
	}
	row := findCredAudit(t, credAuditRows(t, s), domain.AccessActionCredentialRemoved, domain.CredentialKindGitHubPAT)
	if row.Host != host {
		t.Errorf("audit host = %q, want %q", row.Host, host)
	}
}

// TestDangerZoneClear_AuditsEveryBoundCredential: the danger zone's "clear all
// tokens" is the two per-credential DELETEs in sequence. Each removes its
// credential and records one removal, the GitHub one against its host.
func TestDangerZoneClear_AuditsEveryBoundCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	const host = "https://github.example.com"
	seedSourceHost(t, s, eventsource.KindGitHub, host)
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp_x",
		JiraPAT:   "jira_x",
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
	if row := findCredAudit(t, rows, domain.AccessActionCredentialRemoved, domain.CredentialKindGitHubPAT); row.Host != host {
		t.Errorf("github removal host = %q, want the host the PAT was bound to (%q)", row.Host, host)
	}
	findCredAudit(t, rows, domain.AccessActionCredentialRemoved, domain.CredentialKindJiraOrg)
	for _, key := range []string{integrations.KeyGitHubPAT, integrations.KeyJiraPAT} {
		if stored := getSecret(t, s, key); stored != "" {
			t.Errorf("%s still stored after disconnect: %q", key, stored)
		}
	}
}

// TestOrgSettingsPatch_TouchesNoCredential: the config route can neither bind
// nor revoke a credential. Disconnecting is an explicit DELETE, and clearing
// the URL a connected token is bound to is refused rather than taken as one.
func TestOrgSettingsPatch_TouchesNoCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID,
		auth.Credentials{GitHubPAT: "ghp_live"}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	patchOrgSettingsOK(t, s, map[string]any{"github_clone_protocol": "https", "jira_base_url": nil})
	if rec := patchOrgSettings(t, s, map[string]any{"github_base_url": nil}); rec.Code != http.StatusConflict {
		t.Fatalf("clear github_base_url under a bound token = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT)
	if stored != "ghp_live" {
		t.Errorf("stored PAT = %q, want it untouched — a config save is not a disconnect", stored)
	}
	if rows := credAuditRows(t, s); len(rows) != 0 {
		t.Errorf("credential rows = %+v, want none (a config save is not a credential change)", rows)
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
