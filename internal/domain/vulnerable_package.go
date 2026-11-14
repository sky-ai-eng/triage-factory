package domain

import (
	"net/url"
	"slices"
	"strings"
	"time"
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
	// UpdatedAt is when GitHub last changed the alert: when it was created,
	// reopened or reintroduced, for an open alert, unless something since
	// (an assignee) changed it again. Zero when GitHub sent none.
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// DependabotUpdate is what Dependabot did about the alert as TF last read
	// it, nil when TF has not read it.
	DependabotUpdate *DependabotUpdate `json:"dependabot_update,omitempty"`
}

// The states of a DependabotUpdate.
const (
	// DependabotUpdateNone: GitHub reports no update for the alert. Dependabot
	// has not tried: security updates are off on the repository, or its
	// attempt has not started.
	DependabotUpdateNone = "none"
	// DependabotUpdatePending: an attempt has started and produced neither a
	// pull request nor an error yet.
	DependabotUpdatePending = "pending"
	// DependabotUpdatePullRequest: Dependabot opened a pull request for the
	// alert. GitHub goes on naming it once it is closed or merged.
	DependabotUpdatePullRequest = "pull_request"
	// DependabotUpdateError: Dependabot tried and reported an error instead
	// of a pull request.
	DependabotUpdateError = "error"
)

// DependabotUpdate is what Dependabot did about one alert, as GitHub's
// GraphQL alert object reports it (dependabotUpdate). The REST alert object
// does not carry it.
type DependabotUpdate struct {
	// State is one of the DependabotUpdate values.
	State string `json:"state"`
	// PullRequestNumber and PullRequestURL are the pull request Dependabot
	// opened, set when State is DependabotUpdatePullRequest.
	PullRequestNumber int    `json:"pull_request_number,omitempty"`
	PullRequestURL    string `json:"pull_request_url,omitempty"`
	// ErrorType is Dependabot's code for the error, as
	// "security_update_not_possible", and ErrorTitle its one-line description
	// of it, set when State is DependabotUpdateError. The title names
	// dependencies and versions read from the repository and its registries.
	ErrorType  string `json:"error_type,omitempty"`
	ErrorTitle string `json:"error_title,omitempty"`
}

// The reasons Dependabot cannot fix an alert, which DependabotCannotFix
// returns.
const (
	// DependabotCannotFixNoPatchedVersion: no version that fixes the
	// vulnerability has been released, so there is nothing to upgrade to.
	DependabotCannotFixNoPatchedVersion = "no_patched_version"
	// DependabotCannotFixUpdateFailed: Dependabot tried to open a pull request
	// for the alert and reported an error instead.
	DependabotCannotFixUpdateFailed = "update_failed"
)

// dependabotErrorsNotFailures are the update errors that say Dependabot had
// nothing to fix, or that a pull request it opened already proposes the fix:
// the dependency is no longer vulnerable, or an open pull request upgrades it
// to the latest version or already carries the security update. The codes
// are Dependabot's.
var dependabotErrorsNotFailures = map[string]bool{
	"security_update_not_needed":              true,
	"pull_request_exists_for_latest_version":  true,
	"pull_request_exists_for_security_update": true,
}

// DependabotCannotFix is why Dependabot cannot fix the alert, one of the
// DependabotCannotFix values, or "" when it has fixed it or may still: it
// opened a pull request, has not tried, is trying, or reported an error that
// is not a failure to fix (dependabotErrorsNotFailures). An alert with no
// patched version cannot be fixed whatever Dependabot reports. Any other
// error is a failure, a code TF does not know included.
func (a DependabotAlert) DependabotCannotFix() string {
	if a.FirstPatchedVersion == "" {
		return DependabotCannotFixNoPatchedVersion
	}
	if u := a.DependabotUpdate; u != nil && u.State == DependabotUpdateError && !dependabotErrorsNotFailures[u.ErrorType] {
		return DependabotCannotFixUpdateFailed
	}
	return ""
}

// AwaitsDependabot reports whether Dependabot may still act on the alert
// without anyone asking it to: the alert has a patched version, and GitHub
// reported neither a pull request nor an error for it when TF last read it,
// or TF has not read it.
func (a DependabotAlert) AwaitsDependabot() bool {
	if a.FirstPatchedVersion == "" {
		return false
	}
	u := a.DependabotUpdate
	return u == nil || u.State == DependabotUpdateNone || u.State == DependabotUpdatePending
}

// VulnerablePackageSnapshot is the stored state of a vulnerable package: its
// open Dependabot alerts, ordered by number. An alert that closes or opens,
// and an alert becoming one Dependabot cannot fix, are the only changes that
// emit. An empty Alerts is the terminal snapshot: the package has no open
// alert left.
type VulnerablePackageSnapshot struct {
	// Repo is the repository's owner/repo as the listing named it, for
	// display; the entity's repository_id is what identifies it.
	Repo      string            `json:"repo"`
	Ecosystem string            `json:"ecosystem"`
	Package   string            `json:"package"`
	Alerts    []DependabotAlert `json:"alerts"`
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
