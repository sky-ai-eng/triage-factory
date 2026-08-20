package dbtest

import (
	"reflect"
	"testing"
)

// AssertWriteReturnedStoredRow is the acceptance mechanism for the store
// standard that every single-row Insert/Update/Upsert returns the row it
// persisted, sourced from RETURNING on the write statement itself.
//
// Hand it what the write returned and a point read of the same row; it asserts
// the two are equal. That single assertion covers three things a separate test
// each would cover worse:
//
//   - RETURNING semantics. The write is projecting the stored row and not, say,
//     the values the caller proposed — which for these writes differ by design
//     (a COALESCEd id, a preserved base branch, the stored casing of a slug).
//   - Policy visibility, but ONLY when the suite is wired under claims. Under
//     Postgres RLS an upsert's UPDATE arm has to be able to SEE the existing
//     row for `UPDATE … RETURNING` to produce one, so a policy narrowed later
//     fails here rather than silently handing back nothing. On a BYPASSRLS
//     connection there is no policy to narrow and the clause returns its row
//     unconditionally — which is what most store suites run on, deliberately,
//     to keep behavior independent of the auth path. A suite whose store is
//     RLS-gated on the write should run inside a claims-carrying transaction
//     instead; RunJiraAppsReturnedRowConformance's Postgres wiring is the
//     shape.
//   - Column-list drift. The RETURNING list and the point read's SELECT list
//     are supposed to be the same list. A column added to one and not the other
//     shows up as a field that differs between the two rows.
//
// what names the method under test, because a suite applies this to every
// converted write and "returned row != stored row" says nothing about which
// one.
func AssertWriteReturnedStoredRow[T any](t *testing.T, what string, returned T, read func() (*T, error)) {
	t.Helper()
	stored, err := read()
	if err != nil {
		t.Fatalf("%s: reading back the row the write returned: %v", what, err)
	}
	if stored == nil {
		t.Fatalf("%s: the write returned a row that no point read finds", what)
	}
	if !reflect.DeepEqual(returned, *stored) {
		t.Errorf("%s: the returned row is not the stored row\n  returned: %+v\n    stored: %+v",
			what, returned, *stored)
	}
}
