package sqlite_test

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestShippedDefaultsStore_SQLite_SeedShippedIntoTeamRejectsInsideTx pins
// that SeedShippedIntoTeam refuses to run inside WithTx on SQLite too, not
// just Postgres. SQLite has no genuine pool-escape hazard to guard against —
// everything shares one connection — but enforcing the same outside-WithTx
// contract on both dialects means a misuse fails loudly here instead of
// silently working in SQLite/local mode and only surfacing as a
// Postgres/multi-mode production error.
func TestShippedDefaultsStore_SQLite_SeedShippedIntoTeamRejectsInsideTx(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)

	err := stores.Tx.WithTx(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
		return tx.ShippedDefaults.SeedShippedIntoTeam(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, nil, nil)
	})
	if err == nil {
		t.Fatal("SeedShippedIntoTeam inside WithTx returned nil; want explicit refusal")
	}
	if !strings.Contains(err.Error(), "must not be called inside WithTx") {
		t.Errorf("error %q does not mention the inside-WithTx contract", err.Error())
	}
}
