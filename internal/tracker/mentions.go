package tracker

import (
	"slices"
	"strconv"
	"strings"

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
// of themselves; a bot's comment counts like anyone's. A handle mentioned
// several times is attributed to its newest comment or review, or to the body
// when nothing else mentioned it, since the body has no author or time of its
// own.
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
			if !slices.Contains(prev.BodyMentions, h) {
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

// isSelfMention reports whether a user handle names its commenter. A bot's
// login carries "[bot]", which a mention of it does not.
func isSelfMention(handle, commenter string) bool {
	if commenter == "" {
		return false
	}
	return strings.EqualFold(handle, strings.TrimSuffix(commenter, domain.GitHubBotLoginSuffix))
}
