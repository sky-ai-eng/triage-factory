package dbtest

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SystemTxFactory is what a per-backend test file hands to
// RunSystemTxConformance: the wired stores, whose Tx opens the system
// transaction, the org to write for, and the event-queue seeder.
type SystemTxFactory func(t *testing.T) (stores db.Stores, orgID string, seed EventQueueSeeder)

// RunSystemTxConformance pins TxRunner.SystemWithTx through the event queue
// it binds: an event enqueued in the transaction commits with it, a body
// that fails after the enqueue leaves neither the events row nor the queue
// row, and the bound store refuses a claim, which needs transactions of its
// own.
func RunSystemTxConformance(t *testing.T, mk SystemTxFactory) {
	t.Run("enqueue_commits_with_the_transaction", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		ctx := t.Context()
		entityID := seed.Entity(t)

		var eventID string
		if err := stores.Tx.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
			var err error
			eventID, err = tx.EventQueue.Enqueue(ctx, orgID, domain.Event{
				EntityID: &entityID, EventType: domain.EventGitHubPRCICheckFailed,
			}, "")
			return err
		}); err != nil {
			t.Fatalf("SystemWithTx: %v", err)
		}
		rows, err := stores.EventQueue.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 1 || rows[0].EventID != eventID || rows[0].Status != "ready" {
			t.Fatalf("queue rows = %+v; want one ready row for event %s", rows, eventID)
		}
		if n := seed.CountEventRows(t, entityID); n != 1 {
			t.Errorf("events rows = %d; want 1", n)
		}
	})

	t.Run("enqueue_rolls_back_with_the_transaction", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		ctx := t.Context()
		entityID := seed.Entity(t)

		later := errors.New("a later write in the transaction failed")
		err := stores.Tx.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
			if _, err := tx.EventQueue.Enqueue(ctx, orgID, domain.Event{
				EntityID: &entityID, EventType: domain.EventGitHubPRCICheckFailed,
			}, ""); err != nil {
				return err
			}
			return later
		})
		if !errors.Is(err, later) {
			t.Fatalf("SystemWithTx = %v; want the body's error", err)
		}
		rows, err := stores.EventQueue.ListForEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("queue rows = %+v; want none after the rollback", rows)
		}
		if n := seed.CountEventRows(t, entityID); n != 0 {
			t.Errorf("events rows = %d; want 0 after the rollback", n)
		}
	})

	t.Run("claim_refuses_inside_the_transaction", func(t *testing.T) {
		stores, orgID, _ := mk(t)
		ctx := t.Context()

		err := stores.Tx.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
			_, err := tx.EventQueue.Claim(ctx, workitem.Owner{ID: "system-tx-conformance", Epoch: 1}, 1)
			return err
		})
		if !errors.Is(err, db.ErrEventQueueClaimInTx) {
			t.Errorf("Claim inside SystemWithTx = %v; want ErrEventQueueClaimInTx", err)
		}
	})
}
