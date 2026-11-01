package server

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestResolveWebhookSecret_EveryCredentialClass pins that the class alone
// decides the per-org receiver's secret: only a BYO-App org has one, even when
// a registration row is left lying around under another class.
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
			// The row and its secret are seeded for every class, so the class is
			// the only thing that varies.
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
