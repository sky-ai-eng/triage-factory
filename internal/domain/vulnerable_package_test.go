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

// TestDependabotAlert_DependabotCannotFix covers why Dependabot cannot fix an
// alert, and whether it may still act on one, for each update GitHub reports.
func TestDependabotAlert_DependabotCannotFix(t *testing.T) {
	update := func(state, errorType string) *DependabotUpdate {
		return &DependabotUpdate{State: state, ErrorType: errorType}
	}
	for _, tc := range []struct {
		name      string
		alert     DependabotAlert
		cannotFix string
		awaits    bool
	}{
		{"unread", DependabotAlert{FirstPatchedVersion: "1.0"}, "", true},
		{"untried", DependabotAlert{FirstPatchedVersion: "1.0", DependabotUpdate: update(DependabotUpdateNone, "")}, "", true},
		{"trying", DependabotAlert{FirstPatchedVersion: "1.0", DependabotUpdate: update(DependabotUpdatePending, "")}, "", true},
		{"a pull request", DependabotAlert{FirstPatchedVersion: "1.0", DependabotUpdate: update(DependabotUpdatePullRequest, "")}, "", false},
		{"a failure", DependabotAlert{FirstPatchedVersion: "1.0", DependabotUpdate: update(DependabotUpdateError, "security_update_not_possible")}, DependabotCannotFixUpdateFailed, false},
		{"a code TF does not know", DependabotAlert{FirstPatchedVersion: "1.0", DependabotUpdate: update(DependabotUpdateError, "something_new")}, DependabotCannotFixUpdateFailed, false},
		{"a pull request already proposes it", DependabotAlert{FirstPatchedVersion: "1.0", DependabotUpdate: update(DependabotUpdateError, "pull_request_exists_for_latest_version")}, "", false},
		{"no longer vulnerable", DependabotAlert{FirstPatchedVersion: "1.0", DependabotUpdate: update(DependabotUpdateError, "security_update_not_needed")}, "", false},
		{"no patched version", DependabotAlert{}, DependabotCannotFixNoPatchedVersion, false},
		{"no patched version and a failure", DependabotAlert{DependabotUpdate: update(DependabotUpdateError, "security_update_not_found")}, DependabotCannotFixNoPatchedVersion, false},
	} {
		if got := tc.alert.DependabotCannotFix(); got != tc.cannotFix {
			t.Errorf("%s: DependabotCannotFix() = %q, want %q", tc.name, got, tc.cannotFix)
		}
		if got := tc.alert.AwaitsDependabot(); got != tc.awaits {
			t.Errorf("%s: AwaitsDependabot() = %v, want %v", tc.name, got, tc.awaits)
		}
	}
}
