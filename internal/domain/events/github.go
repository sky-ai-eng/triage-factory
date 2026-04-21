package events

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// GitHub PR event schemas.
//
// Every PR event carries:
//
//   - the actor-identity pattern (Author + AuthorIsSelf) so predicates can
//     default to ergonomic self-checks while still supporting exact-match on
//     login;
//   - Labels []string — snapshot at emission time, so predicates can scope
//     to labeled PRs (e.g. HasLabel "self-review"). This is the user opt-in
//     surface for label-driven flows (see Scenario 5 in docs/data-model-
//     target.md). The snapshot is what was on the PR when the event was
//     observed, not what "caused" the event;
//   - Repo + PRNumber for scoping;
//   - IsDraft + HeadSHA for display / context.
//
// Per-event extras (reviewer identity, check run ID, label name, etc.) live
// only on the events that need them. Structs are intentionally duplicated
// rather than composed so that evolving one event type (adding a CI run URL,
// say) never accidentally changes the schema of another.

// -----------------------------------------------------------------------------
// review_requested — "someone asked me to review their PR"
// -----------------------------------------------------------------------------

// GitHubPRReviewRequestedMetadata is emitted when a reviewer is added to a PR.
// This event type is scoped to "someone requested my review" — the inverse
// (`review_submitted`) is for reviews *I* made on others' PRs.
type GitHubPRReviewRequestedMetadata struct {
	Author       string   `json:"author"`         // PR author login
	AuthorIsSelf bool     `json:"author_is_self"` // usually false — you rarely request reviews from yourself
	Repo         string   `json:"repo"`           // "owner/name"
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"` // snapshot at emission time
	Title        string   `json:"title"`
}

type GitHubPRReviewRequestedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty" doc:"Match PRs authored by you."`
	Author       *string `json:"author,omitempty" doc:"Exact-match on PR author login (e.g. 'dependabot[bot]')."`
	Repo         *string `json:"repo,omitempty" doc:"Scope to a specific repo (owner/name)."`
	IsDraft      *bool   `json:"is_draft,omitempty" doc:"Match draft vs. ready-for-review PRs."`
	HasLabel     *string `json:"has_label,omitempty" doc:"Require the PR to currently bear this label."`
}

func (p GitHubPRReviewRequestedPredicate) Matches(m GitHubPRReviewRequestedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// review_submitted — "I reviewed someone else's PR"
// -----------------------------------------------------------------------------

// GitHubPRReviewSubmittedMetadata is emitted when the session user posts a
// review on someone else's PR. The ReviewType is carried in metadata (not
// split into separate event types) because this event is historical — the
// review already happened, so we don't fan out per type.
type GitHubPRReviewSubmittedMetadata struct {
	Author         string   `json:"author"`
	AuthorIsSelf   bool     `json:"author_is_self"`   // usually false
	ReviewerIsSelf bool     `json:"reviewer_is_self"` // always true for this event type
	Reviewer       string   `json:"reviewer"`         // session user login
	ReviewType     string   `json:"review_type"`      // "approved" | "commented" | "changes_requested" | "dismissed"
	ReviewID       int64    `json:"review_id"`
	Repo           string   `json:"repo"`
	PRNumber       int      `json:"pr_number"`
	IsDraft        bool     `json:"is_draft"`
	HeadSHA        string   `json:"head_sha"`
	Labels         []string `json:"labels"`
}

type GitHubPRReviewSubmittedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty" doc:"Match PRs authored by you (e.g. for self-review flows)."`
	Author       *string `json:"author,omitempty"`
	Repo         *string `json:"repo,omitempty"`
	ReviewType   *string `json:"review_type,omitempty" enum:"approved,commented,changes_requested,dismissed" doc:"Filter by the review type you submitted."`
	IsDraft      *bool   `json:"is_draft,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRReviewSubmittedPredicate) Matches(m GitHubPRReviewSubmittedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.Repo, m.Repo) &&
		strEq(p.ReviewType, m.ReviewType) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// review_changes_requested / review_approved / review_commented / review_dismissed
// — split-on-review-type: one event per situation-changing review type.
// Metadata struct is duplicated across the four so each can evolve
// independently.
// -----------------------------------------------------------------------------

type GitHubPRReviewChangesRequestedMetadata struct {
	Author         string   `json:"author"`
	AuthorIsSelf   bool     `json:"author_is_self"`
	Reviewer       string   `json:"reviewer"`
	ReviewerIsSelf bool     `json:"reviewer_is_self"`
	ReviewID       int64    `json:"review_id"`
	Repo           string   `json:"repo"`
	PRNumber       int      `json:"pr_number"`
	IsDraft        bool     `json:"is_draft"`
	HeadSHA        string   `json:"head_sha"`
	Labels         []string `json:"labels"`
}

type GitHubPRReviewChangesRequestedPredicate struct {
	AuthorIsSelf   *bool   `json:"author_is_self,omitempty"`
	Author         *string `json:"author,omitempty"`
	ReviewerIsSelf *bool   `json:"reviewer_is_self,omitempty" doc:"Useful for self-review flows (reviewer_is_self + author_is_self + has_label)."`
	Reviewer       *string `json:"reviewer,omitempty" doc:"Exact-match on reviewer login."`
	Repo           *string `json:"repo,omitempty"`
	IsDraft        *bool   `json:"is_draft,omitempty"`
	HasLabel       *string `json:"has_label,omitempty"`
}

func (p GitHubPRReviewChangesRequestedPredicate) Matches(m GitHubPRReviewChangesRequestedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		boolEq(p.ReviewerIsSelf, m.ReviewerIsSelf) &&
		strEq(p.Reviewer, m.Reviewer) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRReviewApprovedMetadata struct {
	Author         string   `json:"author"`
	AuthorIsSelf   bool     `json:"author_is_self"`
	Reviewer       string   `json:"reviewer"`
	ReviewerIsSelf bool     `json:"reviewer_is_self"`
	ReviewID       int64    `json:"review_id"`
	Repo           string   `json:"repo"`
	PRNumber       int      `json:"pr_number"`
	IsDraft        bool     `json:"is_draft"`
	HeadSHA        string   `json:"head_sha"`
	Labels         []string `json:"labels"`
}

type GitHubPRReviewApprovedPredicate struct {
	AuthorIsSelf   *bool   `json:"author_is_self,omitempty"`
	Author         *string `json:"author,omitempty"`
	ReviewerIsSelf *bool   `json:"reviewer_is_self,omitempty"`
	Reviewer       *string `json:"reviewer,omitempty"`
	Repo           *string `json:"repo,omitempty"`
	IsDraft        *bool   `json:"is_draft,omitempty"`
	HasLabel       *string `json:"has_label,omitempty"`
}

func (p GitHubPRReviewApprovedPredicate) Matches(m GitHubPRReviewApprovedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		boolEq(p.ReviewerIsSelf, m.ReviewerIsSelf) &&
		strEq(p.Reviewer, m.Reviewer) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRReviewCommentedMetadata struct {
	Author         string   `json:"author"`
	AuthorIsSelf   bool     `json:"author_is_self"`
	Reviewer       string   `json:"reviewer"`
	ReviewerIsSelf bool     `json:"reviewer_is_self"`
	ReviewID       int64    `json:"review_id"`
	CommentCount   int      `json:"comment_count"`
	Repo           string   `json:"repo"`
	PRNumber       int      `json:"pr_number"`
	IsDraft        bool     `json:"is_draft"`
	HeadSHA        string   `json:"head_sha"`
	Labels         []string `json:"labels"`
}

type GitHubPRReviewCommentedPredicate struct {
	AuthorIsSelf   *bool   `json:"author_is_self,omitempty"`
	Author         *string `json:"author,omitempty"`
	ReviewerIsSelf *bool   `json:"reviewer_is_self,omitempty"`
	Reviewer       *string `json:"reviewer,omitempty"`
	Repo           *string `json:"repo,omitempty"`
	IsDraft        *bool   `json:"is_draft,omitempty"`
	HasLabel       *string `json:"has_label,omitempty"`
}

func (p GitHubPRReviewCommentedPredicate) Matches(m GitHubPRReviewCommentedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		boolEq(p.ReviewerIsSelf, m.ReviewerIsSelf) &&
		strEq(p.Reviewer, m.Reviewer) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRReviewDismissedMetadata struct {
	Author         string   `json:"author"`
	AuthorIsSelf   bool     `json:"author_is_self"`
	Reviewer       string   `json:"reviewer"`
	ReviewerIsSelf bool     `json:"reviewer_is_self"`
	ReviewID       int64    `json:"review_id"`
	Repo           string   `json:"repo"`
	PRNumber       int      `json:"pr_number"`
	IsDraft        bool     `json:"is_draft"`
	HeadSHA        string   `json:"head_sha"`
	Labels         []string `json:"labels"`
}

type GitHubPRReviewDismissedPredicate struct {
	AuthorIsSelf   *bool   `json:"author_is_self,omitempty"`
	Author         *string `json:"author,omitempty"`
	ReviewerIsSelf *bool   `json:"reviewer_is_self,omitempty"`
	Reviewer       *string `json:"reviewer,omitempty"`
	Repo           *string `json:"repo,omitempty"`
	IsDraft        *bool   `json:"is_draft,omitempty"`
	HasLabel       *string `json:"has_label,omitempty"`
}

func (p GitHubPRReviewDismissedPredicate) Matches(m GitHubPRReviewDismissedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		boolEq(p.ReviewerIsSelf, m.ReviewerIsSelf) &&
		strEq(p.Reviewer, m.Reviewer) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// ci_check_failed / ci_check_passed — split on conclusion; check-run IDs per
// event.
// -----------------------------------------------------------------------------

type GitHubPRCICheckFailedMetadata struct {
	Author       string `json:"author"`
	AuthorIsSelf bool   `json:"author_is_self"`
	CheckRunID   int64  `json:"check_run_id"`
	CheckName    string `json:"check_name"`
	CheckURL     string `json:"check_url"`
	// WorkflowRunID is the GitHub Actions workflow run database ID this
	// check belongs to, parsed from the check's DetailsURL in the tracker.
	// Zero for third-party CI (Supabase, Circle, etc.) where the URL
	// doesn't carry an Actions run — callers fall back to the list-runs
	// subcommand to discover a matching run by PR / SHA.
	WorkflowRunID int64    `json:"workflow_run_id,omitempty"`
	HeadSHA       string   `json:"head_sha"`
	Repo          string   `json:"repo"`
	PRNumber      int      `json:"pr_number"`
	IsDraft       bool     `json:"is_draft"`
	Labels        []string `json:"labels"`
}

type GitHubPRCICheckFailedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	CheckName    *string `json:"check_name,omitempty" doc:"Exact-match on the failing check name (e.g. 'test', 'build')."`
	Repo         *string `json:"repo,omitempty"`
	IsDraft      *bool   `json:"is_draft,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRCICheckFailedPredicate) Matches(m GitHubPRCICheckFailedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.CheckName, m.CheckName) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRCICheckPassedMetadata struct {
	Author        string   `json:"author"`
	AuthorIsSelf  bool     `json:"author_is_self"`
	CheckRunID    int64    `json:"check_run_id"`
	CheckName     string   `json:"check_name"`
	WorkflowRunID int64    `json:"workflow_run_id,omitempty"`
	Conclusion    string   `json:"conclusion"` // "success", "neutral", "skipped", "stale", etc.
	HeadSHA       string   `json:"head_sha"`
	Repo          string   `json:"repo"`
	PRNumber      int      `json:"pr_number"`
	IsDraft       bool     `json:"is_draft"`
	Labels        []string `json:"labels"`
}

type GitHubPRCICheckPassedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	CheckName    *string `json:"check_name,omitempty"`
	Conclusion   *string `json:"conclusion,omitempty" doc:"Filter by specific non-failing conclusion (success, neutral, skipped, stale)."`
	Repo         *string `json:"repo,omitempty"`
	IsDraft      *bool   `json:"is_draft,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRCICheckPassedPredicate) Matches(m GitHubPRCICheckPassedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.CheckName, m.CheckName) &&
		strEq(p.Conclusion, m.Conclusion) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// label_added / label_removed — open-set discriminator; the specific label
// lives in metadata.LabelName AND is mirrored into events.dedup_key at
// emission time (so two tasks for the same PR with different labels don't
// collide). `HasLabel` on these events is usually redundant (the label name
// itself is the primary filter), but it's exposed for consistency with other
// PR predicates.
// -----------------------------------------------------------------------------

type GitHubPRLabelAddedMetadata struct {
	Author       string   `json:"author"`
	AuthorIsSelf bool     `json:"author_is_self"`
	LabelName    string   `json:"label_name"` // the specific label added (also the event's dedup_key)
	Repo         string   `json:"repo"`
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"` // snapshot *after* the add
}

type GitHubPRLabelAddedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	LabelName    *string `json:"label_name,omitempty" doc:"Match only this specific label (e.g. 'self-review', 'urgent')."`
	Repo         *string `json:"repo,omitempty"`
	IsDraft      *bool   `json:"is_draft,omitempty"`
	HasLabel     *string `json:"has_label,omitempty" doc:"Require another label to also be present on the PR."`
}

func (p GitHubPRLabelAddedPredicate) Matches(m GitHubPRLabelAddedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.LabelName, m.LabelName) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRLabelRemovedMetadata struct {
	Author       string   `json:"author"`
	AuthorIsSelf bool     `json:"author_is_self"`
	LabelName    string   `json:"label_name"`
	Repo         string   `json:"repo"`
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"` // snapshot *after* the removal
}

type GitHubPRLabelRemovedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	LabelName    *string `json:"label_name,omitempty"`
	Repo         *string `json:"repo,omitempty"`
	IsDraft      *bool   `json:"is_draft,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRLabelRemovedPredicate) Matches(m GitHubPRLabelRemovedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.LabelName, m.LabelName) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// new_commits — PR HEAD advanced since the last poll.
// -----------------------------------------------------------------------------

type GitHubPRNewCommitsMetadata struct {
	Author       string   `json:"author"`
	AuthorIsSelf bool     `json:"author_is_self"`
	IsDraft      bool     `json:"is_draft"`
	CommitCount  int      `json:"commit_count"`
	HeadSHA      string   `json:"head_sha"`
	PrevHeadSHA  string   `json:"prev_head_sha"`
	Repo         string   `json:"repo"`
	PRNumber     int      `json:"pr_number"`
	Labels       []string `json:"labels"`
}

type GitHubPRNewCommitsPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	IsDraft      *bool   `json:"is_draft,omitempty"`
	Repo         *string `json:"repo,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRNewCommitsPredicate) Matches(m GitHubPRNewCommitsMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		strEq(p.Repo, m.Repo) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// conflicts — PR has merge conflicts with its base.
// -----------------------------------------------------------------------------

type GitHubPRConflictsMetadata struct {
	Author       string   `json:"author"`
	AuthorIsSelf bool     `json:"author_is_self"`
	Repo         string   `json:"repo"`
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRConflictsPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	Repo         *string `json:"repo,omitempty"`
	IsDraft      *bool   `json:"is_draft,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRConflictsPredicate) Matches(m GitHubPRConflictsMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// ready_for_review — draft → ready transition.
// -----------------------------------------------------------------------------

type GitHubPRReadyForReviewMetadata struct {
	Author       string   `json:"author"`
	AuthorIsSelf bool     `json:"author_is_self"`
	Repo         string   `json:"repo"`
	PRNumber     int      `json:"pr_number"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRReadyForReviewPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	Repo         *string `json:"repo,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRReadyForReviewPredicate) Matches(m GitHubPRReadyForReviewMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.Repo, m.Repo) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// opened — new PR first seen.
// -----------------------------------------------------------------------------

type GitHubPROpenedMetadata struct {
	Author       string   `json:"author"`
	AuthorIsSelf bool     `json:"author_is_self"`
	Repo         string   `json:"repo"`
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Title        string   `json:"title"`
	Labels       []string `json:"labels"`
}

type GitHubPROpenedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	Repo         *string `json:"repo,omitempty"`
	IsDraft      *bool   `json:"is_draft,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPROpenedPredicate) Matches(m GitHubPROpenedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// merged / closed — entity-terminating events. Carried through routing for
// audit, but the entity lifecycle state machine is what actually closes
// downstream tasks.
// -----------------------------------------------------------------------------

type GitHubPRMergedMetadata struct {
	Author       string   `json:"author"`
	AuthorIsSelf bool     `json:"author_is_self"`
	Repo         string   `json:"repo"`
	PRNumber     int      `json:"pr_number"`
	MergedBy     string   `json:"merged_by"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRMergedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	Repo         *string `json:"repo,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRMergedPredicate) Matches(m GitHubPRMergedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.Repo, m.Repo) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRClosedMetadata struct {
	Author       string   `json:"author"`
	AuthorIsSelf bool     `json:"author_is_self"`
	Repo         string   `json:"repo"`
	PRNumber     int      `json:"pr_number"`
	ClosedBy     string   `json:"closed_by"`
	Labels       []string `json:"labels"`
}

type GitHubPRClosedPredicate struct {
	AuthorIsSelf *bool   `json:"author_is_self,omitempty"`
	Author       *string `json:"author,omitempty"`
	Repo         *string `json:"repo,omitempty"`
	HasLabel     *string `json:"has_label,omitempty"`
}

func (p GitHubPRClosedPredicate) Matches(m GitHubPRClosedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		strEq(p.Repo, m.Repo) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// mentioned — you were @mentioned in a PR comment.
// -----------------------------------------------------------------------------

type GitHubPRMentionedMetadata struct {
	Author          string   `json:"author"`
	AuthorIsSelf    bool     `json:"author_is_self"`
	Commenter       string   `json:"commenter"`
	CommenterIsSelf bool     `json:"commenter_is_self"` // usually false — we don't alert on self-mentions
	CommentID       int64    `json:"comment_id"`
	Repo            string   `json:"repo"`
	PRNumber        int      `json:"pr_number"`
	IsDraft         bool     `json:"is_draft"`
	HeadSHA         string   `json:"head_sha"`
	Labels          []string `json:"labels"`
}

type GitHubPRMentionedPredicate struct {
	AuthorIsSelf    *bool   `json:"author_is_self,omitempty"`
	Author          *string `json:"author,omitempty"`
	CommenterIsSelf *bool   `json:"commenter_is_self,omitempty"`
	Commenter       *string `json:"commenter,omitempty"`
	Repo            *string `json:"repo,omitempty"`
	IsDraft         *bool   `json:"is_draft,omitempty"`
	HasLabel        *string `json:"has_label,omitempty"`
}

func (p GitHubPRMentionedPredicate) Matches(m GitHubPRMentionedMetadata) bool {
	return boolEq(p.AuthorIsSelf, m.AuthorIsSelf) &&
		strEq(p.Author, m.Author) &&
		boolEq(p.CommenterIsSelf, m.CommenterIsSelf) &&
		strEq(p.Commenter, m.Commenter) &&
		strEq(p.Repo, m.Repo) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// Registration.
// -----------------------------------------------------------------------------

func init() {
	Register(newSchema[GitHubPRReviewRequestedMetadata, GitHubPRReviewRequestedPredicate](domain.EventGitHubPRReviewRequested))
	Register(newSchema[GitHubPRReviewSubmittedMetadata, GitHubPRReviewSubmittedPredicate](domain.EventGitHubPRReviewSubmitted))

	Register(newSchema[GitHubPRReviewChangesRequestedMetadata, GitHubPRReviewChangesRequestedPredicate](domain.EventGitHubPRReviewChangesRequested))
	Register(newSchema[GitHubPRReviewApprovedMetadata, GitHubPRReviewApprovedPredicate](domain.EventGitHubPRReviewApproved))
	Register(newSchema[GitHubPRReviewCommentedMetadata, GitHubPRReviewCommentedPredicate](domain.EventGitHubPRReviewCommented))
	Register(newSchema[GitHubPRReviewDismissedMetadata, GitHubPRReviewDismissedPredicate](domain.EventGitHubPRReviewDismissed))

	Register(newSchema[GitHubPRCICheckFailedMetadata, GitHubPRCICheckFailedPredicate](domain.EventGitHubPRCICheckFailed))
	Register(newSchema[GitHubPRCICheckPassedMetadata, GitHubPRCICheckPassedPredicate](domain.EventGitHubPRCICheckPassed))

	Register(newSchema[GitHubPRLabelAddedMetadata, GitHubPRLabelAddedPredicate](domain.EventGitHubPRLabelAdded))
	Register(newSchema[GitHubPRLabelRemovedMetadata, GitHubPRLabelRemovedPredicate](domain.EventGitHubPRLabelRemoved))

	Register(newSchema[GitHubPRNewCommitsMetadata, GitHubPRNewCommitsPredicate](domain.EventGitHubPRNewCommits))
	Register(newSchema[GitHubPRConflictsMetadata, GitHubPRConflictsPredicate](domain.EventGitHubPRConflicts))
	Register(newSchema[GitHubPRReadyForReviewMetadata, GitHubPRReadyForReviewPredicate](domain.EventGitHubPRReadyForReview))
	Register(newSchema[GitHubPROpenedMetadata, GitHubPROpenedPredicate](domain.EventGitHubPROpened))
	Register(newSchema[GitHubPRMergedMetadata, GitHubPRMergedPredicate](domain.EventGitHubPRMerged))
	Register(newSchema[GitHubPRClosedMetadata, GitHubPRClosedPredicate](domain.EventGitHubPRClosed))
	Register(newSchema[GitHubPRMentionedMetadata, GitHubPRMentionedPredicate](domain.EventGitHubPRMentioned))
}
