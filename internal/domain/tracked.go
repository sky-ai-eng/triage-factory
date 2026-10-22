package domain

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

// GitHubBotLoginSuffix ends a GitHub App bot's login as REST spells it
// ("dependabot[bot]"). Snapshots, event metadata and handler predicates all use
// that spelling; GraphQL's Bot.login omits the suffix.
const GitHubBotLoginSuffix = "[bot]"

// PRSnapshot is the extracted state we store for a GitHub pull request.
// Every field here can trigger events when it changes between poll cycles.
type PRSnapshot struct {
	// Identity
	NodeID string `json:"node_id"` // GitHub GraphQL node ID — stored in snapshot for entity-based refresh
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author string `json:"author"` // login of the PR author, REST-spelled ("<slug>[bot]" for a bot)
	// AuthorID is GitHub's id for the author's account, which a rename leaves
	// alone; Author is what the account is called now. Empty on a snapshot
	// stored before ids were recorded, and for an author GitHub returns
	// without one (a deleted account).
	AuthorID string `json:"author_id,omitempty"`
	Repo     string `json:"repo"`      // "owner/repo"
	HeadRepo string `json:"head_repo"` // fork repo if different
	URL      string `json:"url"`

	// State
	State     string `json:"state"` // OPEN, CLOSED, MERGED
	IsDraft   bool   `json:"is_draft"`
	Merged    bool   `json:"merged"`
	Mergeable string `json:"mergeable"` // MERGEABLE, CONFLICTING, UNKNOWN

	// Branches
	HeadRef string `json:"head_ref"`
	BaseRef string `json:"base_ref"`
	HeadSHA string `json:"head_sha"`
	// HeadCommittedAt is the git-side commit time of the current HEAD (ISO
	// 8601 from GitHub's commit.committedDate). Used as the source time
	// for new_commits events so the factory's chain animation orders
	// transitions by when the push actually happened rather than when we
	// polled. Empty when the field is missing (pre-field snapshots).
	HeadCommittedAt string `json:"head_committed_at,omitempty"`

	// Size
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`

	// CI — structured per-check-run data, scoped to the current head SHA.
	// Deduped by Name at ingest time (latest execution wins by highest ID), so
	// this represents "the current state of each named check" not the full
	// history of executions. nil means "unknown prior state" (old snapshot
	// from before this field existed); empty slice means "polled, no checks".
	CheckRuns []CheckRun `json:"check_runs"`

	// Reviews
	ReviewRequests []string `json:"review_requests"` // logins of users/teams with pending requests
	// ReviewRequestIDs maps an entry of ReviewRequests to GitHub's id for
	// the requested user or team. An entry GitHub returned without an id has
	// none here, and a snapshot stored before ids were recorded has no map.
	// RequestedReviewers reads the two together.
	ReviewRequestIDs map[string]string `json:"review_request_ids,omitempty"`
	Reviews          []ReviewState     `json:"reviews"`      // latest review per reviewer
	ReviewCount      int               `json:"review_count"` // total reviews submitted
	// ReviewMarker is the newest submitted review, the point the next poll
	// reads new reviews from. nil on a snapshot stored before the marker
	// existed and on one seeded from the REST listing of open pull requests,
	// which carries no reviews; the diff against such a snapshot compares each
	// reviewer's latest review instead.
	ReviewMarker *ReviewMarker `json:"review_marker,omitempty"`

	// Metadata
	Labels []string `json:"labels"`
	// CommentCount is how many conversation comments the pull request has.
	// Zero on a snapshot seeded from the REST listing of open pull requests,
	// which carries no comment count.
	CommentCount int `json:"comment_count"`
	// CommentMarker is the newest conversation comment, the point the next
	// poll reads new comments from. nil on a snapshot seeded from the REST
	// listing and on one stored before the marker existed; the first refresh
	// against such a snapshot stores it.
	CommentMarker *CommentMarker `json:"comment_marker,omitempty"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
	MergedAt      string         `json:"merged_at,omitempty"`
	ClosedAt      string         `json:"closed_at,omitempty"`

	// Timeline carries a tail of PR-side events (label add/remove, review
	// request, ready-for-review) with their per-action createdAt
	// timestamps, pulled from GraphQL timelineItems on the same PR query
	// (no extra HTTP request). Used by the diff layer to attach honest
	// source times to events that have no per-field timestamp on the
	// snapshot itself.
	//
	// json:"-" so the timeline doesn't bloat snapshot_json — it's a
	// transient metadata channel, not state we diff against. Each poll
	// brings fresh timeline data; we just need it during the diff that
	// consumes the same wire response.
	Timeline []TimelineEvent `json:"-"`

	// BodyHash fingerprints the complete body, before preview truncation.
	// Empty means no body has been observed, including pre-field snapshots.
	BodyHash string `json:"body_hash,omitempty"`
	// BodyMentions are the users and teams the body @-mentions, in the
	// handle form github.Mentions returns, so a body edit that adds one is
	// found without storing the body. nil means unknown: a snapshot stored
	// before the field existed, or a response without the body. An empty
	// slice is a body that mentions nobody.
	BodyMentions []string `json:"body_mentions"`
	// Body is mirrored into entities.description. Only its fingerprint and
	// mentions are persisted in the snapshot, so a diff never reads the body.
	Body string `json:"-"`
}

// RequestedReviewer is one pending review request on a pull request: the
// requested user (Handle is the login) or team (Handle is "org/slug"), and
// GitHub's id for it when known.
type RequestedReviewer struct {
	Handle string
	ID     string
	Team   bool
}

// Same reports whether r and o name the same requested reviewer: by GitHub's
// id when both carry one, so a renamed user or team is still the same
// reviewer and a handle GitHub has given to another account is not; by
// handle when either lacks it, as a snapshot stored before ids were recorded
// does.
func (r RequestedReviewer) Same(o RequestedReviewer) bool {
	if r.Team != o.Team {
		return false
	}
	if r.ID != "" && o.ID != "" {
		return r.ID == o.ID
	}
	return r.Handle == o.Handle
}

// RequestedReviewers returns the snapshot's pending review requests in
// ReviewRequests order, each with its id from ReviewRequestIDs. A team entry
// is the one whose handle has a slash, which a login cannot contain.
func (s PRSnapshot) RequestedReviewers() []RequestedReviewer {
	out := make([]RequestedReviewer, 0, len(s.ReviewRequests))
	for _, h := range s.ReviewRequests {
		if h == "" {
			continue
		}
		out = append(out, RequestedReviewer{Handle: h, ID: s.ReviewRequestIDs[h], Team: strings.Contains(h, "/")})
	}
	return out
}

// HasRequestedReviewer reports whether one of the snapshot's pending review
// requests is the same reviewer as r (RequestedReviewer.Same).
func (s PRSnapshot) HasRequestedReviewer(r RequestedReviewer) bool {
	for _, o := range s.RequestedReviewers() {
		if o.Same(r) {
			return true
		}
	}
	return false
}

// TimelineEvent is one entry from GitHub's PullRequest.timelineItems.
// Kind discriminates which fields are populated. Only the event types we
// actually use for source-time enrichment are modeled here — adding a new
// kind means extending both the GraphQL fragment and this type.
type TimelineEvent struct {
	Kind      string `json:"kind"`       // labeled | unlabeled | review_requested | ready_for_review
	CreatedAt string `json:"created_at"` // RFC3339 from GraphQL
	// Label is the label name for kind ∈ {labeled, unlabeled}.
	Label string `json:"label,omitempty"`
	// Reviewer is "login" for User reviewers or "org/slug" for Team
	// reviewers; populated for kind = review_requested. Mirrors the
	// shape used by PRSnapshot.ReviewRequests so diff lookups can
	// match on equality.
	Reviewer string `json:"reviewer,omitempty"`
}

// ReviewState captures one reviewer's latest review.
type ReviewState struct {
	// ID is the review's GraphQL node id, when known. Populated by the PR
	// refresh path so the artifact reconciler (TFAC-464) can match a pending
	// review artifact (keyed on this id) to its submitted/dismissed state.
	// Empty on snapshots from before the field existed and on paths that
	// don't request it; the review-transition diff keys on Author+State and
	// never reads ID, so its absence is inert there.
	ID string `json:"id,omitempty"`
	// DatabaseID is the review's numeric id, the one REST and the review
	// events carry. Zero on snapshots from before the field existed.
	DatabaseID int64  `json:"database_id,omitempty"`
	Author     string `json:"author"` // reviewer login, REST-spelled ("<slug>[bot]" for a bot)
	// AuthorID is GitHub's id for the reviewer's account. Empty on snapshots
	// from before the field existed.
	AuthorID    string `json:"author_id,omitempty"`
	State       string `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	SubmittedAt string `json:"submitted_at"`
	// CommentCount is how many inline comments the review holds.
	CommentCount int `json:"comment_count,omitempty"`
}

// ReviewMarker names a pull request's newest submitted review by its numeric
// id and submission time. A zero marker means the pull request has no
// submitted review.
type ReviewMarker struct {
	ID          int64  `json:"id,omitempty"`
	SubmittedAt string `json:"submitted_at,omitempty"`
}

// After reports whether the review submitted at submittedAt with id id is
// newer than m: by submission time, or by id when the times are equal or
// either does not parse.
func (m ReviewMarker) After(submittedAt string, id int64) bool {
	return markedAfter(submittedAt, id, m.SubmittedAt, m.ID)
}

// Newer returns whichever of m and o names the newer review.
func (m ReviewMarker) Newer(o ReviewMarker) ReviewMarker {
	if m.After(o.SubmittedAt, o.ID) {
		return o
	}
	return m
}

// CommentMarker names a pull request's newest conversation comment by its
// numeric id and creation time. A zero marker means the pull request has no
// conversation comment.
type CommentMarker struct {
	ID        int64  `json:"id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// After reports whether the comment created at createdAt with id id is newer
// than m: by creation time, or by id when the times are equal or either does
// not parse.
func (m CommentMarker) After(createdAt string, id int64) bool {
	return markedAfter(createdAt, id, m.CreatedAt, m.ID)
}

// Newer returns whichever of m and o names the newer comment.
func (m CommentMarker) Newer(o CommentMarker) CommentMarker {
	if m.After(o.CreatedAt, o.ID) {
		return o
	}
	return m
}

// markedAfter reports whether the object at at with id id is newer than the
// one at markAt with id markID: by time, or by id when the times are equal or
// neither parses. A time that parses is newer than one that does not. GitHub
// hands out review and comment ids in increasing order.
func markedAfter(at string, id int64, markAt string, markID int64) bool {
	t, okT := ParseExternalTime(at)
	mt, okM := ParseExternalTime(markAt)
	if okT && okM && !t.Equal(mt) {
		return t.After(mt)
	}
	if okT != okM {
		return okT
	}
	return id > markID
}

// CheckRun is a single execution of a CI check on a commit.
//
// ID is GitHub's database ID — monotonically increasing and unique per
// execution, so re-running a workflow on the same SHA produces a new ID.
// That's why ID (not Name) is the identity key for re-trigger detection:
// "same SHA, same name, new ID" means a re-run, and "new ID with a failing
// conclusion" means we have a new failure to act on.
//
// DetailsURL is GitHub's details_url / detailsUrl — the "more info" link the
// CI provider attaches to the check run. For GitHub Actions checks this is
// the workflow-run/job page (/actions/runs/N/job/M); for third-party CI
// systems it's wherever the provider wants users to land. This is NOT the
// narrower GitHub check-run page URL (/runs/N) — we deliberately store the
// details URL because (a) it's the more useful human-facing link across
// providers and (b) parseWorkflowRunIDFromURL depends on the Actions URL
// shape exposed here.
//
// WorkflowRunID is the GitHub Actions workflow run database ID, pulled from
// the GraphQL workflowRun field on the containing check suite. It's zero for
// check runs from third-party CI systems (Supabase, Circle, etc.) — those
// can't be fetched via the Actions log-download endpoint anyway.
type CheckRun struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`     // queued | in_progress | completed
	Conclusion    string `json:"conclusion"` // success | failure | cancelled | timed_out | action_required | neutral | skipped | stale | ""
	CompletedAt   string `json:"completed_at"`
	DetailsURL    string `json:"details_url"`
	WorkflowRunID int64  `json:"workflow_run_id,omitempty"`
}

// IsFailingConclusion reports whether a check-run conclusion is one we treat
// as a failure worth surfacing as an event. Kept in one place so the poller,
// the diff logic, and any UI badge code agree on what "failing" means.
func IsFailingConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "cancelled", "action_required":
		return true
	}
	return false
}

// CIStatusFromCheckRuns derives a lowercase aggregate CI status from a list of
// check runs. Returns "failure" if any check failed, "pending" if any check is
// still running, "success" if all completed non-failing, or "" if the list is
// empty. Used by dashboard and display code for aggregate CI status badges.
//
// The success bucket is intentionally permissive — *any* completed check whose
// conclusion isn't in IsFailingConclusion counts as success-like. That covers
// "success", "neutral", "skipped", "stale" (post-rebase, treated as
// non-blocking by GitHub), empty conclusion on a completed check, and any
// future values GitHub adds to the enum. This matches how the aggregate
// counts in internal/github/status.go classify check runs, so the two paths
// can't drift out of sync.
func CIStatusFromCheckRuns(runs []CheckRun) string {
	if len(runs) == 0 {
		return ""
	}
	var hasFailure, hasPending, hasSuccess bool
	for _, r := range runs {
		if r.Status != "completed" {
			hasPending = true
			continue
		}
		if IsFailingConclusion(r.Conclusion) {
			hasFailure = true
			continue
		}
		// Completed and not failing — treat as success-like regardless of
		// the specific conclusion string.
		hasSuccess = true
	}
	switch {
	case hasFailure:
		return "failure"
	case hasPending:
		return "pending"
	case hasSuccess:
		return "success"
	}
	return ""
}

// DedupCheckRunsByName collapses multiple executions of the same check name
// down to the latest (highest ID). GitHub assigns check-run IDs monotonically
// at creation time, so the highest ID is always the most recent execution.
//
// Returns a deterministically-sorted slice (by Name ascending) so the
// serialized snapshot compares byte-stable across polls when the underlying
// data hasn't changed. Input nil or empty returns a non-nil empty slice so
// callers can distinguish "polled, no checks" from "unknown prior state".
func DedupCheckRunsByName(runs []CheckRun) []CheckRun {
	if len(runs) == 0 {
		return []CheckRun{}
	}
	byName := make(map[string]CheckRun, len(runs))
	for _, r := range runs {
		if existing, ok := byName[r.Name]; ok && existing.ID >= r.ID {
			continue
		}
		byName[r.Name] = r
	}
	out := make([]CheckRun, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// JiraSnapshot is the extracted state we diff against on each poll to emit
// per-action events. Keep it small — large bulk text (issue descriptions,
// PR bodies) lives on entities.description instead so diff reads don't
// drag it through every refresh cycle.
//
// The entity's source_id is the issue key and its external_id is ID, which is
// what the entity is matched on: a project move or a project key rename
// changes the key and leaves the id alone, and the entity is renamed to
// follow it. ID repeats the external id so a stored snapshot is
// self-describing.
type JiraSnapshot struct {
	// BodyHash covers the full description, including ADF links/formatting.
	// Empty means unknown rather than an explicitly cleared description.
	BodyHash string `json:"body_hash,omitempty"`
	// ID is Jira's numeric issue id. Empty on snapshots captured before it was
	// recorded.
	ID  string `json:"id,omitempty"`
	Key string `json:"key"`
	// ProjectID is the id of the project the issue is in. A project key rename
	// changes the key's prefix and leaves this alone, so it is what tells a
	// rename from a move. Empty on snapshots captured before it was recorded.
	ProjectID string `json:"project_id,omitempty"`
	// ProjectKey is that project's key as the same read spelled it: the
	// display value an event or a deck shows the project by. Nothing looks a
	// project up by it; the entity references the project by its registry row
	// (Entity.JiraProjectID). Empty on snapshots captured before it was
	// recorded.
	ProjectKey string `json:"project_key,omitempty"`
	Summary    string `json:"summary"`
	Status     string `json:"status"`
	// StatusID is Jira's identifier for the status Status names. It is the
	// stable half of the pair: a workflow status can be renamed, and matching
	// on the id is what keeps rule membership and change detection right when
	// it is. Empty on snapshots captured before the field existed, and on any
	// response without one — every comparison falls back to the name there.
	StatusID string `json:"status_id,omitempty"`
	Assignee string `json:"assignee"` // display name (UI surfaces)
	// AssigneeAccountID is the Atlassian stable identifier — accountId
	// on Cloud, legacy key on Server / DC. Captured alongside the
	// display name in issueToState so the assignee_in matcher has a
	// stable comparison target, and set on every snapshot of an assigned
	// issue a read produces. Empty on snapshots that predate the field.
	AssigneeAccountID string `json:"assignee_account_id,omitempty"`
	// Reporter and ReporterAccountID are the reporter's display name and
	// stable identifier, derived the way the assignee's are. Empty when the
	// issue has no reporter, and on snapshots that predate the field. A
	// reporter change emits nothing: it does not change whether the issue
	// needs attention.
	Reporter          string `json:"reporter,omitempty"`
	ReporterAccountID string `json:"reporter_account_id,omitempty"`
	Priority          string `json:"priority"`
	// PriorityID is Jira's id for the priority Priority names. An admin can
	// rename a priority, and the id is what tells that from the issue's
	// priority changing. Empty on snapshots that predate the field, where the
	// comparison falls back to the name.
	PriorityID string   `json:"priority_id,omitempty"`
	Labels     []string `json:"labels"`
	IssueType  string   `json:"issue_type"`
	// IssueTypeID is Jira's id for the issue type IssueType names. Empty on
	// snapshots that predate the field.
	IssueTypeID  string `json:"issue_type_id,omitempty"`
	ParentKey    string `json:"parent_key"`
	CommentCount int    `json:"comment_count"`
	// LastCommentID and LastCommentAt are the newest comment's, the marker
	// the next poll reads new comments from. Empty when the issue has no
	// comments, on snapshots that predate the fields, and while the newest
	// comment is past the page Jira returns with the issue and has not been
	// read: the next poll then counts its new comments off CommentCount.
	LastCommentID string `json:"last_comment_id,omitempty"`
	LastCommentAt string `json:"last_comment_at,omitempty"`
	URL           string `json:"url"`
	// CreatedAt is Jira's ISO-8601 timestamp for when the ticket was created
	// (fields.created). Used for newest-first ordering in carry-over. Empty
	// on snapshots that predate this field — callers should fall back to the
	// entity's TF-side created_at when sort-critical.
	CreatedAt string `json:"created_at,omitempty"`
	// UpdatedAt is Jira's last-modified timestamp on the issue (fields.updated).
	// Used by the diff layer as a fallback source time for events without a
	// per-action timestamp on the snapshot (status changes, assignment
	// changes, priority changes, new comments). Better than detection time
	// without firing a separate changelog API call. Empty on snapshots that
	// predate this field — diff falls back to detection time.
	UpdatedAt string `json:"updated_at,omitempty"`
	// OpenSubtaskCount is the number of this issue's child subtasks whose
	// status is NOT in the configured Done.Members set. Used to suppress
	// task creation for parent-of-subtasks tickets (the decomposition
	// exists for a reason — delegating the parent is almost always wrong)
	// and to fire jira:issue:became_atomic when an issue transitions from
	// "has open subtasks" to "none open".
	OpenSubtaskCount int `json:"open_subtask_count"`
}

// StatusRef pairs the snapshot's status name with its id, for comparison
// against a rule's members through JiraStatusRef.SameStatus.
func (s JiraSnapshot) StatusRef() JiraStatusRef {
	return JiraStatusRef{ID: s.StatusID, Name: s.Status}
}

// HasAssignee reports whether the issue is assigned. Either field counts, so a
// snapshot that predates the account id still reads as assigned.
func (s JiraSnapshot) HasAssignee() bool {
	return s.AssigneeAccountID != "" || s.Assignee != ""
}

// SameAssignee reports whether two snapshots name the same assignee, or are
// both unassigned. The account ids decide when both carry one, so a display
// name change is not a reassignment and two people who share a display name
// are not the same assignee. The display names decide only when a side
// predates the account id.
func (s JiraSnapshot) SameAssignee(other JiraSnapshot) bool {
	if s.HasAssignee() != other.HasAssignee() {
		return false
	}
	if s.AssigneeAccountID != "" && other.AssigneeAccountID != "" {
		return s.AssigneeAccountID == other.AssigneeAccountID
	}
	return s.Assignee == other.Assignee
}

// AssignedTo reports whether the issue is assigned to the Jira user whose
// stable id (jira.StableUserID) is accountID. An empty accountID matches
// nothing, so an unassigned issue is never anybody's.
func (s JiraSnapshot) AssignedTo(accountID string) bool {
	return accountID != "" && s.AssigneeAccountID == accountID
}

// SamePriority reports whether two snapshots carry the same priority. The ids
// decide when both carry one, so a priority renamed in Jira is not a change;
// otherwise the names do.
func (s JiraSnapshot) SamePriority(other JiraSnapshot) bool {
	if s.PriorityID != "" && other.PriorityID != "" {
		return s.PriorityID == other.PriorityID
	}
	return s.Priority == other.Priority
}

// LinearSnapshot is the extracted state of a Linear issue that each poll
// diffs against, the Linear sibling of JiraSnapshot. The entity's source_id is
// the issue's identifier ("ENG-123") and its external_id is the UUID, which is
// what the entity is matched on: a team move or a team key rename changes the
// identifier and leaves the UUID alone, and the entity is renamed to follow
// it. ID repeats the UUID so a stored snapshot is self-describing.
type LinearSnapshot struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	// BodyHash covers the raw description string. Empty means unknown rather
	// than an explicitly cleared description.
	BodyHash string         `json:"body_hash,omitempty"`
	State    LinearStateRef `json:"state"`
	Assignee string         `json:"assignee"` // display name
	// AssigneeUserID is the assignee's Linear user id, the same shape a
	// credential's viewer.id has, so it is what identity matching keys on.
	AssigneeUserID string `json:"assignee_user_id,omitempty"`
	// Creator and CreatorUserID are the display name and Linear user id of
	// whoever created the issue. Both are empty for an issue an integration
	// created, and on snapshots that predate the fields. Linear does not let
	// the creator change.
	Creator       string `json:"creator,omitempty"`
	CreatorUserID string `json:"creator_user_id,omitempty"`
	Priority      int    `json:"priority"` // 0 = no priority, 1 = urgent … 4 = low
	PriorityLabel string `json:"priority_label"`
	// Labels are the label names, sorted, which is what predicates and
	// body_updated read. LabelRefs are the same labels with their ids, which
	// the label diff compares on, so a label renamed in Linear is not a
	// removal and an addition. LabelRefs is empty on snapshots that predate
	// it; the diff compares those by name.
	Labels    []string         `json:"labels"`
	LabelRefs []LinearLabelRef `json:"label_refs,omitempty"`
	// TeamID is the Linear team's UUID, the unit a TF team tracks.
	TeamID           string `json:"team_id"`
	TeamKey          string `json:"team_key"`
	ParentID         string `json:"parent_id,omitempty"`
	ParentIdentifier string `json:"parent_identifier,omitempty"`
	// LastCommentID and LastCommentAt are the newest comment's. Linear has no
	// comment count on an issue, so a change in the newest comment is the
	// comment signal.
	LastCommentID string `json:"last_comment_id,omitempty"`
	LastCommentAt string `json:"last_comment_at,omitempty"`
	// OpenChildCount is the number of sub-issues whose state is not in the
	// configured done set.
	OpenChildCount int    `json:"open_child_count"`
	URL            string `json:"url"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	Archived       bool   `json:"archived,omitempty"`
	Trashed        bool   `json:"trashed,omitempty"`
}

// StateRef is the snapshot's workflow state, for comparison against a rule's
// members through LinearStateRef.SameState.
func (s LinearSnapshot) StateRef() LinearStateRef {
	return s.State
}

// LinearLabelRef is one label on a Linear issue: its id and its name. The name
// is the label's own, without the group it may sit in.
type LinearLabelRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// LinearStateRef is one Linear workflow state: its id, its display name, and
// its type (triage, backlog, unstarted, started, completed or canceled).
//
// It is a type of its own rather than a JiraStatusRef because it carries the
// type, which Jira has no analogue for and which the rule picker pre-arms
// from; a Linear value under a Jira type would read as interchangeable with
// one.
type LinearStateRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// IsZero reports whether the ref names nothing at all.
func (r LinearStateRef) IsZero() bool { return r.ID == "" && r.Name == "" }

// SameState reports whether two refs name the same state. Ids decide when
// both carry one; otherwise the names do.
func (r LinearStateRef) SameState(other LinearStateRef) bool {
	if r.ID != "" && other.ID != "" {
		return r.ID == other.ID
	}
	return r.Name != "" && r.Name == other.Name
}

// LinearStateDedupKey is the value a ref is deduplicated and set-compared on:
// the id when it has one, else the name under a prefix so a name can never
// collide with an id that happens to read like it.
func LinearStateDedupKey(r LinearStateRef) string {
	if r.ID != "" {
		return "id:" + r.ID
	}
	return "name:" + r.Name
}

// ContainsState reports whether refs holds one naming the same state as s.
func ContainsState(refs []LinearStateRef, s LinearStateRef) bool {
	return slices.ContainsFunc(refs, func(r LinearStateRef) bool { return r.SameState(s) })
}

// LinearStateIDs renders refs as their state ids, skipping any that carry
// none.
func LinearStateIDs(refs []LinearStateRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.ID != "" {
			out = append(out, r.ID)
		}
	}
	return out
}

// LinearStateMatchSets splits refs for a query that compares the way
// SameState does.
func LinearStateMatchSets(refs []LinearStateRef) StatusMatchSets {
	var out StatusMatchSets
	for _, r := range refs {
		switch {
		case r.ID != "":
			out.IDs = append(out.IDs, r.ID)
			if r.Name != "" {
				out.IDNames = append(out.IDNames, r.Name)
			}
		case r.Name != "":
			out.Names = append(out.Names, r.Name)
		}
	}
	return out
}

// LinearStateNames renders refs as their display names, skipping any that
// carry none.
func LinearStateNames(refs []LinearStateRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Name != "" {
			out = append(out, r.Name)
		}
	}
	return out
}

// MarshalLinearStateRefs renders a rule's members for storage. A nil slice
// renders as [] rather than null, so the stored value is always a JSON array.
func MarshalLinearStateRefs(refs []LinearStateRef) (string, error) {
	if refs == nil {
		refs = []LinearStateRef{}
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// MarshalLinearStateRef renders a rule's canonical for storage. The zero ref
// renders as "", which the stores turn into SQL NULL: an unset canonical.
func MarshalLinearStateRef(ref LinearStateRef) (string, error) {
	if ref.IsZero() {
		return "", nil
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// UnmarshalLinearStateRef reads a canonical back. An empty column is an unset
// canonical, not an error.
func UnmarshalLinearStateRef(raw string) (LinearStateRef, error) {
	if strings.TrimSpace(raw) == "" {
		return LinearStateRef{}, nil
	}
	var ref LinearStateRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		return LinearStateRef{}, err
	}
	return ref, nil
}

// UnmarshalLinearStateRefs reads a rule's members back. An empty column reads
// as no members.
func UnmarshalLinearStateRefs(raw string) ([]LinearStateRef, error) {
	if strings.TrimSpace(raw) == "" {
		return []LinearStateRef{}, nil
	}
	var refs []LinearStateRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil, err
	}
	if refs == nil {
		refs = []LinearStateRef{}
	}
	return refs, nil
}
