package postgres_test

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestGitHubDeliveryDedup_Postgres runs the shared delivery-dedup conformance
// suite against the Postgres impl. AdminDB serves both pool slots: the table
// has RLS enabled with no policy at all, so the admin pool is the only role
// that can reach it. No org or user fixtures — github_webhook_deliveries
// carries no org_id and no FK, because the tenant is deliberately not part of
// the dedup key.
func TestGitHubDeliveryDedup_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunGitHubDeliveryDedupConformance(t, func(t *testing.T) (db.GitHubDeliveryStore, dbtest.GitHubDeliverySeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		seed := dbtest.GitHubDeliverySeeder{
			Stale: func(t *testing.T, installationID, deliveryID string, age time.Duration) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`
					INSERT INTO github_webhook_deliveries (installation_id, delivery_id, received_at)
					VALUES ($1, $2, $3)
				`, installationID, deliveryID, time.Now().UTC().Add(-age)); err != nil {
					t.Fatalf("seed aged delivery: %v", err)
				}
			},
			Recorded: func(t *testing.T, installationID, deliveryID string) bool {
				t.Helper()
				var n int
				if err := h.AdminDB.QueryRow(`
					SELECT count(*) FROM github_webhook_deliveries
					 WHERE installation_id = $1 AND delivery_id = $2
				`, installationID, deliveryID).Scan(&n); err != nil {
					t.Fatalf("count delivery rows: %v", err)
				}
				return n > 0
			},
		}
		return stores.GitHubDeliveries, seed
	})
}
