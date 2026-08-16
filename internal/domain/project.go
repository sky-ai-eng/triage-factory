package domain

import "time"

// Project is the top-level concept that segments work items by *concept*
// rather than by repo. The Curator is the per-project
// long-lived Claude Code session that owns project context — its session
// id lives on this row. The knowledge base lives on disk at
// `~/.triagefactory/projects/<id>/knowledge-base/*.md`; the per-project
// classifier reads it inline at vote time.
//
// The curator's SDK resume handle no longer lives on the project row — the
// curator conversation's sdk_session_id absorbs it — so there is no
// session-id field here.
type Project struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// PinnedRepos holds each pinned repository as the provider's current
	// "owner/repo" name. That is the shape every consumer of a pin spends it
	// in — a worktree path, a bind-mount destination, a token scope, a
	// bundle's portable manifest — so it is the shape the store surfaces,
	// joined out of project_pinned_repos.repository_id on read.
	//
	// It is NOT the wire shape. The API addresses a repository by its
	// registry row id (`pinned_repository_ids`), and the projects handler is
	// where the two meet: it resolves ids in, renders ids out, and is the
	// only place that translates. Hence `json:"-"` — a project serialized
	// straight to a response would otherwise put names back on the wire under
	// a field no client sends.
	PinnedRepos []string `json:"-"`
	// CreatorUserID is the user who created the project — the identity the
	// projects_{insert,update,delete} RLS policies' "private" branch pins
	// visibility="private" writes to (WITH CHECK creator_user_id =
	// current_user()). Exposed read-only so the frontend can gray out the
	// "private" visibility option for anyone else, the same way it grays
	// out "team"/"org" for a non-write-member/non-admin.
	CreatorUserID string `json:"creator_user_id"`
	// TeamID is the project's owning team — the tenancy key validation
	// must scope against. pinned_repos / Jira-rule checks on update run
	// against *this* team's tracked set, not the org default (a non-default
	// team's project would otherwise validate against the wrong team). The
	// stores populate it on read; Create sets it from the chosen team.
	// Empty only on never-persisted structs, or a private/org-visibility
	// project created without one (see Visibility).
	TeamID string `json:"team_id"`
	// Visibility gates who can read/write the row: "private" (creator
	// only), "team" (the owning team's write-members; requires TeamID),
	// or "org" (every org member reads, only org admins create/downgrade
	// into it). Mirrors the projects_visibility_check CHECK and the
	// projects_{select,insert,update,delete} RLS policies, which are the
	// actual enforcement — the API layer's job is to turn a denied write
	// into a clean 400/403 instead of letting it surface as a raw RLS
	// error. Multi-mode only in practice: local mode forces this to
	// "team" regardless of what a client sends (N=1 has no team/org
	// distinction worth making).
	Visibility string `json:"visibility"`
	// JiraProjectKey is the Jira project key (e.g. "PROJ") this
	// project is linked to, or empty if not linked. Validation at the
	// API layer requires a non-empty value to be present in
	// config.Jira.Projects.
	JiraProjectKey string `json:"jira_project_key"`
	// LinearProjectKey is the Linear project key/identifier this
	// project is linked to, or empty if not linked. Independent of
	// JiraProjectKey — both can be set on the same project. Linear
	// integration is future work; until it ships, validation rejects
	// any non-empty value at the API layer.
	LinearProjectKey string `json:"linear_project_key"`
	// SpecAuthorshipBlueprintID points at the blueprint whose single step's
	// prompt the Curator materializes as a Claude Code skill
	// (`.claude/skills/ticket-spec/`) when authoring tickets for this
	// project. Empty = use the seeded system default ("system-ticket-spec").
	// Per-project rather than global so a user with mixed teams can give each
	// its own editorial standard.
	SpecAuthorshipBlueprintID string    `json:"spec_authorship_blueprint_id"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

// Project visibility values — see Project.Visibility.
const (
	ProjectVisibilityPrivate = "private"
	ProjectVisibilityTeam    = "team"
	ProjectVisibilityOrg     = "org"
)

// ValidProjectVisibility reports whether v is one of the three values the
// projects_visibility_check CHECK constraint accepts. Handlers validate
// against this before writing so a garbage value 400s cleanly instead of
// tripping the CHECK as a raw constraint-violation 500.
func ValidProjectVisibility(v string) bool {
	switch v {
	case ProjectVisibilityPrivate, ProjectVisibilityTeam, ProjectVisibilityOrg:
		return true
	}
	return false
}

// SystemTicketSpecPromptID is the stable system_slug of the seeded default
// spec-authorship unit. The shipped prompt and its wrapping 1-step blueprint
// both carry this slug (distinct tables, so no collision); the id of each is
// a random UUID per team copy, so callers resolve by slug. Three sites
// consume it:
//
//   - the seed step (writes the team's prompt + blueprint copies, storing
//     this as each row's system_slug).
//   - the project-create HTTP handler, which resolves the team's blueprint
//     copy via the blueprint store's GetBySystemSlug and auto-points new
//     projects at its id when present. The DB layer stores whatever it's
//     handed (NULL when the field is empty); defaulting lives at the API
//     layer to keep the schema free of any "system blueprint must exist"
//     coupling that would break tests.
//   - the curator dispatch path, which falls back to the team's blueprint
//     copy at skill materialization time when a project's
//     SpecAuthorshipBlueprintID is empty (covers projects created before the
//     seed landed).
const SystemTicketSpecPromptID = "system-ticket-spec"

// SystemJiraFormattingPromptID is the stable system_slug of the seeded Jira
// markup guidance the Curator materializes as an always-on skill. Unlike
// SystemTicketSpecPromptID a project never points at it — the curator resolves
// the team's copy by this slug directly — so only the seed step and that
// dispatch-time lookup consume it.
//
// It is a prompt row rather than an embedded file so a team whose Jira
// conventions differ can fix it on the Prompts page. Shipping a default nobody
// can see or change is how guidance quietly goes stale.
const SystemJiraFormattingPromptID = "system-jira-formatting"
