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

// TestDefaultBaseURL_UnsetIsGitHubCom pins the acceptance criterion that the
// variable unset is byte-for-byte today's behaviour: every derivation that
// takes an empty base answers exactly as it did when github.com was a literal.
func TestDefaultBaseURL_UnsetIsGitHubCom(t *testing.T) {
	SetDefaultBaseURLForTest(t, "")
	if got := DefaultBaseURL(); got != GitHubCom {
		t.Fatalf("DefaultBaseURL() = %q with the variable unset; want %q", got, GitHubCom)
	}
	if got := ResolveBaseURL(""); got != GitHubCom {
		t.Errorf("ResolveBaseURL(\"\") = %q; want %q", got, GitHubCom)
	}
	if got := APIBase(""); got != "https://api.github.com" {
		t.Errorf("APIBase(\"\") = %q; want the public API", got)
	}
}

// TestDefaultBaseURL_SetRePointsTheEmptyBase: with a GHES default, an org that
// configured nothing resolves there — web base and API mount both — while an
// explicit base URL is untouched. This is the whole of what the variable does
// to resolution; every host-keyed row follows from these two functions.
func TestDefaultBaseURL_SetRePointsTheEmptyBase(t *testing.T) {
	SetDefaultBaseURLForTest(t, "https://ghe.example.com/")
	if got := DefaultBaseURL(); got != "https://ghe.example.com" {
		t.Fatalf("DefaultBaseURL() = %q; want the normalized GHES base", got)
	}
	if got := ResolveBaseURL(""); got != "https://ghe.example.com" {
		t.Errorf("ResolveBaseURL(\"\") = %q; want the deployment default", got)
	}
	if got := APIBase(""); got != "https://ghe.example.com/api/v3" {
		t.Errorf("APIBase(\"\") = %q; want the default's GHES mount", got)
	}
	if got := ResolveBaseURL("https://other.example.com/"); got != "https://other.example.com" {
		t.Errorf("ResolveBaseURL(explicit) = %q; a per-org base must win over the default", got)
	}
	if got := APIBase("https://github.com"); got != "https://api.github.com" {
		t.Errorf("APIBase(github.com) = %q; a github.com base stays public whatever the default", got)
	}
	// The literal is a literal: nothing about the default moves it.
	if GitHubCom != "https://github.com" {
		t.Errorf("GitHubCom = %q", GitHubCom)
	}
}

// TestParseDefaultBaseURL holds the variable to the github_base_url rule.
func TestParseDefaultBaseURL(t *testing.T) {
	good := map[string]string{
		"":                               GitHubCom,
		"   ":                            GitHubCom,
		"https://github.com":             GitHubCom,
		"https://ghe.example.com":        "https://ghe.example.com",
		"https://ghe.example.com/":       "https://ghe.example.com",
		" https://ghe.example.com/git/ ": "https://ghe.example.com/git",
		"http://localhost:3000":          "http://localhost:3000",
	}
	for in, want := range good {
		got, err := ParseDefaultBaseURL(in)
		if err != nil || got != want {
			t.Errorf("ParseDefaultBaseURL(%q) = (%q, %v); want (%q, nil)", in, got, err, want)
		}
	}
	bad := []string{
		"ghe.example.com",                 // no scheme: github_base_url refuses it, so the default does too
		"ssh://ghe.example.com",           // not http(s)
		"https://user:pw@ghe.example.com", // credentials
		"https://ghe.example.com?x=1",     // query
		"https://ghe.example.com#frag",    // fragment
		"https://",                        // no host
	}
	for _, in := range bad {
		if got, err := ParseDefaultBaseURL(in); err == nil {
			t.Errorf("ParseDefaultBaseURL(%q) = %q; want a refusal", in, got)
		}
	}
}

// TestInitDefaultBaseURL_RefusesAChangeOfMind mirrors runmode.Init: a repeat
// with the same value is a no-op, a repeat with a different value is an error
// that leaves the resolved default alone.
func TestInitDefaultBaseURL_RefusesAChangeOfMind(t *testing.T) {
	SetDefaultBaseURLForTest(t, "https://ghe.example.com")
	if err := InitDefaultBaseURL("https://ghe.example.com/"); err != nil {
		t.Fatalf("re-init with the same value: %v", err)
	}
	if err := InitDefaultBaseURL("https://other.example.com"); err == nil {
		t.Fatal("re-init with a different value succeeded; want a refusal")
	}
	if got := DefaultBaseURL(); got != "https://ghe.example.com" {
		t.Errorf("DefaultBaseURL() = %q after a refused re-init; the first answer must stand", got)
	}
}
