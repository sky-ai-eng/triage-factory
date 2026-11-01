package server

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TestGitHubHostNormalizationAgrees: the host stamped on an installation row
// (db.EffectiveGitHubHost) must equal the one user_github_identities is keyed
// under (resolveGitHubHost), for every host shape, or the two never match.
func TestGitHubHostNormalizationAgrees(t *testing.T) {
	for _, tc := range []struct {
		name string
		base string
		want string
	}{
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
				t.Errorf("db.EffectiveGitHubHost(%q) = %q; want the identity table's %q", tc.base, got, identity)
			}
		})
	}
}
