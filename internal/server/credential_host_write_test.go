package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The Jira and GitHub credential routes that change a source's host write it
// through OrgsStore.SetSourceBaseURL and nothing else on the settings row. The
// tests below pin, for each of them, what that buys: a settings save loaded
// before the credential write conflicts instead of putting the old host back,
// the credential route itself never conflicts, and in local mode a write that
// fails after its keychain writes puts every key back.

// failingSourceBaseURLTx fails the host write, the last write each of these
// handlers makes before its audit row, so every keychain write before it has
// already landed when the transaction rolls back.
type failingSourceBaseURLTx struct{ db.TxRunner }

func (w failingSourceBaseURLTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.TxRunner.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.Orgs = failingSourceBaseURLOrgs{OrgsStore: tx.Orgs}
		return fn(tx)
	})
}

type failingSourceBaseURLOrgs struct{ db.OrgsStore }

func (failingSourceBaseURLOrgs) SetSourceBaseURL(context.Context, string, string, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, errors.New("host write failed")
}

// hostWriteCase is one credential route that writes a source's host.
type hostWriteCase struct {
	name string
	kind string
	// seed binds the credential the call replaces or removes, writing straight
	// to the stores so it works on a server whose transactions fail, and
	// returns the host it bound.
	seed func(t *testing.T, s *Server) (host string)
	// call makes the request and returns the host a successful call leaves.
	call func(t *testing.T, s *Server) (rec *httptest.ResponseRecorder, wantHost string)
	// keys is every key the call writes in the keychain.
	keys []string
}

func seedSourceHost(t *testing.T, s *Server, kind, host string) {
	t.Helper()
	if _, err := s.orgs.SetSourceBaseURL(t.Context(), runmode.LocalDefaultOrgID, kind, host); err != nil {
		t.Fatalf("seed %s host: %v", kind, err)
	}
}

func seedOrgCreds(t *testing.T, s *Server, c auth.Credentials) {
	t.Helper()
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, c); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
}

func hostWriteCases() []hostWriteCase {
	const (
		jiraHost   = "https://jira-old.example.com"
		githubHost = "https://ghe.example.com"
	)
	seedJira := func(t *testing.T, s *Server) string {
		seedOrgCreds(t, s, auth.Credentials{JiraURL: jiraHost, JiraPAT: "jira_old", JiraAuthMethod: string(jira.AuthMethodDCPAT)})
		seedSourceHost(t, s, eventsource.KindJira, jiraHost)
		return jiraHost
	}
	return []hostWriteCase{
		{
			// A Data Center org rebinding as Cloud: the bind writes the Cloud
			// keys and deletes the PAT, so a restore has both kinds of write to
			// undo.
			name: "jira bind",
			kind: eventsource.KindJira,
			seed: seedJira,
			call: func(t *testing.T, s *Server) (*httptest.ResponseRecorder, string) {
				stub := jiraCloudMyselfStub(t, `{"accountId":"cloud-bot","displayName":"Cloud Bot"}`, nil)
				rec := doJSON(t, s, http.MethodPut, jiraCredentialRoute(), map[string]any{
					"deployment": "cloud", "url": stub.URL, "email": "bot@acme.com", "token": "cloud_tok",
				})
				return rec, stub.URL
			},
			keys: integrations.JiraKeys(),
		},
		{
			name: "jira unbind",
			kind: eventsource.KindJira,
			seed: seedJira,
			call: func(t *testing.T, s *Server) (*httptest.ResponseRecorder, string) {
				return doJSON(t, s, http.MethodDelete, jiraCredentialRoute(), nil), ""
			},
			keys: integrations.JiraKeys(),
		},
		{
			name: "github pat unbind",
			kind: eventsource.KindGitHub,
			seed: func(t *testing.T, s *Server) string {
				seedOrgCreds(t, s, auth.Credentials{GitHubURL: githubHost, GitHubPAT: "ghp_old"})
				seedSourceHost(t, s, eventsource.KindGitHub, githubHost)
				return githubHost
			},
			call: func(t *testing.T, s *Server) (*httptest.ResponseRecorder, string) {
				return doJSON(t, s, http.MethodDelete, patRoute(), nil), ""
			},
			keys: integrations.GitHubKeys(),
		},
		{
			name: "github app disconnect",
			kind: eventsource.KindGitHub,
			seed: func(t *testing.T, s *Server) string {
				seedOrgCreds(t, s, auth.Credentials{GitHubURL: githubHost})
				seedSourceHost(t, s, eventsource.KindGitHub, githubHost)
				seedLocalApp(t, s, true)
				return githubHost
			},
			call: func(t *testing.T, s *Server) (*httptest.ResponseRecorder, string) {
				return doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/disconnect", nil), ""
			},
			keys: append(integrations.GitHubKeys(), "github_app_1_client_secret", "github_app_1_pem", "github_app_1_webhook_secret"),
		},
	}
}

func sourceHost(t *testing.T, s *Server, kind string) string {
	t.Helper()
	set, err := s.orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	if kind == eventsource.KindJira {
		return set.JiraBaseURL
	}
	return set.GitHubBaseURL
}

func storedKeys(t *testing.T, s *Server, keys []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = getSecret(t, s, k)
	}
	return out
}

// TestCredentialHostWrite_StaleSettingsSaveConflicts: a settings save loaded
// before the credential write answers 409 VERSION_CONFLICT and leaves the
// credential's host in place, while the credential route, which takes no
// version, succeeds over a settings save it never saw.
func TestCredentialHostWrite_StaleSettingsSaveConflicts(t *testing.T) {
	for _, c := range hostWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			oldHost := c.seed(t, s)

			// Another admin's settings save lands first.
			if rec := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
				"version": orgSettingsVersion(t, s), "max_concurrent_runs": 3,
			}); rec.Code != http.StatusOK {
				t.Fatalf("settings save: %d: %s", rec.Code, rec.Body.String())
			}
			loaded := orgSettingsVersion(t, s)

			rec, wantHost := c.call(t, s)
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), httpx.ReasonVersionConflict) {
				t.Fatalf("credential route = %d %s, want 200", rec.Code, rec.Body.String())
			}

			stale := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
				"version": loaded, c.kind + "_base_url": oldHost,
			})
			if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), httpx.ReasonVersionConflict) {
				t.Errorf("settings save loaded before the credential write = %d %s, want 409 %s", stale.Code, stale.Body.String(), httpx.ReasonVersionConflict)
			}
			if got := sourceHost(t, s, c.kind); got != wantHost {
				t.Errorf("%s host = %q, want %q", c.kind, got, wantHost)
			}
			set, err := s.orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
			if err != nil {
				t.Fatalf("read org settings: %v", err)
			}
			if set.MaxConcurrentRuns != 3 {
				t.Errorf("max_concurrent_runs = %d after the credential write, want the earlier save's 3", set.MaxConcurrentRuns)
			}
		})
	}
}

// TestCredentialHostWrite_FailedWriteRestoresKeys: in local mode, a write
// whose transaction fails after its keychain writes answers 500 and leaves the
// org as it was — every key put back, the host unchanged, and no audit row.
func TestCredentialHostWrite_FailedWriteRestoresKeys(t *testing.T) {
	for _, c := range hostWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner { return failingSourceBaseURLTx{TxRunner: tx} })
			oldHost := c.seed(t, s)
			before := storedKeys(t, s, c.keys)
			rows := len(credAuditRows(t, s))

			rec, _ := c.call(t, s)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("credential route with a failing host write = %d %s, want 500", rec.Code, rec.Body.String())
			}
			for k, v := range storedKeys(t, s, c.keys) {
				if v != before[k] {
					t.Errorf("%s = %q after the failed write, want %q", k, v, before[k])
				}
			}
			if got := sourceHost(t, s, c.kind); got != oldHost {
				t.Errorf("%s host = %q after the failed write, want %q", c.kind, got, oldHost)
			}
			if got := len(credAuditRows(t, s)); got != rows {
				t.Errorf("credential audit rows = %d after the failed write, want still %d", got, rows)
			}
		})
	}
}

// TestCredentialHostWrite_UnreadableSnapshotRefuses: in local mode a key the
// snapshot cannot read is one a failed write could not put back, so the route
// refuses before touching anything.
func TestCredentialHostWrite_UnreadableSnapshotRefuses(t *testing.T) {
	for _, c := range hostWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			oldHost := c.seed(t, s)
			before := storedKeys(t, s, c.keys)
			secrets := s.secrets
			s.secrets = getFailingSecrets{SecretStore: secrets}

			rec, _ := c.call(t, s)
			s.secrets = secrets
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("credential route with an unreadable snapshot = %d %s, want 500", rec.Code, rec.Body.String())
			}
			for k, v := range storedKeys(t, s, c.keys) {
				if v != before[k] {
					t.Errorf("%s = %q after a refused write, want %q", k, v, before[k])
				}
			}
			if got := sourceHost(t, s, c.kind); got != oldHost {
				t.Errorf("%s host = %q after a refused write, want %q", c.kind, got, oldHost)
			}
		})
	}
}

// TestCredentialHostWrite_MultiMode_StaleSettingsSaveConflicts runs the
// version property on Postgres, through the unbinds' own claims-bound
// transactions: each clears its host under RLS, and a settings save loaded
// before it conflicts rather than putting the host back.
func TestCredentialHostWrite_MultiMode_StaleSettingsSaveConflicts(t *testing.T) {
	const host = "https://old.example.com"
	for _, c := range []struct {
		name, kind, route string
		creds             auth.Credentials
	}{
		{"jira unbind", eventsource.KindJira, "/jira/access/credential",
			auth.Credentials{JiraURL: host, JiraPAT: "jira_old", JiraAuthMethod: string(jira.AuthMethodDCPAT)}},
		{"github pat unbind", eventsource.KindGitHub, "/github/pat",
			auth.Credentials{GitHubURL: host, GitHubPAT: "ghp_old"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newAuthRig(t)
			founder := r.seedUser()
			orgUUID, _ := r.seedOrg(founder, "acme")
			org := orgUUID.String()
			sid := r.signIn(founder)
			ctx := t.Context()
			if err := r.srv.tx.WithTx(ctx, org, founder.String(), func(tx db.TxStores) error {
				if err := integrations.Save(ctx, tx.Secrets, org, c.creds); err != nil {
					return err
				}
				_, err := tx.Orgs.SetSourceBaseURL(ctx, org, c.kind, host)
				return err
			}); err != nil {
				t.Fatalf("seed credential: %v", err)
			}

			settingsPath := "/api/orgs/" + org + "/settings"
			readSettings := func() map[string]any {
				t.Helper()
				rec := r.tokensJSON(http.MethodGet, settingsPath, nil, sid, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("GET settings: %d: %s", rec.Code, rec.Body.String())
				}
				var out map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("decode settings: %v", err)
				}
				return out
			}
			loaded := readSettings()["version"]

			if rec := r.tokensJSON(http.MethodDelete, "/api/orgs/"+org+c.route, nil, sid, ""); rec.Code != http.StatusOK {
				t.Fatalf("DELETE %s: %d: %s", c.route, rec.Code, rec.Body.String())
			}
			stale := r.tokensJSON(http.MethodPatch, settingsPath, map[string]any{
				"version": loaded, c.kind + "_base_url": host,
			}, sid, "")
			if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), httpx.ReasonVersionConflict) {
				t.Errorf("settings save loaded before the unbind = %d %s, want 409 %s", stale.Code, stale.Body.String(), httpx.ReasonVersionConflict)
			}
			if got := readSettings()[c.kind+"_base_url"]; got != "" {
				t.Errorf("%s_base_url = %v after the unbind, want cleared", c.kind, got)
			}
		})
	}
}
