package domain

import "testing"

// TestVulnerablePackageSourceID covers the key's round trip and the readers a
// pull request's key passes through: the repository resolves from it, and it
// names no pull request number.
func TestVulnerablePackageSourceID(t *testing.T) {
	for _, tc := range []struct {
		ecosystem, name string
	}{
		{"npm", "lodash"},
		{"npm", "@babel/core"},
		{"maven", "org.apache.logging.log4j:log4j-core"},
		{"go", "golang.org/x/net"},
		{"pip", "odd#name"},
	} {
		key := VulnerablePackageSourceID("octo/api", tc.ecosystem, tc.name)
		repo, ecosystem, name, ok := SplitVulnerablePackageSourceID(key)
		if !ok || repo != "octo/api" || ecosystem != tc.ecosystem || name != tc.name {
			t.Errorf("SplitVulnerablePackageSourceID(%q) = (%q, %q, %q, %v)", key, repo, ecosystem, name, ok)
		}
		owner, r, number := SplitGitHubEntitySourceID(key)
		if owner != "octo" || r != "api" || number != 0 {
			t.Errorf("SplitGitHubEntitySourceID(%q) = (%q, %q, %d), want octo/api and no number", key, owner, r, number)
		}
	}
	for _, key := range []string{"octo/api#7", "octo/api", "octo#npm:x", "octo/api#npm", "octo/api#:x", "octo/api#npm:", "/api#npm:x"} {
		if _, _, _, ok := SplitVulnerablePackageSourceID(key); ok {
			t.Errorf("SplitVulnerablePackageSourceID(%q) accepted a key that names no package", key)
		}
	}
}

func TestVulnerablePackageURL(t *testing.T) {
	if got, want := VulnerablePackageURL("https://ghe.example.com/", "octo/api", "npm", "@babel/core"),
		"https://ghe.example.com/octo/api/security/dependabot?q=is%3Aopen+ecosystem%3Anpm+package%3A%40babel%2Fcore"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if got := VulnerablePackageURL("", "octo/api", "npm", "x"); got != "" {
		t.Errorf("url with no host = %q, want none", got)
	}
}

func TestVulnerablePackageSnapshot_HighestSeverityAndTerminal(t *testing.T) {
	snap := VulnerablePackageSnapshot{Alerts: []DependabotAlert{{Number: 1, Severity: "low"}, {Number: 2, Severity: "critical"}, {Number: 3, Severity: "medium"}}}
	if got := snap.HighestSeverity(); got != DependabotSeverityCritical {
		t.Errorf("HighestSeverity = %q, want critical", got)
	}
	if snap.Terminal() {
		t.Error("a snapshot with open alerts is terminal")
	}
	if got := (VulnerablePackageSnapshot{}).HighestSeverity(); got != "" || !(VulnerablePackageSnapshot{}).Terminal() {
		t.Errorf("an empty snapshot: highest %q, terminal %v; want none and terminal", got, (VulnerablePackageSnapshot{}).Terminal())
	}
	if a, ok := snap.Alert(2); !ok || a.Severity != "critical" {
		t.Errorf("Alert(2) = %+v, %v", a, ok)
	}
	if _, ok := snap.Alert(9); ok {
		t.Error("Alert(9) found an alert that is not open")
	}
}
