package events

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// GitHub PR event schemas.
//
// Every PR event carries:
//
//   - the actor's GitHub login (Author, Reviewer, Commenter, MergedBy,
//     ClosedBy) and, beside it, GitHub's id for the actor's account
//     (AuthorID, ReviewerID, CommenterID, MergedByID, ClosedByID), which a
//     rename leaves alone. The predicates' identity filters (author_id_in,
//     reviewer_id_in, commenter_id_in, requested_user_id_in,
//     requested_team_id_in) match the ids; the login is for display. The id
//     is empty on an event recorded before ids were, and for an actor GitHub
//     returned without one (a deleted account), so an identity filter never
//     matches such an event;
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
// This event type is scoped to "someone requested my review"; the act of
// reviewing surfaces as the typed review_* events (approved / commented /
// changes_requested / dismissed), which also close this request.
//
// The open-set discriminator for this event is the *requested reviewer* —
// the tracker emits one event per newly-requested TF-known identity, with the
// event's dedup_key namespacing it by GitHub's id ("user_id:<id>" /
// "team_id:<id>"), or by handle ("user:<login>" / "team:<org>/<slug>") for a
// request read without one (see ReviewerDedupKeyUserID). Exactly one of
// RequestedLogin / RequestedTeam is set per event, with its id beside it when
// known; the router routes the task's visibility to that identity's team(s),
// by the id when there is one. AuthorID still carries the PR author for
// predicate matching.
type GitHubPRReviewRequestedMetadata struct {
	Author       string   `json:"author"`              // PR author login
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Repo         string   `json:"repo"`                // "owner/name"
	RepositoryID string   `json:"repository_id"`       // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"` // snapshot at emission time
	Title        string   `json:"title"`
	// RequestedLogin is the requested reviewer's GitHub login when the
	// request named an individual user; empty for github-team requests.
	RequestedLogin string `json:"requested_login,omitempty"`
	// RequestedTeam is the requested github team in "org/slug" form when
	// the request named a team; empty for individual-user requests.
	RequestedTeam string `json:"requested_team,omitempty"`
	// RequestedUserID / RequestedTeamID are GitHub's ids for the requested
	// user or team, beside the handle above. Empty on an event recorded
	// before ids were, or for a request GitHub returned without one.
	RequestedUserID string `json:"requested_user_id,omitempty"`
	RequestedTeamID string `json:"requested_team_id,omitempty"`
}

type GitHubPRReviewRequestedPredicate struct {
	AuthorIDIn        []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	RepoIn            []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft           *bool    `json:"is_draft,omitempty" doc:"Match draft vs. ready-for-review PRs."`
	HasLabel          *string  `json:"has_label,omitempty" doc:"Require the PR to currently bear this label."`
	RequestedUserIDIn []string `json:"requested_user_id_in,omitempty" doc:"Match only when one of these GitHub accounts was the requested reviewer, each by GitHub's account id as author_id_in takes it, not its login."`
	RequestedTeamIDIn []string `json:"requested_team_id_in,omitempty" doc:"Match only when one of these GitHub teams was the requested reviewer, each by GitHub's team id (the github_team_id of one of the team's GET /api/teams/{team_id}/github-groups mappings), not org/slug."`
}

func (p GitHubPRReviewRequestedPredicate) Matches(m GitHubPRReviewRequestedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels) &&
		idIn(p.RequestedUserIDIn, m.RequestedUserID) &&
		idIn(p.RequestedTeamIDIn, m.RequestedTeamID)
}

// -----------------------------------------------------------------------------
// review_request_removed — "my review request was removed"
// -----------------------------------------------------------------------------

// GitHubPRReviewRequestRemovedMetadata mirrors the requested side: the
// per-identity removal carries the same RequestedLogin / RequestedTeam so
// the router can close the one task keyed to that reviewer rather than every
// review_requested task on the entity.
type GitHubPRReviewRequestRemovedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
	Title        string   `json:"title"`
	// RequestedLogin / RequestedTeam identify which requested reviewer was
	// dropped, and RequestedUserID / RequestedTeamID carry GitHub's id for
	// it. A removal names the reviewer the way the review_requested task it
	// reconciles against was keyed: an id-keyed task's removal can carry
	// the id alone.
	RequestedLogin  string `json:"requested_login,omitempty"`
	RequestedTeam   string `json:"requested_team,omitempty"`
	RequestedUserID string `json:"requested_user_id,omitempty"`
	RequestedTeamID string `json:"requested_team_id,omitempty"`
}

type GitHubPRReviewRequestRemovedPredicate struct {
	AuthorIDIn        []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	RepoIn            []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft           *bool    `json:"is_draft,omitempty" doc:"Match draft vs. ready-for-review PRs."`
	HasLabel          *string  `json:"has_label,omitempty" doc:"Require the PR to currently bear this label."`
	RequestedUserIDIn []string `json:"requested_user_id_in,omitempty" doc:"Match only when the removed request named one of these GitHub accounts, each by GitHub's account id as author_id_in takes it, not its login."`
	RequestedTeamIDIn []string `json:"requested_team_id_in,omitempty" doc:"Match only when the removed request named one of these GitHub teams, each by GitHub's team id (the github_team_id of one of the team's GET /api/teams/{team_id}/github-groups mappings), not org/slug."`
}

func (p GitHubPRReviewRequestRemovedPredicate) Matches(m GitHubPRReviewRequestRemovedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels) &&
		idIn(p.RequestedUserIDIn, m.RequestedUserID) &&
		idIn(p.RequestedTeamIDIn, m.RequestedTeamID)
}

// ReviewerDedupKeyUserID / ReviewerDedupKeyTeamID namespace a requested
// reviewer's GitHub id into the review_requested / review_request_removed
// event's dedup_key: the key for a reviewer GitHub returned with an id, which
// a rename leaves alone. ReviewerDedupKeyUser / ReviewerDedupKeyTeam namespace
// a handle instead (a login, an "org/slug"): the key for a reviewer read
// without an id, and the one every mention event and every task created
// before ids were recorded carries. The open-set discriminator is the
// requested reviewer; namespacing keeps a login, a team handle and the two
// kinds of id from colliding (a login may be all digits, so an id has a
// prefix of its own) and lets the router close the one task keyed to a given
// reviewer. Shared so the tracker (which emits the keyed events) and the
// router (which closes by key on review submit / request removal) can't drift
// on the format.
func ReviewerDedupKeyUser(login string) string   { return "user:" + login }
func ReviewerDedupKeyTeam(orgSlug string) string { return "team:" + orgSlug }
func ReviewerDedupKeyUserID(id string) string    { return "user_id:" + id }
func ReviewerDedupKeyTeamID(id string) string    { return "team_id:" + id }

// -----------------------------------------------------------------------------
// review_changes_requested / review_approved / review_commented / review_dismissed
// — split-on-review-type: one event per situation-changing review type.
// Metadata struct is duplicated across the four so each can evolve
// independently.
//
// Every review event names one review: ReviewID is its numeric id,
// CommentCount how many inline comments it holds, and Paths the distinct
// files those comments are on, in the order the comments name them. A
// dismissal, and an event diffed against a snapshot without the review
// marker, names a reviewer's latest review, whose files are not read, so its
// Paths is empty.
// -----------------------------------------------------------------------------

type GitHubPRReviewChangesRequestedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Reviewer     string   `json:"reviewer"`
	ReviewerID   string   `json:"reviewer_id,omitempty"` // GitHub's id for the reviewer's account
	ReviewID     int64    `json:"review_id"`
	CommentCount int      `json:"comment_count"`
	Paths        []string `json:"paths"`
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRReviewChangesRequestedPredicate struct {
	AuthorIDIn   []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	ReviewerIDIn []string `json:"reviewer_id_in,omitempty" doc:"Match reviews submitted by any of these GitHub accounts, each by GitHub's account id as author_id_in takes it, not its login. Empty matches every reviewer."`
	RepoIn       []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft      *bool    `json:"is_draft,omitempty"`
	HasLabel     *string  `json:"has_label,omitempty"`
}

func (p GitHubPRReviewChangesRequestedPredicate) Matches(m GitHubPRReviewChangesRequestedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.ReviewerIDIn, m.ReviewerID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRReviewApprovedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Reviewer     string   `json:"reviewer"`
	ReviewerID   string   `json:"reviewer_id,omitempty"` // GitHub's id for the reviewer's account
	ReviewID     int64    `json:"review_id"`
	CommentCount int      `json:"comment_count"`
	Paths        []string `json:"paths"`
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRReviewApprovedPredicate struct {
	AuthorIDIn   []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	ReviewerIDIn []string `json:"reviewer_id_in,omitempty" doc:"Match reviews submitted by any of these GitHub accounts, each by GitHub's account id as author_id_in takes it, not its login. Empty matches every reviewer."`
	RepoIn       []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft      *bool    `json:"is_draft,omitempty"`
	HasLabel     *string  `json:"has_label,omitempty"`
}

func (p GitHubPRReviewApprovedPredicate) Matches(m GitHubPRReviewApprovedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.ReviewerIDIn, m.ReviewerID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRReviewCommentedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Reviewer     string   `json:"reviewer"`
	ReviewerID   string   `json:"reviewer_id,omitempty"` // GitHub's id for the reviewer's account
	ReviewID     int64    `json:"review_id"`
	CommentCount int      `json:"comment_count"`
	Paths        []string `json:"paths"`
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRReviewCommentedPredicate struct {
	AuthorIDIn   []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	ReviewerIDIn []string `json:"reviewer_id_in,omitempty" doc:"Match reviews submitted by any of these GitHub accounts, each by GitHub's account id as author_id_in takes it, not its login. Empty matches every reviewer."`
	RepoIn       []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft      *bool    `json:"is_draft,omitempty"`
	HasLabel     *string  `json:"has_label,omitempty"`
}

func (p GitHubPRReviewCommentedPredicate) Matches(m GitHubPRReviewCommentedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.ReviewerIDIn, m.ReviewerID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRReviewDismissedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Reviewer     string   `json:"reviewer"`
	ReviewerID   string   `json:"reviewer_id,omitempty"` // GitHub's id for the reviewer's account
	ReviewID     int64    `json:"review_id"`
	CommentCount int      `json:"comment_count"`
	Paths        []string `json:"paths"`
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRReviewDismissedPredicate struct {
	AuthorIDIn   []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	ReviewerIDIn []string `json:"reviewer_id_in,omitempty" doc:"Match reviews submitted by any of these GitHub accounts, each by GitHub's account id as author_id_in takes it, not its login. Empty matches every reviewer."`
	RepoIn       []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft      *bool    `json:"is_draft,omitempty"`
	HasLabel     *string  `json:"has_label,omitempty"`
}

func (p GitHubPRReviewDismissedPredicate) Matches(m GitHubPRReviewDismissedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.ReviewerIDIn, m.ReviewerID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// ci_check_failed / ci_check_passed — split on conclusion; check-run IDs per
// event.
// -----------------------------------------------------------------------------

type GitHubPRCICheckFailedMetadata struct {
	Author     string `json:"author"`
	AuthorID   string `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	CheckRunID int64  `json:"check_run_id"`
	CheckName  string `json:"check_name"`
	CheckURL   string `json:"check_url"`
	// WorkflowRunID is the GitHub Actions workflow run database ID this
	// check belongs to, parsed from the check's DetailsURL in the tracker.
	// Zero for third-party CI (Supabase, Circle, etc.) where the URL
	// doesn't carry an Actions run — callers fall back to the list-runs
	// subcommand to discover a matching run by PR / SHA.
	WorkflowRunID int64    `json:"workflow_run_id,omitempty"`
	HeadSHA       string   `json:"head_sha"`
	Repo          string   `json:"repo"`
	RepositoryID  string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber      int      `json:"pr_number"`
	IsDraft       bool     `json:"is_draft"`
	Labels        []string `json:"labels"`
}

type GitHubPRCICheckFailedPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	CheckName  *string  `json:"check_name,omitempty" doc:"Exact-match on the failing check name (e.g. 'test', 'build')."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft    *bool    `json:"is_draft,omitempty"`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRCICheckFailedPredicate) Matches(m GitHubPRCICheckFailedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		strEq(p.CheckName, m.CheckName) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRCICheckPassedMetadata struct {
	Author        string   `json:"author"`
	AuthorID      string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	CheckRunID    int64    `json:"check_run_id"`
	CheckName     string   `json:"check_name"`
	WorkflowRunID int64    `json:"workflow_run_id,omitempty"`
	Conclusion    string   `json:"conclusion"` // "success", "neutral", "skipped", "stale", etc.
	HeadSHA       string   `json:"head_sha"`
	Repo          string   `json:"repo"`
	RepositoryID  string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber      int      `json:"pr_number"`
	IsDraft       bool     `json:"is_draft"`
	Labels        []string `json:"labels"`
}

type GitHubPRCICheckPassedPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	CheckName  *string  `json:"check_name,omitempty"`
	Conclusion *string  `json:"conclusion,omitempty" doc:"Filter by specific non-failing conclusion (success, neutral, skipped, stale)."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft    *bool    `json:"is_draft,omitempty"`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRCICheckPassedPredicate) Matches(m GitHubPRCICheckPassedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		strEq(p.CheckName, m.CheckName) &&
		strEq(p.Conclusion, m.Conclusion) &&
		idIn(p.RepoIn, m.RepositoryID) &&
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
//
// label_name and has_label, here and on every PR event, are names, not ids: a
// GitHub label's id belongs to one repository, while a filter on a label means
// the label of that name in every repository the team tracks.
// -----------------------------------------------------------------------------

type GitHubPRLabelAddedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	LabelName    string   `json:"label_name"`          // the specific label added (also the event's dedup_key)
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"` // snapshot *after* the add
}

type GitHubPRLabelAddedPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	LabelName  *string  `json:"label_name,omitempty" doc:"Match only this specific label (e.g. 'self-review', 'urgent')."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft    *bool    `json:"is_draft,omitempty"`
	HasLabel   *string  `json:"has_label,omitempty" doc:"Require another label to also be present on the PR."`
}

func (p GitHubPRLabelAddedPredicate) Matches(m GitHubPRLabelAddedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		strEq(p.LabelName, m.LabelName) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRLabelRemovedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	LabelName    string   `json:"label_name"`
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"` // snapshot *after* the removal
}

type GitHubPRLabelRemovedPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	LabelName  *string  `json:"label_name,omitempty"`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft    *bool    `json:"is_draft,omitempty"`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRLabelRemovedPredicate) Matches(m GitHubPRLabelRemovedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		strEq(p.LabelName, m.LabelName) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// new_commits — PR HEAD advanced since the last poll.
// -----------------------------------------------------------------------------

type GitHubPRNewCommitsMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	IsDraft      bool     `json:"is_draft"`
	CommitCount  int      `json:"commit_count"`
	HeadSHA      string   `json:"head_sha"`
	PrevHeadSHA  string   `json:"prev_head_sha"`
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	Labels       []string `json:"labels"`
}

type GitHubPRNewCommitsPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	IsDraft    *bool    `json:"is_draft,omitempty"`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRNewCommitsPredicate) Matches(m GitHubPRNewCommitsMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// conflicts — PR has merge conflicts with its base.
// -----------------------------------------------------------------------------

type GitHubPRConflictsMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRConflictsPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft    *bool    `json:"is_draft,omitempty"`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRConflictsPredicate) Matches(m GitHubPRConflictsMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// ready_for_review — draft → ready transition.
// -----------------------------------------------------------------------------

type GitHubPRReadyForReviewMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRReadyForReviewPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRReadyForReviewPredicate) Matches(m GitHubPRReadyForReviewMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// opened — new PR first seen.
// -----------------------------------------------------------------------------

type GitHubPROpenedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Title        string   `json:"title"`
	Labels       []string `json:"labels"`
}

type GitHubPROpenedPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft    *bool    `json:"is_draft,omitempty"`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPROpenedPredicate) Matches(m GitHubPROpenedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
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
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	MergedBy     string   `json:"merged_by"`
	MergedByID   string   `json:"merged_by_id,omitempty"` // GitHub's id for the merging account
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRMergedPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRMergedPredicate) Matches(m GitHubPRMergedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		hasLabel(p.HasLabel, m.Labels)
}

type GitHubPRClosedMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	ClosedBy     string   `json:"closed_by"`
	ClosedByID   string   `json:"closed_by_id,omitempty"` // GitHub's id for the closing account
	Labels       []string `json:"labels"`
}

type GitHubPRClosedPredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRClosedPredicate) Matches(m GitHubPRClosedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// unreachable — Triage Factory no longer follows a tracked PR. Terminating:
// it closes the entity and its tasks like merged/closed.
// -----------------------------------------------------------------------------

// GitHub unreachable reasons.
const (
	// GitHubUnreachableScopeChanged: the org's GitHub base URL now names
	// another host. The PR was polled from Host, which the new host cannot
	// answer for, so it is retired without a request.
	GitHubUnreachableScopeChanged = "scope_changed"
)

// GitHubPRUnreachableMetadata carries the PR's last known fields, from the
// stored snapshot, plus the host it was polled from and why it is no longer
// followed.
type GitHubPRUnreachableMetadata struct {
	Author       string   `json:"author"`
	AuthorID     string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
	Title        string   `json:"title"`
	Host         string   `json:"host"`
	Reason       string   `json:"reason"` // one of the GitHubUnreachable* values
}

type GitHubPRUnreachablePredicate struct {
	AuthorIDIn []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	RepoIn     []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	HasLabel   *string  `json:"has_label,omitempty"`
}

func (p GitHubPRUnreachablePredicate) Matches(m GitHubPRUnreachableMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// commented — someone other than TF commented in the PR's conversation, the
// sibling of the Jira and Linear commented events. One event per poll however
// many comments landed: CommentID and Commenter are the newest of them, and
// CommenterLogins and CommenterIDs hold the author of each, so commenter_id_in
// matches when any of them wrote one. Logins are spelled as REST spells them,
// a bot's as "<slug>[bot]". A comment TF posted is not one of them, and a poll
// that found only TF's comments emits nothing. Comments in review threads
// arrive as reviews (review_commented), not here.
// -----------------------------------------------------------------------------

type GitHubPRCommentedMetadata struct {
	Author          string   `json:"author"`
	AuthorID        string   `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Commenter       string   `json:"commenter"`
	CommenterLogins []string `json:"commenter_logins"`
	// CommenterID is GitHub's id for Commenter's account, and CommenterIDs
	// the id of each comment's author, once per account in the order they
	// commented. A comment whose author GitHub returned without an id adds
	// nothing to CommenterIDs.
	CommenterID  string   `json:"commenter_id,omitempty"`
	CommenterIDs []string `json:"commenter_ids"`
	CommentID    int64    `json:"comment_id"`
	Repo         string   `json:"repo"`
	RepositoryID string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber     int      `json:"pr_number"`
	IsDraft      bool     `json:"is_draft"`
	HeadSHA      string   `json:"head_sha"`
	Labels       []string `json:"labels"`
}

type GitHubPRCommentedPredicate struct {
	AuthorIDIn    []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	CommenterIDIn []string `json:"commenter_id_in,omitempty" doc:"Match when any of these GitHub accounts wrote one of the new comments, each by GitHub's account id as author_id_in takes it, not its login (a bot is an account too)."`
	RepoIn        []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft       *bool    `json:"is_draft,omitempty" doc:"Match draft vs. ready-for-review PRs."`
	HasLabel      *string  `json:"has_label,omitempty" doc:"Require the PR to currently bear this label."`
}

func (p GitHubPRCommentedPredicate) Matches(m GitHubPRCommentedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		anyIDIn(p.CommenterIDIn, m.CommenterIDs) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// mentioned — a user or team TF knows was @mentioned on a PR.
//
// The open-set discriminator is the mentioned identity, as review_requested's
// is the requested reviewer: one event per newly mentioned TF-known identity,
// keyed by its handle ("user:<login>" / "team:<org>/<slug>"), since a mention
// is text and names no id, and routed as review_requested is, to that
// identity's team(s). Exactly one of MentionedLogin / MentionedTeam is set,
// lower-cased, since GitHub matches logins and slugs without regard to case.
//
// MentionSource says where the mention was: a conversation comment, a
// review's body, an inline comment in a review, or the PR body. Commenter and
// CommentID name that comment or review (CommentID is the review's id for
// "review"), and are empty for "body", which has no author or id of its own.
// -----------------------------------------------------------------------------

// The values of GitHubPRMentionedMetadata.MentionSource.
const (
	MentionSourceComment       = "comment"
	MentionSourceReview        = "review"
	MentionSourceReviewComment = "review_comment"
	MentionSourceBody          = "body"
)

type GitHubPRMentionedMetadata struct {
	Author    string `json:"author"`
	AuthorID  string `json:"author_id,omitempty"` // GitHub's id for the PR author's account
	Commenter string `json:"commenter"`
	// CommenterID is GitHub's id for Commenter's account. A mention has no id
	// of its own: MentionedLogin and MentionedTeam are the handle the text
	// spelled.
	CommenterID    string   `json:"commenter_id,omitempty"`
	CommentID      int64    `json:"comment_id"`
	MentionedLogin string   `json:"mentioned_login,omitempty"`
	MentionedTeam  string   `json:"mentioned_team,omitempty"`
	MentionSource  string   `json:"mention_source"`
	Repo           string   `json:"repo"`
	RepositoryID   string   `json:"repository_id"` // the repository's registry row; what repo_in names
	PRNumber       int      `json:"pr_number"`
	IsDraft        bool     `json:"is_draft"`
	HeadSHA        string   `json:"head_sha"`
	Labels         []string `json:"labels"`
}

type GitHubPRMentionedPredicate struct {
	AuthorIDIn     []string `json:"author_id_in,omitempty" doc:"Match PRs authored by any of these GitHub accounts, each by GitHub's account id, not its login: the github_user_id of GET /api/me or the team roster, or of GET /api/orgs/{org_id}/github/users/by-name/{login} for an account outside TF. Empty matches every author."`
	CommenterIDIn  []string `json:"commenter_id_in,omitempty" doc:"Match @mentions left by any of these GitHub accounts, each by GitHub's account id as author_id_in takes it, not its login. A mention in the PR body has no commenter."`
	MentionedLogin *string  `json:"mentioned_login,omitempty" doc:"Match only when this user login was mentioned, case-insensitively. A mention is text and names no account id, so mentions are matched by the handle they spell."`
	MentionedTeam  *string  `json:"mentioned_team,omitempty" doc:"Match only when this github team ('org/slug') was mentioned."`
	MentionSource  *string  `json:"mention_source,omitempty" enum:"comment,review,review_comment,body" doc:"Match only mentions from this source."`
	RepoIn         []string `json:"repo_in,omitempty" doc:"Match only events in these repositories, each one the team tracks, by registry id (the id field of POST /api/repos/list, not owner/name). Empty matches every repository the team tracks."`
	IsDraft        *bool    `json:"is_draft,omitempty" doc:"Match draft vs. ready-for-review PRs."`
	HasLabel       *string  `json:"has_label,omitempty" doc:"Require the PR to currently bear this label."`
}

func (p GitHubPRMentionedPredicate) Matches(m GitHubPRMentionedMetadata) bool {
	return idIn(p.AuthorIDIn, m.AuthorID) &&
		idIn(p.CommenterIDIn, m.CommenterID) &&
		strEqFold(p.MentionedLogin, m.MentionedLogin) &&
		strEqFold(p.MentionedTeam, m.MentionedTeam) &&
		strEq(p.MentionSource, m.MentionSource) &&
		idIn(p.RepoIn, m.RepositoryID) &&
		boolEq(p.IsDraft, m.IsDraft) &&
		hasLabel(p.HasLabel, m.Labels)
}

// -----------------------------------------------------------------------------
// Registration.
// -----------------------------------------------------------------------------

// Ownership declarations: every github:pr:* type is OwnershipOwned — routed
// via the owning-team ladder to whoever authored the PR — EXCEPT
// review_requested and mentioned, which are OwnershipRequestedParty (routed
// to the requested reviewer's or mentioned identity's team(s) instead; see
// their metadata docs above).
// internal/routing derives its author-centric anchor set from exactly this
// declaration (events.TypesWithOwnership(OwnershipOwned, "github:")) rather
// than a parallel hand-maintained list.
func init() {
	Register(NewSchema[GitHubPRReviewRequestedMetadata, GitHubPRReviewRequestedPredicate](domain.EventGitHubPRReviewRequested, OwnershipRequestedParty))
	Register(NewSchema[GitHubPRReviewRequestRemovedMetadata, GitHubPRReviewRequestRemovedPredicate](domain.EventGitHubPRReviewRequestRemoved, OwnershipOwned))

	Register(NewSchema[GitHubPRReviewChangesRequestedMetadata, GitHubPRReviewChangesRequestedPredicate](domain.EventGitHubPRReviewChangesRequested, OwnershipOwned))
	Register(NewSchema[GitHubPRReviewApprovedMetadata, GitHubPRReviewApprovedPredicate](domain.EventGitHubPRReviewApproved, OwnershipOwned))
	Register(NewSchema[GitHubPRReviewCommentedMetadata, GitHubPRReviewCommentedPredicate](domain.EventGitHubPRReviewCommented, OwnershipOwned))
	Register(NewSchema[GitHubPRReviewDismissedMetadata, GitHubPRReviewDismissedPredicate](domain.EventGitHubPRReviewDismissed, OwnershipOwned))

	Register(NewSchema[GitHubPRCICheckFailedMetadata, GitHubPRCICheckFailedPredicate](domain.EventGitHubPRCICheckFailed, OwnershipOwned))
	Register(NewSchema[GitHubPRCICheckPassedMetadata, GitHubPRCICheckPassedPredicate](domain.EventGitHubPRCICheckPassed, OwnershipOwned))

	Register(NewSchema[GitHubPRLabelAddedMetadata, GitHubPRLabelAddedPredicate](domain.EventGitHubPRLabelAdded, OwnershipOwned))
	Register(NewSchema[GitHubPRLabelRemovedMetadata, GitHubPRLabelRemovedPredicate](domain.EventGitHubPRLabelRemoved, OwnershipOwned))

	Register(NewSchema[GitHubPRNewCommitsMetadata, GitHubPRNewCommitsPredicate](domain.EventGitHubPRNewCommits, OwnershipOwned))
	Register(NewSchema[GitHubPRConflictsMetadata, GitHubPRConflictsPredicate](domain.EventGitHubPRConflicts, OwnershipOwned))
	Register(NewSchema[GitHubPRReadyForReviewMetadata, GitHubPRReadyForReviewPredicate](domain.EventGitHubPRReadyForReview, OwnershipOwned))
	Register(NewSchema[GitHubPROpenedMetadata, GitHubPROpenedPredicate](domain.EventGitHubPROpened, OwnershipOwned))
	Register(NewSchema[GitHubPRMergedMetadata, GitHubPRMergedPredicate](domain.EventGitHubPRMerged, OwnershipOwned))
	Register(NewSchema[GitHubPRClosedMetadata, GitHubPRClosedPredicate](domain.EventGitHubPRClosed, OwnershipOwned))
	Register(NewSchema[GitHubPRUnreachableMetadata, GitHubPRUnreachablePredicate](domain.EventGitHubPRUnreachable, OwnershipOwned))
	Register(NewSchema[GitHubPRMentionedMetadata, GitHubPRMentionedPredicate](domain.EventGitHubPRMentioned, OwnershipRequestedParty))
	Register(NewSchema[GitHubPRCommentedMetadata, GitHubPRCommentedPredicate](domain.EventGitHubPRCommented, OwnershipOwned))
}
