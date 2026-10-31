package domain

import (
	"slices"
	"strconv"
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

var objectKinds = []string{
	ObjectKindPullRequest, ObjectKindIssue, ObjectKindRepository, ObjectKindBranch,
	ObjectKindLabel, ObjectKindRelease, ObjectKindWorkflow, ObjectKindWorkflowRun,
	ObjectKindReview, ObjectKindComment, ObjectKindMessage,
}

// ObjectKinds is the closed object_kind vocabulary, in the order the column's
// CHECK spells it.
func ObjectKinds() []string {
	return slices.Clone(objectKinds)
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

	// Labels and locks on an issue go through the same endpoints. Every
	// GitHub pull request or issue kind here is the endpoint's; the recording
	// funnel replaces it with the kind of the entity the row resolves to
	// (GitHubEntityObjectKind).
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
// "" for an action that names none. For a GitHub pull request or issue it is
// the kind the action's endpoint implies, which the recording funnel replaces
// with the resolved entity's (GitHubEntityObjectKind). A reaction answers
// pull_request, the kind of a reaction on a pull request or an issue; a
// writer that knows the reaction landed on a comment names the comment.
func ActionObjectKind(action string) string {
	return actionObjectKinds[action]
}

// ActionNamesKind reports whether a row provider records for action may name
// an object of kind: the kind ActionObjectKind gives, or a comment for a
// reaction. GitHub numbers pull requests and issues in one sequence and serves
// both through the issue endpoints, so a GitHub action of either kind may name
// the other. An action that names no object (a refusal, a failure, a
// catch-all) may name none.
func ActionNamesKind(provider, action, kind string) bool {
	want := ActionObjectKind(action)
	switch kind {
	case "":
		return false
	case want:
		return true
	case ObjectKindComment:
		return action == ActionReactionAdded || action == ActionReactionRemoved
	}
	return provider == ProviderGitHub && isGitHubEntityObjectKind(kind) && isGitHubEntityObjectKind(want)
}

// isGitHubEntityObjectKind reports whether kind is one GitHubEntityObjectKind
// gives: an object a GitHub entity identifies.
func isGitHubEntityObjectKind(kind string) bool {
	return kind == ObjectKindPullRequest || kind == ObjectKindIssue
}

// GitHubEntityObjectKind is the object kind of a GitHub entity of kind
// entityKind: pull_request for a pull request, issue for an issue, "" for
// anything else.
func GitHubEntityObjectKind(entityKind string) string {
	switch entityKind {
	case EntityKindPullRequest:
		return ObjectKindPullRequest
	case EntityKindIssue:
		return ObjectKindIssue
	}
	return ""
}

var refusalOrFailureActions = []string{
	ActionGitDenied, ActionEgressDenied, ActionGHWriteDenied, ActionCommitSignDenied, ActionBranchPushFailed,
}

// RefusalOrFailureActions is the actions that record a write that was refused
// or failed. The ledger's list reads build their refusal filter from it, so
// the filter and IsRefusalOrFailure cannot disagree.
func RefusalOrFailureActions() []string {
	return slices.Clone(refusalOrFailureActions)
}

// IsRefusalOrFailure reports whether action records a write that was refused
// or failed: nothing landed, and the row names no object.
func IsRefusalOrFailure(action string) bool {
	return slices.Contains(refusalOrFailureActions, action)
}

// ActionsNamingNoObject is every action ActionObjectKind gives no kind: the
// refusals and failures, the catch-alls, and the actions that never land.
// The artifact reads exclude their rows whatever the rows carry, so a writer
// that named an object on one cannot make it an artifact.
func ActionsNamingNoObject() []string {
	var out []string
	for _, action := range ExternalActionTypes() {
		if ActionObjectKind(action) == "" {
			out = append(out, action)
		}
	}
	return out
}

// IsObjectID reports whether s has the shape ObjectID renders: a known object
// kind, a "|", and a non-empty id part. It says nothing about whether any row
// carries it.
func IsObjectID(s string) bool {
	kind, rest, ok := strings.Cut(s, "|")
	if !ok || rest == "" {
		return false
	}
	return slices.Contains(objectKinds, kind)
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
// "github:review:<review id>". "" for an id that is not a positive integer: a
// submit whose response carried no id reads as 0, and every such review would
// otherwise share one identity.
func GitHubReviewObjectKey(reviewID string) string {
	n, err := strconv.ParseInt(reviewID, 10, 64)
	if err != nil || n <= 0 {
		return ""
	}
	return ObjectKey(ProviderGitHub, ObjectKindReview, strconv.FormatInt(n, 10), "")
}

// IssueObjectKey is the object_key of the Jira or Linear issue carrying
// (scope, externalID): ObjectKey(provider, "issue", EntityResource(...), ""),
// which is also the dedup key of the issue's deliverable. A Jira key names the
// site (scope) and the issue id; a Linear key names the issue's UUID alone, so
// its scope is not read. "" when there is no resource to key on.
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
