package sqlite_test

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestGitHubDeliveryDedup_SQLite runs the shared delivery-dedup conformance
// suite against the SQLite impl. Each subtest opens a fresh in-memory DB; the
// seeder writes and reads github_webhook_deliveries by raw SQL, since the store
// exposes one method and it neither backdates a row nor reports one back.
//
// received_at is bound as a UTC time.Time here for the same reason the store
// writes it that way: this driver renders a bound time.Time in Go's own string
// form, and the prune compares the column against another bound time.Time.
func TestGitHubDeliveryDedup_SQLite(t *testing.T) {
	dbtest.RunGitHubDeliveryDedupConformance(t, func(t *testing.T) (db.GitHubDeliveryStore, dbtest.GitHubDeliverySeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		seed := dbtest.GitHubDeliverySeeder{
			Stale: func(t *testing.T, installationID, deliveryID string, age time.Duration) {
				t.Helper()
				if _, err := conn.Exec(`
					INSERT INTO github_webhook_deliveries (installation_id, delivery_id, received_at)
					VALUES (?, ?, ?)
				`, installationID, deliveryID, time.Now().UTC().Add(-age)); err != nil {
					t.Fatalf("seed aged delivery: %v", err)
				}
			},
			Recorded: func(t *testing.T, installationID, deliveryID string) bool {
				t.Helper()
				var n int
				if err := conn.QueryRow(`
					SELECT count(*) FROM github_webhook_deliveries
					 WHERE installation_id = ? AND delivery_id = ?
				`, installationID, deliveryID).Scan(&n); err != nil {
					t.Fatalf("count delivery rows: %v", err)
				}
				return n > 0
			},
		}
		return stores.GitHubDeliveries, seed
	})
}
