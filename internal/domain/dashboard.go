package domain

// DashboardStats holds aggregated PR statistics derived from entity
// snapshots. Returned by db.DashboardStore.Stats and rendered as JSON
// by /api/dashboard/stats.
type DashboardStats struct {
	Merged          int              `json:"merged"`
	Closed          int              `json:"closed"`
	Awaiting        int              `json:"awaiting"`
	Draft           int              `json:"draft"`
	ReviewsGiven    int              `json:"reviews_given"`
	ReviewsReceived int              `json:"reviews_received"`
	MergedOverTime  []DashboardPoint `json:"merged_over_time"`
}

// DashboardPoint is one bucket of the merged-over-time sparkline.
type DashboardPoint struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// PRSummaryRow is a PR as displayed on the dashboard list. Returned
// by db.DashboardStore.PRs and rendered as JSON by the dashboard PR list.
type PRSummaryRow struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Repo      string   `json:"repo"`
	Author    string   `json:"author"`
	State     string   `json:"state"`
	Draft     bool     `json:"draft"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	HTMLURL   string   `json:"html_url"`
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
