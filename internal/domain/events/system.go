package events

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// System events — internal sentinels (poll-completed, scoring-completed,
// breaker trips). They're never user-addressable, but we still register
// schemas so validation and introspection are uniform: a task_rule or
// prompt_trigger attached to one won't blow up at predicate-parse time.
//
// Predicate structs are intentionally empty for the "just fire on any
// occurrence" events. Breaker trips carry entity/prompt identifiers so
// users could, in principle, wire alerts to them (e.g. "notify me when the
// CI-fix breaker trips").

// -----------------------------------------------------------------------------
// system:poll:completed — a poller finished one cycle. Used by the scorer as
// a tick signal.
// -----------------------------------------------------------------------------

type SystemPollCompletedMetadata struct {
	Source   string `json:"source"`   // "github" | "jira"
	Duration string `json:"duration"` // human-readable, e.g. "1.2s"
}

type SystemPollCompletedPredicate struct {
	Source *string `json:"source,omitempty"`
}

func (p SystemPollCompletedPredicate) Matches(m SystemPollCompletedMetadata) bool {
	return strEq(p.Source, m.Source)
}

// -----------------------------------------------------------------------------
// system:scoring:completed — AI scorer finished for a task.
// -----------------------------------------------------------------------------

type SystemScoringCompletedMetadata struct {
	TaskID              string  `json:"task_id"`
	Priority            float64 `json:"priority"`
	AutonomySuitability float64 `json:"autonomy_suitability"`
	Succeeded           bool    `json:"succeeded"`
}

type SystemScoringCompletedPredicate struct {
	Succeeded *bool `json:"succeeded,omitempty"`
}

func (p SystemScoringCompletedPredicate) Matches(m SystemScoringCompletedMetadata) bool {
	return boolEq(p.Succeeded, m.Succeeded)
}

// -----------------------------------------------------------------------------
// system:delegation:completed / system:delegation:failed — terminal run
// signals, emitted once per run.
// -----------------------------------------------------------------------------

type SystemDelegationCompletedMetadata struct {
	RunID    string `json:"run_id"`
	TaskID   string `json:"task_id"`
	PromptID string `json:"prompt_id"`
}

type SystemDelegationCompletedPredicate struct {
	PromptID *string `json:"prompt_id,omitempty"`
}

func (p SystemDelegationCompletedPredicate) Matches(m SystemDelegationCompletedMetadata) bool {
	return strEq(p.PromptID, m.PromptID)
}

type SystemDelegationFailedMetadata struct {
	RunID    string `json:"run_id"`
	TaskID   string `json:"task_id"`
	PromptID string `json:"prompt_id"`
	Reason   string `json:"reason"`
}

type SystemDelegationFailedPredicate struct {
	PromptID *string `json:"prompt_id,omitempty"`
}

func (p SystemDelegationFailedPredicate) Matches(m SystemDelegationFailedMetadata) bool {
	return strEq(p.PromptID, m.PromptID)
}

// -----------------------------------------------------------------------------
// system:prompt:auto_suspended — per-(entity, prompt) breaker tripped.
// -----------------------------------------------------------------------------

type SystemPromptAutoSuspendedMetadata struct {
	EntityID string `json:"entity_id"`
	PromptID string `json:"prompt_id"`
	Failures int    `json:"failures"`
}

type SystemPromptAutoSuspendedPredicate struct {
	PromptID *string `json:"prompt_id,omitempty"`
	EntityID *string `json:"entity_id,omitempty"`
}

func (p SystemPromptAutoSuspendedPredicate) Matches(m SystemPromptAutoSuspendedMetadata) bool {
	return strEq(p.PromptID, m.PromptID) &&
		strEq(p.EntityID, m.EntityID)
}

// -----------------------------------------------------------------------------
// system:task:delegation_blocked_by_subtasks — auto-delegation skipped
// because a Jira parent has open subtasks.
// -----------------------------------------------------------------------------

type SystemTaskDelegationBlockedSubtasksMetadata struct {
	TaskID   string `json:"task_id"`
	IssueKey string `json:"issue_key"`
}

// No predicate fields — fires once per instance, users can only enable / disable.
type SystemTaskDelegationBlockedSubtasksPredicate struct{}

func (p SystemTaskDelegationBlockedSubtasksPredicate) Matches(m SystemTaskDelegationBlockedSubtasksMetadata) bool {
	return true
}

// -----------------------------------------------------------------------------
// Registration.
// -----------------------------------------------------------------------------

func init() {
	Register(newSchema[SystemPollCompletedMetadata, SystemPollCompletedPredicate](domain.EventSystemPollCompleted))
	Register(newSchema[SystemScoringCompletedMetadata, SystemScoringCompletedPredicate](domain.EventSystemScoringCompleted))
	Register(newSchema[SystemDelegationCompletedMetadata, SystemDelegationCompletedPredicate](domain.EventSystemDelegationCompleted))
	Register(newSchema[SystemDelegationFailedMetadata, SystemDelegationFailedPredicate](domain.EventSystemDelegationFailed))
	Register(newSchema[SystemPromptAutoSuspendedMetadata, SystemPromptAutoSuspendedPredicate](domain.EventSystemPromptAutoSuspended))
	Register(newSchema[SystemTaskDelegationBlockedSubtasksMetadata, SystemTaskDelegationBlockedSubtasksPredicate](domain.EventSystemTaskDelegationBlockedSubtasks))
}
