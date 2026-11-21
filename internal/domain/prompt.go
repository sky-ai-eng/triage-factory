package domain

import "time"

// Prompt is a user- or system-defined delegation prompt template.
// The body contains the "mission" — what the agent should do.
// The system envelope (tool guidance, completion format, repo scoping) is always
// injected by the spawner and is not part of the prompt body.
type Prompt struct {
	ID           string `json:"id" doc:"The prompt's id."`
	Name         string `json:"name" doc:"The prompt's name."`
	Body         string `json:"body" doc:"The prompt's text, what an agent running it is asked to do."`
	Source       string `json:"source" doc:"Where the prompt came from: system for one shipped with Triage Factory, user for one a person created, imported for one imported from a SKILL.md file, or marketplace for one installed from a marketplace listing."` // "system", "user", "imported", "marketplace" (a marketplace install's copy)
	AllowedTools string `json:"allowed_tools" doc:"Tools beyond the defaults an agent running the prompt may use, comma-separated, as a SKILL.md file's frontmatter names them, or empty for none."`                                                              // comma-separated extra tools parsed from SKILL.md/agent frontmatter
	Model        string `json:"model" doc:"The model key the prompt runs on, or empty to use the team's default model."`                                                                                                                                          // per-prompt model override; "" = inherit settings.AI.Model at dispatch
	UsageCount   int    `json:"usage_count" doc:"How many agent conversations have run the prompt."`                                                                                                                                                              // how many agent conversations have used this prompt
	// TeamID is the owning team. Every prompt is team-scoped:
	// team_id is NOT NULL and is the sole scoping signal (no visibility
	// column). Handlers read this to enforce same-team references (a
	// blueprint step may only bind a prompt its own team owns).
	// Stores populate it on read; Create stamps it from the resolved
	// acting team. In SQLite (N=1) it is always the local sentinel team.
	TeamID string `json:"team_id" doc:"The id of the team that owns the prompt."`
	// SystemSlug is the stable identifier for a shipped (source='system')
	// prompt — e.g. "system-ci-fix". NULL
	// (empty) for user/imported prompts. The id is a random UUID per team
	// copy, so seed/idempotency and slug→id resolution key on this column
	// instead. promptseed.Prompts() sets it; the seeder dedupes on
	// (org_id, team_id, system_slug).
	SystemSlug string    `json:"system_slug,omitempty" doc:"The stable name of a shipped prompt, absent for any other."`
	CreatedAt  time.Time `json:"created_at" doc:"When the prompt was created, as an RFC 3339 timestamp."`
	UpdatedAt  time.Time `json:"updated_at" doc:"When the prompt was last changed, as an RFC 3339 timestamp."`
}
