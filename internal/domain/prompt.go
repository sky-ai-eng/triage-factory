package domain

import "time"

// Prompt is a user- or system-defined delegation prompt template.
// The body contains the "mission" — what the agent should do.
// The system envelope (tool guidance, completion format, repo scoping) is always
// injected by the spawner and is not part of the prompt body.
type Prompt struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Body         string `json:"body"`
	Source       string `json:"source"`        // "system", "user", "imported", "marketplace" (TFAC-538 install copy)
	AllowedTools string `json:"allowed_tools"` // comma-separated extra tools parsed from SKILL.md/agent frontmatter
	Model        string `json:"model"`         // per-prompt model override; "" = inherit settings.AI.Model at dispatch
	UsageCount   int    `json:"usage_count"`   // how many agent conversations have used this prompt
	// TeamID is the owning team. Every prompt is team-scoped:
	// team_id is NOT NULL and is the sole scoping signal (no visibility
	// column). Handlers read this to enforce same-team references (a
	// blueprint step may only bind a prompt its own team owns).
	// Stores populate it on read; Create stamps it from the resolved
	// acting team. In SQLite (N=1) it is always the local sentinel team.
	TeamID string `json:"team_id"`
	// SystemSlug is the stable identifier for a shipped (source='system')
	// prompt — e.g. "system-ci-fix". NULL
	// (empty) for user/imported prompts. The id is a random UUID per team
	// copy, so seed/idempotency and slug→id resolution key on this column
	// instead. promptseed.Prompts() sets it; the seeder dedupes on
	// (org_id, team_id, system_slug).
	SystemSlug string    `json:"system_slug,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
