package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestJiraAppsStore_Postgres_ReturnedRowConformance runs the shared
// returned-row suite against the Postgres impl under the registrant's claims,
// on the app pool.
//
// The claims are the point. Wiring both pools to AdminDB — what the other
// store conformance suites do, so behavior stays independent of the auth path
// — makes the connection BYPASSRLS, and a RETURNING clause on a BYPASSRLS
// connection returns its row unconditionally. Under RLS it does not: the
// upsert's conflict arm has to satisfy the SELECT policy for the row it hands
// back, so a policy that admits the write but not the read-back yields zero
// rows from a statement that updated one. That is the failure this wiring
// exists to catch, and it is invisible on the admin pool.
//
// The whole suite runs inside ONE claims-carrying transaction, which is also
// what the sequence needs: org_jira_apps is one row per org, so the conflict
// arm is only reachable through the insert arm that preceded it. A subtest
// added to the suite therefore inherits the table its siblings left behind.
func TestJiraAppsStore_Postgres_ReturnedRowConformance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).JiraApps
		dbtest.RunJiraAppsReturnedRowConformance(t, func(t *testing.T) (db.JiraAppsStore, string, string) {
			t.Helper()
			return store, orgID, userID
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

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

		if _, err := stores.JiraApps.UpsertForOrg(ctx, domain.OrgJiraApp{
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
		if _, err := stores.JiraApps.UpsertForOrg(ctx, domain.OrgJiraApp{
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
