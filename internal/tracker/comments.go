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
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// jiraCommentPageSize is how many comments one read of an issue's comment list
// asks for when the comments past the page Jira returns with the issue are
// read.
const jiraCommentPageSize = 50

// NewComment is a comment made on a tracked issue since the snapshot a diff
// compares against. AuthorID is the identity a commenter_in allowlist holds:
// the Jira author's stable id (jira.StableUserID, the rule assignee_in's values
// follow), or the Linear user id, which is empty for a comment an integration
// posted.
type NewComment struct {
	ID        string
	Author    string // display name
	AuthorID  string
	CreatedAt string
	// ByTF is set when TF posted the comment. An agent's comment is recorded
	// as a `comment` artifact keyed on the comment's id, and ByTF is whether
	// that artifact exists. It is matched on the id rather than the author
	// because a personal credential comments as the person who owns it.
	ByTF bool
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

// commentedAt is the source time of a commented event: the newest comment's
// creation, or the issue's last update when that does not parse.
func commentedAt(c NewComment, issueUpdatedAt string) string {
	if _, ok := domain.ParseExternalTime(c.CreatedAt); ok {
		return c.CreatedAt
	}
	return issueUpdatedAt
}

// markTFComments sets ByTF on each comment TF posted: each with a `comment`
// artifact in scope under the key the agent's comment verb records it with.
// resource is the issue's artifact resource (domain.EntityArtifactResource).
// With no resource there is no key to look for, and every comment is taken as
// a person's.
func (t *Tracker) markTFComments(ctx context.Context, provider, scope, resource string, comments []NewComment) error {
	if len(comments) == 0 || resource == "" {
		return nil
	}
	keys := make([]string, len(comments))
	for i, c := range comments {
		keys[i] = domain.ArtifactDedupKey(provider, domain.ArtifactKindComment, resource, c.ID)
	}
	present, err := t.artifacts.PresentDedupKeysSystem(ctx, t.orgID, scope, keys)
	if err != nil {
		return err
	}
	for i := range comments {
		comments[i].ByTF = present[keys[i]]
	}
	return nil
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
	resource := domain.EntityArtifactResource(domain.ArtifactProviderJira, c.scope, curr.ID)
	if err := t.markTFComments(ctx, domain.ArtifactProviderJira, c.scope, resource, fresh); err != nil {
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
	if err := t.markTFComments(ctx, domain.ArtifactProviderLinear, scope, snap.ID, fresh); err != nil {
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
