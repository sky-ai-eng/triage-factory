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

// -----------------------------------------------------------------------------
// issue:assigned — issue was assigned (possibly to someone else; predicates
// scope to self).
// -----------------------------------------------------------------------------

type JiraIssueAssignedMetadata struct {
	Assignee       string `json:"assignee"`
	AssigneeIsSelf bool   `json:"assignee_is_self"`
	Reporter       string `json:"reporter"`
	ReporterIsSelf bool   `json:"reporter_is_self"`
	IssueKey       string `json:"issue_key"` // "SKY-123"
	Project        string `json:"project"`   // "SKY"
	IssueType      string `json:"issue_type"`
	Priority       string `json:"priority"`
	Status         string `json:"status"`
	Summary        string `json:"summary"`
}

type JiraIssueAssignedPredicate struct {
	AssigneeIsSelf *bool   `json:"assignee_is_self,omitempty" doc:"Match issues assigned to you."`
	Assignee       *string `json:"assignee,omitempty" doc:"Exact-match on assignee account ID / email."`
	ReporterIsSelf *bool   `json:"reporter_is_self,omitempty"`
	Project        *string `json:"project,omitempty" doc:"Scope to a specific Jira project key."`
	IssueType      *string `json:"issue_type,omitempty" doc:"Filter by issue type (Story, Bug, Task, ...)."`
	Priority       *string `json:"priority,omitempty" doc:"Exact-match on priority name."`
}

func (p JiraIssueAssignedPredicate) Matches(m JiraIssueAssignedMetadata) bool {
	return boolEq(p.AssigneeIsSelf, m.AssigneeIsSelf) &&
		strEq(p.Assignee, m.Assignee) &&
		boolEq(p.ReporterIsSelf, m.ReporterIsSelf) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType) &&
		strEq(p.Priority, m.Priority)
}

// -----------------------------------------------------------------------------
// issue:available — new unassigned issue lands in a configured pickup status.
// -----------------------------------------------------------------------------

type JiraIssueAvailableMetadata struct {
	Reporter       string `json:"reporter"`
	ReporterIsSelf bool   `json:"reporter_is_self"`
	IssueKey       string `json:"issue_key"`
	Project        string `json:"project"`
	IssueType      string `json:"issue_type"`
	Priority       string `json:"priority"`
	Status         string `json:"status"`
	Summary        string `json:"summary"`
}

type JiraIssueAvailablePredicate struct {
	Project   *string `json:"project,omitempty"`
	IssueType *string `json:"issue_type,omitempty"`
	Priority  *string `json:"priority,omitempty"`
}

func (p JiraIssueAvailablePredicate) Matches(m JiraIssueAvailableMetadata) bool {
	return strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType) &&
		strEq(p.Priority, m.Priority)
}

// -----------------------------------------------------------------------------
// issue:status_changed — open-set discriminator (the new status value is the
// dedup_key). Multiple concurrent status-changed tasks can exist on one
// issue.
// -----------------------------------------------------------------------------

type JiraIssueStatusChangedMetadata struct {
	Assignee       string `json:"assignee"`
	AssigneeIsSelf bool   `json:"assignee_is_self"`
	IssueKey       string `json:"issue_key"`
	Project        string `json:"project"`
	IssueType      string `json:"issue_type"`
	OldStatus      string `json:"old_status"`
	NewStatus      string `json:"new_status"` // also the event's dedup_key
	Priority       string `json:"priority"`
}

type JiraIssueStatusChangedPredicate struct {
	AssigneeIsSelf *bool   `json:"assignee_is_self,omitempty"`
	Project        *string `json:"project,omitempty"`
	IssueType      *string `json:"issue_type,omitempty"`
	NewStatus      *string `json:"new_status,omitempty" doc:"Match transitions into a specific status (e.g. 'In Review')."`
	OldStatus      *string `json:"old_status,omitempty" doc:"Match transitions out of a specific status."`
}

func (p JiraIssueStatusChangedPredicate) Matches(m JiraIssueStatusChangedMetadata) bool {
	return boolEq(p.AssigneeIsSelf, m.AssigneeIsSelf) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType) &&
		strEq(p.NewStatus, m.NewStatus) &&
		strEq(p.OldStatus, m.OldStatus)
}

// -----------------------------------------------------------------------------
// issue:priority_changed — open-set discriminator on new priority.
// -----------------------------------------------------------------------------

type JiraIssuePriorityChangedMetadata struct {
	Assignee       string `json:"assignee"`
	AssigneeIsSelf bool   `json:"assignee_is_self"`
	IssueKey       string `json:"issue_key"`
	Project        string `json:"project"`
	OldPriority    string `json:"old_priority"`
	NewPriority    string `json:"new_priority"` // also the event's dedup_key
}

type JiraIssuePriorityChangedPredicate struct {
	AssigneeIsSelf *bool   `json:"assignee_is_self,omitempty"`
	Project        *string `json:"project,omitempty"`
	NewPriority    *string `json:"new_priority,omitempty"`
	OldPriority    *string `json:"old_priority,omitempty"`
}

func (p JiraIssuePriorityChangedPredicate) Matches(m JiraIssuePriorityChangedMetadata) bool {
	return boolEq(p.AssigneeIsSelf, m.AssigneeIsSelf) &&
		strEq(p.Project, m.Project) &&
		strEq(p.NewPriority, m.NewPriority) &&
		strEq(p.OldPriority, m.OldPriority)
}

// -----------------------------------------------------------------------------
// issue:commented — new comment added.
// -----------------------------------------------------------------------------

type JiraIssueCommentedMetadata struct {
	Assignee        string `json:"assignee"`
	AssigneeIsSelf  bool   `json:"assignee_is_self"`
	Commenter       string `json:"commenter"`
	CommenterIsSelf bool   `json:"commenter_is_self"`
	CommentID       string `json:"comment_id"`
	IssueKey        string `json:"issue_key"`
	Project         string `json:"project"`
}

type JiraIssueCommentedPredicate struct {
	AssigneeIsSelf  *bool   `json:"assignee_is_self,omitempty"`
	CommenterIsSelf *bool   `json:"commenter_is_self,omitempty"`
	Commenter       *string `json:"commenter,omitempty"`
	Project         *string `json:"project,omitempty"`
}

func (p JiraIssueCommentedPredicate) Matches(m JiraIssueCommentedMetadata) bool {
	return boolEq(p.AssigneeIsSelf, m.AssigneeIsSelf) &&
		boolEq(p.CommenterIsSelf, m.CommenterIsSelf) &&
		strEq(p.Commenter, m.Commenter) &&
		strEq(p.Project, m.Project)
}

// -----------------------------------------------------------------------------
// issue:completed — issue entered a "done" state. Entity-terminating (handled
// by the entity lifecycle), but kept as a predicate-capable event in case
// users want to trigger follow-up work (e.g. post-merge cleanups).
// -----------------------------------------------------------------------------

type JiraIssueCompletedMetadata struct {
	Assignee       string `json:"assignee"`
	AssigneeIsSelf bool   `json:"assignee_is_self"`
	IssueKey       string `json:"issue_key"`
	Project        string `json:"project"`
	IssueType      string `json:"issue_type"`
	FinalStatus    string `json:"final_status"`
}

type JiraIssueCompletedPredicate struct {
	AssigneeIsSelf *bool   `json:"assignee_is_self,omitempty"`
	Project        *string `json:"project,omitempty"`
	IssueType      *string `json:"issue_type,omitempty"`
}

func (p JiraIssueCompletedPredicate) Matches(m JiraIssueCompletedMetadata) bool {
	return boolEq(p.AssigneeIsSelf, m.AssigneeIsSelf) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType)
}

// -----------------------------------------------------------------------------
// Registration.
// -----------------------------------------------------------------------------

func init() {
	Register(newSchema[JiraIssueAssignedMetadata, JiraIssueAssignedPredicate](domain.EventJiraIssueAssigned))
	Register(newSchema[JiraIssueAvailableMetadata, JiraIssueAvailablePredicate](domain.EventJiraIssueAvailable))
	Register(newSchema[JiraIssueStatusChangedMetadata, JiraIssueStatusChangedPredicate](domain.EventJiraIssueStatusChanged))
	Register(newSchema[JiraIssuePriorityChangedMetadata, JiraIssuePriorityChangedPredicate](domain.EventJiraIssuePriorityChanged))
	Register(newSchema[JiraIssueCommentedMetadata, JiraIssueCommentedPredicate](domain.EventJiraIssueCommented))
	Register(newSchema[JiraIssueCompletedMetadata, JiraIssueCompletedPredicate](domain.EventJiraIssueCompleted))
}
