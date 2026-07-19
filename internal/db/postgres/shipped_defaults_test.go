package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestShippedDefaultsStore_Postgres_SeedShippedIntoTeamRejectsInsideTx pins
// the contract that SeedShippedIntoTeam must not be invoked from a WithTx
// closure — escaping to the admin pool from inside a caller's tx would
// silently bypass their transaction scope. Mirrors the OrgTemplate /
// Agents.Create / PromptStore.SeedOrUpdate contract.
func TestShippedDefaultsStore_Postgres_SeedShippedIntoTeamRejectsInsideTx(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, ownerID, teamID := pgtest.SeedOrgWithUser(t, h, "sky658-intx")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	err := stores.Tx.WithTx(context.Background(), orgID, ownerID, func(tx db.TxStores) error {
		return tx.ShippedDefaults.SeedShippedIntoTeam(context.Background(), orgID, teamID, nil, nil)
	})
	if err == nil {
		t.Fatal("SeedShippedIntoTeam inside WithTx returned nil; want explicit refusal")
	}
	if !strings.Contains(err.Error(), "must not be called inside WithTx") {
		t.Errorf("error %q does not mention the inside-WithTx contract", err.Error())
	}
}
