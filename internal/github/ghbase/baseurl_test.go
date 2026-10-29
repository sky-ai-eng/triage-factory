package ghbase

import "testing"

// TestCanonicalBaseURL pins the one canonical spelling of a GitHub host or Jira
// site: scheme and authority fold to lowercase, the path keeps its case, and
// whitespace and trailing slashes are trimmed.
func TestCanonicalBaseURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://github.com", "https://github.com"},
		{"https://GitHub.com", "https://github.com"},
		{"HTTPS://GITHUB.COM/", "https://github.com"},
		{"  https://GHE.Acme.com//  ", "https://ghe.acme.com"},
		{"https://GHE.acme.com:8443", "https://ghe.acme.com:8443"},
		{"https://GHE.acme.com/GitHub/Path", "https://ghe.acme.com/GitHub/Path"},
		{"https://GHE.acme.com:8443/GitHub/", "https://ghe.acme.com:8443/GitHub"},
		{"http://[::1]:8080/Ctx", "http://[::1]:8080/Ctx"},
		{"https://Ünicode.Example.com", "https://Ünicode.example.com"},
		{"GHE.acme.com/Path", "GHE.acme.com/Path"},
		{"", ""},
		{"   ", ""},
		{"/", ""},
	}
	for _, tt := range tests {
		if got := CanonicalBaseURL(tt.in); got != tt.want {
			t.Errorf("CanonicalBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAPIBase(t *testing.T) {
	tests := []struct{ in, want string }{
		// No web base is no GitHub, and no API base.
		{"", ""},
		{"   ", ""},
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
		// The host class is read from the canonical form, so capitalisation
		// does not move a host into another class.
		{"https://GitHub.com", "https://api.github.com"},
		{"https://OctoCorp.GHE.com", "https://api.octocorp.ghe.com"},
		{"https://GitHub.Acme.com/Ctx", "https://github.acme.com/Ctx/api/v3"},
	}
	for _, tt := range tests {
		if got := APIBase(tt.in); got != tt.want {
			t.Errorf("APIBase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestDefaultBaseURL_UnsetIsGitHubCom: with the variable unset the
// deployment's GitHub is github.com.
func TestDefaultBaseURL_UnsetIsGitHubCom(t *testing.T) {
	SetDefaultBaseURLForTest(t, "")
	if got := DefaultBaseURL(); got != GitHubCom {
		t.Fatalf("DefaultBaseURL() = %q with the variable unset; want %q", got, GitHubCom)
	}
}

// TestDefaultBaseURL_MovesNothingElse: the variable names the deployment's
// GitHub and nothing more. An empty base is still no GitHub — no API base,
// whatever the default — an explicit base is untouched, and a github.com base
// stays public.
func TestDefaultBaseURL_MovesNothingElse(t *testing.T) {
	SetDefaultBaseURLForTest(t, "https://ghe.example.com/")
	if got := DefaultBaseURL(); got != "https://ghe.example.com" {
		t.Fatalf("DefaultBaseURL() = %q; want the normalized GHES base", got)
	}
	if got := APIBase(""); got != "" {
		t.Errorf("APIBase(\"\") = %q; an empty base is no GitHub, not the default", got)
	}
	if got := APIBase("https://other.example.com/"); got != "https://other.example.com/api/v3" {
		t.Errorf("APIBase(explicit) = %q; a per-org base is its own", got)
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
		"https://GHE.Example.com/Git/":   "https://ghe.example.com/Git",
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

// TestNamesGitHubCom: every spelling of a github.com web base is github.com,
// and nothing else is, including an empty value, a GHES host and a GHE.com
// tenant.
func TestNamesGitHubCom(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://github.com":        true,
		"https://GitHub.com/":       true,
		" https://github.com ":      true,
		"http://github.com":         true,
		"https://github.com:443":    true,
		"":                          false,
		"github.com":                false,
		"https://api.github.com":    false,
		"https://ghe.example.com":   false,
		"https://acme.ghe.com":      false,
		"https://github.com.evil.x": false,
	} {
		if got := NamesGitHubCom(raw); got != want {
			t.Errorf("NamesGitHubCom(%q) = %v, want %v", raw, got, want)
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
