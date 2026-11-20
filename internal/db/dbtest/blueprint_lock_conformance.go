package dbtest

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// BlueprintLockFactory hands RunBlueprintLockConformance the wired stores,
// whose Tx opens the transactions, and the org, user and team to open them as.
// The user writes the team, as the callers that take the locks do.
type BlueprintLockFactory func(t *testing.T) (stores db.Stores, orgID, userID, teamID string)

// RunBlueprintLockConformance pins BlueprintStore.LockForReference and
// LockForRetire by what their callers rely on: each reads the live blueprint
// as Get does; a retire that takes its lock after a reference waits for the
// reference's write to commit and then sees it; a reference that takes its
// lock after a retire waits for the retire to commit and then reads the
// blueprint as absent; and outside a transaction, where nothing would hold
// them, both are refused.
func RunBlueprintLockConformance(t *testing.T, mk BlueprintLockFactory) {
	seed := func(t *testing.T, stores db.Stores, orgID, userID, teamID, name string) string {
		t.Helper()
		id := uuid.New().String()
		if err := stores.Tx.WithTx(t.Context(), orgID, userID, func(tx db.TxStores) error {
			_, err := tx.Blueprints.Create(t.Context(), orgID, teamID, domain.Blueprint{ID: id, Name: name, Source: "user", TeamID: teamID})
			return err
		}); err != nil {
			t.Fatalf("seed blueprint %s: %v", name, err)
		}
		return id
	}
	locks := map[string]func(db.BlueprintStore) func(context.Context, string, string) (*domain.Blueprint, error){
		"LockForReference": func(s db.BlueprintStore) func(context.Context, string, string) (*domain.Blueprint, error) {
			return s.LockForReference
		},
		"LockForRetire": func(s db.BlueprintStore) func(context.Context, string, string) (*domain.Blueprint, error) {
			return s.LockForRetire
		},
	}

	t.Run("reads_the_live_blueprint_as_Get_does", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		host := seed(t, stores, orgID, userID, teamID, "Host")
		live := seed(t, stores, orgID, userID, teamID, "Live")
		retired := seed(t, stores, orgID, userID, teamID, "Retired")
		if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			return tx.Blueprints.MergeInto(ctx, orgID, host, retired)
		}); err != nil {
			t.Fatalf("retire a blueprint: %v", err)
		}
		for name, lock := range locks {
			if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				want, err := tx.Blueprints.Get(ctx, orgID, live)
				if err != nil {
					return err
				}
				got, err := lock(tx.Blueprints)(ctx, orgID, live)
				if err != nil {
					return err
				}
				if got == nil || !reflect.DeepEqual(*got, *want) {
					t.Errorf("%s(live) = %+v, want what Get reads, %+v", name, got, want)
				}
				for _, absent := range []string{retired, uuid.New().String()} {
					got, err := lock(tx.Blueprints)(ctx, orgID, absent)
					if err != nil {
						return err
					}
					if got != nil {
						t.Errorf("%s(%s) = %+v, want nil for a blueprint that is not live", name, absent, got)
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})

	t.Run("a_retire_waits_for_a_reference_and_then_sees_it", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		bp := seed(t, stores, orgID, userID, teamID, "Referenced")

		locked, release := make(chan struct{}), make(chan struct{})
		// A failed check must still end the held transaction, or its locks
		// outlive the subtest.
		var once sync.Once
		free := func() { once.Do(func() { close(release) }) }
		t.Cleanup(free)
		reference := make(chan error, 1)
		go func() {
			reference <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				got, err := tx.Blueprints.LockForReference(ctx, orgID, bp)
				if err != nil {
					return err
				}
				if got == nil {
					t.Error("LockForReference(live) = nil")
				}
				if _, err := tx.EventHandlers.Create(ctx, orgID, teamID, newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPROpened, "on the blueprint", bp)); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			})
		}()
		select {
		case <-locked:
		case err := <-reference:
			t.Fatalf("the reference ended before holding the lock: %v", err)
		}

		type seen struct {
			live     bool
			handlers int
			err      error
		}
		retire := make(chan seen, 1)
		go func() {
			var s seen
			s.err = stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				got, err := tx.Blueprints.LockForRetire(ctx, orgID, bp)
				if err != nil {
					return err
				}
				s.live = got != nil
				handlers, err := tx.EventHandlers.ListForBlueprint(ctx, orgID, bp)
				s.handlers = len(handlers)
				return err
			})
			retire <- s
		}()

		select {
		case s := <-retire:
			t.Fatalf("the retire got past its lock while the reference held one: %+v", s)
		case <-time.After(200 * time.Millisecond):
		}
		free()
		if err := <-reference; err != nil {
			t.Fatalf("reference: %v", err)
		}
		select {
		case s := <-retire:
			if s.err != nil {
				t.Fatalf("retire: %v", s.err)
			}
			if !s.live || s.handlers != 1 {
				t.Errorf("the retire read (live %v, %d handlers), want the live blueprint and the reference's handler", s.live, s.handlers)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the retire never got past its lock after the reference committed")
		}
	})

	t.Run("a_reference_waits_for_a_retire_and_then_reads_it_absent", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		host := seed(t, stores, orgID, userID, teamID, "Host")
		source := seed(t, stores, orgID, userID, teamID, "Absorbed")

		locked, release := make(chan struct{}), make(chan struct{})
		// A failed check must still end the held transaction, or its locks
		// outlive the subtest.
		var once sync.Once
		free := func() { once.Do(func() { close(release) }) }
		t.Cleanup(free)
		retire := make(chan error, 1)
		go func() {
			retire <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				got, err := tx.Blueprints.LockForRetire(ctx, orgID, source)
				if err != nil {
					return err
				}
				if got == nil {
					t.Error("LockForRetire(live) = nil")
				}
				if err := tx.Blueprints.MergeInto(ctx, orgID, host, source); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			})
		}()
		select {
		case <-locked:
		case err := <-retire:
			t.Fatalf("the retire ended before holding the lock: %v", err)
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
				r.blueprint, err = tx.Blueprints.LockForReference(ctx, orgID, source)
				return err
			})
			reference <- r
		}()

		select {
		case r := <-reference:
			t.Fatalf("the reference got past its lock while the retire held one: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		free()
		if err := <-retire; err != nil {
			t.Fatalf("retire: %v", err)
		}
		select {
		case r := <-reference:
			if r.err != nil {
				t.Fatalf("reference: %v", r.err)
			}
			if r.blueprint != nil {
				t.Errorf("the reference read %+v, want nil for the blueprint the retire committed", r.blueprint)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the reference never got past its lock after the retire committed")
		}
	})

	t.Run("refused_outside_a_transaction", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		bp := seed(t, stores, orgID, userID, teamID, "Unlocked")
		for name, lock := range locks {
			if _, err := lock(stores.Blueprints)(t.Context(), orgID, bp); err == nil {
				t.Errorf("%s outside a transaction = nil; want a refusal", name)
			}
		}
	})
}
