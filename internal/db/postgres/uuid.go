package postgres

import "github.com/google/uuid"

// isValidUUID returns true iff s parses as a UUID. Used by store
// methods that take an `id string` argument to short-circuit the
// query when the caller passed a non-UUID literal.
//
// Postgres UUID-typed columns reject non-UUID strings at the type
// layer BEFORE any row-filter runs, surfacing the failure as
// `invalid input syntax for type uuid (SQLSTATE 22P02)` rather than
// the clean "no row matched" outcome a SQLite TEXT-keyed table
// returns. That mismatch leaks Postgres's parse layer into callers
// that just want "is this row present?" semantics; the calling
// handler usually maps a not-found result to 404, but a parse-error
// 22P02 bubbles up as a 500 with an opaque message.
//
// Convention adopted: read methods (Get) return (nil, nil) on an
// invalid UUID — same shape as Postgres-valid-but-absent rows.
// Mutating methods (Update / SetEnabled / Delete) treat an invalid
// UUID as "no row matched" and return nil. Production handlers do a
// Get-then-mutate pattern that 404s on missing rows; the Get already
// has the right shape so the mutating methods just need to not blow
// up.
//
// This is intentionally permissive — Create is NOT covered because
// caller-supplied invalid IDs at INSERT time are a programmer bug
// that should fail loudly, not a "row doesn't exist" semantic.
func isValidUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// filterValidUUIDs returns the subset of ids that parse as UUIDs, preserving
// order. Read methods that bind an id slice as a uuid[] (pgUUIDArray) call this
// first so a caller-supplied non-UUID degrades to "no rows" instead of a 22P02
// parse error surfacing as a 500 — the slice analogue of isValidUUID's
// single-id short-circuit, same read-method convention as Get.
func filterValidUUIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if isValidUUID(id) {
			out = append(out, id)
		}
	}
	return out
}
