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
// the dedup has to actually dedup here too.
type gitHubDeliveryStore struct{ q queryer }

func newGitHubDeliveryStore(q queryer) db.GitHubDeliveryStore {
	return &gitHubDeliveryStore{q: q}
}

var _ db.GitHubDeliveryStore = (*gitHubDeliveryStore)(nil)

func (s *gitHubDeliveryStore) MarkDeliveredSystem(ctx context.Context, installationID, deliveryID string) (bool, error) {
	// received_at is written here rather than defaulted in the schema, always
	// in UTC: this driver stores a bound time.Time as Go's own string form,
	// and the prune below compares the column against another bound time.Time.
	// One writer, one format, one zone — otherwise the comparison is between
	// two different renderings of an instant.
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
	fresh := n > 0

	// Best-effort: a missed prune grows the table a little until the next
	// successful call, which is never a reason to fail the delivery this call
	// is deduping.
	_, _ = s.q.ExecContext(ctx, `
		DELETE FROM github_webhook_deliveries WHERE received_at < ?
	`, now.Add(-db.GitHubDeliveryPruneAge))

	return fresh, nil
}
