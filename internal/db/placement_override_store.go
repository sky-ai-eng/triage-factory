package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PlacementOverrideStore owns the placement_overrides table — the "table for
// the exceptions" half of the placement design (spec §6.1): human
// intent (a manual pin or a hot-key replica count) that wins over the
// computed rendezvous order for a single key. Expected to stay nearly empty;
// the capacity-weighted rendezvous hash (internal/placement) is the default
// map and needs no rows.
//
// Admin-pool-only in Postgres, same posture as InstanceStore: not a browsable
// RLS surface (the explainer reads it for an already-authorized, operator-
// gated orgID), so every method takes an explicit orgID bound by argument
// rather than an app-pool RLS policy — hence no "...System" suffix. SQLite is
// N=1 and unscoped; the rows are inert there (the hash always returns self).
type PlacementOverrideStore interface {
	// Get returns the override for (orgID, keyKind, keyValue), or (nil, nil)
	// when none exists — the common case. Read at enqueue (the placement
	// stamp) and by the explainer.
	Get(ctx context.Context, orgID, keyKind, keyValue string) (*domain.PlacementOverride, error)

	// List returns every override for an org, ordered (key_kind, key_value).
	// Backs the explainer's "overrides in effect" listing and the CLI verb.
	List(ctx context.Context, orgID string) ([]domain.PlacementOverride, error)

	// Upsert writes ov wholesale, keyed on (OrgID, KeyKind, KeyValue),
	// stamping updated_at. The operator write path (CLI); never on the
	// executor's enqueue path.
	//
	// Returns the persisted row, read off RETURNING on the write statement
	// itself rather than from a follow-up SELECT and projecting Get's column
	// list and scanner. ov is an input: it carries no updated_at, and an empty
	// PinnedInstanceID stores NULL rather than the empty string it was handed.
	Upsert(ctx context.Context, ov domain.PlacementOverride) (domain.PlacementOverride, error)

	// Delete removes the override for (orgID, keyKind, keyValue). matched is
	// false when no such row existed.
	//
	// Exempt from the returned-row rule: it is a delete.
	Delete(ctx context.Context, orgID, keyKind, keyValue string) (matched bool, err error)
}
