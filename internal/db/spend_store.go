package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SpendStore is a read-only aggregation over the llm_spend view (TFAC-472) —
// the unified shape that UNION-ALLs runs + curator_requests + system_llm_runs
// onto the category axis (autonomous / manual / curator / system_overhead) so
// the team dashboard + safety cap read from one place and totals reconcile with
// the Anthropic bill. It owns no table; the view is the abstraction boundary.
//
// Postgres / RLS: every method runs on the APP pool. The view is
// security_invoker, so the base tables' existing RLS scopes the read under the
// querying user's identity — a team member sees their team's runs but not
// another team's, with system/curator rows visible at org scope. Wiring it to
// the admin pool would bypass that and leak cross-team spend. SQLite is N=1 and
// unscoped; both impls take orgID and filter on it as defense in depth.
//
// Mirrors the read-only aggregation shape of PromptStore.Stats. See TFAC-449
// (the enterprise usage epic) for the consuming surfaces (dashboards, safety
// cap, org-wide EE pane) — all out of scope here; this is the spine they read.
type SpendStore interface {
	// ListSpend returns raw spend rows for orgID, newest-first, filtered by
	// opts (all optional — see domain.SpendFilter). Powers row-level / drill-down
	// reads; SpendByCategory is the cheaper path for the headline totals.
	ListSpend(ctx context.Context, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error)

	// SpendByCategory aggregates cost + the four token totals per category for
	// orgID over [since, now), one bucket per category present. This is the
	// dashboard's headline query and the cheap "org spend today" the safety cap
	// will key on.
	SpendByCategory(ctx context.Context, orgID string, since time.Time) ([]domain.SpendBucket, error)
}
