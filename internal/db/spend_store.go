package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SpendStore is a read-only aggregation over the llm_spend view (TFAC-472) —
// the unified shape that UNION-ALLs conversation messages + system_llm_runs
// onto the category axis (autonomous / manual / curator / system_overhead) so
// the team dashboard + safety cap read from one place and totals reconcile with
// the Anthropic bill. It owns no table; the view is the abstraction boundary.
//
// Postgres / RLS: every method runs on the APP pool. The view is
// security_invoker, so the base tables' existing RLS scopes the read under the
// querying user's identity — a team member sees their team's spend but not
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
	// APP pool in Postgres (RLS-active) — team-scoped under the caller's claims.
	ListSpend(ctx context.Context, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error)

	// ListSpendSystem is ListSpend on the ADMIN pool (BYPASSRLS) in Postgres —
	// the authorized cross-RLS read the role-gated team / org usage endpoints
	// need (TFAC-478). An org admin may not be a member of the team they're
	// inspecting, so an app-pool ListSpend would exclude that team's rows; the
	// HTTP role gate is the authorization, this System read is the authorized
	// path past RLS. Same opts contract as ListSpend (a non-nil TeamID narrows
	// to one team; an empty filter is the whole org). Mirrors
	// SpendByCategorySystem / artifactStore.ListByConversationSystem. SQLite is N=1 / no
	// RLS, so it delegates to ListSpend.
	ListSpendSystem(ctx context.Context, orgID string, opts domain.SpendFilter) ([]domain.SpendRow, error)

	// SpendByCategory aggregates cost + the four token totals per category for
	// orgID over the window [since, until), one bucket per category present.
	// Both bounds are optional, matching SpendFilter: a zero since imposes no
	// lower bound (all-time), a zero until no upper bound (up to now). So
	// SpendByCategory(…, since, time.Time{}) is the open "spend since X" the
	// dashboard headline + safety cap key on, and a non-zero until gives the
	// closed billing-period window (e.g. all of March) that reconciles against
	// the Anthropic bill. This is the cheap aggregate; ListSpend is the
	// row-level drill-down. APP pool in Postgres (RLS-active) — team-scoped.
	SpendByCategory(ctx context.Context, orgID string, since, until time.Time) ([]domain.SpendBucket, error)

	// SpendByCategorySystem is SpendByCategory on the ADMIN pool (BYPASSRLS) in
	// Postgres — the org-wide aggregate a claims-less system caller needs. The
	// safety cap (TFAC-477) reads it from Spawner.Delegate, which runs under
	// context.Background() with no tf.current_org_id(): an app-pool read there
	// would see nothing and the cap would never trip, so it MUST use this
	// variant. Returns spend summed across EVERY team + curator + system row in
	// the org (the runaway-spend fuse counts all categories). Mirrors
	// OrgsStore.GetSettingsSystem / ArtifactStore.ListByConversationSystem. SQLite is N=1
	// / no RLS, so it delegates to SpendByCategory.
	SpendByCategorySystem(ctx context.Context, orgID string, since, until time.Time) ([]domain.SpendBucket, error)

	// SpendByCategorySystemForTeam is SpendByCategorySystem narrowed to one team:
	// the same ADMIN-pool aggregate, plus `AND team_id = teamID`. The per-team
	// daily spend cap (TFAC-482) reads it from Spawner.Delegate to sum a team's
	// settled spend for today. Filtering on team_id automatically excludes the
	// org's NULL-team rows — system overhead and curator turns on non-team
	// projects — so a team cap never counts spend that isn't the team's (those
	// belong to the org cap alone). Like SpendByCategorySystem it MUST use the
	// admin pool: Delegate is claims-less, so an app-pool/RLS read would see
	// nothing and the cap would never trip. SQLite is N=1 / no RLS, so it
	// delegates to SpendByCategory with the same team filter applied.
	SpendByCategorySystemForTeam(ctx context.Context, orgID, teamID string, since, until time.Time) ([]domain.SpendBucket, error)

	// SpendTotalSystem sums settled llm_spend across ALL orgs in the window —
	// the deployment-wide cost overlay on the fleet dashboard (TFAC-589). The
	// only cross-org (no orgID) spend read: it exists solely for the
	// operator-gated fleet console, never a tenant-facing surface, so it runs on
	// the admin pool with no org filter. Both bounds optional (zero time drops
	// the clause), same convention as SpendByCategorySystem.
	SpendTotalSystem(ctx context.Context, since, until time.Time) (float64, error)
}
