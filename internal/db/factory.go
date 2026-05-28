package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=FactoryReadStore --output=./mocks --case=underscore --with-expecter

// FactoryReadStore is the read-only projection that backs the
// /api/factory/snapshot handler. Every method is scoped to one org;
// in local mode callers pass runmode.LocalDefaultOrg.
//
// Wired against the admin pool in Postgres: the factory snapshot is
// a system-level view (no per-user identity required) and needs to
// see every in-flight run regardless of which user kicked it off.
// SQLite is single-tenant so the pool distinction collapses to "the
// one connection."
//
// "All scoped to active state" is the contract: closed entities ride
// the snapshot for FactoryClosedGracePeriod after closure so the
// chip can finish its terminal animation, but otherwise drop off.
type FactoryReadStore interface {
	// EventCountsSince counts events per event_type emitted after
	// `since`. Used to compute station throughput; keys with zero
	// counts are absent.
	EventCountsSince(ctx context.Context, orgID string, since time.Time) (map[string]int, error)

	// LifetimeDistinctByEventType counts the distinct entities that
	// have ever produced an event of each event_type, scoped to the
	// given org. Read per snapshot request — the factory endpoint is
	// not a hot path. In Postgres the partial index
	// idx_events_org_type_entity (org_id, event_type, entity_id) WHERE
	// entity_id IS NOT NULL is an index-only scan: each org's groups
	// are contiguous and per-group entity_ids are adjacent, so
	// DISTINCT collapses to adjacency dedup with no temp B-tree.
	// SQLite's existing idx_events_type_entity (event_type, entity_id)
	// WHERE entity_id IS NOT NULL covers the single-tenant query
	// equivalently. Replaces the prior in-memory cross-process counter
	// that folded every tenant's events into one map.
	LifetimeDistinctByEventType(ctx context.Context, orgID string) (map[string]int, error)

	// TaskCountsSince counts tasks per event_type created after
	// `since`. Combined with EventCountsSince to compute the
	// "triggered / seen" ratio displayed in the station overlay.
	TaskCountsSince(ctx context.Context, orgID string, since time.Time) (map[string]int, error)

	// ActiveRuns returns every run currently in-flight (status in
	// factoryActiveRunStatuses, defined per-backend) joined with its
	// task and entity. Ordered by started_at DESC so the overlay can
	// render most-recent-first without client-side sorting.
	//
	// memory_missing is derived from a LEFT JOIN to run_memory rather
	// than read off a column — see SKY-204. The agent has not
	// produced its memory file iff no run_memory row exists, or the
	// row's agent_content is NULL/whitespace.
	ActiveRuns(ctx context.Context, orgID string) ([]domain.FactoryActiveRun, error)

	// RecentEventsByEntity returns the last `perEntity` events per
	// entity id, grouped in a map keyed by entity_id with each slice
	// ordered chronologically ascending. Drives the factory's chain
	// animation — when two events fire for one entity in a single
	// poll cycle (new_commits → ci_passed), we want to see the item
	// travel both stations rather than teleport to the second.
	RecentEventsByEntity(ctx context.Context, orgID string, entityIDs []string, perEntity int) (map[string][]domain.FactoryRecentEvent, error)

	// Entities returns the active set of entities (up to `limit`)
	// plus any entity closed within FactoryClosedGracePeriod (up to
	// FactoryClosedGraceLimit) so the chip can finish animating to
	// its terminal station before disappearing.
	//
	// Multi-mode (Postgres) scopes to the viewer's teams by
	// membership: an entity is included iff a task for one of the
	// viewer's teams has ever existed on it (a semi-join through
	// tasks, over all task statuses). The org-level GitHub App polls
	// org-wide, so polling produces cross-team entities; rather than
	// fork the shared entity per team, the entity↔team relationship is
	// derived through tasks (which carry team_id). The semi-join
	// auto-scopes via tasks RLS under tf_app — no explicit team_id
	// needed. An entity no team ever tasked belongs to no team's
	// factory and is excluded.
	//
	// Local mode (SQLite, N=1) applies NO membership filter: the local
	// tracker already scopes discovery to the user's own involvement
	// (author / review-requested / reviewed-by), so every entity is
	// personally relevant by construction. Filtering by task existence
	// there would hide relevant-but-untriaged PRs the user expects to
	// see, so the SQLite impl returns the full active set as before —
	// local mode is unaffected by this scoping.
	//
	// The entity's station (latest-event derivation in the SELECT) is
	// orthogonal to membership and computed from the shared event log,
	// unaffected by task lifecycle.
	//
	// Implemented as two separate queries in both backends instead of
	// a single OR'd WHERE: the OR spans two columns and forces a
	// filtered table scan, and a combined LIMIT lets a closure burst
	// crowd the active set out of the snapshot — the active half
	// should always get its full budget regardless of close pressure.
	Entities(ctx context.Context, orgID string, limit int) ([]domain.FactoryEntityRow, error)
}

// FactoryClosedGracePeriod is the window during which a freshly-closed
// entity remains in the factory snapshot so the chip can ride the final
// bridge into its terminal station before disposal. One full poll cycle
// (~30s baseline) plus headroom — generous enough to cover any chain
// animation duration without being load-bearing.
const FactoryClosedGracePeriod = 60 * time.Second

// FactoryClosedGraceLimit caps how many recently-closed entities ride
// alongside the active set in a snapshot. Bounded separately from the
// active limit so a burst of closures can't crowd active entities out
// of the displayed set. 64 is generous: even a worst-case mass-merge
// of half a sprint's PRs in one poll cycle fits without overflow.
const FactoryClosedGraceLimit = 64
