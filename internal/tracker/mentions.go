package tracker

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// prMention is where a poll found a user or team @-mentioned on a pull
// request.
type prMention struct {
	handle    string // as ghclient.Mentions spells it: "login" or "org/slug"
	source    string // one of the events.MentionSource values
	commenter string // empty for the body
	commentID int64  // the review's id for a review body; zero for the body
	at        string
}

// prMentions returns one mention for each user or team newly mentioned on the
// pull request since prev, in the order they were first found.
//
// A mention is new when it is in a comment or review made since prev, or is
// in curr's body and not prev's. comments and reviews are what the poll read
// past its batch (githubNewComments, githubNewReviews), so an edit to an older
// comment or review adds nothing. The body is compared only when both
// snapshots hold its mentions: a snapshot without them is a baseline.
//
// Skipped are mentions in a comment or review TF posted, and a user's mention
// of themselves; a bot's comment counts like anyone's. The body is taken as
// the PR author's, so the author's own handle there is skipped too. A handle
// mentioned several times is attributed to its newest comment or review, or
// to the body when nothing else mentioned it, since the body has no author or
// time of its own.
func prMentions(prev, curr domain.PRSnapshot, reviews []NewReview, comments []NewComment) []prMention {
	var order []string
	best := map[string]prMention{}
	consider := func(m prMention) {
		if isSelfMention(m.handle, m.commenter) {
			return
		}
		// The body is considered last, so it never displaces a comment or
		// review.
		if held, ok := best[m.handle]; !ok {
			order = append(order, m.handle)
		} else if m.source == events.MentionSourceBody || compareCommentTimes(m.at, held.at) <= 0 {
			return
		}
		best[m.handle] = m
	}

	for _, c := range comments {
		if c.ByTF {
			continue
		}
		commentID, _ := strconv.ParseInt(c.ID, 10, 64)
		for _, h := range c.Mentions {
			consider(prMention{handle: h, source: events.MentionSourceComment, commenter: c.Author, commentID: commentID, at: c.CreatedAt})
		}
	}
	for _, r := range reviews {
		if r.ByTF {
			continue
		}
		for _, h := range r.Mentions {
			consider(prMention{handle: h, source: events.MentionSourceReview, commenter: r.Author, commentID: r.ID, at: r.SubmittedAt})
		}
		for _, c := range r.Comments {
			if c.ByTF {
				continue
			}
			for _, h := range c.Mentions {
				consider(prMention{handle: h, source: events.MentionSourceReviewComment, commenter: r.Author, commentID: c.ID, at: r.SubmittedAt})
			}
		}
	}
	if prev.BodyMentions != nil && curr.BodyMentions != nil {
		for _, h := range curr.BodyMentions {
			if !slices.Contains(prev.BodyMentions, h) && !isSelfMention(h, curr.Author) {
				consider(prMention{handle: h, source: events.MentionSourceBody, at: curr.UpdatedAt})
			}
		}
	}

	out := make([]prMention, len(order))
	for i, h := range order {
		out[i] = best[h]
	}
	return out
}

// openedSlack widens "created since the last listing" by clock skew between
// GitHub and TF. A listing's time is TF's clock as it sent the request, while
// a pull request's creation time is GitHub's, so one GitHub created after the
// listing was sent, and so missing from it, could otherwise read as older
// than the listing. A pull request this window admits wrongly was created
// minutes before TF looked, so its body's mentions are new anyway.
const openedSlack = 5 * time.Minute

// openedSince reports whether the pull request was opened since its
// repository's previous listing, lastListed (githubDiscovery.lastListed):
// created after it, less openedSlack. Every other pull request was open when
// TF last looked, so its body is a baseline. That includes one past the open
// pull requests a listing returns and a reopened one. A repository with no
// previous listing (the zero time), or a creation time that does not parse,
// gives false.
func openedSince(snap domain.PRSnapshot, lastListed time.Time) bool {
	if lastListed.IsZero() {
		return false
	}
	created, ok := domain.ParseExternalTime(snap.CreatedAt)
	return ok && created.After(lastListed.Add(-openedSlack))
}

// openedPRMentions returns a mentioned event for each identity TF knows that
// the body of a newly opened pull request (openedSince) mentions.
func openedPRMentions(entityID string, snap domain.PRSnapshot, resolver ReviewerResolver) []domain.Event {
	var evts []domain.Event
	now := time.Now()
	for _, m := range prMentions(domain.PRSnapshot{BodyMentions: []string{}}, snap, nil, nil) {
		appendMentioned(&evts, &entityID, now, snap, m, resolver)
	}
	return evts
}

// appendMentioned appends the mentioned event for m on the pull request curr,
// when resolver knows the identity m names. Its time is m's, or curr's
// updatedAt when m's does not parse.
func appendMentioned(evts *[]domain.Event, entityID *string, now time.Time, curr domain.PRSnapshot, m prMention, resolver ReviewerResolver) {
	login, team, known := resolveReviewer(resolver, m.handle)
	if !known {
		return
	}
	at := m.at
	if _, ok := domain.ParseExternalTime(at); !ok {
		at = curr.UpdatedAt
	}
	emitWithFallback(domain.EventGitHubPRMentioned, reviewerDedupKey(m.handle), at, events.GitHubPRMentionedMetadata{
		Author: curr.Author, Commenter: m.commenter, CommentID: m.commentID,
		MentionedLogin: login, MentionedTeam: team, MentionSource: m.source,
		Repo: curr.Repo, PRNumber: curr.Number,
		IsDraft: curr.IsDraft, HeadSHA: curr.HeadSHA, Labels: curr.Labels,
	}, entityID, now, evts)
}

// isSelfMention reports whether a user handle names its commenter. A bot's
// login carries "[bot]", which a mention of it does not.
func isSelfMention(handle, commenter string) bool {
	if commenter == "" {
		return false
	}
	return strings.EqualFold(handle, strings.TrimSuffix(commenter, domain.GitHubBotLoginSuffix))
}
