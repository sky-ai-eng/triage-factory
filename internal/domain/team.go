package domain

import "time"

// Team is one row of the teams table — the membership/visibility unit
// inside an org. Most work rows (tasks, projects, prompts, event
// handlers) carry a team_id FK; the multi-team selector reads the
// requesting user's teams to render the per-page filter and the
// write-time picker (both count-gated on ≥2 teams).
//
// Only the display-relevant columns are modeled here. Per-team config
// (AI thresholds, default model, tracked Jira projects) lives on
// TeamSettings; this struct is the lightweight identity row the
// selectors enumerate.
type Team struct {
	ID        string
	OrgID     string
	Slug      string
	Name      string
	CreatedAt time.Time
	// Role is the requesting user's membership role in this team
	// ("admin" | "member" | "viewer"), populated by ListForUser so the
	// settings surface can gate the Team section + its selector to teams
	// the caller admins. Empty on the lightweight identity paths that
	// don't carry a per-user relationship.
	Role string
}

// TeamMember is one row of the team roster (TFAC-444): a member's identity
// facts plus their team-level role. The team-tier sibling of OrgMember,
// structurally identical but distinct so the two membership tiers don't share
// a type whose Role semantics differ (membership_role vs org_role).
// GitHubUsername / JiraAccountID are nil when the member holds no host-scoped
// binding for the org's GitHub / Jira host — the NULL-degrades-gracefully
// contract the frontend renders as a "Not connected" readiness badge. Role is
// the membership_role enum value: "admin" | "member" | "viewer". IsCurrentUser
// is not carried here — the handler stamps it by comparing UserID to the
// caller, since the store has no request identity.
type TeamMember struct {
	UserID         string
	DisplayName    string
	GitHubUsername *string
	JiraAccountID  *string
	Role           string
}
