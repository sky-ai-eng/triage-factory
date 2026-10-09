package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SeedOrgSettings writes set as the org's whole settings row and returns the
// row it stored. It reads the row's current version and writes at it through
// UpdateSettingsVersioned, the one whole-row writer. A missing row reads as
// version 0, which is the create assertion, so the same call seeds an org with
// no settings row and replaces the row of one that has it. set.Version is
// ignored, as the writer ignores it.
//
// Both calls go through orgs, so a fixture seeding inside a transaction passes
// that transaction's store.
func SeedOrgSettings(tb testing.TB, orgs db.OrgsStore, orgID string, set domain.OrgSettings) domain.OrgSettings {
	tb.Helper()
	ctx := context.Background()
	cur, err := orgs.GetSettings(ctx, orgID)
	if err != nil {
		tb.Fatalf("SeedOrgSettings(%s): read: %v", orgID, err)
	}
	stored, err := orgs.UpdateSettingsVersioned(ctx, orgID, set, cur.Version)
	if err != nil {
		tb.Fatalf("SeedOrgSettings(%s): write at version %d: %v", orgID, cur.Version, err)
	}
	return stored
}
