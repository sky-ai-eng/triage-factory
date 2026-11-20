package dbtest

import (
	"database/sql"
	"testing"
)

// MustUpdate runs a fixture write that must change a row, such as a hook
// moving a row's stamps back, and fails the test on an error or when it
// changed none. A write that matches nothing is no error to either dialect,
// and a test that relies on it would pass or fail by the clock instead.
func MustUpdate(t testing.TB, db interface {
	Exec(query string, args ...any) (sql.Result, error)
}, query string, args ...any) {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		t.Fatalf("exec %q changed no row (rows affected %d, err %v)", query, n, err)
	}
}
