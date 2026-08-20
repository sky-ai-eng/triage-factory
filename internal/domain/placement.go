package domain

import "time"

// Placement override key kinds — the namespace of a rendezvous key. "repo"
// is a delegation key ((org, "owner/repo")); "project" is a curator key
// ((org, project-id)). These string values are the placement_overrides.
// key_kind column and the GET /api/fleet/placement `kind` param.
const (
	PlacementKindRepo    = "repo"
	PlacementKindProject = "project"
)

// PlacementOverride is one row in placement_overrides — human intent that
// wins over the computed rendezvous order for a single key (spec §6.1).
// Deliberately narrow: a manual pin, a hot-key replica count, and
// nothing else (drain lives on the executor's own registry row, not here).
// The table is expected to stay nearly empty; the rendezvous hash is the
// default map and needs no rows at all.
//
// A row carries at most one intent that actually changes placement:
//   - PinnedInstanceID set  → the key is pinned to that one instance,
//     regardless of the hash or the instance's liveness (the claim's aging
//     tier still spills a pinned-but-dead owner's conversations, so a pin never
//     wedges work — it only steers the warm path).
//   - Replicas > 0          → the top-Replicas rendezvous candidates all
//     count as preferred, bounding a hot monorepo's cache to K executors on
//     a cost dial (no hard fleet ceiling).
//
// A pin takes precedence over a replica count when both are somehow set.
type PlacementOverride struct {
	OrgID    string
	KeyKind  string // PlacementKindRepo | PlacementKindProject
	KeyValue string // "owner/repo" for repo keys; a project id for project keys

	// PinnedInstanceID names the instance this key is pinned to, or "" for
	// no pin. Not validated against the live registry at write time — a pin
	// to a not-yet-registered (or already-retired) instance is a legal,
	// self-healing statement of intent, exactly like the rendezvous stamp.
	PinnedInstanceID string

	// Replicas is the hot-key K: the top-K rendezvous candidates all count
	// as preferred. 0 means unset (the default single-owner behavior). Only
	// consulted when PinnedInstanceID is empty.
	Replicas int

	UpdatedAt time.Time
}
