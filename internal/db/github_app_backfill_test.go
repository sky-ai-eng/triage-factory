package db

import "testing"

// TestGithubAPIBase pins the private API-base derivation to the same three
// host classes internal/github.APIBase covers. The two copies must agree (the
// db copy exists only to dodge an import cycle), so this mirrors that table.
func TestGithubAPIBase(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "https://api.github.com"},
		{"https://github.com", "https://api.github.com"},
		{"https://github.com/", "https://api.github.com"},
		{"https://github.acme.com", "https://github.acme.com/api/v3"},
		{"https://github.acme.com/", "https://github.acme.com/api/v3"},
		{"http://localhost:3000", "http://localhost:3000/api/v3"},
		{"https://octocorp.ghe.com", "https://api.octocorp.ghe.com"},
		{"https://octocorp.ghe.com/", "https://api.octocorp.ghe.com"},
		// An already-api.* ghe.com host must not be double-prefixed (lockstep
		// with internal/github.APIBase).
		{"https://api.octocorp.ghe.com", "https://api.octocorp.ghe.com"},
	}
	for _, tt := range tests {
		if got := githubAPIBase(tt.in); got != tt.want {
			t.Errorf("githubAPIBase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
