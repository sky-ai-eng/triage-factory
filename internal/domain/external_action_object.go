package domain

import (
	"strings"
)

// Object kinds: what an external action changed, when it landed a change.
// A closed vocabulary (a CHECK on external_actions.object_kind), read
// together with the columns that identify an object of each kind:
//
//   - pull_request, and a GitHub issue: EntityID. ObjectKey is empty.
//   - a Jira or Linear issue: Scope + ObjectKey, the issue's
//     ObjectKey("jira"|"linear", "issue", resource, "").
//   - repository: RepositoryID. ObjectKey is empty.
//   - branch, label, release, workflow, workflow_run: RepositoryID +
//     ObjectKey, which holds the ref, the label name, or GitHub's release,
//     workflow or workflow run id.
//   - review, comment, message: Scope + ObjectKey, which holds
//     GitHubReviewObjectKey, GitHubCommentKey or the Jira/Linear comment's
//     ObjectKey, or the Slack message's ObjectKey.
//
// The kinds identified by EntityID or RepositoryID ride the columns an entity
// merge and a repository absorb already re-point, so an object's identity
// follows those moves.
const (
	ObjectKindPullRequest = "pull_request"
	ObjectKindIssue       = "issue"
	ObjectKindRepository  = "repository"
	ObjectKindBranch      = "branch"
	ObjectKindLabel       = "label"
	ObjectKindRelease     = "release"
	ObjectKindWorkflow    = "workflow"
	ObjectKindWorkflowRun = "workflow_run"
	ObjectKindReview      = "review"
	ObjectKindComment     = "comment"
	ObjectKindMessage     = "message"
)

// ObjectKinds is the closed object_kind vocabulary, in the order the column's
// CHECK spells it.
func ObjectKinds() []string {
	return []string{
		ObjectKindPullRequest, ObjectKindIssue, ObjectKindRepository, ObjectKindBranch,
		ObjectKindLabel, ObjectKindRelease, ObjectKindWorkflow, ObjectKindWorkflowRun,
		ObjectKindReview, ObjectKindComment, ObjectKindMessage,
	}
}

// actionObjectKinds maps every action that lands a change to the kind of
// object it changes. The actions absent from it name no object: the
// refusals and failures (IsRefusalOrFailure), the two catch-alls
// (gh_channel_write, graphql_write), pr_reverted and repo_created.
var actionObjectKinds = map[string]string{
	ActionBranchPushed:        ObjectKindBranch,
	ActionBranchDeleted:       ObjectKindBranch,
	ActionLinkedBranchCreated: ObjectKindBranch,

	ActionPRCreated:            ObjectKindPullRequest,
	ActionPRMarkedReady:        ObjectKindPullRequest,
	ActionPRConvertedToDraft:   ObjectKindPullRequest,
	ActionPREdited:             ObjectKindPullRequest,
	ActionPRClosed:             ObjectKindPullRequest,
	ActionPRReopened:           ObjectKindPullRequest,
	ActionPRMerged:             ObjectKindPullRequest,
	ActionPRAutoMergeEnabled:   ObjectKindPullRequest,
	ActionPRAutoMergeDisabled:  ObjectKindPullRequest,
	ActionPRBranchUpdated:      ObjectKindPullRequest,
	ActionReviewRequested:      ObjectKindPullRequest,
	ActionReviewRequestRemoved: ObjectKindPullRequest,

	// owner/repo#N resolves to a pull request entity even when GitHub's
	// object is an issue, so an issue's label and lock rows name the pull
	// request kind too.
	ActionLabelAdded:           ObjectKindPullRequest,
	ActionLabelRemoved:         ObjectKindPullRequest,
	ActionConversationLocked:   ObjectKindPullRequest,
	ActionConversationUnlocked: ObjectKindPullRequest,

	// A reaction on a comment names the comment instead; only the writer
	// knows which the reaction landed on.
	ActionReactionAdded:   ObjectKindPullRequest,
	ActionReactionRemoved: ObjectKindPullRequest,

	ActionIssueCreated:      ObjectKindIssue,
	ActionIssueUpdated:      ObjectKindIssue,
	ActionIssueClosed:       ObjectKindIssue,
	ActionIssueReopened:     ObjectKindIssue,
	ActionIssueDeleted:      ObjectKindIssue,
	ActionIssuePinned:       ObjectKindIssue,
	ActionIssueUnpinned:     ObjectKindIssue,
	ActionIssueTransferred:  ObjectKindIssue,
	ActionIssueTransitioned: ObjectKindIssue,
	ActionIssueAssigned:     ObjectKindIssue,

	ActionReviewSubmitted: ObjectKindReview,
	ActionReviewDismissed: ObjectKindReview,

	ActionCommentPosted:        ObjectKindComment,
	ActionCommentEdited:        ObjectKindComment,
	ActionCommentDeleted:       ObjectKindComment,
	ActionReviewCommentEdited:  ObjectKindComment,
	ActionReviewCommentDeleted: ObjectKindComment,
	ActionIssueCommentPosted:   ObjectKindComment,

	ActionSlackMessagePosted: ObjectKindMessage,
	ActionSlackMessageEdited: ObjectKindMessage,
	ActionSlackReactionAdded: ObjectKindMessage,

	ActionRepoEdited:     ObjectKindRepository,
	ActionRepoDeleted:    ObjectKindRepository,
	ActionRepoForked:     ObjectKindRepository,
	ActionRepoArchived:   ObjectKindRepository,
	ActionRepoUnarchived: ObjectKindRepository,

	ActionLabelDefined:           ObjectKindLabel,
	ActionLabelDefinitionEdited:  ObjectKindLabel,
	ActionLabelDefinitionDeleted: ObjectKindLabel,

	ActionReleaseCreated: ObjectKindRelease,
	ActionReleaseEdited:  ObjectKindRelease,
	ActionReleaseDeleted: ObjectKindRelease,

	ActionWorkflowDispatched:   ObjectKindWorkflow,
	ActionWorkflowRunCancelled: ObjectKindWorkflowRun,
}

// ActionObjectKind returns the kind of object action changes when it lands,
// "" for an action that names none. A reaction answers pull_request, the kind
// of a reaction on a pull request or an issue; a writer that knows the
// reaction landed on a comment names the comment.
func ActionObjectKind(action string) string {
	return actionObjectKinds[action]
}

// IsRefusalOrFailure reports whether action records a write that was refused
// or failed: nothing landed, and the row names no object.
func IsRefusalOrFailure(action string) bool {
	switch action {
	case ActionGitDenied, ActionEgressDenied, ActionGHWriteDenied, ActionCommitSignDenied, ActionBranchPushFailed:
		return true
	}
	return false
}

// objectKindKeyed reports whether an object of kind is identified with an
// ObjectKey, so a row of that kind with no key identifies nothing.
func objectKindKeyed(kind, provider string) bool {
	switch kind {
	case ObjectKindPullRequest, ObjectKindRepository:
		return false
	case ObjectKindIssue:
		return provider != ProviderGitHub
	}
	return true
}

// SetObject names the object a landed change changed: kind and key, or
// neither when kind's identity needs a key and key is empty. A writer
// without what the key needs (a GraphQL edit naming a comment only by node
// id, a Jira issue whose id never came back) leaves the row naming no object
// rather than one with half an identity. a.Provider must be set first: a
// GitHub issue is identified by its entity, a Jira or Linear one by its key.
func (a *ExternalAction) SetObject(kind, key string) {
	a.ObjectKind, a.ObjectKey = "", ""
	if kind == "" {
		return
	}
	if !objectKindKeyed(kind, a.Provider) {
		a.ObjectKind = kind
		return
	}
	if key == "" {
		return
	}
	a.ObjectKind, a.ObjectKey = kind, key
}

// ObjectID renders the identity of the object a names, within its org:
// "<object_kind>|<id part>", where the id part is the entity id
// (pull_request, a GitHub issue), the repository id (repository),
// "<repository id>|<object_key>" (branch, label, release, workflow,
// workflow_run), or "<scope>|<object_key>" (a Jira or Linear issue, review,
// comment, message). The string is opaque: equal strings are the same object.
// ok=false for a row with no object_kind, an unknown kind, or a kind whose
// identifying columns are empty. Each SQL dialect's external-action store
// holds an expression producing the same string, or NULL where this answers
// false.
func ObjectID(a ExternalAction) (string, bool) {
	join := func(parts ...string) (string, bool) {
		for _, p := range parts {
			if p == "" {
				return "", false
			}
		}
		return a.ObjectKind + "|" + strings.Join(parts, "|"), true
	}
	switch a.ObjectKind {
	case ObjectKindPullRequest:
		return join(a.EntityID)
	case ObjectKindIssue:
		if a.Provider == ProviderGitHub {
			return join(a.EntityID)
		}
		return join(a.Scope, a.ObjectKey)
	case ObjectKindRepository:
		return join(a.RepositoryID)
	case ObjectKindBranch, ObjectKindLabel, ObjectKindRelease, ObjectKindWorkflow, ObjectKindWorkflowRun:
		return join(a.RepositoryID, a.ObjectKey)
	case ObjectKindReview, ObjectKindComment, ObjectKindMessage:
		return join(a.Scope, a.ObjectKey)
	}
	return "", false
}

// GitHubReviewObjectKey is the object_key of a submitted GitHub review:
// "github:review:<review id>". "" when the id is unknown.
func GitHubReviewObjectKey(reviewID string) string {
	if reviewID == "" {
		return ""
	}
	return ObjectKey(ProviderGitHub, ObjectKindReview, reviewID, "")
}

// IssueObjectKey is the object_key of the Jira or Linear issue carrying
// (scope, externalID): ObjectKey(provider, "issue", EntityResource(...), ""),
// the key the issue's deliverables are keyed under too. "" when there is no
// resource to key on.
func IssueObjectKey(provider, scope, externalID string) string {
	resource := EntityResource(provider, scope, externalID)
	if resource == "" {
		return ""
	}
	return ObjectKey(provider, ObjectKindIssue, resource, "")
}

// ObjectKeyEntityIdentity reads the identity of the issue a Jira or Linear
// key names, an object_key or a deliverable dedup key alike: the scope (a
// Jira key carries its site) and the provider id. A comment's key names its
// issue first, so a comment's key answers for the issue it is on. ok=false for
// another provider, a key of another provider, and a stored Jira key that
// still spells an issue key rather than the issue's id.
func ObjectKeyEntityIdentity(provider, key string) (scope, externalID string, ok bool) {
	p, rest, found := strings.Cut(key, ":")
	if !found || p != provider {
		return "", "", false
	}
	_, rest, found = strings.Cut(rest, ":")
	if !found {
		return "", "", false
	}
	resource, _, _ := strings.Cut(rest, ":")
	switch provider {
	case ProviderJira:
		return ParseJiraIssueResource(resource)
	case ProviderLinear:
		return "", resource, resource != ""
	}
	return "", "", false
}
