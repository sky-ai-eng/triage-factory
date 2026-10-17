package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// No login identities in local mode, so no seeder.
func TestUsersVerifiedEmail_SQLite_Conformance(t *testing.T) {
	dbtest.RunUsersVerifiedEmailConformance(t, func(t *testing.T) (db.UsersStore, *dbtest.LoginIdentitySeeder) {
		return sqlitestore.New(openSQLiteForTest(t)).Users, nil
	})
}
