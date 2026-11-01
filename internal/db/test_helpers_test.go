package db

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// newTenantlessTestDB opens the database exactly as Migrate leaves an empty
// one, with no synthetic local tenant, restored from a cached image
// (BootstrapTenantlessSchemaForTest). For tests of what a fresh migration
// produces and of the paths that provision a tenant themselves. Tests of the
// migration runner itself call Migrate on openMigrationsTestDB.
func newTenantlessTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := BootstrapTenantlessSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap tenantless schema: %v", err)
	}
	return database
}
