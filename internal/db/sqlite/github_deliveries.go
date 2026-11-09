package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// gitHubDeliveryStore is the SQLite impl of db.GitHubDeliveryStore. One
// connection — no RLS, no pool split — so the admin/app distinction the
// Postgres impl draws collapses here. Not a local stub: a local install GitHub
// can reach registers a live hook URL and runs the same pre-auth receiver, so
// the dedup has to actually dedup here too. q is the connection, or the
// transaction a db.SystemTxStores is bound to.
type gitHubDeliveryStore struct{ q queryer }

func newGitHubDeliveryStore(q queryer) db.GitHubDeliveryStore {
	return &gitHubDeliveryStore{q: q}
}

var _ db.GitHubDeliveryStore = (*gitHubDeliveryStore)(nil)

func (s *gitHubDeliveryStore) MarkDeliveredSystem(ctx context.Context, installationID, deliveryID string) (bool, error) {
	// received_at is written here rather than defaulted in the schema, always
	// in UTC: this driver stores a bound time.Time as Go's own string form,
	// and PruneDeliveredSystem compares the column against another bound
	// time.Time. One writer, one format, one zone — otherwise the comparison
	// is between two different renderings of an instant.
	now := time.Now().UTC()
	res, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO github_webhook_deliveries (installation_id, delivery_id, received_at)
		VALUES (?, ?, ?)
	`, installationID, deliveryID, now)
	if err != nil {
		return false, fmt.Errorf("insert github_webhook_deliveries: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected github_webhook_deliveries: %w", err)
	}
	return n > 0, nil
}

func (s *gitHubDeliveryStore) PruneDeliveredSystem(ctx context.Context) error {
	if _, err := s.q.ExecContext(ctx, `
		DELETE FROM github_webhook_deliveries WHERE received_at < ?
	`, time.Now().UTC().Add(-db.GitHubDeliveryPruneAge)); err != nil {
		return fmt.Errorf("prune github_webhook_deliveries: %w", err)
	}
	return nil
}
