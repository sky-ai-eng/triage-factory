package domain

import (
	"fmt"
	"time"
)

// DependabotAlertsAccess is whether the org's GitHub credential can read a
// repository's Dependabot alerts, as GitHub last answered the list request.
// Only a readable repository's alerts are polled.
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

// Polled reports whether the poll lists the repository's alerts.
func (a DependabotAlertsAccess) Polled() bool { return a == DependabotAlertsReadable }

// RepositoryDependabotAccess is the stored answer for one repository: its
// registry row, the answer, and when GitHub gave it.
type RepositoryDependabotAccess struct {
	RepositoryID string
	OrgID        string
	Access       DependabotAlertsAccess
	CheckedAt    time.Time
}

// DependabotAccessCounts counts an org's tracked repositories on one GitHub
// host by their stored answer. Unchecked is the tracked repositories with no
// answer yet: the poll has not reached them, or no credential of the org
// reads them.
type DependabotAccessCounts struct {
	Readable     int
	NoPermission int
	Disabled     int
	Unchecked    int
}
