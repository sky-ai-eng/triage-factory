package domain

import (
	"fmt"
	"time"
)

// DependabotAlertsAccess is whether the org's GitHub credential can read a
// repository's Dependabot alerts, as GitHub last answered the list request.
type DependabotAlertsAccess string

const (
	// DependabotAlertsReadable: GitHub listed the repository's alerts.
	DependabotAlertsReadable DependabotAlertsAccess = "readable"
	// DependabotAlertsNoPermission: GitHub refused the credential. An App
	// without the Dependabot alerts permission, a token without the scope,
	// or a token whose account is not allowed to see the repository's alerts.
	DependabotAlertsNoPermission DependabotAlertsAccess = "no_permission"
	// DependabotAlertsDisabled: Dependabot alerts are turned off on the
	// repository, so there is nothing to read whatever the credential holds.
	DependabotAlertsDisabled DependabotAlertsAccess = "disabled"
)

// Validate refuses a value outside the three answers, which the stores would
// otherwise hand to a CHECK constraint to reject.
func (a DependabotAlertsAccess) Validate() error {
	switch a {
	case DependabotAlertsReadable, DependabotAlertsNoPermission, DependabotAlertsDisabled:
		return nil
	}
	return fmt.Errorf("dependabot alerts access %q is not one of readable, no_permission, disabled", string(a))
}

// DependabotAccessRecheck is how long a recorded answer stands before the
// poll asks GitHub again. It is how TF notices a credential gaining or losing
// the permission, or alerts being turned on or off on a repository, at a cost
// of one request per tracked repository per interval.
const DependabotAccessRecheck = 24 * time.Hour

// DependabotAccessStaleAfter is the age past which a recorded answer no longer
// counts. It is twice the recheck interval, so an answer the poll is due to
// replace still counts until it does. An older answer is for a repository the
// poll no longer reaches: it left every grant the org's credential reads, or
// the poll has failed for that long. Such an answer describes a credential
// the org may no longer hold.
const DependabotAccessStaleAfter = 2 * DependabotAccessRecheck

// RepositoryDependabotAccess is the stored answer for one repository: its
// registry row, the answer, and when GitHub gave it. Beside it is the state of
// the poll's listing of the repository's open alerts, which runs only where
// the answer is readable.
type RepositoryDependabotAccess struct {
	RepositoryID string
	OrgID        string
	Access       DependabotAlertsAccess
	CheckedAt    time.Time

	// AlertsETag is the ETag GitHub answered the poll's last listing of the
	// repository's open alerts with, sent back as the next listing's
	// condition. "" when there is none to send: the repository has not been
	// listed since a team last tracked it on the org's host, some of the
	// listing's writes did not land, or the listing took more than one page
	// (GitHub orders the open alerts newest first, so an alert closing past
	// the first page leaves the first page, and its ETag, unchanged).
	AlertsETag string
}

// Listed reports whether the poll lists the repository's open alerts, which
// it does when the answer says they can be read. A repository with no answer
// (a nil a) is not listed. A webhook delivery marks a repository's alerts
// only when this holds, and a pass clears a mark on alerts for which it does
// not.
func (a *RepositoryDependabotAccess) Listed() bool {
	return a != nil && a.Access == DependabotAlertsReadable
}

// DependabotAccessCounts counts an org's tracked repositories on one GitHub
// host by their stored answer. Unchecked is the tracked repositories with no
// current answer: the poll has not reached them yet, or their answer is older
// than DependabotAccessStaleAfter.
type DependabotAccessCounts struct {
	Readable     int
	NoPermission int
	Disabled     int
	Unchecked    int
}
