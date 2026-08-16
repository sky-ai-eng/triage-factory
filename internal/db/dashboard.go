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
// Both methods take the GitHub username because every aggregation
// attributes counts to "the user" — without it the totals are
// meaningless. The handler reads username from the auth context
// before calling.
type DashboardStore interface {
	// Stats returns aggregate PR counts (merged/closed/awaiting/
	// draft) for the user since `since`, plus reviews-given /
	// reviews-received totals and a 14-day merged-per-day timeline
	// for the sparkline. The window is the caller's — the store used
	// to hardcode 30 days behind a `sinceDays` int nobody could see,
	// so the panel's label and the query it described could drift.
	// A zero `since` means unbounded.
	Stats(ctx context.Context, orgID, username string, since time.Time) (*domain.DashboardStats, error)

	// PRs returns one page of the PR summary rows authored by
	// username, newest last_polled_at first, plus the filtered total.
	// The author filter is applied in SQL: this read used to scan
	// every GitHub entity in the org and drop the non-matches in Go,
	// which is a whole-table read per dashboard load and cannot be
	// paged (the window would be over the wrong set).
	PRs(ctx context.Context, orgID, username string, opts ListOpts) ([]domain.PRSummaryRow, int, error)
}
