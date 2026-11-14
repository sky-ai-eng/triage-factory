package domain

import (
	"fmt"
	"strconv"
	"time"
)

// GitHubRefreshKind names what a refresh mark asks the poll to refresh in its
// repository. Stored as github_refresh_marks.kind.
type GitHubRefreshKind string

const (
	// GitHubRefreshPullRequest targets one pull request; the target is its
	// number in decimal.
	GitHubRefreshPullRequest GitHubRefreshKind = "pull_request"
	// GitHubRefreshHeadSHA targets the pull requests whose head is at a commit;
	// the target is the commit's SHA in lowercase hex. A check run or suite on a
	// fork's pull request lists no pull request, so its head SHA is the only
	// way to find the pull request it is about.
	GitHubRefreshHeadSHA GitHubRefreshKind = "head_sha"
	// GitHubRefreshBranch targets a branch; the target is its name without
	// refs/heads/.
	GitHubRefreshBranch GitHubRefreshKind = "branch"
	// GitHubRefreshDependabotAlerts targets the repository's open Dependabot
	// alerts, which the poll lists as a whole; the target is empty, since one
	// listing reads every alert and there is nothing within the repository to
	// name.
	GitHubRefreshDependabotAlerts GitHubRefreshKind = "dependabot_alerts"
)

// GitHubRefreshTarget is one thing in a repository a webhook delivery said
// changed. Two targets that compare equal are the same mark, so a target is
// built through the constructors below or checked by Validate, which hold it
// to one spelling per thing.
type GitHubRefreshTarget struct {
	Kind  GitHubRefreshKind
	Value string
}

// PullRequestRefreshTarget targets pull request number n.
func PullRequestRefreshTarget(n int) GitHubRefreshTarget {
	return GitHubRefreshTarget{Kind: GitHubRefreshPullRequest, Value: strconv.Itoa(n)}
}

// HeadSHARefreshTarget targets the pull requests whose head is at sha.
func HeadSHARefreshTarget(sha string) GitHubRefreshTarget {
	return GitHubRefreshTarget{Kind: GitHubRefreshHeadSHA, Value: sha}
}

// BranchRefreshTarget targets the branch named branch, given without
// refs/heads/.
func BranchRefreshTarget(branch string) GitHubRefreshTarget {
	return GitHubRefreshTarget{Kind: GitHubRefreshBranch, Value: branch}
}

// DependabotAlertsRefreshTarget targets the repository's open Dependabot
// alerts.
func DependabotAlertsRefreshTarget() GitHubRefreshTarget {
	return GitHubRefreshTarget{Kind: GitHubRefreshDependabotAlerts}
}

// DependabotAlerts reports whether the target is the repository's Dependabot
// alerts, which a listing of them refreshes. Every other target names pull
// requests or a branch, which a pass over the repository's pull requests
// refreshes.
func (t GitHubRefreshTarget) DependabotAlerts() bool {
	return t.Kind == GitHubRefreshDependabotAlerts
}

// Validate refuses a target with an unknown kind, or with a value that is not
// the one spelling its kind allows: a pull request number in decimal with no
// sign or leading zero, a SHA as 40 or 64 lowercase hex digits, a non-empty
// branch name, or no value for the Dependabot alerts. A second spelling of one pull request or commit would be a
// second mark for it, and the deliveries naming it would stop collapsing.
func (t GitHubRefreshTarget) Validate() error {
	switch t.Kind {
	case GitHubRefreshPullRequest:
		n, err := strconv.Atoi(t.Value)
		if err != nil || n <= 0 || strconv.Itoa(n) != t.Value {
			return fmt.Errorf("pull request refresh target %q is not a pull request number", t.Value)
		}
	case GitHubRefreshHeadSHA:
		if !isLowerHexSHA(t.Value) {
			return fmt.Errorf("head SHA refresh target %q is not a lowercase hex commit SHA", t.Value)
		}
	case GitHubRefreshBranch:
		if t.Value == "" {
			return fmt.Errorf("branch refresh target is empty")
		}
	case GitHubRefreshDependabotAlerts:
		if t.Value != "" {
			return fmt.Errorf("dependabot alerts refresh target %q names something; it names the repository's alerts as a whole", t.Value)
		}
	default:
		return fmt.Errorf("unknown refresh target kind %q", t.Kind)
	}
	return nil
}

// isLowerHexSHA reports whether s is a SHA-1 or SHA-256 object name as GitHub
// writes one.
func isLowerHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// GitHubRefreshMark is one pending refresh mark: a target in a tracked
// repository that a webhook delivery said changed and no poll pass has
// refreshed since. Every delivery naming the same target lands on the one
// row, so a mark says only that its target changed at least once.
type GitHubRefreshMark struct {
	ID           string
	OrgID        string
	RepositoryID string
	Target       GitHubRefreshTarget
	// Revision counts the deliveries that have landed on the row. A pass
	// clears a mark only at the revision it read, so a delivery that lands
	// while the pass runs keeps the mark for the next one.
	Revision int64
	// FirstMarkedAt is when the first delivery since the mark was last
	// cleared landed; MarkedAt is when the latest one did.
	FirstMarkedAt time.Time
	MarkedAt      time.Time
}
