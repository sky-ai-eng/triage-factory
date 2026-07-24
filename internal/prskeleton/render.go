// Collapse tiers and text rendering. The renderer is a pure function of
// (skeleton, options) — including the clock, which arrives via Options.Now
// rather than a time.Now() call inside, so goldens are deterministic.

package prskeleton

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Defaults for Options. MaxLines is the timeline's line budget, not the
// block's: the header is always rendered in full on top of it.
const (
	DefaultMaxLines   = 32
	DefaultMaxSubject = 72

	// maxFoldActors is how many distinct logins a fold names before it
	// switches to a "+N more" tail. Three covers the common shapes (one
	// author, an author and a reviewer, a pairing) without letting a
	// 40-author elision span run the line off the edge.
	maxFoldActors = 3

	// maxFoldLabels bounds the label names a labeled/unlabeled fold lists.
	// Label names are short by construction, so this is generous.
	maxFoldLabels = 5
)

// Options controls collapse aggressiveness and time rendering.
type Options struct {
	// MaxLines is the timeline line budget. Zero means DefaultMaxLines.
	MaxLines int
	// MaxSubject caps a rendered commit subject in runes. Zero means
	// DefaultMaxSubject. Commit subjects are the only free text a line
	// carries and nothing bounds their length at the source.
	MaxSubject int
	// Now anchors relative ages ("22d ago"). Zero omits ages entirely —
	// the absolute timestamps still render.
	Now time.Time
}

func (o Options) maxLines() int {
	if o.MaxLines <= 0 {
		return DefaultMaxLines
	}
	return o.MaxLines
}

func (o Options) maxSubject() int {
	if o.MaxSubject <= 0 {
		return DefaultMaxSubject
	}
	return o.MaxSubject
}

// tier names the collapse level a render landed on, reported in the block's
// own note line so an agent knows whether it's looking at everything.
type tier int

const (
	tierFull tier = iota
	tierFolded
	tierWindowed
)

// Render produces the whole block: header, a note describing how much of
// the history survived, and the collapsed timeline.
//
// Three tiers, escalated only as far as the budget forces:
//
//   - full: every entry gets its own line.
//   - folded: consecutive same-kind non-landmark entries collapse into one
//     row carrying a count, an author set, and the span they cover.
//   - windowed: landmarks are retained preferentially and the gaps between
//     what's kept collapse into elision rows carrying their duration and a
//     per-kind breakdown.
func Render(sk Skeleton, opts Options) string {
	entries := make([]Entry, len(sk.Entries))
	copy(entries, sk.Entries)
	sortEntries(entries)

	total := 0
	for _, e := range entries {
		total += max(e.Count, 1)
	}

	budget := opts.maxLines()
	rows, lvl := entries, tierFull
	if len(rows) > budget {
		rows, lvl = foldRuns(entries), tierFolded
	}
	if len(rows) > budget {
		rows, lvl = window(rows, budget), tierWindowed
	}

	var b strings.Builder
	writeHeader(&b, sk, opts)
	b.WriteString(noteLine(sk, lvl, total, len(rows)))
	b.WriteString("\n")
	if len(rows) == 0 {
		b.WriteString("  (no recorded history)\n")
		return b.String()
	}
	b.WriteString("\n")
	writeRows(&b, rows, opts)
	return b.String()
}

func writeHeader(b *strings.Builder, sk Skeleton, opts Options) {
	h := sk.Header
	state := h.State
	if h.Draft {
		state += " (draft)"
	}
	fmt.Fprintf(b, "Pull request %s#%d %q — %s\n",
		h.Repo, h.Number, truncate(sanitize(h.Title), 120), state)

	line2 := []string{}
	if h.Author != "" {
		line2 = append(line2, "opened by "+sanitize(h.Author))
	}
	if h.HeadRef != "" && h.BaseRef != "" {
		branches := sanitize(h.HeadRef) + " → " + sanitize(h.BaseRef)
		if h.HeadSHA != "" {
			branches += " @ " + h.HeadSHA
		}
		line2 = append(line2, branches)
	}
	line2 = append(line2, fmt.Sprintf("+%d −%d in %d files", h.Additions, h.Deletions, h.ChangedFiles))
	if h.Mergeable != "" {
		line2 = append(line2, "mergeable: "+sanitize(h.Mergeable))
	}
	fmt.Fprintf(b, "  %s\n", strings.Join(line2, " · "))

	line3 := []string{}
	if !h.CreatedAt.IsZero() {
		line3 = append(line3, "opened "+stamp(h.CreatedAt, opts.Now))
	}
	if !h.UpdatedAt.IsZero() {
		line3 = append(line3, "last activity "+stamp(h.UpdatedAt, opts.Now))
	}
	if len(line3) > 0 {
		fmt.Fprintf(b, "  %s\n", strings.Join(line3, " · "))
	}
}

// noteLine states what the reader is looking at and where the detail lives.
// It always renders, including on the full tier — an agent shouldn't have to
// infer from row count whether anything was dropped, and the pointers to
// `git log` / `pr view -v` are what make the bounded block safe to rely on.
func noteLine(sk Skeleton, lvl tier, total, rows int) string {
	var b strings.Builder
	switch lvl {
	case tierFull:
		fmt.Fprintf(&b, "  %s, shown in full.", plural(total, "event", "events"))
	case tierFolded:
		fmt.Fprintf(&b, "  %s, folded to %d lines — runs of like events are collapsed to counts.",
			plural(total, "event", "events"), rows)
	case tierWindowed:
		fmt.Fprintf(&b, "  %s, folded and windowed to %d lines — elided spans are marked below.",
			plural(total, "event", "events"), rows)
	}
	// Name the cause, not just the fact. An agent weighing whether to trust
	// the block is served differently by "this PR is longer than the block
	// ever shows" than by "the read broke" — and both mean the newest
	// activity is what's absent, which is the part that changes decisions.
	switch sk.Truncation {
	case TruncationPageCap:
		b.WriteString(" This PR's history is longer than the fetch reads: only the earliest events are shown and the most recent activity is missing.")
	case TruncationFetchFailed:
		b.WriteString(" The history could not be read to the end (a timeline page failed to fetch), so the most recent activity is missing.")
	case TruncationParseFailed:
		b.WriteString(" The history could not be read to the end (a timeline page could not be parsed), so the most recent activity is missing.")
	}
	if lvl != tierFull || sk.Truncation != TruncationNone {
		b.WriteString(" For detail: `git log` in the worktree, or `exec gh pr view -v` for review and comment bodies.")
	}
	return b.String()
}

// foldRuns is tier 2: consecutive entries sharing a fold key collapse into
// one row. Landmarks have no fold key and pass through untouched, which is
// also what keeps a fold from spanning across one — five commits, an
// approval, then five more commits stays three rows, not one.
//
// A fold deliberately carries no commit subjects. Which two of the twelve
// subjects would it even show, and each is unbounded at the source; the
// count, the authors, and the span are the whole signal at this density.
func foldRuns(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		key, foldable := entries[i].foldKey()
		if !foldable {
			out = append(out, entries[i])
			continue
		}
		j := i + 1
		for j < len(entries) {
			k, ok := entries[j].foldKey()
			if !ok || k != key {
				break
			}
			j++
		}
		if j-i == 1 {
			out = append(out, entries[i])
			continue
		}
		out = append(out, mergeRun(entries[i:j]))
		i = j - 1
	}
	return out
}

// mergeRun collapses run into a single entry. Since is the first member's
// time, At the last's, so the rendered row spans the real interval rather
// than pinning to whichever end happened to sort first.
func mergeRun(run []Entry) Entry {
	merged := Entry{
		Kind:  run[0].Kind,
		At:    run[len(run)-1].At,
		Since: run[0].At,
	}
	details := make([]string, 0, len(run))
	for _, e := range run {
		merged.Count += max(e.Count, 1)
		for _, a := range e.Actors {
			merged.Actors = addActor(merged.Actors, a)
		}
		if e.Detail != "" {
			details = append(details, e.Detail)
		}
	}
	// Label folds list the names they touched — short by construction and the
	// only case where the individual values still matter at a glance.
	// Everything else (commits, comments) drops its details entirely.
	switch merged.Kind {
	case KindLabeled, KindUnlabeled:
		merged.Detail = joinCapped(details, maxFoldLabels)
	}
	return merged
}

// window is tier 3: keep as much as the budget allows, preferring
// landmarks, then recency; replace each contiguous run of dropped entries
// with one elision row.
//
// The keep count is searched downward because each elision row costs a line
// too — keeping k entries can cost k+g rendered lines, and g isn't known
// until the keep set is chosen. The loop is bounded by the budget.
func window(entries []Entry, budget int) []Entry {
	if len(entries) <= budget {
		return entries
	}
	for k := budget; k >= 1; k-- {
		keep := chooseKeep(entries, k)
		if gaps := countGaps(entries, keep) + k; gaps <= budget {
			return applyKeep(entries, keep)
		}
	}
	// Budget too small for even one entry plus its elision rows: report the
	// whole history as a single elided span rather than an empty timeline.
	return applyKeep(entries, map[int]bool{})
}

// chooseKeep picks at most k indices to retain: every landmark first
// (most-recent-first when landmarks alone overflow k), then the most recent
// non-landmarks to fill what's left. Recency is the tiebreak because the
// agent is acting on the PR's current state; landmarks outrank it because
// an approval three weeks back still governs what the PR is today.
func chooseKeep(entries []Entry, k int) map[int]bool {
	keep := make(map[int]bool, k)
	landmarks := make([]int, 0, len(entries))
	rest := make([]int, 0, len(entries))
	for i, e := range entries {
		if e.landmark() {
			landmarks = append(landmarks, i)
		} else {
			rest = append(rest, i)
		}
	}
	take := func(idx []int) {
		for i := len(idx) - 1; i >= 0 && len(keep) < k; i-- {
			keep[idx[i]] = true
		}
	}
	take(landmarks)
	take(rest)
	return keep
}

// countGaps counts the contiguous runs of dropped entries, each of which
// renders as one elision row.
func countGaps(entries []Entry, keep map[int]bool) int {
	gaps, inGap := 0, false
	for i := range entries {
		if keep[i] {
			inGap = false
			continue
		}
		if !inGap {
			gaps++
			inGap = true
		}
	}
	return gaps
}

// applyKeep walks the timeline in order, emitting kept entries verbatim and
// folding each dropped run into one KindElided row.
func applyKeep(entries []Entry, keep map[int]bool) []Entry {
	out := make([]Entry, 0, len(keep)*2+1)
	var pending []Entry
	flush := func() {
		if len(pending) > 0 {
			out = append(out, elide(pending))
			pending = nil
		}
	}
	for i, e := range entries {
		if keep[i] {
			flush()
			out = append(out, e)
			continue
		}
		pending = append(pending, e)
	}
	flush()
	return out
}

// elide summarizes a dropped span: its duration and what was in it.
func elide(span []Entry) Entry {
	counts := map[Kind]int{}
	e := Entry{Kind: KindElided, Since: span[0].At, At: span[len(span)-1].At}
	for _, s := range span {
		n := max(s.Count, 1)
		e.Count += n
		if s.Kind == KindElided {
			for _, p := range s.Parts {
				counts[p.Kind] += p.Count
			}
			continue
		}
		counts[s.Kind] += n
	}
	for k, n := range counts {
		e.Parts = append(e.Parts, KindCount{Kind: k, Count: n})
	}
	sort.Slice(e.Parts, func(i, j int) bool {
		if e.Parts[i].Count != e.Parts[j].Count {
			return e.Parts[i].Count > e.Parts[j].Count
		}
		return e.Parts[i].Kind < e.Parts[j].Kind
	})
	return e
}

// writeRows renders the timeline as three columns — when, what, who/detail —
// padded to the widest cell so the block scans vertically.
func writeRows(b *strings.Builder, rows []Entry, opts Options) {
	type cell struct{ when, what, who string }
	cells := make([]cell, 0, len(rows))
	whenW, whatW := 0, 0
	// Ages repeat heavily on a dense timeline — twenty rows all "22d ago"
	// is column noise that buries the one row where the age changed. Render
	// each age once, on the first row that carries it.
	prevAge := ""
	for _, e := range rows {
		when := absStamp(e.At)
		if age := ageSuffix(e.At, opts.Now); age != "" && age != prevAge {
			when += " " + age
			prevAge = age
		}
		// Sanitize here rather than trusting the entries: Render is this
		// package's public surface and a Skeleton can be built by hand, so
		// the "one entry per line" structure must be guaranteed by the code
		// that emits the lines, not by whoever populated Actors and Detail.
		// One choke point covers every kind branch, including ones added
		// later. The `when` cell is machine-generated and needs no pass.
		c := cell{when: when, what: sanitize(whatCell(e)), who: sanitize(whoCell(e, opts))}
		whenW = max(whenW, len(c.when))
		whatW = max(whatW, len(c.what))
		cells = append(cells, c)
	}
	for _, c := range cells {
		line := "  " + pad(c.when, whenW) + "  " + pad(c.what, whatW)
		if c.who != "" {
			line += "  " + c.who
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
}

// whatCell is the middle column: what kind of thing happened, carrying a
// count only when the row is a fold. A single row's count is 1 by
// definition, and "1 commit" is noise where "commit" reads.
func whatCell(e Entry) string {
	n := max(e.Count, 1)
	switch e.Kind {
	case KindOpened:
		return "opened"
	case KindCommit:
		return counted(n, "commit", "commits")
	case KindForcePush:
		return "force-push"
	case KindReview:
		// Never folded (a landmark), so the state is always one review's, and
		// Inline is that review's own comment count — joined at fetch time so
		// the verdict and the weight of the feedback read as one fact.
		if e.Inline > 0 {
			return fmt.Sprintf("review %s (%d inline)", e.Detail, e.Inline)
		}
		return "review " + e.Detail
	case KindReviewComments:
		// Only reached when a batch couldn't be joined to its review. Count
		// comes from the batch size, not from folding: one row is one
		// review's inline comments, and separate reviews stay separate rows.
		return counted(n, "review comment", "review comments")
	case KindComment:
		return counted(n, "comment", "comments")
	case KindReviewRequested:
		return counted(n, "review requested", "reviews requested")
	case KindReviewRequestRemoved:
		return counted(n, "request removed", "requests removed")
	case KindLabeled:
		return counted(n, "label added", "labels added")
	case KindUnlabeled:
		return counted(n, "label removed", "labels removed")
	case KindDraft:
		return "converted to draft"
	case KindReadyForReview:
		return "ready for review"
	case KindRenamed:
		return "renamed"
	case KindBaseChanged:
		return "base changed"
	case KindMerged:
		return "merged"
	case KindClosed:
		return "closed"
	case KindReopened:
		return "reopened"
	case KindElided:
		return fmt.Sprintf("… %d events elided", n)
	default:
		return string(e.Kind)
	}
}

// whoCell is the trailing column: actors, plus whatever kind-specific
// detail is bounded enough to render.
func whoCell(e Entry, opts Options) string {
	if e.Kind == KindElided {
		return elidedDetail(e, opts)
	}

	var parts []string
	if who := joinCapped(e.Actors, maxFoldActors); who != "" {
		parts = append(parts, who)
	}
	switch e.Kind {
	case KindCommit:
		// Subjects render only on a per-commit row, where exactly one is in
		// play and the cap bounds it. Folds carry none.
		if max(e.Count, 1) == 1 {
			if e.Ref != "" {
				parts = append(parts, e.Ref)
			}
			if e.Detail != "" {
				parts = append(parts, strconv.Quote(truncate(e.Detail, opts.maxSubject())))
			}
		}
	case KindReviewRequested, KindReviewRequestRemoved, KindLabeled, KindUnlabeled, KindRenamed:
		if e.Detail != "" {
			parts = append(parts, e.Detail)
		}
	}
	if span := spanOf(e); span != "" {
		parts = append(parts, span)
	}
	return strings.Join(parts, " · ")
}

// elidedDetail renders a dropped span's age and composition — the reason
// the windowed tier is worth having. "34 events over 12 days" and "34
// events over 40 minutes" describe entirely different pull requests.
func elidedDetail(e Entry, opts Options) string {
	var when []string
	if d := e.At.Sub(e.Since); d > 0 {
		when = append(when, "over "+humanDuration(d))
	}
	if !e.At.IsZero() && !opts.Now.IsZero() {
		when = append(when, "ending "+humanAge(opts.Now.Sub(e.At))+" ago")
	}
	names := make([]string, 0, len(e.Parts))
	for _, p := range e.Parts {
		names = append(names, plural(p.Count, kindNoun(p.Kind), kindNounPlural(p.Kind)))
	}

	switch {
	case len(when) > 0 && len(names) > 0:
		return strings.Join(when, ", ") + ": " + strings.Join(names, ", ")
	case len(names) > 0:
		return strings.Join(names, ", ")
	default:
		return strings.Join(when, ", ")
	}
}

// spanOf renders a fold's covered interval. It shows the clock time only
// when the fold starts and ends on the same day, since the row's own
// timestamp already carries the date.
func spanOf(e Entry) string {
	if e.Since.IsZero() || !e.Since.Before(e.At) {
		return ""
	}
	if e.Since.UTC().Format("2006-01-02") == e.At.UTC().Format("2006-01-02") {
		return e.Since.UTC().Format("15:04") + "→" + e.At.UTC().Format("15:04")
	}
	return "over " + humanDuration(e.At.Sub(e.Since))
}

// stamp renders an absolute UTC timestamp with a relative age when a clock
// was supplied. The age is what stops an agent doing date arithmetic to
// work out that the approval it's looking at is three weeks stale.
func stamp(t, now time.Time) string {
	s := absStamp(t)
	if age := ageSuffix(t, now); age != "" {
		return s + " " + age
	}
	return s
}

func absStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

// ageSuffix is the parenthesized relative age, empty when no clock was
// supplied (the injectable-time contract) or the time is unknown.
func ageSuffix(t, now time.Time) string {
	if t.IsZero() || now.IsZero() {
		return ""
	}
	return "(" + humanAge(now.Sub(t)) + " ago)"
}

// humanAge renders a duration at one significant unit — precision past that
// is noise for "how stale is this".
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	case d < 90*24*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	default:
		return strconv.Itoa(int(d.Hours()/24/30)) + "mo"
	}
}

// humanDuration renders a span the same way, minus the "just now" case that
// only makes sense for an age.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	return humanAge(d)
}

func kindNoun(k Kind) string {
	switch k {
	case KindCommit:
		return "commit"
	case KindComment:
		return "comment"
	case KindReviewComments:
		return "review comment"
	case KindReview:
		return "review"
	case KindLabeled:
		return "label added"
	case KindUnlabeled:
		return "label removed"
	case KindReviewRequested:
		return "review request"
	case KindReviewRequestRemoved:
		return "review request removal"
	default:
		return strings.ReplaceAll(string(k), "-", " ")
	}
}

func kindNounPlural(k Kind) string {
	switch k {
	case KindCommit:
		return "commits"
	case KindComment:
		return "comments"
	case KindReviewComments:
		return "review comments"
	case KindReview:
		return "reviews"
	case KindLabeled:
		return "labels added"
	case KindUnlabeled:
		return "labels removed"
	case KindReviewRequested:
		return "review requests"
	case KindReviewRequestRemoved:
		return "review request removals"
	default:
		return strings.ReplaceAll(string(k), "-", " ")
	}
}

// joinCapped lists up to max items and reports the remainder as "+N more",
// so an unbounded set can't run a line off the edge.
func joinCapped(items []string, max int) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" (+%d more)", len(items)-max)
}

// plural always carries the number — right for a breakdown, where "1
// commit" is the datum.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// counted drops the number at one — right for a timeline row, where a
// count is meaningful only as evidence of a fold.
func counted(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return strconv.Itoa(n) + " " + many
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
