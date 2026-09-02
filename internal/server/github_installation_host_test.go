package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubHostNormalizationAgrees pins the property the whole column rests
// on: the host stamped onto an installation row is byte-identical to the host
// user_github_identities is keyed under for the same GitHub. The identity
// writer resolves through resolveGitHubHost; the installation writers resolve
// through db.EffectiveGitHubHost (what the poller and the routing subscribers
// already use). Two functions, one answer — otherwise the column cannot be
// compared against anything and is dead weight.
//
// The three classes below are the ones that actually differ in shape: the
// public host (where an unset setting has to land), a *.ghe.com data-residency
// tenant (a real host that is not github.com and is not a path mount), and a
// GHES base carrying a path — the case a bare-origin derivation silently
// truncates into a different host.
func TestGitHubHostNormalizationAgrees(t *testing.T) {
	for _, tc := range []struct {
		name string
		base string
		want string
	}{
		{"unset", "", "https://github.com"},
		{"public", "https://github.com", "https://github.com"},
		{"public trailing slash", "https://github.com/", "https://github.com"},
		{"ghe.com tenant", "https://acme.ghe.com", "https://acme.ghe.com"},
		{"ghe.com tenant trailing slash", "https://acme.ghe.com/", "https://acme.ghe.com"},
		{"ghes with a path", "https://git.example.com/github", "https://git.example.com/github"},
		{"ghes with a path, trailing slash", "https://git.example.com/github/", "https://git.example.com/github"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, ok := resolveGitHubHost(tc.base)
			if !ok {
				t.Fatalf("resolveGitHubHost(%q) rejected a valid base URL", tc.base)
			}
			if identity != tc.want {
				t.Errorf("resolveGitHubHost(%q) = %q; want %q", tc.base, identity, tc.want)
			}
			if got := db.EffectiveGitHubHost(tc.base); got != identity {
				t.Errorf("db.EffectiveGitHubHost(%q) = %q; want the identity table's %q — "+
					"two spellings of one host never match, so the installation row "+
					"cannot be compared against anything",
					tc.base, got, identity)
			}
		})
	}
}

// TestOrgGitHubHost_ResolvesFromSettings covers the fetching sibling: a caller
// holding only an orgID lands on the same string, and an org that configured no
// base URL resolves to the public host rather than to "".
func TestOrgGitHubHost_ResolvesFromSettings(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	got, err := s.orgGitHubHost(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("orgGitHubHost (unconfigured): %v", err)
	}
	if got != "https://github.com" {
		t.Errorf("orgGitHubHost with no base URL configured = %q; want %q", got, "https://github.com")
	}

	setOrgGitHubBase(t, s, "https://git.example.com/github/")
	got, err = s.orgGitHubHost(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("orgGitHubHost (GHES): %v", err)
	}
	if got != "https://git.example.com/github" {
		t.Errorf("orgGitHubHost = %q; want the normalized %q", got, "https://git.example.com/github")
	}
}

// TestGitHubWebhook_InstallationCreated_StampsHost is the webhook half of "both
// installation writers write it": a delivery for a GHES org must mirror the
// installation onto that GHES host, not onto github.com. Getting this wrong is
// invisible until two deployments hand out the same installation id.
func TestGitHubWebhook_InstallationCreated_StampsHost(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	setOrgGitHubBase(t, s, "https://git.example.com/github")

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	got := onlyInstallation(t, s)
	if got.GitHubHost != "https://git.example.com/github" {
		t.Errorf("GitHubHost = %q; want the org's %q", got.GitHubHost, "https://git.example.com/github")
	}
}

// TestGitHubWebhook_InstallationCreated_DefaultsToPublicHost is the same path
// for the common org: no base URL configured means the deployment's default
// GitHub — github.com with the variable unset, as here — so the row says
// github.com rather than nothing at all.
func TestGitHubWebhook_InstallationCreated_DefaultsToPublicHost(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	if got := onlyInstallation(t, s); got.GitHubHost != "https://github.com" {
		t.Errorf("GitHubHost = %q for an org with no base URL configured; want %q", got.GitHubHost, "https://github.com")
	}
}
