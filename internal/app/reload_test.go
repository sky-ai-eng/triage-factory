package app

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestGitHubConfigured pins the profiler-trigger gate (TFAC-331). Profiling
// must fire for a PAT org OR an active-App org, but NOT for a staged App
// (active=false, mid PAT→App switch per TFAC-328) — the bug it fixes was that
// App-only orgs never profiled because both triggers gated on the PAT alone.
func TestGitHubConfigured(t *testing.T) {
	const baseURL = "https://github.com"
	activeApp := &domain.OrgGitHubApp{ClientID: "Iv1.abc", Active: true}
	stagedApp := &domain.OrgGitHubApp{ClientID: "Iv1.abc", Active: false}
	activeNoClientID := &domain.OrgGitHubApp{ClientID: "", Active: true}

	tests := []struct {
		name  string
		creds auth.Credentials
		app   *domain.OrgGitHubApp
		want  bool
	}{
		{"pat only", auth.Credentials{GitHubPAT: "ghp_x", GitHubURL: baseURL}, nil, true},
		{"active app only, no pat", auth.Credentials{}, activeApp, true},
		{"staged app only must not count", auth.Credentials{}, stagedApp, false},
		{"active app with empty client_id must not count", auth.Credentials{}, activeNoClientID, false},
		{"neither pat nor app", auth.Credentials{}, nil, false},
		// The PAT branch keeps its base-URL requirement (a PAT with no host
		// can't reach GitHub); the App branch deliberately does not, since App
		// orgs resolve their host from org_settings inside the resolver.
		{"pat without base url, no app", auth.Credentials{GitHubPAT: "ghp_x"}, nil, false},
		{"pat without base url but active app, app branch wins", auth.Credentials{GitHubPAT: "ghp_x"}, activeApp, true},
		{"pat and active app", auth.Credentials{GitHubPAT: "ghp_x", GitHubURL: baseURL}, activeApp, true},
		{"pat live alongside a staged app still true via pat", auth.Credentials{GitHubPAT: "ghp_x", GitHubURL: baseURL}, stagedApp, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := githubConfigured(tt.creds, tt.app); got != tt.want {
				t.Errorf("githubConfigured(%+v, %+v) = %v, want %v", tt.creds, tt.app, got, tt.want)
			}
		})
	}
}
