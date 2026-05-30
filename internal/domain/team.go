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
}
