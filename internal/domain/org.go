package domain

import "time"

type Org struct {
	ID          string
	Name        string
	Slug        string
	OwnerUserID string
	CreatedAt   time.Time
}

// OrgMember is one row of the org People roster (TFAC-417): a member's
// identity facts plus their org-level role. GitHubUsername / JiraAccountID
// are nil when the member holds no host-scoped binding for the org's
// GitHub / Jira host — the NULL-degrades-gracefully contract the frontend
// renders as a "Not connected" readiness badge. Role is the org_role enum
// value: "owner" | "admin" | "member". IsCurrentUser is not carried here —
// the handler stamps it by comparing UserID to the caller, since the store
// has no request identity.
type OrgMember struct {
	UserID         string
	DisplayName    string
	GitHubUsername *string
	JiraAccountID  *string
	Role           string
}
