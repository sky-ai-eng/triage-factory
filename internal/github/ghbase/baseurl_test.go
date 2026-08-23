package ghbase

import "testing"

func TestResolveBaseURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "https://github.com"},
		{"https://github.com", "https://github.com"},
		{"https://github.com/", "https://github.com"},
		{"https://github.acme.com", "https://github.acme.com"},
		{"https://github.acme.com/", "https://github.acme.com"},
	}
	for _, tt := range tests {
		if got := ResolveBaseURL(tt.in); got != tt.want {
			t.Errorf("ResolveBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAPIBase(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "https://api.github.com"},
		{"https://github.com", "https://api.github.com"},
		{"https://github.com/", "https://api.github.com"},
		{"https://github.acme.com", "https://github.acme.com/api/v3"},
		{"https://github.acme.com/", "https://github.acme.com/api/v3"},
		{"http://localhost:3000", "http://localhost:3000/api/v3"},
		// GitHub Enterprise Cloud data residency: API on an api.* subdomain,
		// not the GHES /api/v3 path mount.
		{"https://octocorp.ghe.com", "https://api.octocorp.ghe.com"},
		{"https://octocorp.ghe.com/", "https://api.octocorp.ghe.com"},
		// An already-api.* ghe.com host must not be double-prefixed.
		{"https://api.octocorp.ghe.com", "https://api.octocorp.ghe.com"},
		// Defensive: a github.com base carrying a port still resolves public.
		{"https://github.com:443", "https://api.github.com"},
	}
	for _, tt := range tests {
		if got := APIBase(tt.in); got != tt.want {
			t.Errorf("APIBase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
