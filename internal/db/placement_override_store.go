package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PlacementOverrideStore owns the placement_overrides table — the "table for
// the exceptions" half of the placement design (spec §6.1): human
// intent (a manual pin or a hot-key replica count) that wins over the
// computed rendezvous order for a single repository. Expected to stay nearly
// empty; the capacity-weighted rendezvous hash (internal/placement) is the
// default map and needs no rows.
//
// Admin-pool-only in Postgres, same posture as InstanceStore: not a browsable
// RLS surface (the explainer reads it for an already-authorized, operator-
// gated orgID), so every method takes an explicit orgID bound by argument
// rather than an app-pool RLS policy — hence no "...System" suffix. SQLite is
// N=1 and unscoped; the rows are inert there (the hash always returns self).
//
// An override is keyed on (org, repository registry row id), the same id the
// rendezvous key carries, and the row references the registry row, so it
// belongs to that repository whatever the repository is called: a rename
// never touches it, a same-named repository on another GitHub host or under
// a freed name is another row with overrides of its own, and deleting the
// registry row deletes its override. A caller holding a name resolves it to
// the registry row first (RepositoryStore.GetByRefSystem).
type PlacementOverrideStore interface {
	// Get returns the override for (orgID, repositoryID), or (nil, nil) when
	// none exists — the common case. Read at enqueue (the placement stamp)
	// and by the explainer.
	Get(ctx context.Context, orgID, repositoryID string) (*domain.PlacementOverride, error)

	// List returns every override for an org, ordered by its repository's
	// host and name (case-folded), then its repository id. Backs the CLI's
	// listing verb.
	List(ctx context.Context, orgID string) ([]domain.PlacementOverride, error)

	// Upsert writes ov wholesale, keyed on (OrgID, RepositoryID), stamping
	// updated_at. The operator write path (CLI); never on the executor's
	// enqueue path. ErrNoSuchRepository when no registry row in the org
	// carries ov.RepositoryID.
	//
	// Returns the persisted row, read off RETURNING on the write statement
	// itself rather than from a follow-up SELECT and projecting Get's column
	// list and scanner. ov is an input: it carries no updated_at, and an empty
	// PinnedInstanceID stores NULL rather than the empty string it was handed.
	Upsert(ctx context.Context, ov domain.PlacementOverride) (domain.PlacementOverride, error)

	// Delete removes the override for (orgID, repositoryID). matched is false
	// when no such row existed.
	//
	// Exempt from the returned-row rule: it is a delete.
	Delete(ctx context.Context, orgID, repositoryID string) (matched bool, err error)
}
