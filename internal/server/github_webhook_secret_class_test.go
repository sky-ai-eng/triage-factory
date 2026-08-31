package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The per-org receiver verifies a delivery against a secret it reaches THROUGH
// the org named in the URL. Which secret that is follows from the org's
// credential class, and all three classes are walked here because the whole
// hazard is that two of them have no registration row and want different
// answers.

// TestResolveWebhookSecret_EveryCredentialClass pins that only a BYO-App
// workspace has a secret to find on this route.
func TestResolveWebhookSecret_EveryCredentialClass(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		class domain.GitHubCredentialClass
		want  string
	}{
		{"byo app", domain.GitHubCredentialClassBYOApp, testWebhookSecret},
		{"pat", domain.GitHubCredentialClassPAT, ""},
		{"managed app", domain.GitHubCredentialClassManagedApp, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			s := newTestServer(t)
			// Seed the registration row and its secret for every case, so what
			// varies between them is the class alone. For the two rowless
			// classes that pairing cannot occur in production, which is the
			// point: the class decides, and a row left lying around must not.
			seedWebhookApp(t, s)
			if _, err := s.orgs.SetGitHubCredentialClass(ctx, runmode.LocalDefaultOrgID, tc.class); err != nil {
				t.Fatalf("set credential class: %v", err)
			}

			got, err := s.resolveWebhookSecret(ctx, runmode.LocalDefaultOrgID)
			if err != nil {
				t.Fatalf("resolveWebhookSecret: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveWebhookSecret = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestGitHubWebhook_ManagedOrg_PerOrgDeliveryIs401 is the reason the managed
// arm returns "" rather than the deployment's secret.
//
// The shared App signs EVERY tenant's deliveries with one secret. If this route
// verified against it, a caller could put any workspace's id in the path and
// have GitHub's own signature let the delivery through — the receiver would
// accept a delivery for a workspace it was never sent for, which is the tenant
// confusion the whole epic exists to prevent. A managed workspace's deliveries
// arrive on a route with no org in its path, which verifies first and works out
// whose installation it was afterwards.
//
// The delivery below is signed correctly with the deployment secret, and is
// still refused with the same bare 401 a forgery gets.
func TestGitHubWebhook_ManagedOrg_PerOrgDeliveryIs401(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	if _, err := s.orgs.SetGitHubCredentialClass(context.Background(), runmode.LocalDefaultOrgID, domain.GitHubCredentialClassManagedApp); err != nil {
		t.Fatalf("set credential class: %v", err)
	}

	const deploymentSecret = "whsec_the_deployment_app"
	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)
	rec := postWebhook(s, "installation", signWith(deploymentSecret, body), body)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a managed workspace's deliveries do not arrive on the per-org URL; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("401 response has body %q, want empty — every org-dependent refusal on this route is the same bare 401", rec.Body.String())
	}
	if got := installations(t, s); len(got) != 0 {
		t.Errorf("a refused delivery wrote %d installation rows; want none", len(got))
	}
}
