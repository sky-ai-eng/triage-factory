package tracker

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"go.opentelemetry.io/otel/codes"
)

// jiraCommentPageSize is how many comments one read of an issue's comment list
// asks for when the comments past the page Jira returns with the issue are
// read.
const jiraCommentPageSize = 50

// NewComment is a comment made on a tracked issue or pull request since the
// snapshot a diff compares against. AuthorID is the identity a commenter
// allowlist holds: the Jira author's stable id (jira.StableUserID, the rule
// assignee_in's values follow) for commenter_in, the Linear user id for
// commenter_in, which is empty for a comment an integration posted, or
// GitHub's id for the author's account for commenter_id_in, which is empty
// for an author GitHub returns without one.
type NewComment struct {
	ID        string
	Author    string // display name; on GitHub, the login
	AuthorID  string
	CreatedAt string
	// ByTF is set when TF posted the comment. An agent's comment is recorded
	// as a `comment` deliverable keyed on the comment's id, and ByTF is whether
	// that deliverable exists. It is matched on the id rather than the author
	// because a personal credential comments as the person who owns it.
	ByTF bool
	// Mentions are the users and teams a GitHub comment @-mentions
	// (ghclient.Mentions). Empty on Jira and Linear.
	Mentions []string
}

// commentedBy reads a poll's new comments, oldest first, for the commented
// event: the newest comment TF did not post, and the author id of each comment
// TF did not post, once per author in the order they commented. ok=false when
// there are none: no comments, or only TF's, which are TF's own output rather
// than something to attend to.
func commentedBy(comments []NewComment) (newest NewComment, authorIDs []string, ok bool) {
	authorIDs = []string{}
	for _, c := range comments {
		if c.ByTF {
			continue
		}
		newest, ok = c, true
		if c.AuthorID != "" && !slices.Contains(authorIDs, c.AuthorID) {
			authorIDs = append(authorIDs, c.AuthorID)
		}
	}
	return newest, authorIDs, ok
}

// commentedByLogins is commentedBy's author list by login rather than id: the
// login of each comment TF did not post, once per login in the order they
// commented. GitHub's commented event carries both, the ids for matching and
// the logins for display.
func commentedByLogins(comments []NewComment) []string {
	logins := []string{}
	for _, c := range comments {
		if !c.ByTF && c.Author != "" && !slices.Contains(logins, c.Author) {
			logins = append(logins, c.Author)
		}
	}
	return logins
}

// commentedAt is the source time of a commented event: the newest comment's
// creation, or the issue's last update when that does not parse.
func commentedAt(c NewComment, issueUpdatedAt string) string {
	if _, ok := domain.ParseExternalTime(c.CreatedAt); ok {
		return c.CreatedAt
	}
	return issueUpdatedAt
}

// markTFComments sets ByTF on each comment TF posted: each with a `comment`
// deliverable in scope under the key the agent's comment verbs record it with,
// which key spells from the comment's id. A comment key spells as "" has no
// key to look for, and is taken as a person's.
func (t *Tracker) markTFComments(ctx context.Context, scope string, comments []NewComment, key func(commentID string) string) error {
	keys := make([]string, 0, len(comments))
	for _, c := range comments {
		if k := key(c.ID); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	present, err := t.deliverables.PresentDedupKeysSystem(ctx, t.orgID, scope, keys)
	if err != nil {
		return err
	}
	for i := range comments {
		if k := key(comments[i].ID); k != "" {
			comments[i].ByTF = present[k]
		}
	}
	return nil
}

// issueCommentKey spells the key a comment on a Jira or Linear issue is
// recorded under. resource is the issue's deliverable resource
// (domain.EntityResource); with none there is no key.
func issueCommentKey(provider, resource string) func(string) string {
	return func(commentID string) string {
		if resource == "" {
			return ""
		}
		return domain.ObjectKey(provider, domain.DeliverableKindComment, resource, commentID)
	}
}

// --- Jira ------------------------------------------------------------------

// jiraNewComments returns the comments made on the issue since prev, oldest
// first with TF's marked, and settles curr's comment marker.
//
// inline is the page of comments Jira returned with the issue. When it holds
// every comment, issueToState has already set curr's marker from it. When the
// issue has more comments than the page, the comments past it are read from the
// end of the list back to prev's marker, but only when the issue changed since
// prev: its comment count moved, or its updated time advanced, which Jira does
// when a comment is added. An issue that did neither has no new comment, and
// prev's marker carries over.
//
// A failed read, of the comments or of which are TF's, leaves curr's comment
// count and marker at prev's and returns none, so the next poll finds the same
// comments new again.
func (t *Tracker) jiraNewComments(ctx context.Context, c *jiraCycle, prev domain.JiraSnapshot, curr *domain.JiraSnapshot, inline []NewComment) []NewComment {
	known := inline
	if len(inline) < curr.CommentCount {
		if curr.CommentCount == prev.CommentCount && !jiraUpdatedSince(prev, *curr) {
			curr.LastCommentID, curr.LastCommentAt = prev.LastCommentID, prev.LastCommentAt
			return nil
		}
		tail, err := readJiraCommentTail(ctx, c.client, cmp.Or(curr.ID, curr.Key), prev, len(inline), curr.CommentCount)
		if err != nil {
			trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "jira comment read failed; the issue's new comments are read again next cycle",
				"source_id", curr.Key, "error", err)
			keepJiraComments(prev, curr)
			return nil
		}
		known = sortJiraComments(slices.Concat(inline, tail))
		if len(known) > 0 {
			newest := known[len(known)-1]
			curr.LastCommentID, curr.LastCommentAt = newest.ID, newest.CreatedAt
		}
	}
	fresh := jiraCommentsSince(prev, curr.CommentCount, known)
	resource := domain.EntityResource(domain.ProviderJira, c.scope, curr.ID)
	if err := t.markTFComments(ctx, c.scope, fresh, issueCommentKey(domain.ProviderJira, resource)); err != nil {
		trackerLog.WarnContext(ctx, "reading which jira comments TF posted failed; the issue's new comments are read again next cycle",
			"source_id", curr.Key, "error", err)
		keepJiraComments(prev, curr)
		return nil
	}
	return fresh
}

// jiraUpdatedSince reports whether curr's updated time is after prev's. A time
// that does not parse, or is absent, counts as a change: the cost of a wrong
// yes is one read of the comment list, and of a wrong no a missed comment.
func jiraUpdatedSince(prev, curr domain.JiraSnapshot) bool {
	tp, okP := domain.ParseExternalTime(prev.UpdatedAt)
	tc, okC := domain.ParseExternalTime(curr.UpdatedAt)
	return !okP || !okC || tc.After(tp)
}

// keepJiraComments puts prev's comment count and marker back on curr.
func keepJiraComments(prev domain.JiraSnapshot, curr *domain.JiraSnapshot) {
	curr.CommentCount, curr.LastCommentID, curr.LastCommentAt = prev.CommentCount, prev.LastCommentID, prev.LastCommentAt
}

// readJiraCommentTail reads the comments past an issue's inline page: a page
// at a time from the end of the list backwards, stopping at the first page
// that reaches back to a comment no newer than prev's marker, or, when prev has
// no marker, once it holds as many comments as prev's count says are new.
// from is the index the inline page ends at, and nothing before it is read.
// Returned oldest first.
func readJiraCommentTail(ctx context.Context, client *jiraclient.Client, idOrKey string, prev domain.JiraSnapshot, from, total int) ([]NewComment, error) {
	var tail []NewComment
	for end := total; end > from; {
		start := max(from, end-jiraCommentPageSize)
		page, err := client.ListComments(ctx, idOrKey, start, end-start)
		if err != nil {
			return nil, err
		}
		tail = append(jiraComments(page), tail...)
		if len(page) == 0 || jiraTailReachesPrev(prev, total, tail) {
			break
		}
		end = start
	}
	return tail, nil
}

// jiraTailReachesPrev reports whether the comments read from the end of the
// list, oldest first, reach back far enough to hold every comment new since
// prev.
func jiraTailReachesPrev(prev domain.JiraSnapshot, total int, tail []NewComment) bool {
	if len(tail) == 0 {
		return false
	}
	if prev.LastCommentID != "" {
		return !jiraCommentAfter(tail[0], prev.LastCommentAt, prev.LastCommentID)
	}
	return len(tail) >= total-prev.CommentCount
}

// jiraCommentsSince returns the comments in known, oldest first, made after
// prev's marker. A snapshot stored without a marker has only its count to go
// on, so the new comments are the last ones, as many as the count rose by.
func jiraCommentsSince(prev domain.JiraSnapshot, total int, known []NewComment) []NewComment {
	if prev.LastCommentID != "" {
		i := slices.IndexFunc(known, func(c NewComment) bool {
			return jiraCommentAfter(c, prev.LastCommentAt, prev.LastCommentID)
		})
		if i < 0 {
			return nil
		}
		return slices.Clone(known[i:])
	}
	n := total - prev.CommentCount
	if n <= 0 {
		return nil
	}
	return slices.Clone(known[max(0, len(known)-n):])
}

// jiraComments converts Jira's comments, oldest first, deduplicated by id.
func jiraComments(in []jiraclient.Comment) []NewComment {
	out := make([]NewComment, 0, len(in))
	for _, c := range in {
		nc := NewComment{ID: c.ID, CreatedAt: c.Created}
		if a := c.Author; a != nil {
			nc.Author = a.DisplayName
			nc.AuthorID = jiraclient.StableUserID(a.AccountID, a.Key, a.Name)
		}
		out = append(out, nc)
	}
	return sortJiraComments(out)
}

// sortJiraComments orders comments oldest first and drops a repeated id.
func sortJiraComments(comments []NewComment) []NewComment {
	slices.SortStableFunc(comments, func(a, b NewComment) int {
		switch {
		case jiraCommentAfter(a, b.CreatedAt, b.ID):
			return 1
		case jiraCommentAfter(b, a.CreatedAt, a.ID):
			return -1
		}
		return 0
	})
	return slices.CompactFunc(comments, func(a, b NewComment) bool { return a.ID == b.ID })
}

// jiraCommentAfter reports whether c was made after the comment created at at
// with id id: by creation time, or by id when the times are equal or either
// does not parse. Jira hands out comment ids in increasing order.
func jiraCommentAfter(c NewComment, at, id string) bool {
	tc, okC := domain.ParseExternalTime(c.CreatedAt)
	tm, okM := domain.ParseExternalTime(at)
	if okC && okM && !tc.Equal(tm) {
		return tc.After(tm)
	}
	a, errA := strconv.ParseInt(c.ID, 10, 64)
	b, errB := strconv.ParseInt(id, 10, 64)
	if errA == nil && errB == nil {
		return a > b
	}
	return strings.Compare(c.ID, id) > 0
}

// --- Linear ----------------------------------------------------------------

// linearNewComments returns the comments made on the issue since prev, oldest
// first with TF's marked. Nothing is new unless the newest comment changed to
// one no older than prev's; an older one is the newest comment deleted. When
// it changed, the comments made after prev's newest are read, along with the
// newest from snap's own read in case its time equals prev's.
//
// A rate limit is returned, as it is everywhere in a Linear cycle. Any other
// failed read, of the comments or of which are TF's, leaves snap's newest
// comment at prev's and returns none, so the next poll finds the same comments
// new again.
func (t *Tracker) linearNewComments(ctx context.Context, client LinearClient, scope string, prev domain.LinearSnapshot, snap *domain.LinearSnapshot, last *linear.Comment) ([]NewComment, error) {
	if snap.LastCommentID == "" || snap.LastCommentID == prev.LastCommentID || linearCommentOlder(snap.LastCommentAt, prev.LastCommentAt) {
		return nil, nil
	}
	// The filter is strictly after prev's newest, so a comment sharing its
	// millisecond is missed unless it is snap's newest, which is added back
	// below. It would have to be committed after the last poll read prev's
	// newest yet stamped the same millisecond. An inclusive filter would
	// instead report again any comment that already shared that millisecond.
	read, err := client.ListCommentsSince(ctx, snap.ID, prev.LastCommentAt)
	if errors.Is(err, linear.ErrRateLimited) {
		return nil, err
	}
	if err != nil {
		trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "linear comment read failed; the issue's new comments are read again next cycle",
			"source_id", snap.Identifier, "error", err)
		keepLinearComments(prev, snap)
		return nil, nil
	}
	if last != nil {
		read = append(read, *last)
	}
	fresh := make([]NewComment, 0, len(read))
	for _, c := range read {
		if c.ID == prev.LastCommentID || slices.ContainsFunc(fresh, func(f NewComment) bool { return f.ID == c.ID }) {
			continue
		}
		fresh = append(fresh, linearComment(c))
	}
	slices.SortStableFunc(fresh, func(a, b NewComment) int { return compareCommentTimes(a.CreatedAt, b.CreatedAt) })
	if err := t.markTFComments(ctx, scope, fresh, issueCommentKey(domain.ProviderLinear, snap.ID)); err != nil {
		trackerLog.WarnContext(ctx, "reading which linear comments TF posted failed; the issue's new comments are read again next cycle",
			"source_id", snap.Identifier, "error", err)
		keepLinearComments(prev, snap)
		return nil, nil
	}
	return fresh, nil
}

// compareCommentTimes orders two comment times by the moment they name, so a
// time written in another offset or precision still sorts where it belongs. A
// time that does not parse is compared as text.
func compareCommentTimes(a, b string) int {
	ta, okA := domain.ParseExternalTime(a)
	tb, okB := domain.ParseExternalTime(b)
	if okA && okB {
		return ta.Compare(tb)
	}
	return strings.Compare(a, b)
}

// keepLinearComments puts prev's newest comment back on snap.
func keepLinearComments(prev domain.LinearSnapshot, snap *domain.LinearSnapshot) {
	snap.LastCommentID, snap.LastCommentAt = prev.LastCommentID, prev.LastCommentAt
}

// linearComment converts a Linear comment. A comment with no user was posted
// by an integration, named by its bot actor and carrying no user id.
func linearComment(c linear.Comment) NewComment {
	nc := NewComment{ID: c.ID, CreatedAt: c.CreatedAt}
	switch {
	case c.User != nil:
		nc.Author = cmp.Or(c.User.Name, c.User.DisplayName)
		nc.AuthorID = c.User.ID
	case c.BotActor != nil:
		nc.Author = c.BotActor.Name
	}
	return nc
}

// --- GitHub ----------------------------------------------------------------

// CommentReader reads a page of a pull request's conversation comments.
// *ghclient.Client satisfies it.
type CommentReader interface {
	PRCommentsBefore(ctx context.Context, prNodeID, before string) (ghclient.PRCommentPage, error)
}

// prActivityReader reads the reviews and comments a refresh cycle reads past
// its batch.
type prActivityReader interface {
	ReviewReader
	CommentReader
}

// rateLimitedReader answers every read with the rate limit a read earlier in
// the cycle hit, and sends nothing.
type rateLimitedReader struct{ err error }

func (r rateLimitedReader) PRReviewsBefore(context.Context, string, string) (ghclient.PRReviewPage, error) {
	return ghclient.PRReviewPage{}, r.err
}

func (r rateLimitedReader) PRCommentsBefore(context.Context, string, string) (ghclient.PRCommentPage, error) {
	return ghclient.PRCommentPage{}, r.err
}

// isGitHubRateLimit reports whether err is GitHub refusing for its rate
// limit.
func isGitHubRateLimit(err error) bool {
	var rl *ghclient.ErrRateLimited
	return errors.As(err, &rl)
}

// githubNewComments returns the conversation comments made on the pull request
// since prev, oldest first with TF's marked, and settles curr's comment marker
// and count.
//
// GitHub lists a pull request's comments in the order they were made, so a new
// comment is always the newest one. The comments are read only when curr's
// newest is newer than prev's marker; an older one means the marker's comment
// was deleted, and nothing is new. The read pages back from the end of the list
// to the first page that reaches a comment no newer than the marker.
//
// A prev without the marker was seeded from the REST listing of open pull
// requests, or stored before the marker existed. One that counted comments was
// stored from a GraphQL read, so its count holds: the new comments are the
// last ones, as many as the count rose by. One that counted none may be a REST
// seed, whose count says nothing, so the comments already on the pull request
// are taken as seen and curr's marker becomes the baseline. That costs the
// event for a pull request stored with no comments before the marker existed
// and first commented on in the poll that stores it.
//
// A failed read, of the comments or of which are TF's, puts prev's marker and
// count back on curr and returns none, so the next poll reads the same comments
// again. A rate limit is also returned, for the caller to stop reading.
func (t *Tracker) githubNewComments(ctx context.Context, client CommentReader, scope, prNodeID string, prev domain.PRSnapshot, curr *domain.PRSnapshot) ([]NewComment, error) {
	if curr.CommentMarker == nil {
		return nil, nil
	}
	if prev.CommentMarker != nil {
		if !prev.CommentMarker.After(curr.CommentMarker.CreatedAt, curr.CommentMarker.ID) {
			return nil, nil
		}
	} else if prev.CommentCount == 0 || curr.CommentCount <= prev.CommentCount {
		return nil, nil
	}

	read, total, err := t.readGitHubComments(ctx, client, prNodeID, prev)
	if err != nil {
		keepGitHubComments(prev, curr)
		if isGitHubRateLimit(err) {
			return nil, err
		}
		trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "github comment read failed; the pull request's new comments are read again next cycle",
			"repo", curr.Repo, "number", curr.Number, "error", err)
		return nil, nil
	}
	newest := *curr.CommentMarker
	for _, c := range read {
		newest = newest.Newer(domain.CommentMarker{ID: c.ID, CreatedAt: c.CreatedAt})
	}
	fresh := githubCommentsSince(prev, total, read)
	if err := t.markTFComments(ctx, scope, fresh, githubCommentKey); err != nil {
		trackerLog.WarnContext(ctx, "reading which github comments TF posted failed; the pull request's new comments are read again next cycle",
			"repo", curr.Repo, "number", curr.Number, "error", err)
		keepGitHubComments(prev, curr)
		return nil, nil
	}
	curr.CommentMarker, curr.CommentCount = &newest, total
	return fresh, nil
}

// readGitHubComments reads the pull request's conversation comments a page at a
// time from the end of the list backwards, oldest first, and returns them with
// the count of comments as of the read. It stops at the first page that
// reaches back to a comment no newer than prev's marker, or, when prev has
// none, once it holds as many comments as the count rose by.
func (t *Tracker) readGitHubComments(ctx context.Context, client CommentReader, prNodeID string, prev domain.PRSnapshot) (read []ghclient.PRComment, total int, err error) {
	ctx, span := tracer.Start(ctx, "tracker.github.read_comments")
	defer func() {
		span.SetAttributes(telemetry.Count(len(read)))
		if err != nil {
			span.SetStatus(codes.Error, "read comments")
		}
		span.End()
	}()

	before := ""
	for first := true; ; first = false {
		page, err := client.PRCommentsBefore(ctx, prNodeID, before)
		if err != nil {
			return nil, 0, err
		}
		if first {
			total = page.TotalCount
		}
		read = append(slices.Clone(page.Comments), read...)
		if !page.HasPrevious || page.Before == "" || githubReadReachesPrev(prev, total, read) {
			break
		}
		before = page.Before
	}
	slices.SortStableFunc(read, func(a, b ghclient.PRComment) int {
		am := domain.CommentMarker{ID: a.ID, CreatedAt: a.CreatedAt}
		switch {
		case am.After(b.CreatedAt, b.ID):
			return -1
		case (domain.CommentMarker{ID: b.ID, CreatedAt: b.CreatedAt}).After(a.CreatedAt, a.ID):
			return 1
		}
		return 0
	})
	return read, total, nil
}

// githubReadReachesPrev reports whether the comments read from the end of the
// list, oldest first, reach back far enough to hold every comment new since
// prev.
func githubReadReachesPrev(prev domain.PRSnapshot, total int, read []ghclient.PRComment) bool {
	if len(read) == 0 {
		return false
	}
	if prev.CommentMarker != nil {
		return !prev.CommentMarker.After(read[0].CreatedAt, read[0].ID)
	}
	return len(read) >= total-prev.CommentCount
}

// githubCommentsSince returns the comments in read, oldest first, made after
// prev's marker, or, when prev has none, the last ones, as many as the count
// rose by.
func githubCommentsSince(prev domain.PRSnapshot, total int, read []ghclient.PRComment) []NewComment {
	if prev.CommentMarker == nil {
		read = read[max(0, len(read)-max(0, total-prev.CommentCount)):]
	}
	fresh := []NewComment{}
	for _, c := range read {
		if prev.CommentMarker == nil || prev.CommentMarker.After(c.CreatedAt, c.ID) {
			fresh = append(fresh, NewComment{ID: strconv.FormatInt(c.ID, 10), Author: c.Author, AuthorID: c.AuthorID, CreatedAt: c.CreatedAt, Mentions: c.Mentions})
		}
	}
	return fresh
}

// githubCommentKey spells the key a conversation comment TF posted on a pull
// request is recorded under, the same through the exec verbs and through the
// raw gh channel. An id that is not a number names no comment.
func githubCommentKey(commentID string) string {
	id, err := strconv.ParseInt(commentID, 10, 64)
	if err != nil {
		return ""
	}
	return domain.GitHubCommentKey(domain.GitHubIssueComment, id)
}

// keepGitHubComments puts prev's comment marker and count back on curr.
func keepGitHubComments(prev domain.PRSnapshot, curr *domain.PRSnapshot) {
	curr.CommentMarker, curr.CommentCount = nil, prev.CommentCount
	if prev.CommentMarker != nil {
		marker := *prev.CommentMarker
		curr.CommentMarker = &marker
	}
}
