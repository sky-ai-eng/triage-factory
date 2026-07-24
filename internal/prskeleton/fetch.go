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
	// cap sets Skeleton.Truncated so the block says so rather than reading
	// as a short history.
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
	if opened := pr.openedEntry(); opened != nil {
		sk.Entries = append(sk.Entries, *opened)
	}

	for page := 1; page <= maxTimelinePages; page++ {
		path := fmt.Sprintf("%s/issues/%d/timeline?per_page=%d&page=%d",
			base, number, timelinePageSize, page)
		data, err := g.Get(ctx, path)
		if err != nil {
			// A partial timeline is still worth rendering: the header and
			// whatever pages landed beat no history at all. Mark it short
			// and stop rather than failing the whole fetch.
			sk.Truncated = true
			break
		}
		var items []timelineItem
		if err := json.Unmarshal(data, &items); err != nil {
			sk.Truncated = true
			break
		}
		for _, it := range items {
			if e, ok := it.entry(); ok {
				sk.Entries = append(sk.Entries, e)
			}
		}
		if len(items) < timelinePageSize {
			return sk, nil
		}
		if page == maxTimelinePages {
			sk.Truncated = true
		}
	}
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

	// Review fields.
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
	// comments not carried by a review submission.
	Comments []struct {
		CreatedAt string `json:"created_at"`
		User      *login `json:"user"`
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

// entry maps one timeline item onto the neutral model. ok=false drops the
// item: an event this package doesn't model (subscribed, mentioned,
// cross-referenced, milestoned — feed noise that says nothing about the
// PR's progress) or one with no usable timestamp.
func (t timelineItem) entry() (Entry, bool) {
	e := Entry{Count: 1, At: parseTime(t.CreatedAt)}
	actor := sanitize(t.Actor.name())

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
	case "line-commented":
		e.Kind = KindReviewComments
		e.Count = len(t.Comments)
		if e.Count == 0 {
			return Entry{}, false
		}
		for _, c := range t.Comments {
			if at := parseTime(c.CreatedAt); at.After(e.At) {
				e.At = at
			}
			e.Actors = addActor(e.Actors, sanitize(c.User.name()))
		}
		return e, !e.At.IsZero()
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
		return Entry{}, false
	}

	if e.At.IsZero() {
		return Entry{}, false
	}
	e.Actors = addActor(e.Actors, actor)
	return e, true
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
