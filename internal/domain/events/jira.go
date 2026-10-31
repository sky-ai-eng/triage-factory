package events

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// Jira issue event schemas.
//
// Actor identity on Jira is primarily Assignee + Reporter; Commenter appears
// on `commented`. Status and Priority are open-set discriminators (Jira
// projects configure their own workflows), so transitions carry the *new*
// value in the event's dedup_key — multiple concurrent status-changed tasks
// can exist on the same issue when it transitions through several states
// before being addressed. The dedup_key is the value's id, which a rename in
// Jira does not move, and its name only when the read carried no id.
//
// Statuses, priorities and issue types are named both ways in metadata: by
// Jira's id (status_id, priority_id, issue_type_id and their old_, new_,
// final_ and last_ siblings), which is what the *_id_in predicates match, and
// by name for display. A Jira admin can rename any of the three, and a filter
// on the name would then match nothing. A predicate's ids are checked when it
// is saved: each must be a status or an issue type of a project the handler's
// team tracks, or one of the instance's priorities.
//
// Labels are matched by name (has_label, label_name): a Jira label is a plain
// string with no id, and it cannot be renamed.
//
// Actor predicates moved from `*_is_self` booleans to `*_in`
// allowlists of Atlassian account IDs. "Self" relative to a team is
// N-valued, so the team-shared rule needs a slice of identifiers. The
// metadata carries the *_account_id alongside the existing display-name
// fields; the matcher compares account IDs via stringInSliceFold.
//
// Project is the project's key as the read that produced the event spelled
// it, a display value. jira_project_id is the jira_projects registry row of
// the same project, which routing gates on and jira_project_id_in matches: a
// project key rename changes the key and not the row, so a team that tracks
// the project keeps receiving its events under the new key, and a predicate
// on the project keeps matching them.
//
// Only the ids a save adds are checked, so an id can go stale afterwards: a
// status, priority or issue type deleted in Jira, a status a project's
// workflow drops, or a project the team stops tracking leaves a filter that
// never matches it. The drift pass (internal/jiradrift) reads the same
// catalogs as the save and tells the team's admins.

// JiraCatalog is a Jira catalog whose values a predicate field names by id.
type JiraCatalog string

const (
	// JiraStatusCatalog is the statuses of the workflows of the projects the
	// handler's team tracks.
	JiraStatusCatalog JiraCatalog = "status"
	// JiraIssueTypeCatalog is the issue types of the projects the handler's
	// team tracks.
	JiraIssueTypeCatalog JiraCatalog = "issue_type"
	// JiraPriorityCatalog is the Jira instance's priorities.
	JiraPriorityCatalog JiraCatalog = "priority"
)

// jiraCatalogFields is every Jira predicate field that names catalog values,
// in the order the catalogs are checked and reported.
var jiraCatalogFields = []struct {
	field   string
	catalog JiraCatalog
}{
	{"status_id_in", JiraStatusCatalog},
	{"new_status_id_in", JiraStatusCatalog},
	{"old_status_id_in", JiraStatusCatalog},
	{"issue_type_id_in", JiraIssueTypeCatalog},
	{"priority_id_in", JiraPriorityCatalog},
	{"new_priority_id_in", JiraPriorityCatalog},
	{"old_priority_id_in", JiraPriorityCatalog},
}

// JiraCatalogFields returns the Jira predicate fields that name c's values.
func JiraCatalogFields(c JiraCatalog) []string {
	var out []string
	for _, f := range jiraCatalogFields {
		if f.catalog == c {
			out = append(out, f.field)
		}
	}
	return out
}

// JiraCatalogOf returns the catalog a Jira predicate field names values from,
// and false for a field that names none.
func JiraCatalogOf(field string) (JiraCatalog, bool) {
	for _, f := range jiraCatalogFields {
		if f.field == field {
			return f.catalog, true
		}
	}
	return "", false
}

// -----------------------------------------------------------------------------
// issue:assigned — issue was assigned (possibly to someone else; predicates
// scope to self).
// -----------------------------------------------------------------------------

type JiraIssueAssignedMetadata struct {
	Assignee          string   `json:"assignee"`            // Jira display name
	AssigneeAccountID string   `json:"assignee_account_id"` // Atlassian stable identifier
	Reporter          string   `json:"reporter"`
	ReporterAccountID string   `json:"reporter_account_id"`
	IssueKey          string   `json:"issue_key"` // "PROJ-123"
	IssueID           string   `json:"issue_id"`  // Jira's numeric issue id
	Project           string   `json:"project"`   // "SKY"
	JiraProjectID     string   `json:"jira_project_id"`
	IssueType         string   `json:"issue_type"`
	IssueTypeID       string   `json:"issue_type_id"`
	Priority          string   `json:"priority"`
	PriorityID        string   `json:"priority_id"`
	Status            string   `json:"status"`
	StatusID          string   `json:"status_id"`
	Summary           string   `json:"summary"`
	Labels            []string `json:"labels"`
}

type JiraIssueAssignedPredicate struct {
	AssigneeIn      []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs, case-insensitive)."`
	ReporterIn      []string `json:"reporter_in,omitempty" doc:"Match issues whose reporter is in this list (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
	IssueTypeIDIn   []string `json:"issue_type_id_in,omitempty" doc:"Match only issues of these issue types, by Jira issue type id (an issue type of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list serves it), not its name. Empty matches every issue type."`
	PriorityIDIn    []string `json:"priority_id_in,omitempty" doc:"Match only issues whose priority is one of these, by Jira priority id (as POST /api/orgs/{org_id}/jira/priorities/list serves it), not its name. Empty matches every priority."`
	StatusIDIn      []string `json:"status_id_in,omitempty" doc:"Match only issues whose current status is one of these, by Jira status id (the id of a status in the workflow of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list serves it), not its name. Empty matches every status."`
	HasLabel        *string  `json:"has_label,omitempty" doc:"Require the issue to currently bear this label."`
}

func (p JiraIssueAssignedPredicate) Matches(m JiraIssueAssignedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		stringInSliceFold(p.ReporterIn, m.ReporterAccountID) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		idIn(p.IssueTypeIDIn, m.IssueTypeID) &&
		idIn(p.PriorityIDIn, m.PriorityID) &&
		idIn(p.StatusIDIn, m.StatusID) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// issue:available — new unassigned issue lands in a configured pickup status.
// -----------------------------------------------------------------------------

type JiraIssueAvailableMetadata struct {
	Reporter          string   `json:"reporter"`
	ReporterAccountID string   `json:"reporter_account_id"`
	IssueKey          string   `json:"issue_key"`
	IssueID           string   `json:"issue_id"`
	Project           string   `json:"project"`
	JiraProjectID     string   `json:"jira_project_id"`
	IssueType         string   `json:"issue_type"`
	IssueTypeID       string   `json:"issue_type_id"`
	Priority          string   `json:"priority"`
	PriorityID        string   `json:"priority_id"`
	Status            string   `json:"status"`
	StatusID          string   `json:"status_id"`
	Summary           string   `json:"summary"`
	Labels            []string `json:"labels"`
}

type JiraIssueAvailablePredicate struct {
	ReporterIn      []string `json:"reporter_in,omitempty" doc:"Match issues whose reporter is in this list (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
	IssueTypeIDIn   []string `json:"issue_type_id_in,omitempty" doc:"Match only issues of these issue types, by Jira issue type id (an issue type of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list serves it), not its name. Empty matches every issue type."`
	PriorityIDIn    []string `json:"priority_id_in,omitempty" doc:"Match only issues whose priority is one of these, by Jira priority id (as POST /api/orgs/{org_id}/jira/priorities/list serves it), not its name. Empty matches every priority."`
	StatusIDIn      []string `json:"status_id_in,omitempty" doc:"Match only issues whose current status is one of these, by Jira status id (the id of a status in the workflow of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list serves it), not its name. Empty matches every status."`
	HasLabel        *string  `json:"has_label,omitempty" doc:"Require the issue to currently bear this label."`
}

func (p JiraIssueAvailablePredicate) Matches(m JiraIssueAvailableMetadata) bool {
	return stringInSliceFold(p.ReporterIn, m.ReporterAccountID) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		idIn(p.IssueTypeIDIn, m.IssueTypeID) &&
		idIn(p.PriorityIDIn, m.PriorityID) &&
		idIn(p.StatusIDIn, m.StatusID) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// issue:status_changed — open-set discriminator (the new status's id is the
// dedup_key, its name when the read carried no id). Multiple concurrent
// status-changed tasks can exist on one issue.
// -----------------------------------------------------------------------------

type JiraIssueStatusChangedMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	IssueID           string `json:"issue_id"`
	Project           string `json:"project"`
	JiraProjectID     string `json:"jira_project_id"`
	IssueType         string `json:"issue_type"`
	IssueTypeID       string `json:"issue_type_id"`
	OldStatus         string `json:"old_status"`
	OldStatusID       string `json:"old_status_id"`
	NewStatus         string `json:"new_status"`
	NewStatusID       string `json:"new_status_id"` // the event's dedup_key, or new_status when this is empty
	Priority          string `json:"priority"`
	PriorityID        string `json:"priority_id"`
}

type JiraIssueStatusChangedPredicate struct {
	AssigneeIn      []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
	IssueTypeIDIn   []string `json:"issue_type_id_in,omitempty" doc:"Match only issues of these issue types, by Jira issue type id (an issue type of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list serves it), not its name. Empty matches every issue type."`
	NewStatusIDIn   []string `json:"new_status_id_in,omitempty" doc:"Match only transitions into one of these statuses, by Jira status id (a status in the workflow of a project the team tracks), not its name. Empty matches every status."`
	OldStatusIDIn   []string `json:"old_status_id_in,omitempty" doc:"Match only transitions out of one of these statuses, by Jira status id (a status in the workflow of a project the team tracks), not its name. Empty matches every status."`
}

func (p JiraIssueStatusChangedPredicate) Matches(m JiraIssueStatusChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		idIn(p.IssueTypeIDIn, m.IssueTypeID) &&
		idIn(p.NewStatusIDIn, m.NewStatusID) &&
		idIn(p.OldStatusIDIn, m.OldStatusID)
}

// -----------------------------------------------------------------------------
// issue:priority_changed — open-set discriminator on the new priority's id
// (its name when the read carried no id).
// -----------------------------------------------------------------------------

type JiraIssuePriorityChangedMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	IssueID           string `json:"issue_id"`
	Project           string `json:"project"`
	JiraProjectID     string `json:"jira_project_id"`
	OldPriority       string `json:"old_priority"`
	OldPriorityID     string `json:"old_priority_id"`
	NewPriority       string `json:"new_priority"`
	NewPriorityID     string `json:"new_priority_id"` // the event's dedup_key, or new_priority when this is empty
}

type JiraIssuePriorityChangedPredicate struct {
	AssigneeIn      []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
	NewPriorityIDIn []string `json:"new_priority_id_in,omitempty" doc:"Match only changes to one of these priorities, by Jira priority id (as POST /api/orgs/{org_id}/jira/priorities/list serves it), not its name. Empty matches every priority."`
	OldPriorityIDIn []string `json:"old_priority_id_in,omitempty" doc:"Match only changes from one of these priorities, by Jira priority id, not its name. Empty matches every priority."`
}

func (p JiraIssuePriorityChangedPredicate) Matches(m JiraIssuePriorityChangedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		idIn(p.NewPriorityIDIn, m.NewPriorityID) &&
		idIn(p.OldPriorityIDIn, m.OldPriorityID)
}

// -----------------------------------------------------------------------------
// issue:commented — someone other than TF commented. One event per poll however
// many comments landed: CommentID, Commenter and CommenterAccountID are the
// newest of them, and CommenterAccountIDs holds the author of each, so
// commenter_in matches when any of them wrote one. A comment TF posted is not
// one of them, and a poll that found only TF's comments emits nothing.
// -----------------------------------------------------------------------------

type JiraIssueCommentedMetadata struct {
	Assignee            string   `json:"assignee"`
	AssigneeAccountID   string   `json:"assignee_account_id"`
	Commenter           string   `json:"commenter"`
	CommenterAccountID  string   `json:"commenter_account_id"`
	CommenterAccountIDs []string `json:"commenter_account_ids"`
	CommentID           string   `json:"comment_id"`
	IssueKey            string   `json:"issue_key"`
	IssueID             string   `json:"issue_id"`
	Project             string   `json:"project"`
	JiraProjectID       string   `json:"jira_project_id"`
}

type JiraIssueCommentedPredicate struct {
	AssigneeIn      []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	CommenterIn     []string `json:"commenter_in,omitempty" doc:"Match when anyone in this list wrote one of the new comments (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
}

func (p JiraIssueCommentedPredicate) Matches(m JiraIssueCommentedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		anyInSliceFold(p.CommenterIn, m.CommenterAccountIDs) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID)
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
	JiraProjectID     string `json:"jira_project_id"`
	IssueType         string `json:"issue_type"`
	IssueTypeID       string `json:"issue_type_id"`
	FinalStatus       string `json:"final_status"`
	FinalStatusID     string `json:"final_status_id"`
}

type JiraIssueCompletedPredicate struct {
	AssigneeIn      []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
	IssueTypeIDIn   []string `json:"issue_type_id_in,omitempty" doc:"Match only issues of these issue types, by Jira issue type id (an issue type of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list serves it), not its name. Empty matches every issue type."`
}

func (p JiraIssueCompletedPredicate) Matches(m JiraIssueCompletedMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		idIn(p.IssueTypeIDIn, m.IssueTypeID)
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
	Assignee          string   `json:"assignee"`
	AssigneeAccountID string   `json:"assignee_account_id"`
	Reporter          string   `json:"reporter"`
	ReporterAccountID string   `json:"reporter_account_id"`
	IssueKey          string   `json:"issue_key"`
	IssueID           string   `json:"issue_id"`
	Project           string   `json:"project"`
	JiraProjectID     string   `json:"jira_project_id"`
	IssueType         string   `json:"issue_type"`
	IssueTypeID       string   `json:"issue_type_id"`
	Priority          string   `json:"priority"`
	PriorityID        string   `json:"priority_id"`
	Status            string   `json:"status"`
	StatusID          string   `json:"status_id"`
	Summary           string   `json:"summary"`
	Labels            []string `json:"labels"`
}

type JiraIssueBecameAtomicPredicate struct {
	AssigneeIn      []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	ReporterIn      []string `json:"reporter_in,omitempty" doc:"Match issues whose reporter is in this list (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
	IssueTypeIDIn   []string `json:"issue_type_id_in,omitempty" doc:"Match only issues of these issue types, by Jira issue type id (an issue type of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list serves it), not its name. Empty matches every issue type."`
	PriorityIDIn    []string `json:"priority_id_in,omitempty" doc:"Match only issues whose priority is one of these, by Jira priority id (as POST /api/orgs/{org_id}/jira/priorities/list serves it), not its name. Empty matches every priority."`
	StatusIDIn      []string `json:"status_id_in,omitempty" doc:"Match only issues whose current status is one of these, by Jira status id (the id of a status in the workflow of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list serves it), not its name. Empty matches every status."`
	HasLabel        *string  `json:"has_label,omitempty" doc:"Require the issue to currently bear this label."`
}

func (p JiraIssueBecameAtomicPredicate) Matches(m JiraIssueBecameAtomicMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		stringInSliceFold(p.ReporterIn, m.ReporterAccountID) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		idIn(p.IssueTypeIDIn, m.IssueTypeID) &&
		idIn(p.PriorityIDIn, m.PriorityID) &&
		idIn(p.StatusIDIn, m.StatusID) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// issue:label_added / issue:label_removed — open-set discriminator, as on
// GitHub: the label's name is in label_name and is the event's dedup_key, and
// labels is the issue's whole set after the change. A Jira label is a plain
// string with no id and cannot be renamed, so the name is its identity. Never
// emitted on first discovery: the labels an issue already has when TF first
// sees it are its baseline, and has_label on assigned / available /
// became_atomic is how a rule matches them. Not suppressed on an issue with
// open subtasks, unlike assigned / available: someone added the label on
// purpose, and suppressing it would make the label silently do nothing.
// -----------------------------------------------------------------------------

type JiraIssueLabelMetadata struct {
	Assignee          string   `json:"assignee"`
	AssigneeAccountID string   `json:"assignee_account_id"`
	Reporter          string   `json:"reporter"`
	ReporterAccountID string   `json:"reporter_account_id"`
	IssueKey          string   `json:"issue_key"`
	IssueID           string   `json:"issue_id"`
	Project           string   `json:"project"`
	JiraProjectID     string   `json:"jira_project_id"`
	IssueType         string   `json:"issue_type"`
	IssueTypeID       string   `json:"issue_type_id"`
	Priority          string   `json:"priority"`
	PriorityID        string   `json:"priority_id"`
	Status            string   `json:"status"`
	StatusID          string   `json:"status_id"`
	Summary           string   `json:"summary"`
	LabelName         string   `json:"label_name"` // also the event's dedup_key
	Labels            []string `json:"labels"`     // the issue's labels after the change
}

type JiraIssueLabelPredicate struct {
	AssigneeIn      []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
	LabelName       *string  `json:"label_name,omitempty" doc:"Match only this specific label (e.g. 'tf-agent')."`
	HasLabel        *string  `json:"has_label,omitempty" doc:"Require the issue to bear this label after the change."`
}

func (p JiraIssueLabelPredicate) Matches(m JiraIssueLabelMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		strEq(p.LabelName, m.LabelName) &&
		hasLabel(p.HasLabel, m.Labels)
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
	JiraProjectID     string `json:"jira_project_id"`
	IssueType         string `json:"issue_type"`
	IssueTypeID       string `json:"issue_type_id"`
	LastStatus        string `json:"last_status"`
	LastStatusID      string `json:"last_status_id"`
	Summary           string `json:"summary"`
	Reason            string `json:"reason"` // one of the JiraUnreachable* values
}

type JiraIssueUnreachablePredicate struct {
	AssigneeIn      []string `json:"assignee_in,omitempty" doc:"Match issues assigned to anyone in this list (Atlassian account IDs)."`
	JiraProjectIDIn []string `json:"jira_project_id_in,omitempty" doc:"Match only issues in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches every project the team tracks."`
	IssueTypeIDIn   []string `json:"issue_type_id_in,omitempty" doc:"Match only issues of these issue types, by Jira issue type id (an issue type of a project the team tracks, as POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list serves it), not its name. Empty matches every issue type."`
}

func (p JiraIssueUnreachablePredicate) Matches(m JiraIssueUnreachableMetadata) bool {
	return stringInSliceFold(p.AssigneeIn, m.AssigneeAccountID) &&
		idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		idIn(p.IssueTypeIDIn, m.IssueTypeID)
}

// -----------------------------------------------------------------------------
// issue:key_changed — a tracked issue answers under a new key: it moved to
// another project, or its project's key was renamed. The issue's id is
// unchanged, so the entity is renamed in place and keeps its tasks,
// conversations and memory. Emitted before any other event from the same
// refresh, in the same commit as the snapshot. Not terminating.
//
// Unlike every other Jira event, it reaches a team that tracks either
// project: the one in jira_project_id, or the one in old_jira_project_id,
// which the issue left. A team whose issue moved to a project it does not track keeps its
// tasks on the entity, and this event is the only thing that tells it the
// issue left.
// -----------------------------------------------------------------------------

type JiraIssueKeyChangedMetadata struct {
	Assignee          string `json:"assignee"`
	AssigneeAccountID string `json:"assignee_account_id"`
	IssueKey          string `json:"issue_key"`
	IssueID           string `json:"issue_id"`
	Project           string `json:"project"`
	JiraProjectID     string `json:"jira_project_id"`
	IssueType         string `json:"issue_type"`
	IssueTypeID       string `json:"issue_type_id"`
	Summary           string `json:"summary"`
	OldIssueKey       string `json:"old_issue_key"`
	OldProject        string `json:"old_project"`
	OldJiraProjectID  string `json:"old_jira_project_id"`
}

type JiraIssueKeyChangedPredicate struct {
	JiraProjectIDIn    []string `json:"jira_project_id_in,omitempty" doc:"Match only issues now in these Jira projects, each one the team tracks, by registry id (the id of an entry in jira_projects on GET /api/teams/{team_id}/settings, not the project key). Empty matches any project."`
	OldJiraProjectIDIn []string `json:"old_jira_project_id_in,omitempty" doc:"Match only issues that left these Jira projects, each one the team tracks, by registry id. Empty matches any project."`
}

func (p JiraIssueKeyChangedPredicate) Matches(m JiraIssueKeyChangedMetadata) bool {
	return idIn(p.JiraProjectIDIn, m.JiraProjectID) &&
		idIn(p.OldJiraProjectIDIn, m.OldJiraProjectID)
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
	Register(NewSchema[JiraIssueLabelMetadata, JiraIssueLabelPredicate](domain.EventJiraIssueLabelAdded, OwnershipOwned))
	Register(NewSchema[JiraIssueLabelMetadata, JiraIssueLabelPredicate](domain.EventJiraIssueLabelRemoved, OwnershipOwned))
	Register(NewSchema[JiraIssueUnreachableMetadata, JiraIssueUnreachablePredicate](domain.EventJiraIssueUnreachable, OwnershipOwned))
	Register(NewSchema[JiraIssueKeyChangedMetadata, JiraIssueKeyChangedPredicate](domain.EventJiraIssueKeyChanged, OwnershipOwned))
}
