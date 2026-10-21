package domain

import "time"

// PlacementKindRepo is the namespace of a delegation's rendezvous key, the
// repository's registry row id. This string value is folded into the key the
// rendezvous hash ranks on, and is the GET /api/fleet/placement `kind` param.
const (
	PlacementKindRepo = "repo"
)

// PlacementOverride is one row in placement_overrides — human intent that
// wins over the computed rendezvous order for one repository (spec §6.1).
// Deliberately narrow: a manual pin, a hot-key replica count, and
// nothing else (drain lives on the executor's own registry row, not here).
// The table is expected to stay nearly empty; the rendezvous hash is the
// default map and needs no rows at all.
//
// A row carries at most one intent that actually changes placement:
//   - PinnedInstanceID set  → the repository is pinned to that one instance,
//     regardless of the hash or the instance's liveness (the claim's aging
//     tier still spills a pinned-but-dead owner's conversations, so a pin never
//     wedges work — it only steers the warm path).
//   - Replicas > 0          → the top-Replicas rendezvous candidates all
//     count as preferred, bounding a hot monorepo's cache to K executors on
//     a cost dial (no hard fleet ceiling).
//
// A pin takes precedence over a replica count when both are somehow set.
type PlacementOverride struct {
	OrgID string
	// RepositoryID is the registry row of the repository the override is for,
	// which is also the value of the rendezvous key it overrides. A rename
	// moves the row's name and not its id, so it never moves an override, and
	// a repository created under a freed name has a row, and overrides, of its
	// own. Its name is read off that row, never stored here.
	RepositoryID string

	// PinnedInstanceID names the instance this repository is pinned to, or ""
	// for no pin. Not validated against the live registry at write time — a
	// pin to a not-yet-registered (or already-retired) instance is a legal,
	// self-healing statement of intent, exactly like the rendezvous stamp.
	PinnedInstanceID string

	// Replicas is the hot-key K: the top-K rendezvous candidates all count
	// as preferred. 0 means unset (the default single-owner behavior). Only
	// consulted when PinnedInstanceID is empty.
	Replicas int

	UpdatedAt time.Time
}
