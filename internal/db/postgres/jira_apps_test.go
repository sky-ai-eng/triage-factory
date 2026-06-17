package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestJiraAppsStore_Postgres_UpsertGetDelete exercises Upsert + Get + Delete
// through the app pool with matching claims. RLS gates writes via
// tf.user_is_org_admin(org_id); the upsert replaces in place on conflict.
func TestJiraAppsStore_Postgres_UpsertGetDelete(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)

		got, err := stores.JiraApps.GetForOrg(ctx, orgID)
		if err != nil {
			return fmt.Errorf("GetForOrg (empty): %w", err)
		}
		if got != nil {
			t.Error("GetForOrg on empty table returned non-nil")
		}

		if err := stores.JiraApps.UpsertForOrg(ctx, domain.OrgJiraApp{
			OrgID:              orgID,
			ClientID:           "atl-client-1",
			ClientSecretRef:    "jira_oauth_client_secret",
			RegisteredByUserID: userID,
		}); err != nil {
			return fmt.Errorf("UpsertForOrg (insert): %w", err)
		}

		got, err = stores.JiraApps.GetForOrg(ctx, orgID)
		if err != nil {
			return fmt.Errorf("GetForOrg: %w", err)
		}
		if got == nil || got.ClientID != "atl-client-1" {
			t.Fatalf("GetForOrg = %+v, want client_id atl-client-1", got)
		}
		if got.RegisteredByUserID != userID {
			t.Errorf("RegisteredByUserID = %q, want %q", got.RegisteredByUserID, userID)
		}
		firstRegistered := got.RegisteredAt

		// Replace in place; registered_at preserved.
		if err := stores.JiraApps.UpsertForOrg(ctx, domain.OrgJiraApp{
			OrgID:              orgID,
			ClientID:           "atl-client-2",
			ClientSecretRef:    "jira_oauth_client_secret",
			RegisteredByUserID: userID,
		}); err != nil {
			return fmt.Errorf("UpsertForOrg (replace): %w", err)
		}
		got, err = stores.JiraApps.GetForOrg(ctx, orgID)
		if err != nil {
			return fmt.Errorf("GetForOrg after replace: %w", err)
		}
		if got.ClientID != "atl-client-2" {
			t.Errorf("after replace client_id = %q, want atl-client-2", got.ClientID)
		}
		if !got.RegisteredAt.Equal(firstRegistered) {
			t.Errorf("RegisteredAt changed on replace: %v -> %v", firstRegistered, got.RegisteredAt)
		}

		if err := stores.JiraApps.DeleteForOrg(ctx, orgID); err != nil {
			return fmt.Errorf("DeleteForOrg: %w", err)
		}
		got, err = stores.JiraApps.GetForOrg(ctx, orgID)
		if err != nil {
			return fmt.Errorf("GetForOrg after delete: %w", err)
		}
		if got != nil {
			t.Errorf("GetForOrg after delete = %+v, want nil", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}
