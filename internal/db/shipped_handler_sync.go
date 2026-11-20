package db

import (
	"encoding/json"
	"reflect"
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
	// HandlerSkip: the team's row is user_modified or soft-deleted — leave it
	// alone.
	HandlerSkip HandlerSyncAction = iota
	// HandlerEqual: the team's row already equals shipped content — no writes.
	HandlerEqual
	// HandlerInsert: the team has no row for this shipped system_slug.
	HandlerInsert
	// HandlerApply: rewrite the content fields of an existing row.
	HandlerApply
)

// TeamHandlerRow is the team-side loaded state of one shipped event handler,
// keyed by its system_slug. Exists is false when the team has no row for
// (org, team, slug) at all; Deleted marks a soft-deleted occupant of that
// slot.
type TeamHandlerRow struct {
	Exists       bool
	Deleted      bool
	UserModified bool
	ID           string

	Predicate       string // "" == NULL (match-all)
	Name            string
	DefaultPriority float64
	// BlueprintID is the team's resolved blueprint-copy id (never a slug),
	// "" for NULL — comparable directly against a resolved shipped target.
	BlueprintID            string
	BreakerThreshold       int
	MinAutonomySuitability float64
}

// HandlerSyncPlan is the executable result of PlanHandlerSync.
type HandlerSyncPlan struct {
	Action HandlerSyncAction

	Predicate       string
	Name            string
	DefaultPriority float64
	// WriteBlueprint says whether the plan writes blueprint_id at all. It is
	// false when the shipped handler names a blueprint the team has no live
	// copy of: an insert then writes NULL, and an apply leaves the column as
	// it is.
	WriteBlueprint bool
	// BlueprintID is the team's blueprint-copy id to write, "" for NULL.
	BlueprintID            string
	BreakerThreshold       int
	MinAutonomySuitability float64
	// Mode and SortOrder are meaningful only on HandlerInsert (the shipped
	// starting values for a brand-new row) — HandlerApply never writes
	// either, because what a handler does and where it sorts are the team's.
	Mode      string
	SortOrder int
}

// PlanHandlerSync decides what EventHandlerStore.Sync must do for one shipped
// handler, given the loaded team-side row and its shipped blueprint slug
// already resolved to the team's blueprint-copy id. blueprintResolved is
// false when the shipped handler names a blueprint slug with no (non-deleted)
// team copy; a handler that names none resolves to "" with blueprintResolved
// true. Pure and dialect-agnostic.
func PlanHandlerSync(shipped ShippedEventHandler, resolvedBlueprintID string, blueprintResolved bool, row TeamHandlerRow) HandlerSyncPlan {
	if row.Exists && (row.Deleted || row.UserModified) {
		return HandlerSyncPlan{Action: HandlerSkip}
	}

	want := HandlerSyncPlan{
		Predicate:              shipped.Predicate,
		Name:                   shipped.Name,
		DefaultPriority:        shipped.DefaultPriority,
		WriteBlueprint:         blueprintResolved,
		BreakerThreshold:       shipped.BreakerThreshold,
		MinAutonomySuitability: shipped.MinAutonomySuitability,
		Mode:                   shipped.Mode,
		SortOrder:              shipped.SortOrder,
	}
	if blueprintResolved {
		want.BlueprintID = resolvedBlueprintID
	}

	if !row.Exists {
		want.Action = HandlerInsert
		return want
	}
	if handlerContentEqual(row, want) {
		return HandlerSyncPlan{Action: HandlerEqual}
	}
	want.Action = HandlerApply
	return want
}

// ResolveShippedBlueprint resolves a shipped handler's blueprint slug against
// a team's slug→id map, in the shape PlanHandlerSync takes: a handler that
// names no blueprint resolves to "" and true, and one whose slug has no copy
// to "" and false.
func ResolveShippedBlueprint(h ShippedEventHandler, blueprintIDsBySlug map[string]string) (string, bool) {
	if h.BlueprintID == "" {
		return "", true
	}
	id, ok := blueprintIDsBySlug[h.BlueprintID]
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// handlerContentEqual reports whether a loaded team handler row already
// matches the shipped content the sync would write. Deliberately excludes
// mode and sort_order — neither counts as content (see PlanHandlerSync) — and
// blueprint_id when the plan does not write it.
func handlerContentEqual(row TeamHandlerRow, want HandlerSyncPlan) bool {
	if !PredicateJSONEqual(row.Predicate, want.Predicate) {
		return false
	}
	if want.WriteBlueprint && row.BlueprintID != want.BlueprintID {
		return false
	}
	return row.Name == want.Name &&
		row.DefaultPriority == want.DefaultPriority &&
		row.BreakerThreshold == want.BreakerThreshold &&
		row.MinAutonomySuitability == want.MinAutonomySuitability
}
