package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestGitHubAppsStore_Postgres_RoundTrip exercises Create + Get through
// the app pool with matching claims. RLS gates writes via
// tf.user_is_org_admin(org_id).
func TestGitHubAppsStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)

		got, err := stores.GitHubApps.GetForOrg(ctx, orgID)
		if err != nil {
			return fmt.Errorf("GetForOrg (empty): %w", err)
		}
		if got != nil {
			t.Error("GetForOrg on empty table returned non-nil")
		}

		app := domain.OrgGitHubApp{
			OrgID:              orgID,
			AppID:              "42",
			Slug:               "tf-test",
			ClientID:           "Iv1.abc123",
			ClientSecretRef:    "github_app_42_client_secret",
			PEMRef:             "github_app_42_pem",
			WebhookSecretRef:   "github_app_42_webhook_secret",
			RegisteredByUserID: userID,
		}
		if err := stores.GitHubApps.CreateForOrg(ctx, app); err != nil {
			return fmt.Errorf("CreateForOrg: %w", err)
		}

		got, err = stores.GitHubApps.GetForOrg(ctx, orgID)
		if err != nil {
			return fmt.Errorf("GetForOrg: %w", err)
		}
		if got == nil {
			t.Fatal("GetForOrg returned nil after Create")
		}
		if got.AppID != "42" {
			t.Errorf("AppID = %q, want 42", got.AppID)
		}
		if got.Slug != "tf-test" {
			t.Errorf("Slug = %q, want tf-test", got.Slug)
		}
		if got.ClientID != "Iv1.abc123" {
			t.Errorf("ClientID = %q, want Iv1.abc123", got.ClientID)
		}
		if !got.Active {
			t.Error("Active = false, want true (default)")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestGitHubAppsStore_Postgres_ConflictOnDuplicate verifies that a
// second CreateForOrg for the same org returns ErrGitHubAppExists.
func TestGitHubAppsStore_Postgres_ConflictOnDuplicate(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)

		app := domain.OrgGitHubApp{
			OrgID:            orgID,
			AppID:            "100",
			Slug:             "first",
			ClientID:         "Iv1.first",
			ClientSecretRef:  "ref_cs",
			PEMRef:           "ref_pem",
			WebhookSecretRef: "ref_wh",
		}
		if err := stores.GitHubApps.CreateForOrg(ctx, app); err != nil {
			return fmt.Errorf("first Create: %w", err)
		}

		// Wrap the duplicate insert in a savepoint so the constraint
		// violation doesn't abort the outer tx (Postgres puts the tx
		// in an error state on any unhandled error).
		if _, err := tx.ExecContext(ctx, "SAVEPOINT dup"); err != nil {
			return fmt.Errorf("savepoint: %w", err)
		}
		app.AppID = "200"
		app.Slug = "second"
		err := stores.GitHubApps.CreateForOrg(ctx, app)
		if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT dup"); rerr != nil {
			return fmt.Errorf("rollback savepoint: %w", rerr)
		}
		if err == nil {
			t.Error("second CreateForOrg should have failed with ErrGitHubAppExists")
			return nil
		}
		var exists *db.ErrGitHubAppExists
		if !errors.As(err, &exists) {
			t.Errorf("second CreateForOrg error type = %T, want *db.ErrGitHubAppExists", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestGitHubAppsStore_Postgres_CrossOrgRLSDenied proves that a user
// from org B cannot read or write org A's org_github_apps row.
func TestGitHubAppsStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA := seedPgOrgAndUserForGitHubApps(t, h)
	orgB, userB := seedPgOrgAndUserForGitHubApps(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed a row in orgA via the admin pool.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO org_github_apps (org_id, app_id, slug, client_id, client_secret_ref, pem_ref, webhook_secret_ref)
		VALUES ($1, '99', 'tf-a', 'Iv1.a', 'ref_cs', 'ref_pem', 'ref_wh')
	`, orgA); err != nil {
		t.Fatalf("seed org_github_apps: %v", err)
	}

	// User A can read their own org's app.
	if err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)
		got, err := stores.GitHubApps.GetForOrg(ctx, orgA)
		if err != nil {
			return fmt.Errorf("userA GetForOrg(orgA): %w", err)
		}
		if got == nil {
			t.Error("userA GetForOrg(orgA) = nil, want non-nil")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser userA/orgA: %v", err)
	}

	// User B with claims for orgB cannot see orgA's row.
	if err := h.WithUser(t, userB, orgB, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)
		got, err := stores.GitHubApps.GetForOrg(ctx, orgA)
		if err != nil {
			return fmt.Errorf("userB GetForOrg(orgA): %w", err)
		}
		if got != nil {
			t.Error("userB GetForOrg(orgA) should be nil (cross-org), got non-nil")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser userB/orgB: %v", err)
	}
}

// TestGitHubAppsStore_Postgres_NonAdminWriteDenied proves that a
// non-admin member (role='member') cannot INSERT into org_github_apps.
func TestGitHubAppsStore_Postgres_NonAdminWriteDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, _ := seedPgOrgAndUserForGitHubApps(t, h)
	memberID := seedPgMember(t, h, orgID, "non-admin", "member")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx)
		return stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
			OrgID:            orgID,
			AppID:            "77",
			Slug:             "member-attempt",
			ClientID:         "Iv1.m",
			ClientSecretRef:  "r1",
			PEMRef:           "r2",
			WebhookSecretRef: "r3",
		})
	})
	if err == nil {
		t.Fatal("non-admin CreateForOrg should have been rejected by RLS")
	}
	pgtest.AssertRLSViolation(t, err)
}

func seedPgOrgAndUserForGitHubApps(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("ghapp-%s@test.local", userID[:8])
	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, "GH App Test User"); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "GH App Org "+orgID[:8], "ghapp-"+orgID[:8], userID); err != nil {
		t.Fatalf("seed orgs: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID); err != nil {
		t.Fatalf("seed org_memberships: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}
