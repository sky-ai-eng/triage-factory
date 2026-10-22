package events

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// Linear issue event schemas, the Jira set's siblings.
//
// Every metadata struct carries the same identity block: the issue's
// identifier and UUID, its Linear team (id and key), its assignee (display
// name and Linear user id) and its title. linear_team_id is what the router's
// team gate reads and what linear_team_id_in matches (with old_linear_team_id
// and old_linear_team_id_in on identifier_changed), so the tracker sets it on
// every Linear event. It comes from the stored snapshot or a fresh read of
// the issue, never from the identifier's prefix: an identifier is a display
// key that a team move or a team key rename changes, and the UUID is the
// issue's identity. The one event that can lack it is unreachable for an
// entity that has neither a snapshot nor an answer from Linear, and the gate
// refuses that event for every team. linear_team_key is for display only, for
// the same reason: a team key rename would leave a filter on it matching
// nothing.
// The assignee's user id is what assignee-centric routing joins against
// user_linear_identities. The creator's is what creator_in matches; both
// creator fields are empty for an issue an integration created.
//
// Status and priority are open-set discriminators, as on Jira: a transition
// carries the new value in the event's dedup_key. status_changed's is the new
// workflow state's id, which a rename in Linear does not move, and
// priority_changed's is the new priority's label.
//
// A workflow state is named both ways in metadata: by id (status_id and its
// old_, new_, final_ and last_ siblings), which is what the status predicates
// match, and by name for display. Labels are named both ways too: label_ids,
// label_id and label_group_id are what the label predicates match, and the
// names beside them are for display. A team admin can rename a state, a label
// or a label group, and a filter on the name would then match nothing. A
// predicate's ids are checked when it is saved, against the states and labels
// of the Linear teams the handler's team tracks.
//
// Priority is matched by its label: Linear's five priorities (No priority,
// Urgent, High, Medium, Low) are a fixed set that no workspace can rename or
// extend, so the label never drifts from the priority it names.

// LinearIssueIdentity is the block every Linear issue event's metadata
// embeds. Embedded rather than repeated so every event carries linear_team_id
// by one declaration.
type LinearIssueIdentity struct {
	IssueIdentifier string `json:"issue_identifier"` // "ENG-123"
	IssueID         string `json:"issue_id"`         // Linear's UUID
	LinearTeamID    string `json:"linear_team_id"`   // what linear_team_id_in names
	LinearTeamKey   string `json:"linear_team_key"`  // "ENG"
	Assignee        string `json:"assignee"`         // display name
	AssigneeUserID  string `json:"assignee_user_id"`
	Creator         string `json:"creator"` // display name
	CreatorUserID   string `json:"creator_user_id"`
	Title           string `json:"title"`
}

// -----------------------------------------------------------------------------
// issue:assigned — the issue was assigned, or first seen already assigned.
// -----------------------------------------------------------------------------

type LinearIssueAssignedMetadata struct {
	LinearIssueIdentity
	Priority string   `json:"priority"`  // priority label
	Status   string   `json:"status"`    // state name
	StatusID string   `json:"status_id"` // state id; what status_id_in matches
	Labels   []string `json:"labels"`    // label names
	LabelIDs []string `json:"label_ids"` // label ids; what has_label_id_in matches
}

type LinearIssueAssignedPredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	CreatorIn      []string `json:"creator_in,omitempty" doc:"Match issues created by anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
	Priority       *string  `json:"priority,omitempty" doc:"Exact-match on the priority label (Urgent, High, Medium, Low, No priority)."`
	StatusIDIn     []string `json:"status_id_in,omitempty" doc:"Match only issues whose current workflow state is one of these, by Linear state id (a state of a Linear team the team tracks, as POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/list serves it), not its name. Empty matches every state."`
	HasLabelIDIn   []string `json:"has_label_id_in,omitempty" doc:"Require the issue to currently bear at least one of these labels, by Linear label id (a label the team's tracked Linear teams can use, as POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list serves it), not its name. Empty matches any labels."`
}

func (p LinearIssueAssignedPredicate) Matches(m LinearIssueAssignedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		stringInSliceFold(p.CreatorIn, m.CreatorUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		strEq(p.Priority, m.Priority) &&
		idIn(p.StatusIDIn, m.StatusID) &&
		anyIDIn(p.HasLabelIDIn, m.LabelIDs)
}

// -----------------------------------------------------------------------------
// issue:available — an unassigned issue in one of its team's pickup states.
// -----------------------------------------------------------------------------

type LinearIssueAvailableMetadata struct {
	LinearIssueIdentity
	Priority string   `json:"priority"`
	Status   string   `json:"status"`
	StatusID string   `json:"status_id"`
	Labels   []string `json:"labels"`
	LabelIDs []string `json:"label_ids"`
}

type LinearIssueAvailablePredicate struct {
	CreatorIn      []string `json:"creator_in,omitempty" doc:"Match issues created by anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
	Priority       *string  `json:"priority,omitempty" doc:"Exact-match on the priority label (Urgent, High, Medium, Low, No priority)."`
	StatusIDIn     []string `json:"status_id_in,omitempty" doc:"Match only issues whose current workflow state is one of these, by Linear state id (a state of a Linear team the team tracks, as POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/list serves it), not its name. Empty matches every state."`
	HasLabelIDIn   []string `json:"has_label_id_in,omitempty" doc:"Require the issue to currently bear at least one of these labels, by Linear label id (a label the team's tracked Linear teams can use, as POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list serves it), not its name. Empty matches any labels."`
}

func (p LinearIssueAvailablePredicate) Matches(m LinearIssueAvailableMetadata) bool {
	return stringInSliceFold(p.CreatorIn, m.CreatorUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		strEq(p.Priority, m.Priority) &&
		idIn(p.StatusIDIn, m.StatusID) &&
		anyIDIn(p.HasLabelIDIn, m.LabelIDs)
}

// -----------------------------------------------------------------------------
// issue:status_changed — the new state's id is the dedup_key.
// -----------------------------------------------------------------------------

type LinearIssueStatusChangedMetadata struct {
	LinearIssueIdentity
	OldStatus   string `json:"old_status"`
	OldStatusID string `json:"old_status_id"`
	NewStatus   string `json:"new_status"`
	NewStatusID string `json:"new_status_id"` // also the event's dedup_key
	Priority    string `json:"priority"`
}

type LinearIssueStatusChangedPredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
	NewStatusIDIn  []string `json:"new_status_id_in,omitempty" doc:"Match only transitions into one of these workflow states, by Linear state id (a state of a Linear team the team tracks), not its name. Empty matches every state."`
	OldStatusIDIn  []string `json:"old_status_id_in,omitempty" doc:"Match only transitions out of one of these workflow states, by Linear state id (a state of a Linear team the team tracks), not its name. Empty matches every state."`
}

func (p LinearIssueStatusChangedPredicate) Matches(m LinearIssueStatusChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		idIn(p.NewStatusIDIn, m.NewStatusID) &&
		idIn(p.OldStatusIDIn, m.OldStatusID)
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
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
	NewPriority    *string  `json:"new_priority,omitempty" doc:"Match only changes to this priority label (Urgent, High, Medium, Low, No priority)."`
	OldPriority    *string  `json:"old_priority,omitempty" doc:"Match only changes from this priority label."`
}

func (p LinearIssuePriorityChangedPredicate) Matches(m LinearIssuePriorityChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		strEq(p.NewPriority, m.NewPriority) &&
		strEq(p.OldPriority, m.OldPriority)
}

// -----------------------------------------------------------------------------
// issue:commented — someone other than TF commented, the Jira event's sibling.
// CommentID, Commenter and CommenterUserID are the newest of the comments a
// poll found, and CommenterUserIDs holds the author of each. A comment an
// integration posted has the integration's name as its commenter and no user
// id. A comment TF posted is not one of them, and a poll that found only TF's
// comments emits nothing.
// -----------------------------------------------------------------------------

type LinearIssueCommentedMetadata struct {
	LinearIssueIdentity
	CommentID        string   `json:"comment_id"`
	Commenter        string   `json:"commenter"` // display name
	CommenterUserID  string   `json:"commenter_user_id"`
	CommenterUserIDs []string `json:"commenter_user_ids"`
}

type LinearIssueCommentedPredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	CommenterIn    []string `json:"commenter_in,omitempty" doc:"Match when anyone in this list wrote one of the new comments (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
}

func (p LinearIssueCommentedPredicate) Matches(m LinearIssueCommentedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		anyInSliceFold(p.CommenterIn, m.CommenterUserIDs) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID)
}

// -----------------------------------------------------------------------------
// issue:completed — the issue entered one of its team's done states.
// Entity-terminating, and predicate-capable for follow-up work.
// -----------------------------------------------------------------------------

type LinearIssueCompletedMetadata struct {
	LinearIssueIdentity
	FinalStatus   string `json:"final_status"`
	FinalStatusID string `json:"final_status_id"`
}

type LinearIssueCompletedPredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
}

func (p LinearIssueCompletedPredicate) Matches(m LinearIssueCompletedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID)
}

// -----------------------------------------------------------------------------
// issue:body_updated — the description changed. Hashes, not bodies, as on the
// other sources' body_updated.
// -----------------------------------------------------------------------------

type LinearIssueBodyUpdatedMetadata struct {
	LinearIssueIdentity
	Labels           []string `json:"labels"`
	LabelIDs         []string `json:"label_ids"`
	PreviousBodyHash string   `json:"previous_body_hash"`
	BodyHash         string   `json:"body_hash"`
}

type LinearIssueBodyUpdatedPredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match assignees by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
	HasLabelIDIn   []string `json:"has_label_id_in,omitempty" doc:"Require the issue to currently bear at least one of these labels, by Linear label id (a label the team's tracked Linear teams can use, as POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list serves it), not its name. Empty matches any labels."`
}

func (p LinearIssueBodyUpdatedPredicate) Matches(m LinearIssueBodyUpdatedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		anyIDIn(p.HasLabelIDIn, m.LabelIDs)
}

// -----------------------------------------------------------------------------
// issue:parent_changed — the issue moved under another parent, or lost its
// parent. The dedup_key is the new parent's UUID, or "none" when it was
// cleared: an identifier changes when the parent moves team, and a dedup key
// that changed with it would open a second task for the same parent. Never
// emitted on first discovery: a parent that was already set when TF started
// watching is not a change.
//
// The parents are named both ways: by UUID (old_parent_id / new_parent_id),
// which is what new_parent_id matches, and by identifier (old_parent /
// new_parent) for display. A filter on the identifier would stop matching
// when the parent moves team or its team key is renamed.
// -----------------------------------------------------------------------------

type LinearIssueParentChangedMetadata struct {
	LinearIssueIdentity
	OldParentID string `json:"old_parent_id"` // UUID, "" when there was none
	NewParentID string `json:"new_parent_id"` // UUID, "" when cleared; what new_parent_id matches
	OldParent   string `json:"old_parent"`    // identifier, "" when there was none
	NewParent   string `json:"new_parent"`    // identifier, "" when cleared
}

type LinearIssueParentChangedPredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
	NewParentID    *string  `json:"new_parent_id,omitempty" doc:"Match moves under a specific parent issue, by Linear's id for it (a lowercase UUID, the new_parent_id a parent_changed event carries), not its identifier."`
}

func (p LinearIssueParentChangedPredicate) Matches(m LinearIssueParentChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		strEq(p.NewParentID, m.NewParentID)
}

// -----------------------------------------------------------------------------
// issue:label_added / issue:label_removed — the Jira events' siblings, with
// one difference: a Linear label has an id and can be renamed, so labels are
// compared by id and the dedup_key is the label's id. A rename is not a
// removal and an addition, and a label renamed after it was added and then
// added again does not open a second task. label_id is what label_id_in
// matches, and label_name is the name the label has now, for display.
//
// A label can sit in a group (a "Difficulty" group holding Easy / Medium /
// Hard), and its name is only its own, so two groups can each hold a
// "Medium". A group is itself a label: label_group_id is its id, which
// label_group_id_in matches, and label_group its name, both empty for a label
// in none. Switching a single-select group from one value to another is a
// removal and an addition in the same poll, each carrying the group.
//
// Never emitted on first discovery, and not suppressed on an issue with open
// sub-issues, for the reasons the Jira events give.
// -----------------------------------------------------------------------------

type LinearIssueLabelMetadata struct {
	LinearIssueIdentity
	Priority     string   `json:"priority"`  // priority label
	Status       string   `json:"status"`    // state name
	StatusID     string   `json:"status_id"` // state id
	LabelID      string   `json:"label_id"`  // "" for a label only a snapshot from before label ids were recorded knew
	LabelName    string   `json:"label_name"`
	LabelGroupID string   `json:"label_group_id"` // "" for a label in no group
	LabelGroup   string   `json:"label_group"`    // the group's name, "" for a label in no group
	Labels       []string `json:"labels"`         // the issue's label names after the change
	LabelIDs     []string `json:"label_ids"`      // the issue's label ids after the change
}

type LinearIssueLabelPredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
	LabelIDIn      []string `json:"label_id_in,omitempty" doc:"Match only these labels, by Linear label id (a label the team's tracked Linear teams can use, as POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list serves it), not its name. Empty matches every label."`
	LabelGroupIDIn []string `json:"label_group_id_in,omitempty" doc:"Match only labels in these label groups, by the group's Linear label id (the group of a label the team's tracked Linear teams can use), not its name. Empty matches labels in any group or none."`
	HasLabelIDIn   []string `json:"has_label_id_in,omitempty" doc:"Require the issue to bear at least one of these labels after the change, by Linear label id, not its name. Empty matches any labels."`
}

func (p LinearIssueLabelPredicate) Matches(m LinearIssueLabelMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		idIn(p.LabelIDIn, m.LabelID) &&
		idIn(p.LabelGroupIDIn, m.LabelGroupID) &&
		anyIDIn(p.HasLabelIDIn, m.LabelIDs)
}

// -----------------------------------------------------------------------------
// issue:became_atomic — the last open sub-issue closed. The belated discovery
// path, exactly as on Jira: a parent with open sub-issues is discovered
// without an assigned/available event, and this is the event that creates its
// task once the decomposition collapses.
// -----------------------------------------------------------------------------

type LinearIssueBecameAtomicMetadata struct {
	LinearIssueIdentity
	Priority string   `json:"priority"`
	Status   string   `json:"status"`
	StatusID string   `json:"status_id"`
	Labels   []string `json:"labels"`
	LabelIDs []string `json:"label_ids"`
}

type LinearIssueBecameAtomicPredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	CreatorIn      []string `json:"creator_in,omitempty" doc:"Match issues created by anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
	Priority       *string  `json:"priority,omitempty" doc:"Exact-match on the priority label (Urgent, High, Medium, Low, No priority)."`
	StatusIDIn     []string `json:"status_id_in,omitempty" doc:"Match only issues whose current workflow state is one of these, by Linear state id (a state of a Linear team the team tracks, as POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/list serves it), not its name. Empty matches every state."`
	HasLabelIDIn   []string `json:"has_label_id_in,omitempty" doc:"Require the issue to currently bear at least one of these labels, by Linear label id (a label the team's tracked Linear teams can use, as POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list serves it), not its name. Empty matches any labels."`
}

func (p LinearIssueBecameAtomicPredicate) Matches(m LinearIssueBecameAtomicMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		stringInSliceFold(p.CreatorIn, m.CreatorUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		strEq(p.Priority, m.Priority) &&
		idIn(p.StatusIDIn, m.StatusID) &&
		anyIDIn(p.HasLabelIDIn, m.LabelIDs)
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
	LastStatus   string `json:"last_status"`
	LastStatusID string `json:"last_status_id"`
	Reason       string `json:"reason"` // one of the LinearUnreachable* values
}

type LinearIssueUnreachablePredicate struct {
	AssigneeIn     []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (each by Linear's user id, as POST /api/orgs/{org_id}/linear/users/list serves it, not a name)."`
	LinearTeamIDIn []string `json:"linear_team_id_in,omitempty" doc:"Match only issues in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches every Linear team the team tracks."`
}

func (p LinearIssueUnreachablePredicate) Matches(m LinearIssueUnreachableMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeUserID) &&
		idIn(p.LinearTeamIDIn, m.LinearTeamID)
}

// -----------------------------------------------------------------------------
// issue:identifier_changed — a tracked issue answers under a new identifier:
// it moved to another Linear team, or its team's key was renamed. The issue's
// UUID is unchanged, so the entity is renamed in place and keeps its tasks,
// conversations and memory. Emitted before any other event from the same
// refresh, in the same commit as the snapshot. Unlike every other Linear
// event, it reaches a team that tracks either Linear team: the one in
// linear_team_id or the one in old_linear_team_id, which the issue left.
// -----------------------------------------------------------------------------

type LinearIssueIdentifierChangedMetadata struct {
	LinearIssueIdentity
	OldIdentifier    string `json:"old_identifier"`
	OldLinearTeamID  string `json:"old_linear_team_id"`
	OldLinearTeamKey string `json:"old_linear_team_key"`
}

type LinearIssueIdentifierChangedPredicate struct {
	LinearTeamIDIn    []string `json:"linear_team_id_in,omitempty" doc:"Match only issues now in these Linear teams, each one the team tracks, by Linear team id (the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the key). Empty matches any Linear team."`
	OldLinearTeamIDIn []string `json:"old_linear_team_id_in,omitempty" doc:"Match only issues that left these Linear teams, each one the team tracks, by Linear team id. Empty matches any Linear team."`
}

func (p LinearIssueIdentifierChangedPredicate) Matches(m LinearIssueIdentifierChangedMetadata) bool {
	return idIn(p.LinearTeamIDIn, m.LinearTeamID) &&
		idIn(p.OldLinearTeamIDIn, m.OldLinearTeamID)
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
	Register(NewSchema[LinearIssueLabelMetadata, LinearIssueLabelPredicate](domain.EventLinearIssueLabelAdded, OwnershipOwned))
	Register(NewSchema[LinearIssueLabelMetadata, LinearIssueLabelPredicate](domain.EventLinearIssueLabelRemoved, OwnershipOwned))
	Register(NewSchema[LinearIssueBecameAtomicMetadata, LinearIssueBecameAtomicPredicate](domain.EventLinearIssueBecameAtomic, OwnershipOwned))
	Register(NewSchema[LinearIssueUnreachableMetadata, LinearIssueUnreachablePredicate](domain.EventLinearIssueUnreachable, OwnershipOwned))
	Register(NewSchema[LinearIssueIdentifierChangedMetadata, LinearIssueIdentifierChangedPredicate](domain.EventLinearIssueIdentifierChanged, OwnershipOwned))
}
