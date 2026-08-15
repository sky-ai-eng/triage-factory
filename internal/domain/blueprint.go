package domain

import (
	"encoding/json"
	"time"
)

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

// Terminal reports whether the status is an end state (no further steps will
// run). A terminal blueprint_run must hold zero non-terminal child runs — the
// invariant the cancellation path and the boot reconcile enforce: a
// child left 'running' under a cancelled/completed parent strands the
// dispatcher on phantom work and pins a worktree's branch, requeuing forever.
// 'aborted' is terminal but message-resumable (ReopenRunForResume flips it back
// to running); its retained child is 'completed', itself terminal, so the
// invariant holds.
func (s BlueprintRunStatus) Terminal() bool {
	switch s {
	case BlueprintRunStatusCompleted, BlueprintRunStatusAborted,
		BlueprintRunStatusFailed, BlueprintRunStatusCancelled:
		return true
	default:
		return false
	}
}

// BlueprintAbortOrphanedAtMint is the abort_reason of a blueprint run failed
// because it holds no step conversation at all. The firing path commits the
// blueprint_run first and enqueues its first step second, so a hard death
// between the two leaves a 'running' parent with nothing to drive it: no
// conversation means no claim, so every recovery surface that joins through
// conversations looks straight past it, and (in Postgres) it keeps holding the
// one-active-auto-run index against its task.
//
// The transition running → failed on this reason belongs exclusively to the
// two recovery surfaces — the leader reaper's sweep and the boot reconcile. No
// live writer may use it: an in-process enqueue failure has its own error path
// and reports what actually failed.
const BlueprintAbortOrphanedAtMint = "orphaned_at_mint"

// BlueprintOrphanedAtMintGrace is how long a childless 'running' blueprint run
// is left alone before the recovery surfaces fail it. Shared by both surfaces
// so the two can never disagree about what "old enough" means.
//
// The window it has to clear is two DB round trips — a placement read and the
// step's INSERT — with no worktree, clone, or provider call between them (those
// happen later, on claim). Sub-second when healthy. The only way a live mint
// exceeds that without having crashed is a DB stall, and the one that happens
// in normal operation is a failover of a few tens of seconds; two minutes
// leaves margin over a pessimistic one while keeping recovery inside a couple
// of minutes rather than a coffee break. Lowering it trades margin for latency
// against that failover number, not against the code path.
//
// Losing that race degrades rather than corrupts, which is what makes a bounded
// margin defensible instead of needing an arbitrarily large one: the late INSERT
// lands a child under a now-terminal parent, the claim gate refuses to run a
// step under one, and the boot reconcile's park arm retires the child. The
// delegation dies visibly; it never double-runs.
const BlueprintOrphanedAtMintGrace = 2 * time.Minute

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
	Source string `json:"source"` // "system", "user", "imported", "marketplace" (TFAC-538 install copy)
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
// materializing the steps. Most shipped blueprints are 1-step (each wraps one
// shipped prompt a trigger fires or the curator uses); system-pr-review is the
// first multi-step shipped blueprint (security → correctness → aggregate), so
// StepPromptSlugs carries its N ordered slugs.
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

// BlueprintPlanStep is one step of the resolved plan frozen onto a
// BlueprintRun at mint (BlueprintRun.StepPlan). It snapshots the step's
// prompt content — not just ids — so an in-flight run executes the plan it
// was minted with, immune to later edits of the blueprint's steps or the
// step prompts' bodies. Edits are forward-only: they govern subsequent
// firings, never a run already in flight.
//
// It carries everything the dispatcher/reactor/resume need to sequence a
// step without re-reading blueprint_steps/prompts:
//
//   - StepIndex / Brief reconstruct the BlueprintStep (Step).
//   - PromptID is kept (despite the frozen content) for the step's usage
//     increment, the conversations row's prompt_id, and skill-slug stability.
//   - PromptName / PromptBody / Source / AllowedTools / Model reconstruct the
//     *Prompt (Prompt). Source is load-bearing: MaterializeStepSkill writes an
//     imported skill verbatim but synthesizes frontmatter for a user/system
//     prompt, so dropping it would change what an imported-skill step runs.
type BlueprintPlanStep struct {
	StepIndex    int    `json:"step_index"`
	PromptID     string `json:"prompt_id"`
	PromptName   string `json:"prompt_name"`
	PromptBody   string `json:"prompt_body"`
	Source       string `json:"source"`
	AllowedTools string `json:"allowed_tools"`
	Model        string `json:"model"`
	Brief        string `json:"brief"`
}

// Prompt reconstructs the *Prompt the dispatcher hands to MaterializeStepSkill
// / collectExtraTools from the frozen plan step, so the in-flight sequencing
// path never re-reads the prompts table.
func (p BlueprintPlanStep) Prompt() *Prompt {
	return &Prompt{
		ID:           p.PromptID,
		Name:         p.PromptName,
		Body:         p.PromptBody,
		Source:       p.Source,
		AllowedTools: p.AllowedTools,
		Model:        p.Model,
	}
}

// Step reconstructs the BlueprintStep (the wrapper-prompt + enqueue inputs)
// from the frozen plan step. blueprintID is the owning run's blueprint_id.
func (p BlueprintPlanStep) Step(blueprintID string) BlueprintStep {
	return BlueprintStep{
		BlueprintID:  blueprintID,
		StepIndex:    p.StepIndex,
		StepPromptID: p.PromptID,
		Brief:        p.Brief,
	}
}

// BlueprintRun is the in-flight instance for a blueprint. One row per
// delegation — every blueprint, including a 1-step one, mints exactly one (a
// single prompt is a 1-step blueprint; there is no separate single-prompt
// path). Owns the shared worktree across all steps. Per-step state lives on the
// conversations table linked back via conversations.blueprint_run_id (NOT NULL).
type BlueprintRun struct {
	ID          string               `json:"id"`
	BlueprintID string               `json:"blueprint_id"`
	TaskID      string               `json:"task_id"`
	TriggerType BlueprintTriggerType `json:"trigger_type"`
	TriggerID   string               `json:"trigger_id,omitempty"`
	// TriggeringEventID is the event instance that fired this blueprint run,
	// for event-triggered runs; empty for manual. Paired with TriggerID it
	// drives the replay fence: the firing path mints the blueprint_run via
	// CreateRunIfNotFiredSystem, whose (triggering_event_id, trigger_id) unique
	// index returns ErrAlreadyFired on an at-least-once event replay. The
	// blueprint_run is the firing unit, so the fence lives here rather than on
	// the per-step runs. Forward-only provenance — not read back into the run
	// projection.
	TriggeringEventID string `json:"triggering_event_id,omitempty"`
	// ActorAgentID is the agents.id of the bot executing this blueprint run
	// (blueprint_runs.actor_agent_id). Resolved once at the
	// delegation entry point — from the task's bot claim, or the org agent the
	// router/handler already holds — and frozen here at mint. Every step run
	// inherits it onto conversations.actor_agent_id, so execution attribution is stable
	// across steps and survives a mid-blueprint user takeover (which clears the
	// task claim but not who ran the bot's steps). Empty = NULL (no actor: minted
	// before agent bootstrap, or the agent row was later deleted → SET NULL).
	ActorAgentID string             `json:"actor_agent_id,omitempty"`
	Status       BlueprintRunStatus `json:"status"`
	// CurrentStepIndex is the 0-based step the blueprint is on — the durable
	// sequencing the queue-driven reactor advances (replacing the goroutine
	// stack's loop index). A mid-flight blueprint resumes by re-enqueuing this
	// step at boot.
	CurrentStepIndex int `json:"current_step_index"`
	// CancelRequested is the DB sequence-cancel signal: when set, the claim
	// skips this blueprint's queued steps and the reactor finalizes it
	// 'cancelled' instead of advancing. The active-subprocess kill is separate
	// (in-memory s.cancels).
	CancelRequested bool   `json:"cancel_requested,omitempty"`
	AbortReason     string `json:"abort_reason,omitempty"`
	AbortedAtStep   *int   `json:"aborted_at_step,omitempty"`
	// StepPlan is the resolved step list frozen at mint (the step_plan JSON
	// column). The dispatcher/reactor/resume sequence off this snapshot, not
	// the live blueprint_steps/prompts, so an in-flight run is insulated from
	// edits to its blueprint. json:"-" keeps the snapshotted prompt bodies out
	// of the API run projection — it is a persistence/runtime field carried via
	// MarshalStepPlan/UnmarshalStepPlan, not part of the public envelope.
	StepPlan     []BlueprintPlanStep `json:"-"`
	WorktreePath string              `json:"worktree_path"`
	StartedAt    time.Time           `json:"started_at"`
	CompletedAt  *time.Time          `json:"completed_at,omitempty"`
}

// MarshalStepPlan serializes a frozen step plan for the blueprint_runs.step_plan
// column. An empty plan marshals to "[]" so the NOT NULL column always holds
// valid JSON — never SQL NULL, never the literal "null".
func MarshalStepPlan(plan []BlueprintPlanStep) (string, error) {
	if len(plan) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalStepPlan parses a blueprint_runs.step_plan blob back into the frozen
// plan. An empty string (defensive — the column is NOT NULL) yields a nil plan
// rather than an error.
func UnmarshalStepPlan(raw string) ([]BlueprintPlanStep, error) {
	if raw == "" {
		return nil, nil
	}
	var plan []BlueprintPlanStep
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return nil, err
	}
	return plan, nil
}
