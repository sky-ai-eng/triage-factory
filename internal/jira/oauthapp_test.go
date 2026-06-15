package jira

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fakeJiraApps is a minimal db.JiraAppsStore for the OAuth-app resolver tests.
// Only GetForOrgSystem is exercised; the rest panic via the embedded interface
// if the resolver unexpectedly reaches for them.
type fakeJiraApps struct {
	db.JiraAppsStore
	app *domain.OrgJiraApp
	err error
}

func (f *fakeJiraApps) GetForOrgSystem(_ context.Context, _ string) (*domain.OrgJiraApp, error) {
	return f.app, f.err
}

// TestResolveOAuthApp_PerOrgOverride: a per-org row + its secret resolve to the
// override credential — tier 1 wins over the first-party app.
func TestResolveOAuthApp_PerOrgOverride(t *testing.T) {
	apps := &fakeJiraApps{app: &domain.OrgJiraApp{ClientID: "org-client", ClientSecretRef: jiraSecretRef}}
	secrets := &fakeSecrets{sys: map[string]string{jiraSecretRef: "org-secret"}}
	r := NewOAuthAppResolver(apps, secrets, OAuthApp{ClientID: "fp-client", ClientSecret: "fp-secret"})

	got, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "org-client" || got.ClientSecret != "org-secret" {
		t.Fatalf("Resolve = %+v, want org override", got)
	}
}

// TestResolveOAuthApp_FirstPartyFallback: no per-org row → the deployment
// first-party app (hosted) resolves.
func TestResolveOAuthApp_FirstPartyFallback(t *testing.T) {
	r := NewOAuthAppResolver(&fakeJiraApps{}, &fakeSecrets{}, OAuthApp{ClientID: "fp-client", ClientSecret: "fp-secret"})

	got, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "fp-client" || got.ClientSecret != "fp-secret" {
		t.Fatalf("Resolve = %+v, want first-party", got)
	}
}

// TestResolveOAuthApp_NotConfigured: no per-org row and no first-party app
// (the local-mode shape) → the typed not-configured error.
func TestResolveOAuthApp_NotConfigured(t *testing.T) {
	r := NewOAuthAppResolver(&fakeJiraApps{}, &fakeSecrets{}, OAuthApp{})

	_, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, ErrNoAtlassianOAuthApp) {
		t.Fatalf("Resolve err = %v, want ErrNoAtlassianOAuthApp", err)
	}
}

// TestResolveOAuthApp_OverrideMissingSecretFallsThrough: a row exists but its
// secret read clean-and-empty → the override is unusable, so the resolver falls
// through to the first-party app rather than returning a half-credential.
func TestResolveOAuthApp_OverrideMissingSecretFallsThrough(t *testing.T) {
	apps := &fakeJiraApps{app: &domain.OrgJiraApp{ClientID: "org-client", ClientSecretRef: jiraSecretRef}}
	r := NewOAuthAppResolver(apps, &fakeSecrets{}, OAuthApp{ClientID: "fp-client", ClientSecret: "fp-secret"})

	got, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "fp-client" {
		t.Fatalf("Resolve = %+v, want first-party fallback", got)
	}
}

// TestResolveOAuthApp_StoreErrorPropagates: a backend read error on the
// per-org lookup propagates rather than being misreported as not-configured —
// a transient outage must not silently fall through to the first-party app.
func TestResolveOAuthApp_StoreErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	r := NewOAuthAppResolver(&fakeJiraApps{err: boom}, &fakeSecrets{}, OAuthApp{ClientID: "fp", ClientSecret: "fp"})

	_, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, boom) {
		t.Fatalf("Resolve err = %v, want wrapped db error", err)
	}
	if errors.Is(err, ErrNoAtlassianOAuthApp) {
		t.Fatalf("Resolve err = %v, must not be ErrNoAtlassianOAuthApp", err)
	}
}

// TestResolveOAuthApp_SecretErrorPropagates: a backend error reading the
// override's client_secret propagates too.
func TestResolveOAuthApp_SecretErrorPropagates(t *testing.T) {
	boom := errors.New("vault down")
	apps := &fakeJiraApps{app: &domain.OrgJiraApp{ClientID: "org-client", ClientSecretRef: jiraSecretRef}}
	r := NewOAuthAppResolver(apps, &fakeSecrets{sysErr: boom}, OAuthApp{})

	_, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, boom) {
		t.Fatalf("Resolve err = %v, want wrapped vault error", err)
	}
}

const jiraSecretRef = "jira_oauth_client_secret"
