package sqlite_test

import (
	"context"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestJiraAppsStore_SQLite_UpsertGetDelete pins the org_jira_apps round-trip:
// an empty table reads nil, an upsert reads back, a second upsert REPLACES the
// credentials in place (the "enter or replace" card behavior), and a delete
// clears the row.
func TestJiraAppsStore_SQLite_UpsertGetDelete(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	seedSQLiteOrgForApps(t, conn, orgID)

	dbtest.RunJiraAppsReturnedRowConformance(t, func(t *testing.T) (db.JiraAppsStore, string, string) {
		t.Helper()
		fresh := openSQLiteForTest(t)
		seedSQLiteOrgForApps(t, fresh, runmode.LocalDefaultOrgID)
		return sqlitestore.New(fresh).JiraApps, runmode.LocalDefaultOrgID, ""
	})

	// Empty table → nil, no error.
	got, err := stores.JiraApps.GetForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("GetForOrg (empty): %v", err)
	}
	if got != nil {
		t.Fatalf("GetForOrg on empty table = %+v, want nil", got)
	}

	if _, err := stores.JiraApps.UpsertForOrg(ctx, domain.OrgJiraApp{
		OrgID:           orgID,
		ClientID:        "atl-client-1",
		ClientSecretRef: "jira_oauth_client_secret",
	}); err != nil {
		t.Fatalf("UpsertForOrg (insert): %v", err)
	}

	got, err = stores.JiraApps.GetForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("GetForOrg: %v", err)
	}
	if got == nil || got.ClientID != "atl-client-1" {
		t.Fatalf("GetForOrg = %+v, want client_id atl-client-1", got)
	}
	if got.RegisteredAt.IsZero() {
		t.Fatalf("RegisteredAt is zero, want a default timestamp")
	}
	firstRegistered := got.RegisteredAt

	// GetForOrgSystem collapses to the same read in local mode.
	sys, err := stores.JiraApps.GetForOrgSystem(ctx, orgID)
	if err != nil || sys == nil || sys.ClientID != "atl-client-1" {
		t.Fatalf("GetForOrgSystem = %+v, err=%v", sys, err)
	}

	// Second upsert replaces client_id in place and preserves registered_at.
	if _, err := stores.JiraApps.UpsertForOrg(ctx, domain.OrgJiraApp{
		OrgID:           orgID,
		ClientID:        "atl-client-2",
		ClientSecretRef: "jira_oauth_client_secret",
	}); err != nil {
		t.Fatalf("UpsertForOrg (replace): %v", err)
	}
	got, err = stores.JiraApps.GetForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("GetForOrg after replace: %v", err)
	}
	if got.ClientID != "atl-client-2" {
		t.Fatalf("after replace client_id = %q, want atl-client-2", got.ClientID)
	}
	if !got.RegisteredAt.Equal(firstRegistered) {
		t.Fatalf("RegisteredAt changed on replace: %v -> %v", firstRegistered, got.RegisteredAt)
	}

	// Delete clears the row; a second delete is a no-op.
	if err := stores.JiraApps.DeleteForOrg(ctx, orgID); err != nil {
		t.Fatalf("DeleteForOrg: %v", err)
	}
	got, err = stores.JiraApps.GetForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("GetForOrg after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("GetForOrg after delete = %+v, want nil", got)
	}
	if err := stores.JiraApps.DeleteForOrg(ctx, orgID); err != nil {
		t.Fatalf("DeleteForOrg (idempotent): %v", err)
	}
}
