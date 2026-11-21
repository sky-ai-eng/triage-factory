package domain

// DashboardStats holds aggregated PR statistics derived from entity
// snapshots. Returned by db.DashboardStore.Stats and rendered as JSON
// by /api/dashboard/stats.
type DashboardStats struct {
	Merged          int              `json:"merged" doc:"How many of the caller's pull requests merged within the window."`
	Closed          int              `json:"closed" doc:"How many of the caller's pull requests closed without merging within the window."`
	Awaiting        int              `json:"awaiting" doc:"How many of the caller's pull requests are open and ready for review."`
	Draft           int              `json:"draft" doc:"How many of the caller's pull requests are open drafts."`
	ReviewsGiven    int              `json:"reviews_given" doc:"How many reviews the caller has left on other people's pull requests."`
	ReviewsReceived int              `json:"reviews_received" doc:"How many reviews other people have left on the caller's pull requests."`
	MergedOverTime  []DashboardPoint `json:"merged_over_time" doc:"How many of the caller's pull requests merged on each of the last 14 days, oldest first, counting only merges within the window; null when the caller has no GitHub identity bound."`
}

// DashboardPoint is one bucket of the merged-over-time sparkline.
type DashboardPoint struct {
	Date  string `json:"date" doc:"The day, as a YYYY-MM-DD date in UTC."`
	Count int    `json:"count" doc:"How many of the caller's pull requests merged that day."`
}

// PRSummaryRow is a PR as displayed on the dashboard list. Returned
// by db.DashboardStore.PRs and rendered as JSON by the dashboard PR list.
type PRSummaryRow struct {
	// EntityID is the pull request's entity, the id a client addresses
	// the PR by when it calls back about it (the dashboard's per-PR
	// routes). Repo and Number are display values.
	EntityID  string   `json:"entity_id" doc:"The id of the pull request's entity, which the per-pull-request routes take."`
	Number    int      `json:"number" doc:"The pull request's number in its repository."`
	Title     string   `json:"title" doc:"The pull request's title."`
	Repo      string   `json:"repo" doc:"The repository, as owner/name."`
	Author    string   `json:"author" doc:"The GitHub login of the pull request's author."`
	State     string   `json:"state" doc:"The pull request's state: open, merged, or closed."`
	Draft     bool     `json:"draft" doc:"Whether the pull request is a draft."`
	Labels    []string `json:"labels" doc:"The names of the pull request's labels."`
	CreatedAt string   `json:"created_at" doc:"When the pull request was opened, as an RFC 3339 timestamp."`
	UpdatedAt string   `json:"updated_at" doc:"When the pull request last changed on GitHub, as an RFC 3339 timestamp."`
	HTMLURL   string   `json:"html_url" doc:"The pull request's page on GitHub."`
}

// The pull-request list's state vocabulary. These are the values
// PRSummaryRow.State renders AND the values the list routes' `states` filter
// accepts, deliberately the same set: a client narrows by what it reads back,
// rather than learning a second spelling of the same three answers.
//
// GitHub reports a merged pull request as CLOSED with merged=true on one wire
// shape and as MERGED on another, so "merged" is a state of its own here and
// "closed" means closed-and-not-merged. A snapshot state outside the three
// passes through PRSummaryFromSnapshot verbatim rather than being folded into
// one of them, so a value the UI cannot render stays legible in a bug report.
const (
	PRStateOpen   = "open"
	PRStateMerged = "merged"
	PRStateClosed = "closed"
)

// PRListStates is that vocabulary as a list, for the strict validation the
// pull-request list routes do on their `states` filter — and for the message
// they name the accepted values in when a caller misses.
var PRListStates = []string{PRStateOpen, PRStateMerged, PRStateClosed}

// PRSummaryFromSnapshot projects a stored PR snapshot onto the dashboard row.
// It lives here rather than in each dialect because the projection is between
// two domain types and has nothing to do with either backend — the two copies
// that used to sit in the store packages were a place for the row shapes to
// drift apart by one field.
func PRSummaryFromSnapshot(snap PRSnapshot) PRSummaryRow {
	// The dashboard renders lowercase states, and "merged" is a state of its
	// own there though GitHub reports a merged PR as CLOSED with merged=true.
	// An unrecognized state passes through verbatim rather than being
	// lowercased, so a value the UI can't render stays legible in a bug report.
	state := snap.State
	switch state {
	case "OPEN":
		state = PRStateOpen
	case "CLOSED":
		state = PRStateClosed
	case "MERGED":
		state = PRStateMerged
	}
	if snap.Merged {
		state = PRStateMerged
	}
	return PRSummaryRow{
		Number:    snap.Number,
		Title:     snap.Title,
		Repo:      snap.Repo,
		Author:    snap.Author,
		State:     state,
		Draft:     snap.IsDraft,
		Labels:    snap.Labels,
		CreatedAt: snap.CreatedAt,
		UpdatedAt: snap.UpdatedAt,
		HTMLURL:   snap.URL,
	}
}
