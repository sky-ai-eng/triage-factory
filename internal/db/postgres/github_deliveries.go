package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// gitHubDeliveryStore is the Postgres impl of db.GitHubDeliveryStore. Admin
// pool only: the pre-auth webhook receiver has no request claims for RLS to
// gate on, and github_webhook_deliveries carries no policy at all. admin is
// the admin pool, or the admin-pool transaction a db.SystemTxStores is bound
// to.
type gitHubDeliveryStore struct{ admin queryer }

func newGitHubDeliveryStore(admin queryer) db.GitHubDeliveryStore {
	return &gitHubDeliveryStore{admin: admin}
}

var _ db.GitHubDeliveryStore = (*gitHubDeliveryStore)(nil)

func (s *gitHubDeliveryStore) MarkDeliveredSystem(ctx context.Context, installationID, deliveryID string) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		INSERT INTO github_webhook_deliveries (installation_id, delivery_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, installationID, deliveryID)
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
	if _, err := s.admin.ExecContext(ctx, `
		DELETE FROM github_webhook_deliveries WHERE received_at < $1
	`, time.Now().UTC().Add(-db.GitHubDeliveryPruneAge)); err != nil {
		return fmt.Errorf("prune github_webhook_deliveries: %w", err)
	}
	return nil
}
