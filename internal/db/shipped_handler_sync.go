package db

import (
	"encoding/json"
	"reflect"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PredicateJSONEqual reports whether two scope_predicate_json values are
// semantically equal, tolerating a text round-trip through Postgres's jsonb
// column (which re-serializes with different whitespace/key-ordering than
// whatever text was written — e.g. `{"a":1}` in, `{"a": 1}` out). A byte
// comparison would treat that as "changed" on every equal re-sync, needlessly
// churning updated_at and (via Update's contentChanged) spuriously stamping
// user_modified. "" (NULL / match-all) is equal only to itself. Malformed
// JSON on either side falls back to a raw string comparison.
func PredicateJSONEqual(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	var av, bv any
	if json.Unmarshal([]byte(a), &av) != nil || json.Unmarshal([]byte(b), &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// This file holds the dialect-agnostic core of the event-handler half of the
// boot-time shipped-defaults sync (EventHandlerStore.Sync): the per-handler
// decision (PlanHandlerSync). It mirrors shipped_sync.go's PlanUnitSync, but a
// shipped handler has no steps — the sync unit is just the row itself — so the
// shape is simpler: no insert/apply split across sub-rows, no drop list.

// HandlerSyncAction is the decision PlanHandlerSync returns for one shipped
// event handler.
type HandlerSyncAction int

const (
	// HandlerSkip: the team's row is user_modified or soft-deleted, or (for a
	// trigger) its shipped blueprint slug didn't resolve — leave it alone.
	HandlerSkip HandlerSyncAction = iota
	// HandlerEqual: the team's row already equals shipped content — no writes.
	HandlerEqual
	// HandlerInsert: the team has no row for this shipped system_slug.
	HandlerInsert
	// HandlerApply: rewrite the diverging content fields of an existing row.
	HandlerApply
)

// TeamHandlerRow is the team-side loaded state of one shipped event handler,
// keyed by its system_slug. Exists is false when the team has no row for
// (org, team, slug) at all; Deleted marks a soft-deleted occupant of that
// slot. Content fields are populated regardless of kind (irrelevant fields
// read as the zero value) — the caller compares only the fields its kind
// carries.
type TeamHandlerRow struct {
	Exists       bool
	Deleted      bool
	UserModified bool
	ID           string

	Predicate string // "" == NULL (match-all)

	// Rule-only.
	Name            string
	DefaultPriority float64

	// Trigger-only. BlueprintID is the team's resolved blueprint-copy id
	// (never a slug) — comparable directly against a resolved shipped target.
	BlueprintID            string
	BreakerThreshold       int
	MinAutonomySuitability float64
}

// HandlerSyncPlan is the executable result of PlanHandlerSync. For
// HandlerInsert / HandlerApply, only the fields relevant to the handler's kind
// are meaningful; the impl writes the rest as NULL per the CHECK-constrained
// per-kind shape (unchanged from Create/Seed).
type HandlerSyncPlan struct {
	Action HandlerSyncAction

	Predicate string

	// Rule-only.
	Name            string
	DefaultPriority float64
	// SortOrder is meaningful only on HandlerInsert (the shipped starting
	// value for a brand-new row) — HandlerApply never writes it, matching the
	// "sort_order is presentation order the user owns" rule.
	SortOrder int

	// Trigger-only.
	BlueprintID            string
	BreakerThreshold       int
	MinAutonomySuitability float64
}

// PlanHandlerSync decides what EventHandlerStore.Sync must do for one shipped
// handler, given the loaded team-side row and (for a trigger) its shipped
// blueprint slug already resolved to the team's blueprint-copy id.
// blueprintResolved is false when the shipped blueprint slug has no
// (non-deleted) team copy — the caller must not wire a trigger to a blueprint
// that isn't there. Pure and dialect-agnostic.
func PlanHandlerSync(shipped ShippedEventHandler, resolvedBlueprintID string, blueprintResolved bool, row TeamHandlerRow) HandlerSyncPlan {
	if row.Exists && (row.Deleted || row.UserModified) {
		return HandlerSyncPlan{Action: HandlerSkip}
	}
	if shipped.Kind == domain.EventHandlerKindTrigger && !blueprintResolved {
		return HandlerSyncPlan{Action: HandlerSkip}
	}

	want := HandlerSyncPlan{Predicate: shipped.Predicate}
	switch shipped.Kind {
	case domain.EventHandlerKindRule:
		want.Name = shipped.Name
		want.DefaultPriority = shipped.DefaultPriority
		want.SortOrder = shipped.SortOrder
	case domain.EventHandlerKindTrigger:
		want.BlueprintID = resolvedBlueprintID
		want.BreakerThreshold = shipped.BreakerThreshold
		want.MinAutonomySuitability = shipped.MinAutonomySuitability
	}

	if !row.Exists {
		want.Action = HandlerInsert
		return want
	}
	if handlerContentEqual(shipped.Kind, row, want) {
		return HandlerSyncPlan{Action: HandlerEqual}
	}
	want.Action = HandlerApply
	return want
}

// handlerContentEqual reports whether a loaded team handler row already
// matches the shipped content the sync would write. Deliberately excludes
// enabled and sort_order — neither counts as content (see PlanHandlerSync).
func handlerContentEqual(kind string, row TeamHandlerRow, want HandlerSyncPlan) bool {
	if !PredicateJSONEqual(row.Predicate, want.Predicate) {
		return false
	}
	switch kind {
	case domain.EventHandlerKindRule:
		return row.Name == want.Name && row.DefaultPriority == want.DefaultPriority
	case domain.EventHandlerKindTrigger:
		return row.BlueprintID == want.BlueprintID &&
			row.BreakerThreshold == want.BreakerThreshold &&
			row.MinAutonomySuitability == want.MinAutonomySuitability
	}
	return true
}
