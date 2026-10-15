package tracker

import (
	"context"
	"log/slog"
	"slices"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"go.opentelemetry.io/otel/codes"
)

// NewReview is a review submitted on a tracked pull request since the snapshot
// a diff compares against.
type NewReview struct {
	ID           int64
	Author       string // REST-spelled login; empty when GitHub names no author
	State        string // APPROVED, CHANGES_REQUESTED, COMMENTED or DISMISSED
	SubmittedAt  string
	CommentCount int
	// Paths are the distinct files the review's inline comments are on, in
	// the order the comments name them.
	Paths []string
	// ByTF is set when TF submitted the review, or when the review is a reply
	// TF posted in a review thread: a COMMENTED review each of whose comments
	// TF recorded as a `comment` artifact. A submitted review's artifact
	// carries the review's id once it is posted, and a reply's artifact is
	// keyed on the comment's id, so both are matched on ids rather than on the
	// author, who under a personal credential is the person who owns it.
	ByTF bool
	// Mentions are the users and teams the review's body @-mentions.
	Mentions []string
	// Comments are the review's inline comments that were read.
	Comments []NewReviewComment
}

// NewReviewComment is an inline comment in a NewReview.
type NewReviewComment struct {
	ID       int64
	Mentions []string
	// ByTF is set when TF recorded the comment as a `comment` artifact. A
	// reply TF posted while the credential's owner had a review pending joins
	// that review, so it can be TF's in a review that is not.
	ByTF bool
}

// ReviewReader reads a page of a pull request's submitted reviews.
// *ghclient.Client satisfies it.
type ReviewReader interface {
	PRReviewsBefore(ctx context.Context, prNodeID, before string) (ghclient.PRReviewPage, error)
}

// githubNewReviews returns the reviews submitted on the pull request since
// prev, oldest first with TF's marked, and settles curr's review marker and
// review count.
//
// Nothing is read when prev has no marker: the diff then compares each
// reviewer's latest review, and curr's marker becomes the baseline. Otherwise
// the reviews are read only when the count of submitted reviews moved or curr's
// newest review is newer than prev's marker. A thread reply is a review of its
// own, so a reply moves both.
//
// The reviews are read a page at a time from the end of GitHub's list
// backwards, and a review is new when it was submitted after prev's marker.
// GitHub documents no order for the list. If it lists by submission, the new
// reviews are the last ones, and the read stops at the first page that reaches
// a review that is not new. If it lists by creation, a review started as
// pending before prev's poll and submitted since sits further back, so the read
// also goes on until the reviews it found make up the rise in the count.
//
// A failed read, of the reviews or of which are TF's, puts prev's marker and
// count back on curr and returns none, so the next poll reads the same reviews
// again. A rate limit is also returned, for the caller to stop reading.
func (t *Tracker) githubNewReviews(ctx context.Context, client ReviewReader, scope, prNodeID string, prev domain.PRSnapshot, curr *domain.PRSnapshot) ([]NewReview, error) {
	if prev.ReviewMarker == nil || curr.ReviewMarker == nil {
		return nil, nil
	}
	since := *prev.ReviewMarker
	newest := since.Newer(*curr.ReviewMarker)
	if curr.ReviewCount == prev.ReviewCount && newest == since {
		curr.ReviewMarker = &newest
		return nil, nil
	}

	read, total, err := t.readNewReviews(ctx, client, prNodeID, since, prev.ReviewCount)
	if err != nil {
		keepGitHubReviews(prev, curr)
		if isGitHubRateLimit(err) {
			return nil, err
		}
		trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "github review read failed; the pull request's new reviews are read again next cycle",
			"repo", curr.Repo, "number", curr.Number, "error", err)
		return nil, nil
	}
	fresh := make([]NewReview, 0, len(read))
	for _, r := range read {
		fresh = append(fresh, newReview(r))
		newest = newest.Newer(domain.ReviewMarker{ID: r.ID, SubmittedAt: r.SubmittedAt})
	}
	if err := t.markTFReviews(ctx, scope, read, fresh); err != nil {
		trackerLog.WarnContext(ctx, "reading which github reviews TF submitted failed; the pull request's new reviews are read again next cycle",
			"repo", curr.Repo, "number", curr.Number, "error", err)
		keepGitHubReviews(prev, curr)
		return nil, nil
	}
	curr.ReviewMarker, curr.ReviewCount = &newest, total
	return fresh, nil
}

// readNewReviews reads the reviews submitted after since, oldest first, and
// the count of submitted reviews as of the read. prevCount is the count prev's
// poll saw. See githubNewReviews for when the read stops.
func (t *Tracker) readNewReviews(ctx context.Context, client ReviewReader, prNodeID string, since domain.ReviewMarker, prevCount int) (fresh []ghclient.PRReview, total int, err error) {
	ctx, span := tracer.Start(ctx, "tracker.github.read_reviews")
	defer func() {
		span.SetAttributes(telemetry.Count(len(fresh)))
		if err != nil {
			span.SetStatus(codes.Error, "read reviews")
		}
		span.End()
	}()

	before := ""
	reachedOld := false
	for first := true; ; first = false {
		page, err := client.PRReviewsBefore(ctx, prNodeID, before)
		if err != nil {
			return nil, 0, err
		}
		if first {
			total = page.TotalCount
		}
		for _, r := range page.Reviews {
			if since.After(r.SubmittedAt, r.ID) {
				fresh = append(fresh, r)
			} else {
				reachedOld = true
			}
		}
		if !page.HasPrevious || page.Before == "" || (reachedOld && len(fresh) >= total-prevCount) {
			break
		}
		before = page.Before
	}
	slices.SortStableFunc(fresh, func(a, b ghclient.PRReview) int {
		am := domain.ReviewMarker{ID: a.ID, SubmittedAt: a.SubmittedAt}
		switch {
		case am.After(b.SubmittedAt, b.ID):
			return -1
		case (domain.ReviewMarker{ID: b.ID, SubmittedAt: b.SubmittedAt}).After(a.SubmittedAt, a.ID):
			return 1
		}
		return 0
	})
	return fresh, total, nil
}

// markTFReviews sets ByTF on each of fresh that TF submitted or that is a
// thread reply TF posted. read holds the same reviews as GitHub returned them,
// with their comments.
func (t *Tracker) markTFReviews(ctx context.Context, scope string, read []ghclient.PRReview, fresh []NewReview) error {
	if len(read) == 0 {
		return nil
	}
	ids := make([]string, len(read))
	var commentKeys []string
	for i, r := range read {
		ids[i] = strconv.FormatInt(r.ID, 10)
		for _, c := range r.Comments {
			commentKeys = append(commentKeys, domain.GitHubCommentDedupKey(domain.GitHubReviewComment, c.ID))
		}
	}
	submitted, err := t.artifacts.PresentExternalIDsSystem(ctx, t.orgID, scope, domain.ArtifactProviderGitHub, domain.ArtifactKindReview, ids)
	if err != nil {
		return err
	}
	posted, err := t.artifacts.PresentDedupKeysSystem(ctx, t.orgID, scope, commentKeys)
	if err != nil {
		return err
	}
	for i, r := range read {
		fresh[i].ByTF = submitted[ids[i]] || isTFReply(r, posted)
		for j, c := range r.Comments {
			fresh[i].Comments[j].ByTF = posted[domain.GitHubCommentDedupKey(domain.GitHubReviewComment, c.ID)]
		}
	}
	return nil
}

// isTFReply reports whether r is a reply TF posted in a review thread: a
// COMMENTED review with comments, every one of which TF recorded. A review
// whose comments were not all read is not taken as TF's.
func isTFReply(r ghclient.PRReview, posted map[string]bool) bool {
	if r.State != "COMMENTED" || r.CommentCount == 0 || len(r.Comments) != r.CommentCount {
		return false
	}
	for _, c := range r.Comments {
		if !posted[domain.GitHubCommentDedupKey(domain.GitHubReviewComment, c.ID)] {
			return false
		}
	}
	return true
}

// newReview converts a review GitHub returned, collecting the files its
// comments are on. Its comments keep GitHub's order, which markTFReviews
// relies on.
func newReview(r ghclient.PRReview) NewReview {
	paths := []string{}
	comments := make([]NewReviewComment, 0, len(r.Comments))
	for _, c := range r.Comments {
		if c.Path != "" && !slices.Contains(paths, c.Path) {
			paths = append(paths, c.Path)
		}
		comments = append(comments, NewReviewComment{ID: c.ID, Mentions: c.Mentions})
	}
	return NewReview{
		ID: r.ID, Author: r.Author, State: r.State, SubmittedAt: r.SubmittedAt,
		CommentCount: r.CommentCount, Paths: paths, Mentions: r.Mentions, Comments: comments,
	}
}

// keepGitHubReviews puts prev's review marker and count back on curr.
func keepGitHubReviews(prev domain.PRSnapshot, curr *domain.PRSnapshot) {
	marker := *prev.ReviewMarker
	curr.ReviewMarker, curr.ReviewCount = &marker, prev.ReviewCount
}
