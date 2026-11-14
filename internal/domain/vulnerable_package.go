package domain

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// VulnerablePackageSourceID is the key of a vulnerable package's entity:
// "owner/repo#ecosystem:name", as "octo/api#npm:lodash". A vulnerable package
// is one package of one ecosystem in one repository with at least one open
// Dependabot alert. GitHub has no object for that grouping, so the entity has
// no provider id, and the key is its identity among the active rows of its
// scope: once the package has no open alert the entity closes, and a later
// alert on the package creates a new entity under the same key.
//
// The repository comes first and ends at '#', the shape of a pull request's
// key, so the repository is resolved from the key through the same door
// (db.GitHubEntityRepoRef) and a repository rename rewrites the key with every
// other key of the repository. The ecosystem is one word from GitHub's
// vocabulary, so the first ':' after it ends it, and the name may hold
// anything after that (a Maven "group:artifact", a Go module path).
func VulnerablePackageSourceID(repoSlug, ecosystem, name string) string {
	return repoSlug + "#" + ecosystem + ":" + name
}

// SplitVulnerablePackageSourceID splits a vulnerable package's key into the
// repository's owner/repo, the ecosystem and the package name. ok is false for
// a key with any of the three missing, which is every pull request's key.
func SplitVulnerablePackageSourceID(key string) (repoSlug, ecosystem, name string, ok bool) {
	repoSlug, pkg, found := strings.Cut(key, "#")
	if !found {
		return "", "", "", false
	}
	owner, repo, _ := strings.Cut(repoSlug, "/")
	ecosystem, name, found = strings.Cut(pkg, ":")
	if owner == "" || repo == "" || !found || ecosystem == "" || name == "" {
		return "", "", "", false
	}
	return repoSlug, ecosystem, name, true
}

// VulnerablePackageURL is the link a vulnerable package's entity is stored
// under: the repository's Dependabot alert list on host (a GitHubHost value),
// filtered to the package's open alerts. "" when host names no GitHub.
func VulnerablePackageURL(host, repoSlug, ecosystem, name string) string {
	host = strings.TrimRight(host, "/")
	if host == "" {
		return ""
	}
	q := url.Values{"q": {"is:open ecosystem:" + ecosystem + " package:" + name}}
	return host + "/" + repoSlug + "/security/dependabot?" + q.Encode()
}

// DependabotAlertURL is GitHub's page for one Dependabot alert, the html_url
// GitHub answers with it: alert number in the repository on host (a
// GitHubHost value). "" when host names no GitHub.
func DependabotAlertURL(host, repoSlug string, number int) string {
	host = strings.TrimRight(host, "/")
	if host == "" {
		return ""
	}
	return host + "/" + repoSlug + "/security/dependabot/" + strconv.Itoa(number)
}

// VulnerablePackageTitle is a vulnerable package's entity title: the package
// and its ecosystem, as "lodash (npm)".
func VulnerablePackageTitle(ecosystem, name string) string {
	return name + " (" + ecosystem + ")"
}

// The severities GitHub gives a Dependabot alert, lowest first.
const (
	DependabotSeverityLow      = "low"
	DependabotSeverityMedium   = "medium"
	DependabotSeverityHigh     = "high"
	DependabotSeverityCritical = "critical"
)

// DependabotSeverityRank orders severity among GitHub's four, low being 1 and
// critical 4. A value outside them ranks 0, below every known severity.
func DependabotSeverityRank(severity string) int {
	switch severity {
	case DependabotSeverityLow:
		return 1
	case DependabotSeverityMedium:
		return 2
	case DependabotSeverityHigh:
		return 3
	case DependabotSeverityCritical:
		return 4
	}
	return 0
}

// The scopes GitHub gives an alert's vulnerable dependency. An alert can have
// neither, when GitHub does not know.
const (
	DependabotScopeRuntime     = "runtime"
	DependabotScopeDevelopment = "development"
)

// DependabotAlert is one open Dependabot alert, as a vulnerable package's
// snapshot records it: the fields that tell one alert from another and that
// a person deciding on the upgrade reads. The advisory's Markdown description
// is not kept: it has no length limit and is repeated on every alert of the
// advisory, and the summary names what the advisory is about.
type DependabotAlert struct {
	// Number is the alert's number in its repository, which GitHub never
	// reuses there and which survives the alert being fixed and reopened.
	Number int `json:"number"`
	// GHSAID is the advisory's GitHub id, and CVEID its CVE id, empty when
	// the advisory has none.
	GHSAID string `json:"ghsa_id"`
	CVEID  string `json:"cve_id,omitempty"`
	// Summary is the advisory's one-line plain-text summary, at most 1,024
	// characters. It is written outside the org.
	Summary string `json:"summary,omitempty"`
	// Severity is one of the DependabotSeverity values.
	Severity string `json:"severity"`
	// ManifestPath is the manifest the vulnerable dependency was found in,
	// relative to the repository root.
	ManifestPath string `json:"manifest_path"`
	// Scope is DependabotScopeRuntime or DependabotScopeDevelopment, empty
	// when GitHub does not know.
	Scope string `json:"scope,omitempty"`
	// VulnerableVersionRange is the package versions the vulnerability
	// affects, in GitHub's range syntax, as "< 4.17.21".
	VulnerableVersionRange string `json:"vulnerable_version_range,omitempty"`
	// FirstPatchedVersion is the first version that fixes the vulnerability,
	// empty when none has been released.
	FirstPatchedVersion string `json:"first_patched_version,omitempty"`
}

// VulnerablePackageSnapshot is the stored state of a vulnerable package: its
// open Dependabot alerts, ordered by number. The diff reads nothing else, so
// an alert that closes or opens is the only change that emits. An empty
// Alerts is the terminal snapshot: the package has no open alert left.
type VulnerablePackageSnapshot struct {
	// Repo is the repository's owner/repo as the listing named it, for
	// display; the entity's repository_id is what identifies it.
	Repo      string            `json:"repo"`
	Ecosystem string            `json:"ecosystem"`
	Package   string            `json:"package"`
	Alerts    []DependabotAlert `json:"alerts"`
}

// ParseVulnerablePackageSnapshot parses a vulnerable package's stored
// snapshot. ok=false when there is none, or one that does not parse.
func ParseVulnerablePackageSnapshot(raw string) (VulnerablePackageSnapshot, bool) {
	if raw == "" {
		return VulnerablePackageSnapshot{}, false
	}
	var snap VulnerablePackageSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil || snap.Package == "" {
		return VulnerablePackageSnapshot{}, false
	}
	return snap, true
}

// TaskVulnerablePackage is what a task read carries of its vulnerable
// package's entity: the GitHub host the entity is keyed under, which every
// alert link is on, and the stored snapshot, whose alerts are the ones open
// when TF last listed the repository. Snapshot is the zero value when the
// entity has no readable snapshot.
type TaskVulnerablePackage struct {
	Host     string                    `json:"host"`
	Snapshot VulnerablePackageSnapshot `json:"snapshot"`
}

// Terminal reports whether the package has no open alert left.
func (s VulnerablePackageSnapshot) Terminal() bool {
	return len(s.Alerts) == 0
}

// HighestSeverity is the highest severity among the snapshot's open alerts,
// "" when it has none.
func (s VulnerablePackageSnapshot) HighestSeverity() string {
	best := ""
	for _, a := range s.Alerts {
		if DependabotSeverityRank(a.Severity) > DependabotSeverityRank(best) {
			best = a.Severity
		}
	}
	return best
}

// Alert returns the open alert numbered number, ok=false when none is.
func (s VulnerablePackageSnapshot) Alert(number int) (DependabotAlert, bool) {
	i := slices.IndexFunc(s.Alerts, func(a DependabotAlert) bool { return a.Number == number })
	if i < 0 {
		return DependabotAlert{}, false
	}
	return s.Alerts[i], true
}

// SortDependabotAlerts orders alerts by number, the order a snapshot stores
// them in, so two listings of the same alerts store the same bytes.
func SortDependabotAlerts(alerts []DependabotAlert) {
	slices.SortFunc(alerts, func(a, b DependabotAlert) int { return a.Number - b.Number })
}
