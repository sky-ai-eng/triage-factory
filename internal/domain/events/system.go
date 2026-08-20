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
	Source    string `json:"source"`     // "github" | "jira"
	StartedAt int64  `json:"started_at"` // UnixNano of poll start — lets late sentinels from pre-restart generations be filtered out
	Entities  int    `json:"entities"`   // active entity count at refresh
	Events    int    `json:"events"`     // events emitted during this cycle
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
// system:delegation:completed / system:delegation:failed — terminal
// conversation signals, emitted once per conversation.
// -----------------------------------------------------------------------------

type SystemDelegationCompletedMetadata struct {
	ConversationID string `json:"conversation_id"`
	TaskID         string `json:"task_id"`
	PromptID       string `json:"prompt_id"`
}

type SystemDelegationCompletedPredicate struct {
	PromptID *string `json:"prompt_id,omitempty"`
}

func (p SystemDelegationCompletedPredicate) Matches(m SystemDelegationCompletedMetadata) bool {
	return strEq(p.PromptID, m.PromptID)
}

type SystemDelegationFailedMetadata struct {
	ConversationID string `json:"conversation_id"`
	TaskID         string `json:"task_id"`
	PromptID       string `json:"prompt_id"`
	Reason         string `json:"reason"`
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
// system:conversation:status / system:conversation:activity — EE-observable
// conversation-lifecycle sentinels (TFAC-592), mirroring the two websocket
// choke points in internal/delegate/spawner.go onto the bus. Deliberately
// minimal: no TaskID/EntitySource, so consumers resolve
// conversation→task→entity context themselves (and cache it) rather than
// costing a DB read on the hot broadcast path.
// -----------------------------------------------------------------------------

type SystemConversationStatusMetadata struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`                 // the broadcast status string
	FailureKind    string `json:"failure_kind,omitempty"` // set on the failed arm only
	// Resumable rides the parked status when a conversation parked by one pod
	// becomes resumable on another — the executor holding the workspace
	// records that it owes a persist for it, moments after control announced
	// the park. That is the moment a follow-up becomes possible, and no status
	// change of its own marks it. A pointer because absence means "unchanged,
	// ask the conversation read", which is not the same claim as false.
	Resumable *bool `json:"resumable,omitempty"`
}

// No predicate fields — fires once per instance, users can only enable / disable.
type SystemConversationStatusPredicate struct{}

func (p SystemConversationStatusPredicate) Matches(m SystemConversationStatusMetadata) bool {
	return true
}

type ConversationActivityTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"` // Input["description"] when a string; else ""
}

type SystemConversationActivityMetadata struct {
	ConversationID string                     `json:"conversation_id"`
	Tools          []ConversationActivityTool `json:"tools"`
}

// No predicate fields — fires once per instance, users can only enable / disable.
type SystemConversationActivityPredicate struct{}

func (p SystemConversationActivityPredicate) Matches(m SystemConversationActivityMetadata) bool {
	return true
}

// -----------------------------------------------------------------------------
// system:conversation:resumed — a user's follow-up woke a parked or concluded
// conversation. Recorded (not published) so the task_events junction can link
// the follow-up to the task it continued: the task's own status is deliberately
// untouched, so this row is the only evidence the card has that anything
// happened after it closed.
//
// The blueprint run id is carried because a follow-up on a finished blueprint
// is the case the row exists to make visible; it is empty for a conversation
// with no blueprint parent.
// -----------------------------------------------------------------------------

type SystemConversationResumedMetadata struct {
	ConversationID string `json:"conversation_id"`
	TaskID         string `json:"task_id"`
	UserID         string `json:"user_id"`
	BlueprintRunID string `json:"blueprint_run_id,omitempty"`
	// FromStatus / FromOutcome are the rest state the follow-up woke the
	// conversation from: `open` is a parked turn being picked back up,
	// `completed` is a follow-up on work that already concluded.
	FromStatus  string `json:"from_status"`
	FromOutcome string `json:"from_outcome,omitempty"`
}

// No predicate fields — fires once per instance, users can only enable / disable.
type SystemConversationResumedPredicate struct{}

func (p SystemConversationResumedPredicate) Matches(m SystemConversationResumedMetadata) bool {
	return true
}

// -----------------------------------------------------------------------------
// system:routing:disposition — one sentinel per event Router.HandleEvent
// finishes handling (TFAC-593). Routing is async (event_queue → worker), so
// this is how a source that published an event (e.g. Slack) learns
// synchronously-unavailable outcomes: frozen, taskless (no handler/owner/
// unroutable), task created/bumped, or an internal error. Deliberately
// lean — a consumer needing the original event's payload reads it via
// EventStore.GetMetadataSystem(orgID, EventID); taskless events are still
// recorded, so the row exists.
// -----------------------------------------------------------------------------

type SystemRoutingDispositionMetadata struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EntityID      string `json:"entity_id,omitempty"`
	Disposition   string `json:"disposition"`
	OwnerTeamID   string `json:"owner_team_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	TriggersFired int    `json:"triggers_fired"`
}

const (
	DispositionFrozen             = "frozen"
	DispositionTasklessUnroutable = "taskless_unroutable" // system event / closed entity
	DispositionTasklessNoHandler  = "taskless_no_handler"
	DispositionTasklessNoOwner    = "taskless_no_owner"
	DispositionTaskCreated        = "task_created"
	DispositionTaskBumped         = "task_bumped"
	// DispositionError means the pipeline hit an internal failure (a store
	// query it depends on errored) rather than reaching a legitimate
	// outcome — kept distinct from the taskless_* values so a consumer
	// (e.g. Slack deciding between an acknowledging reaction and a
	// "not configured" reply) never mistakes "we couldn't tell" for "there
	// really is nothing configured here."
	DispositionError = "error"
)

// No predicate fields — fires once per instance, users can only enable / disable.
type SystemRoutingDispositionPredicate struct{}

func (p SystemRoutingDispositionPredicate) Matches(m SystemRoutingDispositionMetadata) bool {
	return true
}

// -----------------------------------------------------------------------------
// Registration.
// -----------------------------------------------------------------------------

// System events are all OwnershipUnrouted — none of them route via the
// owning-team ladder, the requested-party path, or handler-team pooling;
// they're bus-only sentinels, never router-bound (see RouterBound) and
// never entity-scoped, so they never reach resolveTeamRouting at all.
func init() {
	Register(NewSchema[SystemPollCompletedMetadata, SystemPollCompletedPredicate](domain.EventSystemPollCompleted, OwnershipUnrouted))
	Register(NewSchema[SystemScoringCompletedMetadata, SystemScoringCompletedPredicate](domain.EventSystemScoringCompleted, OwnershipUnrouted))
	Register(NewSchema[SystemDelegationCompletedMetadata, SystemDelegationCompletedPredicate](domain.EventSystemDelegationCompleted, OwnershipUnrouted))
	Register(NewSchema[SystemDelegationFailedMetadata, SystemDelegationFailedPredicate](domain.EventSystemDelegationFailed, OwnershipUnrouted))
	Register(NewSchema[SystemPromptAutoSuspendedMetadata, SystemPromptAutoSuspendedPredicate](domain.EventSystemPromptAutoSuspended, OwnershipUnrouted))
	Register(NewSchema[SystemTaskDelegationBlockedSubtasksMetadata, SystemTaskDelegationBlockedSubtasksPredicate](domain.EventSystemTaskDelegationBlockedSubtasks, OwnershipUnrouted))
	Register(NewSchema[SystemConversationStatusMetadata, SystemConversationStatusPredicate](domain.EventSystemConversationStatus, OwnershipUnrouted))
	Register(NewSchema[SystemConversationActivityMetadata, SystemConversationActivityPredicate](domain.EventSystemConversationActivity, OwnershipUnrouted))
	Register(NewSchema[SystemConversationResumedMetadata, SystemConversationResumedPredicate](domain.EventSystemConversationResumed, OwnershipUnrouted))
	Register(NewSchema[SystemRoutingDispositionMetadata, SystemRoutingDispositionPredicate](domain.EventSystemRoutingDisposition, OwnershipUnrouted))
}
