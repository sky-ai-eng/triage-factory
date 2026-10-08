package events

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// Linear issue event schemas, the Jira set's siblings.
//
// Every metadata struct carries the same identity block: the issue's
// identifier and UUID, its Linear team (id and key), its assignee (display
// name and Linear user id) and its title. linear_team_id is what the router's
// team gate reads, so the tracker sets it on every Linear event. It comes from
// the stored snapshot or a fresh read of the issue, never from the
// identifier's prefix: an identifier is a display key that a team move or a
// team key rename changes, and the UUID is the issue's identity. The one event
// that can lack it is unreachable for an entity that has neither a snapshot
// nor an answer from Linear, and the gate refuses that event for every team.
// The assignee's user id is what assignee-centric routing joins against
// user_linear_identities.
//
// Status and priority are open-set discriminators, as on Jira: a transition
// carries the new state name or priority label in both metadata and the
// event's dedup_key. Names, not state ids, because the name is what a
// predicate is written in and what a reader of the event sees.

// LinearIssueIdentity is the block every Linear issue event's metadata
// embeds. Embedded rather than repeated so every event carries linear_team_id
// by one declaration.
type LinearIssueIdentity struct {
	IssueIdentifier string `json:"issue_identifier"` // "ENG-123"
	IssueID         string `json:"issue_id"`         // Linear's UUID
	LinearTeamID    string `json:"linear_team_id"`
	LinearTeamKey   string `json:"linear_team_key"` // "ENG"
	Assignee        string `json:"assignee"`        // display name
	AssigneeUserID  string `json:"assignee_user_id"`
	Title           string `json:"title"`
}

// -----------------------------------------------------------------------------
// issue:assigned — the issue was assigned, or first seen already assigned.
// -----------------------------------------------------------------------------

type LinearIssueAssignedMetadata struct {
	LinearIssueIdentity
	Priority string `json:"priority"` // priority label
	Status   string `json:"status"`   // state name
}

type LinearIssueAssignedPredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Linear user IDs)."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty" doc:"Scope to a specific Linear team key (e.g. ENG)."`
	Priority      *string  `json:"priority,omitempty" doc:"Exact-match on the priority label (Urgent, High, Medium, Low, No priority)."`
	Status        *string  `json:"status,omitempty" doc:"Filter by the issue's current state name (e.g. 'Todo')."`
}

func (p LinearIssueAssignedPredicate) Matches(m LinearIssueAssignedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey) &&
		strEq(p.Priority, m.Priority) &&
		strEq(p.Status, m.Status)
}

// -----------------------------------------------------------------------------
// issue:available — an unassigned issue in one of its team's pickup states.
// -----------------------------------------------------------------------------

type LinearIssueAvailableMetadata struct {
	LinearIssueIdentity
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

type LinearIssueAvailablePredicate struct {
	LinearTeamKey *string `json:"linear_team_key,omitempty" doc:"Scope to a specific Linear team key (e.g. ENG)."`
	Priority      *string `json:"priority,omitempty"`
	Status        *string `json:"status,omitempty" doc:"Filter by the issue's current state name (e.g. 'Backlog')."`
}

func (p LinearIssueAvailablePredicate) Matches(m LinearIssueAvailableMetadata) bool {
	return strEq(p.LinearTeamKey, m.LinearTeamKey) &&
		strEq(p.Priority, m.Priority) &&
		strEq(p.Status, m.Status)
}

// -----------------------------------------------------------------------------
// issue:status_changed — the new state name is the dedup_key.
// -----------------------------------------------------------------------------

type LinearIssueStatusChangedMetadata struct {
	LinearIssueIdentity
	OldStatus string `json:"old_status"`
	NewStatus string `json:"new_status"` // also the event's dedup_key
	Priority  string `json:"priority"`
}

type LinearIssueStatusChangedPredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Linear user IDs)."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty"`
	NewStatus     *string  `json:"new_status,omitempty" doc:"Match transitions into a specific state (e.g. 'In Review')."`
	OldStatus     *string  `json:"old_status,omitempty" doc:"Match transitions out of a specific state."`
}

func (p LinearIssueStatusChangedPredicate) Matches(m LinearIssueStatusChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey) &&
		strEq(p.NewStatus, m.NewStatus) &&
		strEq(p.OldStatus, m.OldStatus)
}

// -----------------------------------------------------------------------------
// issue:priority_changed — the new priority label is the dedup_key.
// -----------------------------------------------------------------------------

type LinearIssuePriorityChangedMetadata struct {
	LinearIssueIdentity
	OldPriority string `json:"old_priority"`
	NewPriority string `json:"new_priority"` // also the event's dedup_key
}

type LinearIssuePriorityChangedPredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Linear user IDs)."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty"`
	NewPriority   *string  `json:"new_priority,omitempty"`
	OldPriority   *string  `json:"old_priority,omitempty"`
}

func (p LinearIssuePriorityChangedPredicate) Matches(m LinearIssuePriorityChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey) &&
		strEq(p.NewPriority, m.NewPriority) &&
		strEq(p.OldPriority, m.OldPriority)
}

// -----------------------------------------------------------------------------
// issue:commented — the newest comment changed. Linear has no comment count,
// so the signal is the newest comment's id; CommentID is that comment.
// -----------------------------------------------------------------------------

type LinearIssueCommentedMetadata struct {
	LinearIssueIdentity
	CommentID string `json:"comment_id"`
}

type LinearIssueCommentedPredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Linear user IDs)."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty"`
}

func (p LinearIssueCommentedPredicate) Matches(m LinearIssueCommentedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey)
}

// -----------------------------------------------------------------------------
// issue:completed — the issue entered one of its team's done states.
// Entity-terminating, and predicate-capable for follow-up work.
// -----------------------------------------------------------------------------

type LinearIssueCompletedMetadata struct {
	LinearIssueIdentity
	FinalStatus string `json:"final_status"`
}

type LinearIssueCompletedPredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Linear user IDs)."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty"`
}

func (p LinearIssueCompletedPredicate) Matches(m LinearIssueCompletedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey)
}

// -----------------------------------------------------------------------------
// issue:body_updated — the description changed. Hashes, not bodies, as on the
// other sources' body_updated.
// -----------------------------------------------------------------------------

type LinearIssueBodyUpdatedMetadata struct {
	LinearIssueIdentity
	Labels           []string `json:"labels"`
	PreviousBodyHash string   `json:"previous_body_hash"`
	BodyHash         string   `json:"body_hash"`
}

type LinearIssueBodyUpdatedPredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match assignees by Linear user ID."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty"`
	HasLabel      *string  `json:"has_label,omitempty"`
}

func (p LinearIssueBodyUpdatedPredicate) Matches(m LinearIssueBodyUpdatedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// issue:parent_changed — the issue moved under another parent, or lost its
// parent. The dedup_key is the new parent's UUID, or "none" when it was
// cleared: an identifier changes when the parent moves team, and a dedup key
// that changed with it would open a second task for the same parent. Never
// emitted on first discovery: a parent that was already set when TF started
// watching is not a change.
// -----------------------------------------------------------------------------

type LinearIssueParentChangedMetadata struct {
	LinearIssueIdentity
	OldParent string `json:"old_parent"` // identifier, "" when there was none
	NewParent string `json:"new_parent"` // identifier, "" when cleared
}

type LinearIssueParentChangedPredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Linear user IDs)."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty"`
	NewParent     *string  `json:"new_parent,omitempty" doc:"Match moves under a specific parent issue (identifier, e.g. 'ENG-12')."`
}

func (p LinearIssueParentChangedPredicate) Matches(m LinearIssueParentChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey) &&
		strEq(p.NewParent, m.NewParent)
}

// -----------------------------------------------------------------------------
// issue:became_atomic — the last open sub-issue closed. The belated discovery
// path, exactly as on Jira: a parent with open sub-issues is discovered
// without an assigned/available event, and this is the event that creates its
// task once the decomposition collapses.
// -----------------------------------------------------------------------------

type LinearIssueBecameAtomicMetadata struct {
	LinearIssueIdentity
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

type LinearIssueBecameAtomicPredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Linear user IDs)."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty" doc:"Scope to a specific Linear team key (e.g. ENG)."`
	Priority      *string  `json:"priority,omitempty"`
	Status        *string  `json:"status,omitempty"`
}

func (p LinearIssueBecameAtomicPredicate) Matches(m LinearIssueBecameAtomicMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey) &&
		strEq(p.Priority, m.Priority) &&
		strEq(p.Status, m.Status)
}

// -----------------------------------------------------------------------------
// issue:unreachable — TF will not follow the issue any more. Terminal for the
// entity. As with Jira's, the fields are the last known state off the stored
// snapshot, and an issue is never retired on its absence from a batch read.
// Reason says which of these it was.
// -----------------------------------------------------------------------------

// Linear unreachable reasons.
const (
	// LinearUnreachableNotFound: Linear answers not-found for the issue,
	// asked about directly.
	LinearUnreachableNotFound = "not_found"
	// LinearUnreachableTrashed: the issue is in the trash.
	LinearUnreachableTrashed = "trashed"
	// LinearUnreachableArchived: archived in a state outside its team's done
	// states.
	LinearUnreachableArchived = "archived"
	// LinearUnreachableMoved: the issue moved to a Linear team no rule arms.
	// The entity is renamed to the new identifier first, and the event names
	// the team the issue left, which is the team that was tracking it.
	LinearUnreachableMoved = "moved"
	// LinearUnreachableScopeChanged: the org's Linear credential now belongs
	// to another workspace. Linear is not asked: the current credential
	// cannot see the old workspace.
	LinearUnreachableScopeChanged = "scope_changed"
)

type LinearIssueUnreachableMetadata struct {
	LinearIssueIdentity
	LastStatus string `json:"last_status"`
	Reason     string `json:"reason"` // one of the LinearUnreachable* values
}

type LinearIssueUnreachablePredicate struct {
	AssigneeIn    []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Linear user IDs)."`
	LinearTeamKey *string  `json:"linear_team_key,omitempty" doc:"Scope to a specific Linear team key (e.g. ENG)."`
}

func (p LinearIssueUnreachablePredicate) Matches(m LinearIssueUnreachableMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		strEq(p.LinearTeamKey, m.LinearTeamKey)
}

// -----------------------------------------------------------------------------
// issue:identifier_changed — a tracked issue answers under a new identifier:
// it moved to another Linear team, or its team's key was renamed. The issue's
// UUID is unchanged, so the entity is renamed in place and keeps its tasks,
// conversations and memory. Emitted before any other event from the same
// refresh, in the same commit as the snapshot, and routed like every other
// Linear event by the current linear_team_id.
// -----------------------------------------------------------------------------

type LinearIssueIdentifierChangedMetadata struct {
	LinearIssueIdentity
	OldIdentifier    string `json:"old_identifier"`
	OldLinearTeamID  string `json:"old_linear_team_id"`
	OldLinearTeamKey string `json:"old_linear_team_key"`
}

type LinearIssueIdentifierChangedPredicate struct {
	LinearTeamKey    *string `json:"linear_team_key,omitempty" doc:"Scope to the Linear team key the issue now has (e.g. OPS)."`
	OldLinearTeamKey *string `json:"old_linear_team_key,omitempty" doc:"Scope to the Linear team key the issue had before (e.g. ENG)."`
}

func (p LinearIssueIdentifierChangedPredicate) Matches(m LinearIssueIdentifierChangedMetadata) bool {
	return strEq(p.LinearTeamKey, m.LinearTeamKey) &&
		strEq(p.OldLinearTeamKey, m.OldLinearTeamKey)
}

// Ownership: every linear:issue:* type routes through the owning-team ladder
// to whoever is assigned the issue, except issue:available, which is
// unassigned by definition and routes to the pool — the Jira declarations.
func init() {
	Register(NewSchema[LinearIssueAssignedMetadata, LinearIssueAssignedPredicate](domain.EventLinearIssueAssigned, OwnershipOwned))
	Register(NewSchema[LinearIssueAvailableMetadata, LinearIssueAvailablePredicate](domain.EventLinearIssueAvailable, OwnershipPool))
	Register(NewSchema[LinearIssueStatusChangedMetadata, LinearIssueStatusChangedPredicate](domain.EventLinearIssueStatusChanged, OwnershipOwned))
	Register(NewSchema[LinearIssuePriorityChangedMetadata, LinearIssuePriorityChangedPredicate](domain.EventLinearIssuePriorityChanged, OwnershipOwned))
	Register(NewSchema[LinearIssueCommentedMetadata, LinearIssueCommentedPredicate](domain.EventLinearIssueCommented, OwnershipOwned))
	Register(NewSchema[LinearIssueCompletedMetadata, LinearIssueCompletedPredicate](domain.EventLinearIssueCompleted, OwnershipOwned))
	Register(NewSchema[LinearIssueBodyUpdatedMetadata, LinearIssueBodyUpdatedPredicate](domain.EventLinearIssueBodyUpdated, OwnershipOwned))
	Register(NewSchema[LinearIssueParentChangedMetadata, LinearIssueParentChangedPredicate](domain.EventLinearIssueParentChanged, OwnershipOwned))
	Register(NewSchema[LinearIssueBecameAtomicMetadata, LinearIssueBecameAtomicPredicate](domain.EventLinearIssueBecameAtomic, OwnershipOwned))
	Register(NewSchema[LinearIssueUnreachableMetadata, LinearIssueUnreachablePredicate](domain.EventLinearIssueUnreachable, OwnershipOwned))
	Register(NewSchema[LinearIssueIdentifierChangedMetadata, LinearIssueIdentifierChangedPredicate](domain.EventLinearIssueIdentifierChanged, OwnershipOwned))
}
