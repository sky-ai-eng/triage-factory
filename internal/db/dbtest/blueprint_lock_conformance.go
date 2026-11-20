package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// BlueprintLockFactory hands RunBlueprintLockConformance the wired stores,
// whose Tx opens the transactions, and the org, user and team to open them
// as. The user can write the team, as the callers that take the lock can.
type BlueprintLockFactory func(t *testing.T) (stores db.Stores, orgID, userID, teamID string)

// blueprintLocks are the two locking reads, each a method expression so one
// subtest body covers both.
var blueprintLocks = []struct {
	name string
	read func(db.BlueprintStore, context.Context, string, string) (*domain.Blueprint, error)
}{
	{"GetForShare", db.BlueprintStore.GetForShare},
	{"GetForUpdate", db.BlueprintStore.GetForUpdate},
}

// RunBlueprintLockConformance pins BlueprintStore.GetForShare and
// GetForUpdate by what their callers rely on: each reads the live blueprint as
// Get does; a write that holds a blueprint to reference it waits for a retire
// that held it first, and then reads it as absent; outside a write
// transaction, where nothing would hold the row, both are refused; and
// EventHandlers.Update, which holds a blueprint it attaches the same way,
// refuses an attach that waited on a retire.
func RunBlueprintLockConformance(t *testing.T, mk BlueprintLockFactory) {
	for _, lock := range blueprintLocks {
		t.Run(lock.name+"_reads_the_live_blueprint", func(t *testing.T) {
			stores, orgID, userID, teamID := mk(t)
			ctx := t.Context()
			live := seedLockBlueprint(t, stores, orgID, userID, teamID, "live")
			deleted := seedLockBlueprint(t, stores, orgID, userID, teamID, "deleted")
			if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				return tx.Blueprints.Delete(ctx, orgID, deleted)
			}); err != nil {
				t.Fatalf("delete blueprint: %v", err)
			}

			if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				got, err := lock.read(tx.Blueprints, ctx, orgID, live)
				if err != nil {
					return err
				}
				want, err := tx.Blueprints.Get(ctx, orgID, live)
				if err != nil {
					return err
				}
				if got == nil || want == nil || got.ID != want.ID || got.Name != want.Name ||
					got.TeamID != want.TeamID || !got.UpdatedAt.Equal(want.UpdatedAt) {
					t.Errorf("%s(live) = %+v; want what Get reads, %+v", lock.name, got, want)
				}
				for what, id := range map[string]string{"a deleted blueprint": deleted, "an unknown id": uuid.NewString()} {
					got, err := lock.read(tx.Blueprints, ctx, orgID, id)
					if err != nil {
						return err
					}
					if got != nil {
						t.Errorf("%s(%s) = %+v; want nil", lock.name, what, got)
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("read in a write transaction: %v", err)
			}
		})

		t.Run(lock.name+"_is_refused_outside_a_write_transaction", func(t *testing.T) {
			stores, orgID, userID, teamID := mk(t)
			ctx := t.Context()
			id := seedLockBlueprint(t, stores, orgID, userID, teamID, "refused")
			if _, err := lock.read(stores.Blueprints, ctx, orgID, id); err == nil {
				t.Errorf("%s outside a transaction = nil error; want a refusal", lock.name)
			}
			if err := stores.Tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
				_, err := lock.read(tx.Blueprints, ctx, orgID, id)
				return err
			}); err == nil {
				t.Errorf("%s in a read transaction = nil error; want a refusal", lock.name)
			}
		})
	}

	// The shape the event handler writes and the blueprint deletes take: the
	// delete holds the blueprint, retires it, and commits; a reference write
	// that asked for it meanwhile gets it only after that commit, and reads it
	// as retired.
	t.Run("a_share_waits_for_an_update_and_reads_its_retire", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		id := seedLockBlueprint(t, stores, orgID, userID, teamID, "retired")

		held, release := make(chan struct{}), make(chan struct{})
		retire := make(chan error, 1)
		go func() {
			retire <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				b, err := tx.Blueprints.GetForUpdate(ctx, orgID, id)
				if err != nil {
					return err
				}
				if b == nil {
					t.Error("GetForUpdate read the live blueprint as absent")
				}
				if err := tx.Blueprints.Delete(ctx, orgID, id); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		select {
		case <-held:
		case err := <-retire:
			t.Fatalf("the retire ended before holding the blueprint: %v", err)
		}

		type read struct {
			blueprint *domain.Blueprint
			err       error
		}
		reference := make(chan read, 1)
		go func() {
			var r read
			r.err = stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				var err error
				r.blueprint, err = tx.Blueprints.GetForShare(ctx, orgID, id)
				return err
			})
			reference <- r
		}()
		select {
		case r := <-reference:
			t.Fatalf("GetForShare answered while the retire held the blueprint: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		if err := <-retire; err != nil {
			t.Fatalf("retire: %v", err)
		}
		select {
		case r := <-reference:
			if r.err != nil {
				t.Fatalf("GetForShare after the retire committed: %v", r.err)
			}
			if r.blueprint != nil {
				t.Errorf("GetForShare after the retire committed = %+v; want the blueprint absent", r.blueprint)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("GetForShare never answered after the retire committed")
		}
	})

	// EventHandlers.Update holds a blueprint it attaches by its own read of
	// the row, not the caller's: a caller writing back a blueprint from an
	// older read has not held it. The attach waits for a retire in flight and
	// is then refused, leaving the handler as it was.
	t.Run("a_handler_update_attaching_it_waits_for_a_retire_and_is_refused", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		id := seedLockBlueprint(t, stores, orgID, userID, teamID, "retired")
		handler := domain.EventHandler{
			ID: uuid.NewString(), Mode: domain.EventHandlerModeWatch, EventType: domain.EventGitHubPROpened,
			Source: domain.EventHandlerSourceUser, Name: "attaches late", DefaultPriority: 0.5, BreakerThreshold: 4,
		}
		if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			_, err := tx.EventHandlers.Create(ctx, orgID, teamID, handler)
			return err
		}); err != nil {
			t.Fatalf("seed handler: %v", err)
		}

		held, release := make(chan struct{}), make(chan struct{})
		retire := make(chan error, 1)
		go func() {
			retire <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				if _, err := tx.Blueprints.GetForUpdate(ctx, orgID, id); err != nil {
					return err
				}
				if err := tx.EventHandlers.DetachBlueprint(ctx, orgID, id); err != nil {
					return err
				}
				if err := tx.Blueprints.Delete(ctx, orgID, id); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		select {
		case <-held:
		case err := <-retire:
			t.Fatalf("the retire ended before holding the blueprint: %v", err)
		}

		attach := handler
		attach.BlueprintID = id
		update := make(chan error, 1)
		go func() {
			update <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				_, err := tx.EventHandlers.Update(ctx, orgID, attach)
				return err
			})
		}()
		select {
		case err := <-update:
			t.Fatalf("the attaching Update answered while the retire held the blueprint: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		if err := <-retire; err != nil {
			t.Fatalf("retire: %v", err)
		}
		select {
		case err := <-update:
			if !errors.Is(err, db.ErrEventHandlerBlueprintChanged) {
				t.Fatalf("attaching Update after the retire committed = %v; want ErrEventHandlerBlueprintChanged", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the attaching Update never answered after the retire committed")
		}
		var stored *domain.EventHandler
		if err := stores.Tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
			var err error
			stored, err = tx.EventHandlers.Get(ctx, orgID, handler.ID)
			return err
		}); err != nil || stored == nil {
			t.Fatalf("read handler: %+v, %v", stored, err)
		}
		if stored.BlueprintID != "" {
			t.Errorf("handler blueprint after the refused attach = %q; want none", stored.BlueprintID)
		}
	})
}

// seedLockBlueprint creates a blueprint on teamID under userID's claims and
// returns its id.
func seedLockBlueprint(t *testing.T, stores db.Stores, orgID, userID, teamID, name string) string {
	t.Helper()
	id := uuid.NewString()
	if err := stores.Tx.WithTx(t.Context(), orgID, userID, func(tx db.TxStores) error {
		_, err := tx.Blueprints.Create(t.Context(), orgID, teamID, domain.Blueprint{ID: id, Name: name, Source: "user"})
		return err
	}); err != nil {
		t.Fatalf("seed blueprint %s: %v", name, err)
	}
	return id
}
