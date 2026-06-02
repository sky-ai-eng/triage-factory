package domain

import "time"

// BlueprintRunStatus is the lifecycle state of a BlueprintRun.
type BlueprintRunStatus string

// BlueprintRun statuses. running until any terminal; aborted when a step
// records --abort or omits a verdict; failed for infrastructure errors;
// cancelled when the user cancels mid-blueprint.
const (
	BlueprintRunStatusRunning   BlueprintRunStatus = "running"
	BlueprintRunStatusCompleted BlueprintRunStatus = "completed"
	BlueprintRunStatusAborted   BlueprintRunStatus = "aborted"
	BlueprintRunStatusFailed    BlueprintRunStatus = "failed"
	BlueprintRunStatusCancelled BlueprintRunStatus = "cancelled"
)

// BlueprintTriggerType distinguishes how a blueprint run was initiated.
type BlueprintTriggerType string

const (
	BlueprintTriggerManual BlueprintTriggerType = "manual"
	BlueprintTriggerEvent  BlueprintTriggerType = "event"
)

// Blueprint is the triggerable, team-scoped composition: an ordered list of
// prompt steps (BlueprintStep), length >= 1. Everything an event handler
// fires is a blueprint; a single prompt is just a 1-step blueprint. Modeled
// on Prompt (same team-scoping, same system_slug idempotency key).
type Blueprint struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"` // "system", "user", "imported"
	// TeamID is the owning team — NOT NULL, the sole scoping signal. A
	// trigger may only fire a blueprint its own team owns. Stores populate it
	// on read; Create stamps it from the resolved acting team.
	TeamID string `json:"team_id"`
	// SystemSlug is the stable identifier for a shipped (source='system')
	// blueprint — reuses the wrapped prompt's slug (different table, no
	// collision). NULL (empty) for user/imported blueprints. The id is a
	// random UUID per team copy; seed/idempotency keys on
	// (org_id, team_id, system_slug).
	SystemSlug   string    `json:"system_slug,omitempty"`
	UsageCount   int       `json:"usage_count"`
	UserModified bool      `json:"user_modified"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SeedBlueprint is a shipped blueprint definition used by the boot seeder:
// the header (system_slug + name) plus the ordered prompt system_slugs its
// steps wrap. The seeder resolves each slug to the team's prompt-copy id when
// materializing the steps. All shipped blueprints are 1-step today (each wraps
// one shipped prompt a trigger fires or the curator uses), but the shape
// carries N slugs so a multi-step shipped blueprint is representable.
type SeedBlueprint struct {
	SystemSlug      string
	Name            string
	StepPromptSlugs []string
}

// BlueprintStep is one position in a blueprint's ordered step list.
// step_index is 0-based and densely packed by ReplaceSteps.
type BlueprintStep struct {
	BlueprintID  string    `json:"blueprint_id"`
	StepIndex    int       `json:"step_index"`
	StepPromptID string    `json:"step_prompt_id"`
	Brief        string    `json:"brief"`
	CreatedAt    time.Time `json:"created_at"`
}

// BlueprintRun is the in-flight instance for a multi-step blueprint. One row
// per delegateBlueprint call. Owns the shared worktree across all steps.
// Per-step state lives on the runs table linked back via runs.blueprint_run_id.
// (A 1-step blueprint runs through the single-prompt path and produces no
// BlueprintRun row.)
type BlueprintRun struct {
	ID            string               `json:"id"`
	BlueprintID   string               `json:"blueprint_id"`
	TaskID        string               `json:"task_id"`
	TriggerType   BlueprintTriggerType `json:"trigger_type"`
	TriggerID     string               `json:"trigger_id,omitempty"`
	Status        BlueprintRunStatus   `json:"status"`
	AbortReason   string               `json:"abort_reason,omitempty"`
	AbortedAtStep *int                 `json:"aborted_at_step,omitempty"`
	WorktreePath  string               `json:"worktree_path"`
	StartedAt     time.Time            `json:"started_at"`
	CompletedAt   *time.Time           `json:"completed_at,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────
// Verdict path — kept verbatim. The unified terminal envelope (next ticket)
// replaces ChainVerdict + the `exec chain verdict` CLI + the
// run_artifacts(kind='chain:verdict') channel wholesale. Until then, a
// multi-step blueprint records per-step verdicts here exactly as today's
// chain did, so behavior is preserved. Do not extend this — it is slated
// for deletion.
// ─────────────────────────────────────────────────────────────────────────

// ChainVerdictOutcome is the tri-state result of a blueprint step's verdict.
type ChainVerdictOutcome string

const (
	ChainVerdictAdvance ChainVerdictOutcome = "advance"
	ChainVerdictAbort   ChainVerdictOutcome = "abort"
	ChainVerdictFinal   ChainVerdictOutcome = "final"
)

// ChainVerdict is the structured handoff a blueprint step records via
// `triagefactory exec chain verdict`. Stored as run_artifacts.metadata_json
// with kind='chain:verdict'. Latest by created_at wins per step (idempotent
// re-recording within a step).
//
// Outcome semantics:
//   - ChainVerdictAdvance → advance to next step
//   - ChainVerdictAbort   → abort the blueprint; leave task open for human
//   - ChainVerdictFinal   → end the blueprint successfully at this step; close the task
type ChainVerdict struct {
	Outcome   ChainVerdictOutcome `json:"outcome"`
	Reason    string              `json:"reason"`
	Notes     string              `json:"notes,omitempty"`
	Synthetic bool                `json:"-"` // internal flag, never on wire
}
