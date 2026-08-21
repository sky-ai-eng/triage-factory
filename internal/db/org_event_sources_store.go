package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OrgEventSourceStore owns the org_event_sources table — declared per-(org,
// source) policy, which today is one fact: whether an org admin has paused the
// source's event production.
//
// Split-pool in Postgres. The reads a request serves run on the app pool under
// RLS (member SELECT, admin write — a source belongs to no team, so the
// org-wide-mutation rule reduces to plain org admin and RLS can express it),
// and the ...System reads route through the admin pool for the two producers
// that have no JWT claims: the event router and the poller.
//
// Every read answers about the whole org rather than one kind. Both consumers
// that matter — the availability derivation resolving a vocabulary, the router
// gating a stream of events across sources — want the set, and a per-kind read
// would make each of them N queries to answer the same question once.
type OrgEventSourceStore interface {
	// ListDisabled returns the kinds this org has turned off, sorted by kind.
	// An org that has never touched the surface returns an empty slice, which
	// is the same answer as one that turned everything back on — nothing about
	// the derivation distinguishes them.
	ListDisabled(ctx context.Context, orgID string) ([]string, error)

	// ListDisabledSystem is ListDisabled on the admin pool, for the JWT-less
	// producers (router, poller) with org_id bound by argument.
	ListDisabledSystem(ctx context.Context, orgID string) ([]string, error)

	// Get returns one source's row, or (nil, nil) when the org has recorded
	// nothing for it — absence is an answer ("no per-source overrides"), not a
	// miss. It is the point read the returned-row rule pairs SetDisabled
	// against, and the only way to read who paused a source and when.
	Get(ctx context.Context, orgID, kind string) (*domain.OrgEventSource, error)

	// SetDisabled records the org's policy for one source and returns the
	// stored row. Upserts: the first call for a (org, kind) pair mints the
	// row.
	//
	// actorUserID is stamped as disabled_by when disabling. Re-enabling clears
	// both disabled_at and disabled_by rather than leaving the last pause's
	// stamps on an active source — the row describes the LIVE policy, and the
	// history of who paused what is what access_changes is for.
	SetDisabled(ctx context.Context, orgID, kind string, disabled bool, actorUserID string) (domain.OrgEventSource, error)
}
