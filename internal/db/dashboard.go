package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=DashboardStore --output=./mocks --case=underscore --with-expecter

// DashboardStore is a read-only projection over entities + their
// snapshot_json blobs. Doesn't own any table — every method
// aggregates data EntityStore would otherwise have to expose.
// Carved out as its own interface so the dashboard handler depends
// on a 2-method surface rather than pulling in the full
// EntityStore + JSON-snapshot reading.
//
// Both methods are viewer-relative: every aggregation attributes counts
// to "the user", so the caller's identity is a parameter and without it
// the totals are meaningless. Stats takes the GitHub username alone
// (a count of reviews given is a count about a login); PRs takes the
// PRViewer pair, because the list it answers is a union over both the
// login rows were authored under and the TF user runs were commissioned
// by. The handler resolves both from the auth context before calling.
type DashboardStore interface {
	// Stats returns aggregate PR counts (merged/closed/awaiting/
	// draft) for the user since `since`, plus reviews-given /
	// reviews-received totals and a 14-day merged-per-day timeline
	// for the sparkline. The window is the caller's — the store used
	// to hardcode 30 days behind a `sinceDays` int nobody could see,
	// so the panel's label and the query it described could drift.
	// A zero `since` means unbounded.
	Stats(ctx context.Context, orgID, username string, since time.Time) (*domain.DashboardStats, error)

	// PRs returns one page of the caller's PR summary rows, newest
	// last_polled_at first, plus the filtered total. Org-wide across
	// every polled repo — the personal view has no tracked-set gate,
	// because a pull request of mine in a repo no team of mine tracks
	// is still mine.
	//
	// "Mine" is a union of two legs, and viewer carries one id for each:
	// authored by my GitHub login, or commissioned by me (a run I asked
	// for opened it under the bot's login). Both are applied in SQL:
	// this read used to scan every GitHub entity in the org and drop
	// the non-matches in Go, which is a whole-table read per dashboard
	// load and cannot be paged (the window would be over the wrong set).
	//
	// f narrows within that population; an unknown state is
	// ErrUnknownPRState.
	PRs(ctx context.Context, orgID string, viewer PRViewer, f PRListFilter, opts ListOpts) ([]domain.PRSummaryRow, int, error)
}
