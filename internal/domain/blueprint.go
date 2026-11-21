package domain

import (
	"encoding/json"
	"slices"
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

// BlueprintRunStatuses is the closed vocabulary above, for the list route's
// filter validation and its error message. Order is lifecycle-then-terminal so
// the message reads naturally.
var BlueprintRunStatuses = []string{
	string(BlueprintRunStatusRunning),
	string(BlueprintRunStatusCompleted),
	string(BlueprintRunStatusAborted),
	string(BlueprintRunStatusFailed),
	string(BlueprintRunStatusCancelled),
}

// ValidBlueprintRunStatus reports whether s is one of BlueprintRunStatuses. A
// status outside the vocabulary is a client fault, not an empty page.
func ValidBlueprintRunStatus(s string) bool {
	return slices.Contains(BlueprintRunStatuses, s)
}

// Terminal reports whether the status is an end state (no further steps will
// run). A terminal run nobody called off can still hold one child that runs:
// its current step, when a follow-up re-queues a concluded final step, which
// the claim gate drives. Any other child left mid-flight under a terminal run
// — under a run called off, or on a step the run moved past — is one the claim
// gate refuses for good, so it would read as mid-flight forever. The
// cancellation path and the orphan park settle those. 'aborted' is terminal
// but message-resumable (ReopenRunForResume flips it back to running before
// its step runs again).
func (s BlueprintRunStatus) Terminal() bool {
	switch s {
	case BlueprintRunStatusCompleted, BlueprintRunStatusAborted,
		BlueprintRunStatusFailed, BlueprintRunStatusCancelled:
		return true
	default:
		return false
	}
}

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
	ID     string `json:"id" doc:"The blueprint's id."`
	Name   string `json:"name" doc:"The blueprint's name."`
	Source string `json:"source" doc:"Where the blueprint came from: system for one shipped with Triage Factory, user for one a person created, imported, or marketplace for one installed from a marketplace listing."` // "system", "user", "imported", "marketplace" (TFAC-538 install copy)
	// TeamID is the owning team — NOT NULL, the sole scoping signal. A
	// trigger may only fire a blueprint its own team owns. Stores populate it
	// on read; Create stamps it from the resolved acting team.
	TeamID string `json:"team_id" doc:"The id of the team that owns the blueprint."`
	// SystemSlug is the stable identifier for a shipped (source='system')
	// blueprint — reuses the wrapped prompt's slug (different table, no
	// collision). NULL (empty) for user/imported blueprints. The id is a
	// random UUID per team copy; seed/idempotency keys on
	// (org_id, team_id, system_slug).
	SystemSlug   string    `json:"system_slug,omitempty" doc:"The stable name of a shipped blueprint, absent for any other."`
	UsageCount   int       `json:"usage_count" doc:"How many blueprint runs the blueprint has started."`
	UserModified bool      `json:"user_modified" doc:"Whether the blueprint's name or steps have been changed since it was created; a shipped blueprint that has been changed is no longer updated to match the version Triage Factory ships."`
	CreatedAt    time.Time `json:"created_at" doc:"When the blueprint was created, as an RFC 3339 timestamp."`
	UpdatedAt    time.Time `json:"updated_at" doc:"When the blueprint was last changed, as an RFC 3339 timestamp."`
	// Version starts at 1 and rises with every write that changes the
	// blueprint's name or its steps. A steps write names the version it read,
	// and lands only on a blueprint still at it.
	Version int64 `json:"version" doc:"A number that starts at 1 and rises with every change to the blueprint's name or steps, which a steps replace names."`
}

// SeedBlueprint is a shipped blueprint definition used by the boot seeder:
// the header (system_slug + name) plus the ordered prompt system_slugs its
// steps wrap. The seeder resolves each slug to the team's prompt-copy id when
// materializing the steps. Most shipped blueprints are 1-step (each wraps
// one shipped prompt a trigger fires or the project-create default uses);
// system-pr-review is the first multi-step shipped blueprint (security →
// correctness → aggregate), so
// StepPromptSlugs carries its N ordered slugs.
type SeedBlueprint struct {
	SystemSlug      string
	Name            string
	StepPromptSlugs []string
}

// BlueprintStep is one position in a blueprint's ordered step list.
// step_index is 0-based and densely packed by ReplaceSteps.
type BlueprintStep struct {
	BlueprintID  string    `json:"blueprint_id" doc:"The id of the step's blueprint."`
	StepIndex    int       `json:"step_index" doc:"The step's 0-based position in its blueprint."`
	StepPromptID string    `json:"step_prompt_id" doc:"The id of the prompt the step runs."`
	Brief        string    `json:"brief" doc:"Instructions for this step that are added to its prompt, or empty for none."`
	CreatedAt    time.Time `json:"created_at" doc:"When the step was added to its blueprint, as an RFC 3339 timestamp."`
	// BlueprintVersion is the step's blueprint's Version, as of the read
	// that returned the step, so a steps PUT can name the version of the
	// steps it was decided against whatever order the caller read the
	// blueprint and its steps in. The step list reads it in the same
	// statement as the step, and the step writes' answers copy it from the
	// blueprint they return. A blueprint run's steps leave it zero, and it is
	// omitted.
	BlueprintVersion int64 `json:"blueprint_version,omitempty" doc:"The blueprint's version as of the read that returned the step, which a steps replace names; absent where the answer carries no version."`
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
	// TaskTitle is the task's title when the run was minted, which every
	// step's mission names. Frozen with the rest of the plan so a step's
	// instructions do not change when the pull request or issue is renamed.
	// Empty on a plan frozen before titles were, which names the task's
	// current title instead.
	TaskTitle string `json:"task_title,omitempty"`
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
	ID string `json:"id" doc:"The blueprint run's id."`
	// TeamID is the team the run belongs to: the team the mint creates its
	// first step conversation on, resolved inside the mint, so a value passed
	// in is not written. It never changes, and who may read and write the run
	// follows it. Empty only for a local run that predates the column and had
	// no conversation to take a team from.
	TeamID      string               `json:"team_id,omitempty" doc:"The id of the team the run belongs to, absent only for a local run older than the field."`
	BlueprintID string               `json:"blueprint_id" doc:"The id of the blueprint the run runs."`
	TaskID      string               `json:"task_id" doc:"The id of the task the run works on."`
	TriggerType BlueprintTriggerType `json:"trigger_type" doc:"How the run started: manual when a person delegated the task, or event when an event handler started it."`
	TriggerID   string               `json:"trigger_id,omitempty" doc:"The id of the event handler that started the run, absent for a manual run."`
	// TriggeringEventID is the event instance that fired this blueprint run,
	// for event-triggered runs; empty for manual. Paired with TriggerID it
	// drives the replay fence: the firing path mints the blueprint_run via
	// CreateRunWithFirstStepSystem, whose (triggering_event_id, trigger_id) unique
	// index returns ErrAlreadyFired on an at-least-once event replay. The
	// blueprint_run is the firing unit, so the fence lives here rather than on
	// the per-step conversations. Forward-only provenance — not read back into the run
	// projection.
	TriggeringEventID string `json:"triggering_event_id,omitempty" doc:"The id of the event that started the run, absent for a manual run."`
	// ScopePredicateJSON is the predicate of the trigger that fired this run
	// (blueprint_runs.scope_predicate_json), as the trigger row held it at
	// the firing; empty when the trigger had none, and for a manual run. A
	// trigger's breaker counts a run only while the trigger still holds this
	// predicate. Written at mint and read only by the breaker's SQL, so like
	// TriggeringEventID it is not in the run projection, and it is not on the
	// wire.
	ScopePredicateJSON string `json:"-"`
	// ActorAgentID is the agents.id of the bot executing this blueprint run
	// (blueprint_runs.actor_agent_id). Resolved once at the
	// delegation entry point — from the task's bot claim, or the org agent the
	// router/handler already holds — and frozen here at mint. Every step's
	// conversation inherits it onto conversations.actor_agent_id, so execution attribution is stable
	// across steps and survives a mid-blueprint user takeover (which clears the
	// task claim but not who ran the bot's steps). Empty = NULL (no actor: minted
	// before agent bootstrap, or the agent row was later deleted → SET NULL).
	ActorAgentID string             `json:"actor_agent_id,omitempty" doc:"The id of the agent that runs the run's steps, absent when none is recorded."`
	Status       BlueprintRunStatus `json:"status" doc:"The run's state: running, completed, aborted, failed, or cancelled."`
	// CurrentStepIndex is the 0-based step the blueprint is on — the durable
	// sequencing the queue-driven reactor advances (replacing the goroutine
	// stack's loop index). A mid-flight blueprint resumes by re-enqueuing this
	// step at boot.
	CurrentStepIndex int `json:"current_step_index" doc:"The 0-based index of the step the run is on."`
	// CancelRequested is the DB sequence-cancel signal: when set, the claim
	// skips this blueprint's queued steps and the reactor finalizes it
	// 'cancelled' instead of advancing. The active-subprocess kill is separate
	// (in-memory s.cancels).
	CancelRequested bool   `json:"cancel_requested,omitempty" doc:"Whether someone has asked for the run to be cancelled, absent when no one has."`
	AbortReason     string `json:"abort_reason,omitempty" doc:"Why a step stopped the run, absent unless the run was aborted."`
	AbortedAtStep   *int   `json:"aborted_at_step,omitempty" doc:"The 0-based index of the step that stopped the run, absent unless the run was aborted."`
	// StepPlan is the resolved step list frozen at mint (the step_plan JSON
	// column). The dispatcher/reactor/resume sequence off this snapshot, not
	// the live blueprint_steps/prompts, so an in-flight run is insulated from
	// edits to its blueprint. json:"-" keeps the snapshotted prompt bodies out
	// of the API run projection — it is a persistence/runtime field carried via
	// MarshalStepPlan/UnmarshalStepPlan, not part of the public envelope.
	StepPlan     []BlueprintPlanStep `json:"-"`
	WorktreePath string              `json:"worktree_path" doc:"The path of the run's working directory on the executor that runs it."`
	StartedAt    time.Time           `json:"started_at" doc:"When the run started, as an RFC 3339 timestamp."`
	CompletedAt  *time.Time          `json:"completed_at,omitempty" doc:"When the run finished, as an RFC 3339 timestamp, absent while it is running."`
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
