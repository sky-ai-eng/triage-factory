package sqlite_test

import (
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestLinearAppsStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunLinearAppsReturnedRowConformance(t, func(t *testing.T) (db.LinearAppsStore, string, string) {
		t.Helper()
		conn := openSQLiteForTest(t)
		seedSQLiteOrgForApps(t, conn, runmode.LocalDefaultOrgID)
		return sqlitestore.New(conn).LinearApps, runmode.LocalDefaultOrgID, ""
	})
}

func TestLinearInstallsStore_SQLite_Conformance(t *testing.T) {
	const otherOrg = "22222222-2222-2222-2222-222222222222"
	dbtest.RunLinearInstallsConformance(t, func(t *testing.T) (db.LinearInstallsStore, string, string, string) {
		t.Helper()
		conn := openSQLiteForTest(t)
		seedSQLiteOrgForApps(t, conn, runmode.LocalDefaultOrgID)
		seedSQLiteOrgForApps(t, conn, otherOrg)
		return sqlitestore.New(conn).LinearInstalls, runmode.LocalDefaultOrgID, otherOrg, ""
	})
}
