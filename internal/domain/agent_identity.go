package domain

import "time"

// Agent is the workload-identity domain type — the org's bot. One row
// per org in multi mode; one row in the synthetic single-org world in
// local mode. Distinct from Conversation (which is the execution domain in
// the same package): an Agent is "who acts," a Conversation is "what they
// did in one delegation." This domain gives the bot first-class
// identity separate from human users.
//
// Credentials: GitHubPATUserID is the PAT-borrow tier of the credential
// resolver. Multi mode prefers a per-org GitHub App installation looked
// up out-of-row in org_github_app_installations; local + B2C trials
// fall back to PAT-borrow. Local mode populates
// GitHubPATUserID with the sentinel user (the lone human in the
// synthetic single-tenant world) at bootstrap; the PAT itself still
// lives in the OS keychain and is looked up at run dispatch via the
// user identity here.
type Agent struct {
	ID                         string
	DisplayName                string
	DefaultModel               string   // "" = no default; consumer falls through to global default
	DefaultAutonomySuitability *float64 // nil = no default; consumer uses the trigger-level threshold instead
	GitHubPATUserID            string   // "" if not borrowing a PAT; local mode populates with the sentinel user at bootstrap
	GitHubOrgLogin             string   // the org credential's OWN GitHub login (PAT path) — "" until a PAT bind persists it; distinct from GitHubPATUserID (the user who pasted the PAT). The credential resolver's OrgIdentityFor reads this to stamp the org commit identity (TFAC-452).
	JiraServiceAccountID       string   // "" if no Jira service account (v2 surface)
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

// TeamAgent is the team-level membership row for an Agent. Default-
// enabled at team creation; team members can toggle Enabled per team
// and override the agent-level model / autonomy settings without
// touching the org-wide row.
//
// PerTeamModel / PerTeamAutonomySuitability override the corresponding
// fields on Agent when populated. Empty / nil falls back to the agent's
// defaults — consumers read with a small helper, not by string-compare.
type TeamAgent struct {
	TeamID                     string
	AgentID                    string
	Enabled                    bool
	PerTeamModel               string   // "" → use Agent.DefaultModel
	PerTeamAutonomySuitability *float64 // nil → use Agent.DefaultAutonomySuitability
	AddedAt                    time.Time
}
