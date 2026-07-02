package events

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// Jira issue event schemas.
//
// Actor identity on Jira is primarily Assignee + Reporter; Commenter appears
// on `commented`. Status and Priority are open-set discriminators (Jira
// projects configure their own workflows), so transitions carry the *new*
// value in both metadata and the event's dedup_key — multiple concurrent
// status-changed tasks can exist on the same issue when it transitions
// through several states before being addressed.
//
// SKY-270: actor predicates moved from `*_is_self` booleans to `*_in`
// allowlists of Atlassian account IDs. "Self" relative to a team is
// N-valued, so the team-shared rule needs a slice of identifiers. The
// metadata carries the *_account_id alongside the existing display-name
// fields; the matcher compares account IDs via stringInSliceFold.

// -----------------------------------------------------------------------------
// issue:assigned — issue was assigned (possibly to someone else; predicates
// scope to self).
// -----------------------------------------------------------------------------

type JiraIssueAssignedMetadata struct {
	Assignee          string `json:"assignee"`            // Jira display name
	AssigneeAccountID string `json:"assignee_account_id"` // Atlassian stable identifier
	Reporter          string `json:"reporter"`
	ReporterAccountID string `json:"reporter_account_id"`
	IssueKey          string `json:"issue_key"` // "SKY-123"
	Project           string `json:"project"`   // "SKY"
	IssueType         string `json:"issue_type"`
	Priority          string `json:"priority"`
	Status            string `json:"status"`
	Summary           string `json:"summary"`
}

type JiraIssueAssignedPredicate struct {
	AssigneeIn []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs, case-insensitive)."`
	ReporterIn []string `json:"reporter_in,omitempty" doc:"Match issues whose reporter is in this list (Atlassian account IDs)."`
	Project    *string  `json:"project,omitempty" doc:"Scope to a specific Jira project key."`
	IssueType  *string  `json:"issue_type,omitempty" doc:"Filter by issue type (Story, Bug, Task, ...)."`
	Priority   *string  `json:"priority,omitempty" doc:"Exact-match on priority name."`
	Status     *string  `json:"status,omitempty" doc:"Filter by the issue's current status (e.g. 'To Do', 'In Progress')."`
}

func (p JiraIssueAssignedPredicate) Matches(m JiraIssueAssignedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		stringInSliceFold(p.ReporterIn, m.ReporterAccountID) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType) &&
		strEq(p.Priority, m.Priority) &&
		strEq(p.Status, m.Status)
}

// -----------------------------------------------------------------------------
// issue:available — new unassigned issue lands in a configured pickup status.
// -----------------------------------------------------------------------------

type JiraIssueAvailableMetadata struct {
	Reporter          string `json:"reporter"`
	ReporterAccountID string `json:"reporter_account_id"`
	IssueKey          string `json:"issue_key"`
	Project           string `json:"project"`
	IssueType         string `json:"issue_type"`
	Priority          string `json:"priority"`
	Status            string `json:"status"`
	Summary           string `json:"summary"`
}

type JiraIssueAvailablePredicate struct {
	ReporterIn []string `json:"reporter_in,omitempty" doc:"Match issues whose reporter is in this list (Atlassian account IDs)."`
	Project    *string  `json:"project,omitempty"`
	IssueType  *string  `json:"issue_type,omitempty"`
	Priority   *string  `json:"priority,omitempty"`
	Status     *string  `json:"status,omitempty" doc:"Filter by the issue's current status (e.g. 'To Do', 'Backlog')."`
}

func (p JiraIssueAvailablePredicate) Matches(m JiraIssueAvailableMetadata) bool {
	return stringInSliceFold(p.ReporterIn, m.ReporterAccountID) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType) &&
		strEq(p.Priority, m.Priority) &&
		strEq(p.Status, m.Status)
}

// -----------------------------------------------------------------------------
// issue:status_changed — open-set discriminator (the new status value is the
// dedup_key). Multiple concurrent status-changed tasks can exist on one
// issue.
// -----------------------------------------------------------------------------

type JiraIssueStatusChangedMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	Project           string `json:"project"`
	IssueType         string `json:"issue_type"`
	OldStatus         string `json:"old_status"`
	NewStatus         string `json:"new_status"` // also the event's dedup_key
	Priority          string `json:"priority"`
}

type JiraIssueStatusChangedPredicate struct {
	AssigneeIn []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	Project    *string  `json:"project,omitempty"`
	IssueType  *string  `json:"issue_type,omitempty"`
	NewStatus  *string  `json:"new_status,omitempty" doc:"Match transitions into a specific status (e.g. 'In Review')."`
	OldStatus  *string  `json:"old_status,omitempty" doc:"Match transitions out of a specific status."`
}

func (p JiraIssueStatusChangedPredicate) Matches(m JiraIssueStatusChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType) &&
		strEq(p.NewStatus, m.NewStatus) &&
		strEq(p.OldStatus, m.OldStatus)
}

// -----------------------------------------------------------------------------
// issue:priority_changed — open-set discriminator on new priority.
// -----------------------------------------------------------------------------

type JiraIssuePriorityChangedMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	Project           string `json:"project"`
	OldPriority       string `json:"old_priority"`
	NewPriority       string `json:"new_priority"` // also the event's dedup_key
}

type JiraIssuePriorityChangedPredicate struct {
	AssigneeIn  []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	Project     *string  `json:"project,omitempty"`
	NewPriority *string  `json:"new_priority,omitempty"`
	OldPriority *string  `json:"old_priority,omitempty"`
}

func (p JiraIssuePriorityChangedPredicate) Matches(m JiraIssuePriorityChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		strEq(p.Project, m.Project) &&
		strEq(p.NewPriority, m.NewPriority) &&
		strEq(p.OldPriority, m.OldPriority)
}

// -----------------------------------------------------------------------------
// issue:commented — new comment added.
// -----------------------------------------------------------------------------

type JiraIssueCommentedMetadata struct {
	Assignee           string `json:"assignee"`
	AssigneeAccountID  string `json:"assignee_account_id"`
	Commenter          string `json:"commenter"`
	CommenterAccountID string `json:"commenter_account_id"`
	CommentID          string `json:"comment_id"`
	IssueKey           string `json:"issue_key"`
	Project            string `json:"project"`
}

type JiraIssueCommentedPredicate struct {
	AssigneeIn  []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	CommenterIn []string `json:"commenter_in,omitempty" doc:"Match comments authored by anyone in this list (Atlassian account IDs)."`
	Project     *string  `json:"project,omitempty"`
}

func (p JiraIssueCommentedPredicate) Matches(m JiraIssueCommentedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		stringInSliceFold(p.CommenterIn, m.CommenterAccountID) &&
		strEq(p.Project, m.Project)
}

// -----------------------------------------------------------------------------
// issue:completed — issue entered a "done" state. Entity-terminating (handled
// by the entity lifecycle), but kept as a predicate-capable event in case
// users want to trigger follow-up work (e.g. post-merge cleanups).
// -----------------------------------------------------------------------------

type JiraIssueCompletedMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	Project           string `json:"project"`
	IssueType         string `json:"issue_type"`
	FinalStatus       string `json:"final_status"`
}

type JiraIssueCompletedPredicate struct {
	AssigneeIn []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	Project    *string  `json:"project,omitempty"`
	IssueType  *string  `json:"issue_type,omitempty"`
}

func (p JiraIssueCompletedPredicate) Matches(m JiraIssueCompletedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType)
}

// -----------------------------------------------------------------------------
// issue:became_atomic — last open subtask closed, parent is now an atomic
// work unit. Fires when prev.OpenSubtaskCount > 0 && curr.OpenSubtaskCount
// == 0. Acts as the belated discovery path: initial discovery of a parent
// with open subtasks suppresses jira:issue:assigned/available so the ticket
// doesn't clutter the queue; when the decomposition collapses, this event
// runs the same task-creation path.
// -----------------------------------------------------------------------------

type JiraIssueBecameAtomicMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	Project           string `json:"project"`
	IssueType         string `json:"issue_type"`
	Priority          string `json:"priority"`
	Status            string `json:"status"`
	Summary           string `json:"summary"`
}

type JiraIssueBecameAtomicPredicate struct {
	AssigneeIn []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	Project    *string  `json:"project,omitempty" doc:"Scope to a specific Jira project key."`
	IssueType  *string  `json:"issue_type,omitempty"`
	Priority   *string  `json:"priority,omitempty"`
	Status     *string  `json:"status,omitempty"`
}

func (p JiraIssueBecameAtomicPredicate) Matches(m JiraIssueBecameAtomicMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType) &&
		strEq(p.Priority, m.Priority) &&
		strEq(p.Status, m.Status)
}

// -----------------------------------------------------------------------------
// Registration.
// -----------------------------------------------------------------------------

// Ownership declarations: every jira:issue:* type is OwnershipOwned — routed
// via the owning-team ladder to whoever is assigned the issue — EXCEPT
// issue:available, which is OwnershipPool (unassigned by definition; see its
// metadata doc above). internal/routing derives its assignee-centric anchor
// set from exactly this declaration (events.TypesWithOwnership(OwnershipOwned,
// "jira:")) rather than a parallel hand-maintained list.
func init() {
	Register(newSchema[JiraIssueAssignedMetadata, JiraIssueAssignedPredicate](domain.EventJiraIssueAssigned, OwnershipOwned))
	Register(newSchema[JiraIssueAvailableMetadata, JiraIssueAvailablePredicate](domain.EventJiraIssueAvailable, OwnershipPool))
	Register(newSchema[JiraIssueStatusChangedMetadata, JiraIssueStatusChangedPredicate](domain.EventJiraIssueStatusChanged, OwnershipOwned))
	Register(newSchema[JiraIssuePriorityChangedMetadata, JiraIssuePriorityChangedPredicate](domain.EventJiraIssuePriorityChanged, OwnershipOwned))
	Register(newSchema[JiraIssueCommentedMetadata, JiraIssueCommentedPredicate](domain.EventJiraIssueCommented, OwnershipOwned))
	Register(newSchema[JiraIssueCompletedMetadata, JiraIssueCompletedPredicate](domain.EventJiraIssueCompleted, OwnershipOwned))
	Register(newSchema[JiraIssueBecameAtomicMetadata, JiraIssueBecameAtomicPredicate](domain.EventJiraIssueBecameAtomic, OwnershipOwned))
}
