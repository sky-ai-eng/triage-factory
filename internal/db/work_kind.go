package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
)

// WorkSubject is how a kind describes one of its rows to a person: a label
// they recognize, an optional detail line, and named fields. Every value is
// the kind's own domain data; the surface renders it and never interprets it.
type WorkSubject struct {
	Label  string            // "sky-ai-eng/triage-factory#18", "SKY-123"
	Detail string            // the PR or issue title; "" when unknown
	Fields map[string]string // "event_type": "github:pr:ci_check_failed", "entity_id": …
}

// WorkAccess is the access policy a kind declares for its operator controls.
// One value today; a kind needing a named visibility join adds a second, and
// the handler's switch must grow an arm for it — there is no default arm that
// allows.
type WorkAccess int

const (
	// WorkAccessOrgAdmin is admin-pool access behind the org-admin role, with
	// no RLS backstop: the handler's predicate is the whole enforcement.
	WorkAccessOrgAdmin WorkAccess = iota + 1
)

// WorkControls is which operator controls the kind offers. Redrive is every
// kind's; Supersede only where replacement work exists; Cancel wherever a
// ready or leased row may be dropped.
type WorkControls struct {
	Redrive   bool
	Supersede bool
	Cancel    bool
}

// WorkObjective is the kind's monitoring objective: how old its oldest ready
// row may get before the drain is behind. The package's own type, so the
// metrics observer reads it without the store bundle.
type WorkObjective = workitem.Objective

// WorkKindHandle is one adopting table as the operator surface and the
// metrics depth observer see it. Implemented by the adopting store and
// registered on Stores.WorkKinds by each dialect's constructor, so a new kind
// inherits the surface and the gauges by registering rather than by building
// a second panel.
type WorkKindHandle interface {
	// Name is the kind's identifier: the path segment and the metric label.
	// A bare lower-case identifier, unique across the registry.
	Name() string
	// Label is the panel's heading for this kind.
	Label() string
	Kind() workitem.Kind
	// Conn is the pool the kind's worker uses — the admin pool in multi —
	// which the surface's reads and controls run on.
	Conn() workitem.DBTX
	Access() WorkAccess
	Controls() WorkControls
	Objective() WorkObjective
	// Describe returns the subjects for the named rows of this org. A row it
	// cannot describe is absent from the map, not an error; the surface
	// renders the block alone.
	Describe(ctx context.Context, orgID string, ids []int64) (map[int64]WorkSubject, error)
}

// MaxRedriveIDs bounds how many ids one redrive or cancel call may name. A
// selection is made from a page, and a page is bounded by the list contract,
// so this only rejects a hand-rolled request — and rejecting it keeps the
// per-id statement loop bounded.
const MaxRedriveIDs = 500
