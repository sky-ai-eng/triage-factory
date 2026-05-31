package domain

import "time"

// Project is the top-level concept that segments work items by *concept*
// rather than by repo. SKY-211 / SKY-215. The Curator is the per-project
// long-lived Claude Code session that owns project context — its session
// id lives on this row. The knowledge base lives on disk at
// `~/.triagefactory/projects/<id>/knowledge-base/*.md`; the per-project
// classifier (SKY-220) reads it inline at vote time.
//
// CuratorSessionID was originally named DesignerSessionID; SKY-216
// renamed it to match the runtime that actually populates it. The
// rename happened via the 20260503_001_curator.sql migration on
// existing installs, with the new name carried through Go code in
// the same release.
type Project struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	CuratorSessionID string   `json:"curator_session_id,omitempty"`
	PinnedRepos      []string `json:"pinned_repos"`
	// TeamID is the project's owning team — the tenancy key validation
	// must scope against. pinned_repos / Jira-rule checks on update run
	// against *this* team's tracked set, not the org default (a non-default
	// team's project would otherwise validate against the wrong team). The
	// stores populate it on read; Create sets it from the chosen team.
	// Empty only on never-persisted structs.
	TeamID string `json:"team_id"`
	// JiraProjectKey is the Jira project key (e.g. "SKY") this
	// project is linked to, or empty if not linked. Validation at the
	// API layer requires a non-empty value to be present in
	// config.Jira.Projects. SKY-217.
	JiraProjectKey string `json:"jira_project_key"`
	// LinearProjectKey is the Linear project key/identifier this
	// project is linked to, or empty if not linked. Independent of
	// JiraProjectKey — both can be set on the same project. Linear
	// integration is future work; until it ships, validation rejects
	// any non-empty value at the API layer.
	LinearProjectKey string `json:"linear_project_key"`
	// SpecAuthorshipPromptID points at the prompt the Curator
	// materializes as a Claude Code skill (`.claude/skills/ticket-spec/`)
	// when authoring tickets for this project. Empty = use the seeded
	// system default ("system-ticket-spec"). Per-project rather than
	// global so a user with mixed teams can give each its own
	// editorial standard. SKY-221.
	SpecAuthorshipPromptID string    `json:"spec_authorship_prompt_id"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// SystemTicketSpecPromptID is the stable system_slug of the seeded
// default spec-authorship prompt (post-SKY-380 the prompt's id is a
// random UUID per team copy; this constant identifies it by slug).
// Three sites consume it:
//
//   - the seed step in main.go (writes the team's copy via SeedOrUpdate,
//     which stores this as the row's system_slug).
//   - the project-create HTTP handler, which resolves the team's copy via
//     GetBySystemSlug and auto-points new projects at its id when present.
//     The DB layer itself stores whatever it's handed (NULL when the field
//     is empty); defaulting lives at the API layer to keep the schema free
//     of any "system prompt must exist" coupling that would break tests.
//   - the curator dispatch path, which falls back to the team's copy
//     (resolved via GetBySystemSlug) at skill materialization time when a
//     project's SpecAuthorshipPromptID is empty (covers projects created
//     before the seed landed).
const SystemTicketSpecPromptID = "system-ticket-spec"
