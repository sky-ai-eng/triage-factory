// Fetching a PR's history over the REST transport.
//
// Two calls: the PR object for the header, and the issue timeline for the
// entries. The timeline endpoint is the one that makes this cheap — it
// returns commits, reviews, comments and every lifecycle transition in one
// chronological feed, so there is no per-resource fan-out.
//
// The only capability required of a caller is Get(ctx, path), which both
// the server-side GitHub client and the sandboxed agent's host-routed
// adapter already satisfy — no new credential path, no new RPC.

package prskeleton

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Getter is the transport: an authenticated GET returning a raw response
// body. Deliberately the narrowest possible surface so this package stays
// independent of which side of the sandbox boundary it runs on.
type Getter interface {
	Get(ctx context.Context, path string) ([]byte, error)
}

const (
	timelinePageSize = 100
	// maxTimelinePages bounds one PR's timeline fetch. A five-year-old
	// epic PR must not turn run setup into an unbounded paging loop; 500
	// events is far past what any collapse tier renders, and hitting the
	// cap sets Skeleton.Truncation so the block says so rather than reading
	// as a short history.
	//
	// The feed is oldest-first and there is no way to page it in reverse
	// (the endpoint takes no direction, and the Link header that would
	// address the last page directly isn't visible through the Getter's
	// body-only surface), so a capped read keeps the oldest events and
	// loses the newest. That's the wrong end to lose, which is the reason
	// the ceiling sits far above any realistic PR rather than at a tighter
	// bound: it should essentially never be reached.
	maxTimelinePages = 5
)

// FetchPR builds a skeleton for one pull request.
func FetchPR(ctx context.Context, g Getter, owner, repo string, number int) (*Skeleton, error) {
	base := fmt.Sprintf("/repos/%s/%s", owner, repo)

	data, err := g.Get(ctx, fmt.Sprintf("%s/pulls/%d", base, number))
	if err != nil {
		return nil, fmt.Errorf("fetch PR %s#%d: %w", owner+"/"+repo, number, err)
	}
	var pr restPR
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("parse PR %s#%d: %w", owner+"/"+repo, number, err)
	}

	sk := &Skeleton{Header: pr.header(owner+"/"+repo, number)}

	// Rows are collected with their correlation keys and resolved once at the
	// end: an inline-comment batch can arrive on a different page from the
	// review it belongs to, so the join can't happen while paging.
	var rows []parsed
	if opened := pr.openedEntry(); opened != nil {
		rows = append(rows, parsed{entry: *opened})
	}

	for page := 1; page <= maxTimelinePages; page++ {
		path := fmt.Sprintf("%s/issues/%d/timeline?per_page=%d&page=%d",
			base, number, timelinePageSize, page)
		data, err := g.Get(ctx, path)
		if err != nil {
			// A partial timeline is still worth rendering: the header and
			// whatever pages landed beat no history at all. Record why it
			// stopped and return what we have, rather than failing the whole
			// fetch — but never let the shortfall pass as a complete history.
			sk.Truncation = TruncationFetchFailed
			break
		}
		var items []timelineItem
		if err := json.Unmarshal(data, &items); err != nil {
			sk.Truncation = TruncationParseFailed
			break
		}
		for _, it := range items {
			rows = append(rows, it.parse()...)
		}
		// A short page is the end of the feed — the only path that reads the
		// history to completion.
		if len(items) < timelinePageSize {
			break
		}
		if page == maxTimelinePages {
			sk.Truncation = TruncationPageCap
		}
	}

	sk.Entries = attachInlineComments(rows)
	return sk, nil
}

// restPR is the slice of GET /pulls/{n} the header needs.
type restPR struct {
	Title     string `json:"title"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	Merged    bool   `json:"merged"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      *struct {
		Login string `json:"login"`
	} `json:"user"`
	Head *struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base *struct {
		Ref string `json:"ref"`
	} `json:"base"`
	MergeableState string `json:"mergeable_state"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	ChangedFiles   int    `json:"changed_files"`
	Commits        int    `json:"commits"`
	Comments       int    `json:"comments"`
	ReviewComments int    `json:"review_comments"`
}

func (p restPR) header(repo string, number int) Header {
	h := Header{
		Repo:           repo,
		Number:         number,
		Title:          p.Title,
		Draft:          p.Draft,
		Mergeable:      p.MergeableState,
		Additions:      p.Additions,
		Deletions:      p.Deletions,
		ChangedFiles:   p.ChangedFiles,
		Commits:        p.Commits,
		Comments:       p.Comments,
		ReviewComments: p.ReviewComments,
		CreatedAt:      parseTime(p.CreatedAt),
		UpdatedAt:      parseTime(p.UpdatedAt),
	}
	// REST reports state as open/closed and merge as a separate boolean;
	// the three-way vocabulary is what a reader actually wants.
	switch {
	case p.Merged:
		h.State = "MERGED"
	case p.State != "":
		h.State = upper(p.State)
	}
	if p.User != nil {
		h.Author = p.User.Login
	}
	if p.Head != nil {
		h.HeadRef = p.Head.Ref
		h.HeadSHA = shortSHA(p.Head.SHA)
	}
	if p.Base != nil {
		h.BaseRef = p.Base.Ref
	}
	return h
}

// openedEntry synthesizes the timeline's first row. GitHub's timeline feed
// does not include the opening itself, and without it a rendered history
// starts mid-story.
func (p restPR) openedEntry() *Entry {
	at := parseTime(p.CreatedAt)
	if at.IsZero() {
		return nil
	}
	e := Entry{Kind: KindOpened, At: at, Count: 1}
	if p.User != nil {
		e.Actors = addActor(nil, sanitize(p.User.Login))
	}
	return &e
}

// timelineItem is the union of every timeline event shape this package
// maps. Fields are pointers or plain strings so an absent one decodes to
// its zero value — the feed is heterogeneous by design and each event type
// populates a different subset.
type timelineItem struct {
	Event     string `json:"event"`
	CreatedAt string `json:"created_at"`

	Actor *login `json:"actor"`
	User  *login `json:"user"`

	// Review fields. On a reviewed event the item IS the review, so ID is
	// the review's id — the key an inline-comment batch names to find it.
	ID          int64  `json:"id"`
	State       string `json:"state"`
	SubmittedAt string `json:"submitted_at"`

	// Commit fields. Author here is a git identity (name/email/date), not
	// an account — a commit's timeline entry has no actor.
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  *struct {
		Name string `json:"name"`
		Date string `json:"date"`
	} `json:"author"`

	Label *struct {
		Name string `json:"name"`
	} `json:"label"`

	RequestedReviewer *login `json:"requested_reviewer"`
	RequestedTeam     *struct {
		Slug string `json:"slug"`
	} `json:"requested_team"`

	Rename *struct {
		To string `json:"to"`
	} `json:"rename"`

	// Comments is populated on a line-commented event: a batch of inline
	// comments. Each names the review it was submitted under, which is what
	// lets the batch fold onto its review row.
	Comments []struct {
		CreatedAt           string `json:"created_at"`
		User                *login `json:"user"`
		PullRequestReviewID int64  `json:"pull_request_review_id"`
	} `json:"comments"`
}

type login struct {
	Login string `json:"login"`
}

func (l *login) name() string {
	if l == nil {
		return ""
	}
	return l.Login
}

// parsed is one mapped row plus the key that joins an inline-comment batch
// to the review it was submitted under. reviewID is the review's own id on a
// review row, the parent review's id on a batch, and empty everywhere else
// (and on either side when the source didn't report one).
type parsed struct {
	entry    Entry
	reviewID string
}

// parse maps one timeline item onto zero or more rows. Most items yield one;
// a line-commented item yields one per review its comments belong to, so a
// batch spanning two reviews can't collapse two reviewers into one row.
func (t timelineItem) parse() []parsed {
	if t.Event == "line-commented" {
		return t.inlineBatches()
	}
	e, key, ok := t.single()
	if !ok {
		return nil
	}
	return []parsed{{entry: e, reviewID: key}}
}

// inlineBatches splits one line-commented item into a row per parent review.
func (t timelineItem) inlineBatches() []parsed {
	var order []int64
	byReview := map[int64]*Entry{}
	for _, c := range t.Comments {
		at := parseTime(c.CreatedAt)
		if at.IsZero() {
			continue
		}
		e, seen := byReview[c.PullRequestReviewID]
		if !seen {
			e = &Entry{Kind: KindReviewComments}
			byReview[c.PullRequestReviewID] = e
			order = append(order, c.PullRequestReviewID)
		}
		e.Count++
		if at.After(e.At) {
			e.At = at
		}
		e.Actors = addActor(e.Actors, sanitize(c.User.name()))
	}

	out := make([]parsed, 0, len(order))
	for _, id := range order {
		e := byReview[id]
		key := ""
		if id != 0 {
			key = strconv.FormatInt(id, 10)
		}
		out = append(out, parsed{entry: *e, reviewID: key})
	}
	return out
}

// single maps a non-batch timeline item. ok=false drops the item: an event
// this package doesn't model (subscribed, mentioned, cross-referenced,
// milestoned — feed noise that says nothing about the PR's progress) or one
// with no usable timestamp.
func (t timelineItem) single() (Entry, string, bool) {
	e := Entry{Count: 1, At: parseTime(t.CreatedAt)}
	actor := sanitize(t.Actor.name())
	reviewID := ""

	switch t.Event {
	case "committed":
		e.Kind = KindCommit
		e.Ref = shortSHA(t.SHA)
		e.Detail = sanitize(firstLine(t.Message))
		if t.Author != nil {
			e.At = parseTime(t.Author.Date)
			actor = sanitize(t.Author.Name)
		}
	case "reviewed":
		e.Kind = KindReview
		e.Detail = upper(t.State)
		e.At = parseTime(t.SubmittedAt)
		actor = sanitize(t.User.name())
		if t.ID != 0 {
			reviewID = strconv.FormatInt(t.ID, 10)
		}
	case "commented":
		e.Kind = KindComment
		actor = sanitize(t.Actor.name())
		if actor == "" {
			actor = sanitize(t.User.name())
		}
	case "labeled", "unlabeled":
		e.Kind = KindLabeled
		if t.Event == "unlabeled" {
			e.Kind = KindUnlabeled
		}
		if t.Label != nil {
			e.Detail = sanitize(t.Label.Name)
		}
	case "review_requested", "review_request_removed":
		e.Kind = KindReviewRequested
		if t.Event == "review_request_removed" {
			e.Kind = KindReviewRequestRemoved
		}
		switch {
		case t.RequestedReviewer != nil:
			e.Detail = "→ " + sanitize(t.RequestedReviewer.Login)
		case t.RequestedTeam != nil:
			e.Detail = "→ team " + sanitize(t.RequestedTeam.Slug)
		}
	case "ready_for_review":
		e.Kind = KindReadyForReview
	case "convert_to_draft":
		e.Kind = KindDraft
	case "head_ref_force_pushed":
		e.Kind = KindForcePush
	case "renamed":
		e.Kind = KindRenamed
		if t.Rename != nil {
			e.Detail = truncate(sanitize(t.Rename.To), DefaultMaxSubject)
		}
	case "base_ref_changed":
		e.Kind = KindBaseChanged
	case "merged":
		e.Kind = KindMerged
	case "closed":
		e.Kind = KindClosed
	case "reopened":
		e.Kind = KindReopened
	default:
		return Entry{}, "", false
	}

	if e.At.IsZero() {
		return Entry{}, "", false
	}
	e.Actors = addActor(e.Actors, actor)
	return e, reviewID, true
}

// attachInlineComments folds each inline-comment batch onto the review it was
// submitted under, so a review renders as one row carrying its verdict and
// its comment count instead of two rows a reader has to associate by
// adjacency.
//
// A batch survives as its own row when its parent can't be resolved — the
// source didn't report the linkage, or the parent review fell outside the
// pages that were read. That fallback is the reason the join is best-effort
// rather than assumed: worst case the block reads as it did before.
func attachInlineComments(rows []parsed) []Entry {
	reviewAt := make(map[string]int, len(rows))
	for i, p := range rows {
		if p.entry.Kind != KindReview || p.reviewID == "" {
			continue
		}
		if _, dup := reviewAt[p.reviewID]; !dup {
			reviewAt[p.reviewID] = i
		}
	}

	folded := make([]bool, len(rows))
	for i, p := range rows {
		if p.entry.Kind != KindReviewComments || p.reviewID == "" {
			continue
		}
		if idx, ok := reviewAt[p.reviewID]; ok {
			rows[idx].entry.Inline += p.entry.Count
			folded[i] = true
		}
	}

	out := make([]Entry, 0, len(rows))
	for i := range rows {
		if !folded[i] {
			out = append(out, rows[i].entry)
		}
	}
	return out
}

// parseTime decodes GitHub's RFC3339 timestamps, yielding the zero time on
// anything unparseable so a malformed field drops its entry rather than
// sorting it to the epoch.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// firstLine takes a commit message's subject. The body is never rendered.
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}

func upper(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'z' {
			out[i] -= 'a' - 'A'
		}
	}
	return string(out)
}
