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
// Actor predicates moved from `*_is_self` booleans to `*_in`
// allowlists of Atlassian account IDs. "Self" relative to a team is
// N-valued, so the team-shared rule needs a slice of identifiers. The
// metadata carries the *_account_id alongside the existing display-name
// fields; the matcher compares account IDs via stringInSliceFold.
//
// TODO(TFAC-878): status predicates are free-text names the user types, matched
// against the name on the event. Nothing validates them against the project's
// live workflow, so a status renamed in Jira leaves a predicate that matches
// nothing and a trigger that silently stops firing. They cannot simply become
// ids: the name is also the dedup_key on stored events. Reporting the drift
// needs the durable notification channel.

// -----------------------------------------------------------------------------
// issue:assigned — issue was assigned (possibly to someone else; predicates
// scope to self).
// -----------------------------------------------------------------------------

type JiraIssueAssignedMetadata struct {
	Assignee          string `json:"assignee"`            // Jira display name
	AssigneeAccountID string `json:"assignee_account_id"` // Atlassian stable identifier
	Reporter          string `json:"reporter"`
	ReporterAccountID string `json:"reporter_account_id"`
	IssueKey          string `json:"issue_key"` // "PROJ-123"
	IssueID           string `json:"issue_id"`  // Jira's numeric issue id
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
	IssueID           string `json:"issue_id"`
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
	IssueID           string `json:"issue_id"`
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
	IssueID           string `json:"issue_id"`
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
	IssueID            string `json:"issue_id"`
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
	IssueID           string `json:"issue_id"`
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
	IssueID           string `json:"issue_id"`
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
// issue:unreachable — TF will not follow a tracked issue any more. Terminal
// for the entity: nothing further will be observed about it, so it is the one
// Jira event that reports the disappearance of its own subject. Reason says
// which of these it was:
//
//   - not_found: Jira answers 404 for the issue, asked about directly by id
//     (or by key, for an entity whose id TF has not learned yet). Named for
//     what was observed rather than what probably caused it: Jira answers 404
//     both for an issue that was deleted and for one the credential may no
//     longer see — deliberately, so that existence isn't disclosed — so the
//     two are indistinguishable from here, and a reason asserting deletion
//     would be a claim this cannot support.
//   - moved: the issue moved to a project no rule configures. The entity is
//     renamed to the new key first and key_changed is emitted ahead of this;
//     Project names the project the issue left, which is the one that was
//     tracking it, so that project's teams receive the close.
//   - scope_changed: the org's Jira base URL now names another site (or the
//     same site under another URL). Jira is not asked: issue ids and keys
//     repeat across sites, so no answer from the new one would be about this
//     issue.
//
// Fields are last-known state read off the entity's stored snapshot, filled
// from a fresh read where one exists. There is no dedup_key: an issue stops
// being followed once.
//
// Never emitted on an issue's absence from a search result (see the tracker's
// confirmation pass) — absence is equally consistent with an unindexed or
// archived issue or a paging bug, and this event closes the entity and every
// task on it.
// -----------------------------------------------------------------------------

// Jira unreachable reasons.
const (
	// JiraUnreachableNotFound: Jira answers 404 for the issue, asked about
	// directly.
	JiraUnreachableNotFound = "not_found"
	// JiraUnreachableMoved: the issue moved to a project no rule configures.
	JiraUnreachableMoved = "moved"
	// JiraUnreachableScopeChanged: the org's Jira base URL now names another
	// site.
	JiraUnreachableScopeChanged = "scope_changed"
)

type JiraIssueUnreachableMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	IssueID           string `json:"issue_id"`
	Project           string `json:"project"`
	IssueType         string `json:"issue_type"`
	LastStatus        string `json:"last_status"`
	Summary           string `json:"summary"`
	Reason            string `json:"reason"` // one of the JiraUnreachable* values
}

type JiraIssueUnreachablePredicate struct {
	AssigneeIn []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	Project    *string  `json:"project,omitempty" doc:"Scope to a specific Jira project key."`
	IssueType  *string  `json:"issue_type,omitempty"`
}

func (p JiraIssueUnreachablePredicate) Matches(m JiraIssueUnreachableMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		strEq(p.Project, m.Project) &&
		strEq(p.IssueType, m.IssueType)
}

// -----------------------------------------------------------------------------
// issue:key_changed — a tracked issue answers under a new key: it moved to
// another project, or its project's key was renamed. The issue's id is
// unchanged, so the entity is renamed in place and keeps its tasks,
// conversations and memory. Emitted before any other event from the same
// refresh, in the same commit as the snapshot. Not terminating.
//
// Unlike every other Jira event, it reaches a team that tracks either
// project: the one in project, or the one in old_project, which the issue
// left. A team whose issue moved to a project it does not track keeps its
// tasks on the entity, and this event is the only thing that tells it the
// issue left.
// -----------------------------------------------------------------------------

type JiraIssueKeyChangedMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	IssueID           string `json:"issue_id"`
	Project           string `json:"project"`
	IssueType         string `json:"issue_type"`
	Summary           string `json:"summary"`
	OldIssueKey       string `json:"old_issue_key"`
	OldProject        string `json:"old_project"`
}

type JiraIssueKeyChangedPredicate struct {
	Project    *string `json:"project,omitempty" doc:"Scope to the Jira project key the issue now has (e.g. OPS)."`
	OldProject *string `json:"old_project,omitempty" doc:"Scope to the Jira project key the issue had before (e.g. ENG)."`
}

func (p JiraIssueKeyChangedPredicate) Matches(m JiraIssueKeyChangedMetadata) bool {
	return strEq(p.Project, m.Project) &&
		strEq(p.OldProject, m.OldProject)
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
	Register(NewSchema[JiraIssueAssignedMetadata, JiraIssueAssignedPredicate](domain.EventJiraIssueAssigned, OwnershipOwned))
	Register(NewSchema[JiraIssueAvailableMetadata, JiraIssueAvailablePredicate](domain.EventJiraIssueAvailable, OwnershipPool))
	Register(NewSchema[JiraIssueStatusChangedMetadata, JiraIssueStatusChangedPredicate](domain.EventJiraIssueStatusChanged, OwnershipOwned))
	Register(NewSchema[JiraIssuePriorityChangedMetadata, JiraIssuePriorityChangedPredicate](domain.EventJiraIssuePriorityChanged, OwnershipOwned))
	Register(NewSchema[JiraIssueCommentedMetadata, JiraIssueCommentedPredicate](domain.EventJiraIssueCommented, OwnershipOwned))
	Register(NewSchema[JiraIssueCompletedMetadata, JiraIssueCompletedPredicate](domain.EventJiraIssueCompleted, OwnershipOwned))
	Register(NewSchema[JiraIssueBecameAtomicMetadata, JiraIssueBecameAtomicPredicate](domain.EventJiraIssueBecameAtomic, OwnershipOwned))
	Register(NewSchema[JiraIssueUnreachableMetadata, JiraIssueUnreachablePredicate](domain.EventJiraIssueUnreachable, OwnershipOwned))
	Register(NewSchema[JiraIssueKeyChangedMetadata, JiraIssueKeyChangedPredicate](domain.EventJiraIssueKeyChanged, OwnershipOwned))
}
