package jira

import (
	"context"
	"errors"
	"slices"
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
// override credential — tier 1 wins over the deployment app.
func TestResolveOAuthApp_PerOrgOverride(t *testing.T) {
	apps := &fakeJiraApps{app: &domain.OrgJiraApp{ClientID: "org-client", ClientSecretRef: jiraSecretRef}}
	secrets := &fakeSecrets{sys: map[string]string{jiraSecretRef: "org-secret"}}
	r := NewOAuthAppResolver(apps, secrets, OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"})

	got, source, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "org-client" || got.ClientSecret != "org-secret" {
		t.Fatalf("Resolve = %+v, want org override", got)
	}
	if source != SourceOrgOverride {
		t.Fatalf("source = %v, want SourceOrgOverride", source)
	}
}

// TestResolveOAuthApp_DeploymentFallback: no per-org row → the deployment
// app (multi) resolves.
func TestResolveOAuthApp_DeploymentFallback(t *testing.T) {
	r := NewOAuthAppResolver(&fakeJiraApps{}, &fakeSecrets{}, OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"})

	got, source, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "dep-client" || got.ClientSecret != "dep-secret" {
		t.Fatalf("Resolve = %+v, want deployment app", got)
	}
	if source != SourceDeployment {
		t.Fatalf("source = %v, want SourceDeployment", source)
	}
}

// TestResolveOAuthApp_NotConfigured: no per-org row and no deployment app
// (the local-mode shape) → the typed not-configured error.
func TestResolveOAuthApp_NotConfigured(t *testing.T) {
	r := NewOAuthAppResolver(&fakeJiraApps{}, &fakeSecrets{}, OAuthApp{})

	_, source, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, ErrNoAtlassianOAuthApp) {
		t.Fatalf("Resolve err = %v, want ErrNoAtlassianOAuthApp", err)
	}
	if source != SourceNone {
		t.Fatalf("source = %v, want SourceNone", source)
	}
}

// TestResolveOAuthApp_OverrideMissingSecretFallsThrough: a row exists but its
// secret read clean-and-empty → the override is unusable, so the resolver falls
// through to the deployment app rather than returning a half-credential.
func TestResolveOAuthApp_OverrideMissingSecretFallsThrough(t *testing.T) {
	apps := &fakeJiraApps{app: &domain.OrgJiraApp{ClientID: "org-client", ClientSecretRef: jiraSecretRef}}
	r := NewOAuthAppResolver(apps, &fakeSecrets{}, OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"})

	got, source, err := r.Resolve(context.Background(), testOrgID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ClientID != "dep-client" {
		t.Fatalf("Resolve = %+v, want deployment fallback", got)
	}
	// The dead override must NOT be reported as the live source — this is the
	// state that would otherwise make the settings card claim "configured".
	if source != SourceDeployment {
		t.Fatalf("source = %v, want SourceDeployment (dead override falls through)", source)
	}
}

// TestResolveOAuthApp_StoreErrorPropagates: a backend read error on the
// per-org lookup propagates rather than being misreported as not-configured —
// a transient outage must not silently fall through to the deployment app.
func TestResolveOAuthApp_StoreErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	r := NewOAuthAppResolver(&fakeJiraApps{err: boom}, &fakeSecrets{}, OAuthApp{ClientID: "dep", ClientSecret: "dep"})

	_, _, err := r.Resolve(context.Background(), testOrgID)
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

	_, _, err := r.Resolve(context.Background(), testOrgID)
	if !errors.Is(err, boom) {
		t.Fatalf("Resolve err = %v, want wrapped vault error", err)
	}
}

// TestResolveClient: a token an app issued is answered with that app, and its
// tier, while the org can still authenticate as it — its own app or the
// deployment app, whichever Resolve would pick now — and with
// ErrNoAtlassianOAuthApp once it cannot.
func TestResolveClient(t *testing.T) {
	deployment := OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"}
	withOrgApp := NewOAuthAppResolver(
		&fakeJiraApps{app: &domain.OrgJiraApp{ClientID: "org-client", ClientSecretRef: jiraSecretRef}},
		&fakeSecrets{sys: map[string]string{jiraSecretRef: "org-secret"}},
		deployment)
	for _, tc := range []struct {
		clientID string
		want     OAuthApp
		source   OAuthAppSource
	}{
		{"org-client", OAuthApp{ClientID: "org-client", ClientSecret: "org-secret"}, SourceOrgOverride},
		// The deployment app still refreshes the tokens it issued after the
		// org registered its own.
		{"dep-client", deployment, SourceDeployment},
	} {
		got, source, err := withOrgApp.ResolveClient(context.Background(), testOrgID, tc.clientID)
		if err != nil || got != tc.want || source != tc.source {
			t.Errorf("ResolveClient(%s) = (%+v, %v, %v), want (%+v, %v)", tc.clientID, got, source, err, tc.want, tc.source)
		}
	}
	for _, clientID := range []string{"replaced-client", ""} {
		_, source, err := withOrgApp.ResolveClient(context.Background(), testOrgID, clientID)
		if !errors.Is(err, ErrNoAtlassianOAuthApp) || source != SourceNone {
			t.Errorf("ResolveClient(%q) = (%v, %v), want ErrNoAtlassianOAuthApp", clientID, source, err)
		}
	}

	// A row whose secret is gone authenticates nothing.
	noSecret := NewOAuthAppResolver(
		&fakeJiraApps{app: &domain.OrgJiraApp{ClientID: "org-client", ClientSecretRef: jiraSecretRef}},
		&fakeSecrets{}, OAuthApp{})
	if _, _, err := noSecret.ResolveClient(context.Background(), testOrgID, "org-client"); !errors.Is(err, ErrNoAtlassianOAuthApp) {
		t.Errorf("ResolveClient with the secret gone err = %v, want ErrNoAtlassianOAuthApp", err)
	}

	boom := errors.New("db down")
	broken := NewOAuthAppResolver(&fakeJiraApps{err: boom}, &fakeSecrets{}, deployment)
	_, _, err := broken.ResolveClient(context.Background(), testOrgID, "dep-client")
	if !errors.Is(err, boom) || errors.Is(err, ErrNoAtlassianOAuthApp) {
		t.Errorf("ResolveClient over a failed read err = %v, want the read error", err)
	}
}

// TestCandidates: the apps an unattributed token is tried with, in Resolve's
// order, the same app registered as both tiers once.
func TestCandidates(t *testing.T) {
	deployment := OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"}
	org := OAuthApp{ClientID: "org-client", ClientSecret: "org-secret"}
	orgRow := func(clientID string) *fakeJiraApps {
		return &fakeJiraApps{app: &domain.OrgJiraApp{ClientID: clientID, ClientSecretRef: jiraSecretRef}}
	}
	for _, tc := range []struct {
		name string
		r    OAuthAppResolver
		want []CandidateApp
	}{
		{"both", NewOAuthAppResolver(orgRow("org-client"), &fakeSecrets{sys: map[string]string{jiraSecretRef: "org-secret"}}, deployment),
			[]CandidateApp{{org, SourceOrgOverride}, {deployment, SourceDeployment}}},
		{"deployment_only", NewOAuthAppResolver(&fakeJiraApps{}, &fakeSecrets{}, deployment),
			[]CandidateApp{{deployment, SourceDeployment}}},
		{"org_only", NewOAuthAppResolver(orgRow("org-client"), &fakeSecrets{sys: map[string]string{jiraSecretRef: "org-secret"}}, OAuthApp{}),
			[]CandidateApp{{org, SourceOrgOverride}}},
		{"same_app_twice", NewOAuthAppResolver(orgRow("dep-client"), &fakeSecrets{sys: map[string]string{jiraSecretRef: "dep-secret"}}, deployment),
			[]CandidateApp{{deployment, SourceOrgOverride}}},
		{"none", NewOAuthAppResolver(&fakeJiraApps{}, &fakeSecrets{}, OAuthApp{}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.r.Candidates(context.Background(), testOrgID)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("Candidates = (%+v, %v), want %+v", got, err, tc.want)
			}
		})
	}
}

const jiraSecretRef = "jira_oauth_client_secret"
