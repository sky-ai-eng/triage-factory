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

	// AlertsListedAt is when the poll last listed the repository's open
	// alerts in full. nil when it has not since the repository was last
	// tracked: the next listing is then the repository's baseline, which
	// stores the packages it finds and emits nothing for the alerts already
	// open.
	AlertsListedAt *time.Time
	// AlertsETag is the ETag GitHub answered that listing with, sent back as
	// the next listing's condition. "" when the listing took more than one
	// page: GitHub orders the open alerts newest first, so an alert closing
	// past the first page leaves the first page, and its ETag, unchanged.
	AlertsETag string
}

// AlertsBaselined reports whether the repository's alerts have a baseline, so
// a listing of them emits.
func (a RepositoryDependabotAccess) AlertsBaselined() bool {
	return a.AlertsListedAt != nil
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
