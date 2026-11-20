package dbtest

import (
	"errors"
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
// The user writes the team, as the callers that take the lock do.
type BlueprintLockFactory func(t *testing.T) (stores db.Stores, orgID, userID, teamID string)

// RunBlueprintLockConformance pins BlueprintStore.LockForReference by what
// its callers rely on, against the retire MergeInto writes: the lock reads the
// live blueprint as Get does; a merge that retires the blueprint after a
// reference took the lock waits for the reference to commit, and a check the
// merge makes after its retire sees the reference; a reference that comes
// after the retire waits for the merge to commit and reads the blueprint as
// absent; MergeInto answers a source that is no longer live with
// ErrNoSuchBlueprint; and outside a transaction, where nothing would hold it,
// the lock is refused.
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
	// hold is a transaction a test keeps open until it frees it. A failed
	// check must still end the held transaction, or its locks outlive the
	// subtest.
	type hold struct {
		locked, release chan struct{}
		free            func()
	}
	newHold := func(t *testing.T) hold {
		h := hold{locked: make(chan struct{}), release: make(chan struct{})}
		var once sync.Once
		h.free = func() { once.Do(func() { close(h.release) }) }
		t.Cleanup(h.free)
		return h
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
		if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			want, err := tx.Blueprints.Get(ctx, orgID, live)
			if err != nil {
				return err
			}
			got, err := tx.Blueprints.LockForReference(ctx, orgID, live)
			if err != nil {
				return err
			}
			if got == nil || !reflect.DeepEqual(*got, *want) {
				t.Errorf("LockForReference(live) = %+v, want what Get reads, %+v", got, want)
			}
			for _, absent := range []string{retired, uuid.New().String()} {
				got, err := tx.Blueprints.LockForReference(ctx, orgID, absent)
				if err != nil {
					return err
				}
				if got != nil {
					t.Errorf("LockForReference(%s) = %+v, want nil for a blueprint that is not live", absent, got)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("LockForReference: %v", err)
		}
	})

	t.Run("a_retire_waits_for_a_reference_and_then_sees_it", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		host := seed(t, stores, orgID, userID, teamID, "Host")
		bp := seed(t, stores, orgID, userID, teamID, "Referenced")

		held := newHold(t)
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
				close(held.locked)
				<-held.release
				return nil
			})
		}()
		select {
		case <-held.locked:
		case err := <-reference:
			t.Fatalf("the reference ended before holding the lock: %v", err)
		}

		type seen struct {
			handlers int
			err      error
		}
		merge := make(chan seen, 1)
		go func() {
			var s seen
			s.err = stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				if err := tx.Blueprints.MergeInto(ctx, orgID, host, bp); err != nil {
					return err
				}
				handlers, err := tx.EventHandlers.ListForBlueprint(ctx, orgID, bp)
				s.handlers = len(handlers)
				return err
			})
			merge <- s
		}()

		select {
		case s := <-merge:
			t.Fatalf("the merge retired the blueprint while the reference held its lock: %+v", s)
		case <-time.After(200 * time.Millisecond):
		}
		held.free()
		if err := <-reference; err != nil {
			t.Fatalf("reference: %v", err)
		}
		select {
		case s := <-merge:
			if s.err != nil {
				t.Fatalf("merge: %v", s.err)
			}
			if s.handlers != 1 {
				t.Errorf("the merge's check after its retire read %d handlers, want the reference's", s.handlers)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the merge never retired the blueprint after the reference committed")
		}
	})

	t.Run("a_reference_waits_for_a_retire_and_then_reads_it_absent", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		host := seed(t, stores, orgID, userID, teamID, "Host")
		source := seed(t, stores, orgID, userID, teamID, "Absorbed")

		held := newHold(t)
		merge := make(chan error, 1)
		go func() {
			merge <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				if err := tx.Blueprints.MergeInto(ctx, orgID, host, source); err != nil {
					return err
				}
				close(held.locked)
				<-held.release
				return nil
			})
		}()
		select {
		case <-held.locked:
		case err := <-merge:
			t.Fatalf("the merge ended before holding its retire: %v", err)
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
			t.Fatalf("the reference got past its lock while the merge held the retire: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		held.free()
		if err := <-merge; err != nil {
			t.Fatalf("merge: %v", err)
		}
		select {
		case r := <-reference:
			if r.err != nil {
				t.Fatalf("reference: %v", r.err)
			}
			if r.blueprint != nil {
				t.Errorf("the reference read %+v, want nil for the blueprint the merge retired", r.blueprint)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the reference never got past its lock after the merge committed")
		}
	})

	t.Run("a_merge_of_a_source_that_is_not_live_is_ErrNoSuchBlueprint", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		host := seed(t, stores, orgID, userID, teamID, "Host")
		source := seed(t, stores, orgID, userID, teamID, "Absorbed")
		merge := func() error {
			return stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				return tx.Blueprints.MergeInto(ctx, orgID, host, source)
			})
		}
		if err := merge(); err != nil {
			t.Fatalf("first merge: %v", err)
		}
		if err := merge(); !errors.Is(err, db.ErrNoSuchBlueprint) {
			t.Errorf("merge of a retired source = %v, want db.ErrNoSuchBlueprint", err)
		}
	})

	t.Run("refused_outside_a_transaction", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		bp := seed(t, stores, orgID, userID, teamID, "Unlocked")
		if _, err := stores.Blueprints.LockForReference(t.Context(), orgID, bp); err == nil {
			t.Error("LockForReference outside a transaction = nil; want a refusal")
		}
	})
}
