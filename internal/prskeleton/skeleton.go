// Package prskeleton renders a pull request's history as a compact,
// bounded "skeleton" — the shape of what happened (commits, reviews,
// lifecycle transitions) rather than its content.
//
// The block it produces rides in the static context every delegated run
// starts with, so it answers the question the point-in-time event data
// can't: has this PR been churning for three weeks across four reviewers,
// or was it opened an hour ago? An agent fixing review feedback needs the
// second fact as much as the first.
//
// It is a map, never a replacement for the detail surfaces. Commit
// subjects are the only free text it ever renders, and only where a
// per-commit line already bounds them; review and comment bodies are
// summarized to counts and actors. An agent that needs the content runs
// `git log` in its worktree (the PR ref is already fetched) or
// `exec gh pr view -v`.
package prskeleton

import (
	"sort"
	"strings"
	"time"
)

// Kind discriminates one entry on the timeline. The set is closed: an
// unmapped source event is dropped at parse time rather than rendered as an
// unknown row, so a new GitHub event type can never leak an unlabeled line
// into an agent's context.
type Kind string

const (
	KindOpened               Kind = "opened"
	KindCommit               Kind = "commit"
	KindForcePush            Kind = "force-push"
	KindReview               Kind = "review"
	KindReviewComments       Kind = "review-comments"
	KindComment              Kind = "comment"
	KindReviewRequested      Kind = "review-requested"
	KindReviewRequestRemoved Kind = "review-request-removed"
	KindLabeled              Kind = "labeled"
	KindUnlabeled            Kind = "unlabeled"
	KindDraft                Kind = "draft"
	KindReadyForReview       Kind = "ready-for-review"
	KindRenamed              Kind = "renamed"
	KindBaseChanged          Kind = "base-changed"
	KindMerged               Kind = "merged"
	KindClosed               Kind = "closed"
	KindReopened             Kind = "reopened"

	// KindElided is synthesized by the renderer's windowing tier, never by a
	// fetch. It stands in for a contiguous span of entries that didn't fit
	// the line budget and carries the span's duration and per-kind counts —
	// the whole point of the tier being that "34 events in forty minutes"
	// and "34 events over twelve days" are different pull requests.
	KindElided Kind = "elided"
)

// Review states, uppercased from the source's lowercase vocabulary. Only
// COMMENTED is foldable; the other three change what the PR is and survive
// every collapse tier.
const (
	ReviewApproved         = "APPROVED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
	ReviewCommented        = "COMMENTED"
	ReviewDismissed        = "DISMISSED"
)

// Entry is one point on the timeline, or — once Count > 1 — a fold of
// several consecutive points of the same kind.
type Entry struct {
	Kind Kind
	// At is when the entry happened; for a fold, when the last of its
	// members happened. Sorting is on this field.
	At time.Time
	// Since is the first member's time on a fold, zero on a single entry.
	Since time.Time
	// Actors are the distinct logins involved, in first-seen order.
	Actors []string
	// Detail is the kind-specific discriminator: a review state, a label
	// name, a requested reviewer, a commit subject, a rename's new title.
	//
	// A review's inline-comment count is deliberately NOT here: the source
	// timeline reports inline comments as their own event rather than as a
	// property of the review, so they arrive as a neighbouring
	// KindReviewComments row carrying its own count.
	Detail string
	// Ref is the abbreviated commit sha on a commit entry.
	Ref string
	// Count is the number of source entries this row stands for: 1 for a
	// single point, n for a fold, n for an elided span.
	Count int
	// Parts is the per-kind breakdown of an elided span, ordered by
	// descending count then kind so rendering is deterministic.
	Parts []KindCount
}

// KindCount is one line item in an elided span's breakdown.
type KindCount struct {
	Kind  Kind
	Count int
}

// landmark reports whether an entry must survive every collapse tier.
// Landmarks are the transitions that change what the PR *is* — it went
// draft, it got approved, someone force-pushed out from under a review,
// the base branch moved. A tier-3 window drops thirty commits before it
// drops one of these, because "approved, then force-pushed, then
// re-requested" is the story; the last thirty commit subjects are not.
func (e Entry) landmark() bool {
	switch e.Kind {
	case KindOpened, KindForcePush, KindDraft, KindReadyForReview,
		KindRenamed, KindBaseChanged, KindMerged, KindClosed, KindReopened:
		return true
	case KindReview:
		return e.Detail != ReviewCommented
	default:
		return false
	}
}

// foldKey returns the grouping key for tier-2 run folding, and whether the
// entry may fold at all. Landmarks never fold. Reviews key on their state
// so a COMMENTED run can't absorb a neighbouring approval.
func (e Entry) foldKey() (string, bool) {
	if e.landmark() {
		return "", false
	}
	switch e.Kind {
	case KindReview:
		return string(e.Kind) + ":" + e.Detail, true
	case KindCommit, KindComment, KindReviewComments, KindLabeled, KindUnlabeled,
		KindReviewRequested, KindReviewRequestRemoved:
		return string(e.Kind), true
	default:
		return "", false
	}
}

// Header is the PR's identity and current state — the part of the block
// that doesn't depend on history length and is always rendered in full.
type Header struct {
	Repo         string // "owner/repo"
	Number       int
	Title        string
	Author       string
	State        string // OPEN | CLOSED | MERGED
	Draft        bool
	BaseRef      string
	HeadRef      string
	HeadSHA      string // abbreviated
	Mergeable    string // GitHub's mergeable_state; "" when unknown
	Additions    int
	Deletions    int
	ChangedFiles int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// Commits / Comments / ReviewComments are the PR object's own totals.
	// They are reported by GitHub independently of the timeline, so a
	// mismatch against what the timeline yielded is how a truncated fetch
	// becomes visible rather than silently short.
	Commits        int
	Comments       int
	ReviewComments int
}

// Skeleton is a fetched PR history: identity plus the full un-collapsed
// entry list. Collapsing happens at render time, so the same skeleton can
// be rendered against different line budgets.
type Skeleton struct {
	Header  Header
	Entries []Entry
	// Truncated is set when the timeline fetch hit its page cap, meaning
	// the OLDEST history is missing. Rendered as an explicit note — a
	// silently-short timeline reads as a short PR history, which is
	// exactly the wrong conclusion.
	Truncated bool
}

// sortEntries orders the timeline chronologically. The sort is stable so
// entries sharing a timestamp — a push landing five commits with one
// committer date is common — keep their source order, which is the order
// they actually happened in.
func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].At.Before(entries[j].At)
	})
}

// addActor appends login to actors if it isn't already present, preserving
// first-seen order. Folds accumulate authors this way, so "alice, carol"
// reads in the order they showed up rather than an arbitrary map order.
func addActor(actors []string, login string) []string {
	if login == "" {
		return actors
	}
	for _, a := range actors {
		if a == login {
			return actors
		}
	}
	return append(actors, login)
}

// sanitize flattens external text onto a single line and drops control
// characters. Every string this package renders — commit subjects, labels,
// logins, the PR title — is authored by whoever opened the PR, and the
// block is emitted inside the task context's untrusted region where one
// entry per line is the structure. An embedded newline would forge a
// timeline row; a stray control byte would corrupt the column alignment
// the rest of the block relies on.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n', r == '\r', r == '\v', r == '\f', r == '\t',
			r == '\u0085', r == '\u2028', r == '\u2029':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			// Drop outright: unlike a newline these carry no spacing
			// intent, and a terminal-escape prefix in a commit subject
			// has no business reaching a rendered block.
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(collapseSpaces(b.String()))
}

// collapseSpaces squeezes runs of spaces left behind by sanitize's newline
// folding, so a multi-line commit subject doesn't render as a wide gap.
func collapseSpaces(s string) string {
	if !strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// truncate caps s at max runes, marking the cut with an ellipsis. Commit
// subjects are the one piece of free text a rendered line carries, and
// nothing bounds how long a subject can be — so every path that renders
// one goes through here.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return strings.TrimRight(string(runes[:max-1]), " ") + "…"
}

// shortSHA abbreviates a commit sha to the 7 characters git and GitHub
// both use in their own UIs.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
