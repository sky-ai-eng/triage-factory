package dbtest

import (
	"context"
	"errors"
	"slices"
	"sync"
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

// blueprintLocks are the locking reads, each a function of one id so one
// subtest body covers all three.
var blueprintLocks = []struct {
	name string
	read func(db.BlueprintStore, context.Context, string, string) (*domain.Blueprint, error)
}{
	{"GetForShare", db.BlueprintStore.GetForShare},
	{"GetForUpdate", db.BlueprintStore.GetForUpdate},
	{"GetAllForUpdate", func(s db.BlueprintStore, ctx context.Context, orgID, id string) (*domain.Blueprint, error) {
		held, err := s.GetAllForUpdate(ctx, orgID, []string{id})
		return held[id], err
	}},
}

// RunBlueprintLockConformance pins BlueprintStore.GetForShare, GetForUpdate
// and GetAllForUpdate by what their callers rely on: each reads the live
// blueprint as Get does; a write that holds a blueprint to reference it waits
// for a retire that held it first, and then reads it as absent; outside a
// write transaction, where nothing would hold the row, all three are refused;
// EventHandlers.Update, which holds a blueprint it attaches the same way,
// refuses an attach that waited on a retire; MergeInto's hold of its source
// orders it against GetForShare in both directions, so a check the merge
// makes after it sees a reference that came first; every write that changes a
// blueprint's steps answers one that is not live with ErrNoSuchBlueprint and
// writes nothing; and a merge into a blueprint that a merge in flight absorbs
// waits for it, then finds its host retired and leaves its source's steps
// where they were.
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

	// A merge holds its source before it moves a step, and the handler check
	// a caller makes after it runs under that hold, so the hold has to order
	// the merge against GetForShare in both directions.
	t.Run("a_share_waits_for_a_merges_retire_and_reads_it_absent", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		host := seedLockBlueprint(t, stores, orgID, userID, teamID, "host")
		source := seedLockBlueprint(t, stores, orgID, userID, teamID, "absorbed")

		held, release := make(chan struct{}), make(chan struct{})
		free := releaseOnCleanup(t, release)
		merge := make(chan error, 1)
		go func() {
			merge <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				if err := tx.Blueprints.MergeInto(ctx, orgID, host, source); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		select {
		case <-held:
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
				r.blueprint, err = tx.Blueprints.GetForShare(ctx, orgID, source)
				return err
			})
			reference <- r
		}()
		select {
		case r := <-reference:
			t.Fatalf("GetForShare answered while the merge held its retire: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		free()
		if err := <-merge; err != nil {
			t.Fatalf("merge: %v", err)
		}
		select {
		case r := <-reference:
			if r.err != nil {
				t.Fatalf("GetForShare after the merge committed: %v", r.err)
			}
			if r.blueprint != nil {
				t.Errorf("GetForShare after the merge committed = %+v; want the source absent", r.blueprint)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("GetForShare never answered after the merge committed")
		}
	})

	t.Run("a_merges_retire_waits_for_a_share_and_its_check_sees_the_reference", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		host := seedLockBlueprint(t, stores, orgID, userID, teamID, "host")
		source := seedLockBlueprint(t, stores, orgID, userID, teamID, "referenced")

		held, release := make(chan struct{}), make(chan struct{})
		free := releaseOnCleanup(t, release)
		reference := make(chan error, 1)
		go func() {
			reference <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				b, err := tx.Blueprints.GetForShare(ctx, orgID, source)
				if err != nil {
					return err
				}
				if b == nil {
					t.Error("GetForShare read the live blueprint as absent")
				}
				if _, err := tx.EventHandlers.Create(ctx, orgID, teamID, newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPROpened, "on the source", source)); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		select {
		case <-held:
		case err := <-reference:
			t.Fatalf("the reference ended before holding the blueprint: %v", err)
		}

		type seen struct {
			handlers int
			err      error
		}
		merge := make(chan seen, 1)
		go func() {
			var s seen
			s.err = stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				if err := tx.Blueprints.MergeInto(ctx, orgID, host, source); err != nil {
					return err
				}
				handlers, err := tx.EventHandlers.ListForBlueprint(ctx, orgID, source)
				s.handlers = len(handlers)
				return err
			})
			merge <- s
		}()
		select {
		case s := <-merge:
			t.Fatalf("the merge retired the source while a reference held it: %+v", s)
		case <-time.After(200 * time.Millisecond):
		}
		free()
		if err := <-reference; err != nil {
			t.Fatalf("reference: %v", err)
		}
		select {
		case s := <-merge:
			if s.err != nil {
				t.Fatalf("merge: %v", s.err)
			}
			if s.handlers != 1 {
				t.Errorf("the merge's check after its retire read %d handlers; want the reference's", s.handlers)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the merge never retired the source after the reference committed")
		}
	})

	t.Run("GetAllForUpdate_leaves_out_what_is_not_live", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		first := seedLockBlueprint(t, stores, orgID, userID, teamID, "first")
		second := seedLockBlueprint(t, stores, orgID, userID, teamID, "second")
		deleted := seedLockBlueprint(t, stores, orgID, userID, teamID, "deleted")
		if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			return tx.Blueprints.Delete(ctx, orgID, deleted)
		}); err != nil {
			t.Fatalf("delete blueprint: %v", err)
		}
		if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			held, err := tx.Blueprints.GetAllForUpdate(ctx, orgID, []string{second, deleted, uuid.NewString(), first})
			if err != nil {
				return err
			}
			if len(held) != 2 || held[first] == nil || held[first].Name != "first" || held[second] == nil || held[second].Name != "second" {
				t.Errorf("GetAllForUpdate = %+v; want the two live blueprints by id", held)
			}
			return nil
		}); err != nil {
			t.Fatalf("hold in a write transaction: %v", err)
		}
	})

	// Each step writer checks before it writes, so it leaves nothing behind
	// even in a transaction that commits after it refuses.
	t.Run("a_step_write_on_a_blueprint_that_is_not_live_writes_nothing", func(t *testing.T) {
		writes := map[string]func(tx db.TxStores, ctx context.Context, orgID, retired, live string) error{
			"ReplaceSteps": func(tx db.TxStores, ctx context.Context, orgID, retired, live string) error {
				_, err := tx.Blueprints.ReplaceSteps(ctx, orgID, retired, nil, nil)
				return err
			},
			"SplitAt": func(tx db.TxStores, ctx context.Context, orgID, retired, live string) error {
				_, err := tx.Blueprints.SplitAt(ctx, orgID, retired, 1, uuid.NewString(), "downstream")
				return err
			},
			"DeleteStep": func(tx db.TxStores, ctx context.Context, orgID, retired, live string) error {
				_, err := tx.Blueprints.DeleteStep(ctx, orgID, retired, 0, "downstream")
				return err
			},
			"MergeInto_a_retired_host": func(tx db.TxStores, ctx context.Context, orgID, retired, live string) error {
				return tx.Blueprints.MergeInto(ctx, orgID, retired, live)
			},
			"MergeInto_a_retired_source": func(tx db.TxStores, ctx context.Context, orgID, retired, live string) error {
				return tx.Blueprints.MergeInto(ctx, orgID, live, retired)
			},
		}
		for name, write := range writes {
			t.Run(name, func(t *testing.T) {
				stores, orgID, userID, teamID := mk(t)
				ctx := t.Context()
				retired := seedLockChain(t, stores, orgID, userID, teamID, "retired", 2)
				live := seedLockChain(t, stores, orgID, userID, teamID, "live", 1)
				retiredSteps, liveSteps := lockStepPrompts(t, stores, orgID, userID, retired), lockStepPrompts(t, stores, orgID, userID, live)
				if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
					return tx.Blueprints.Delete(ctx, orgID, retired)
				}); err != nil {
					t.Fatalf("retire blueprint: %v", err)
				}

				var writeErr error
				if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
					writeErr = write(tx, ctx, orgID, retired, live)
					return nil
				}); err != nil {
					t.Fatalf("commit after the write: %v", err)
				}
				if !errors.Is(writeErr, db.ErrNoSuchBlueprint) {
					t.Errorf("%s = %v; want db.ErrNoSuchBlueprint", name, writeErr)
				}
				if got := lockStepPrompts(t, stores, orgID, userID, retired); !slices.Equal(got, retiredSteps) {
					t.Errorf("retired blueprint's steps after %s = %v; want %v", name, got, retiredSteps)
				}
				if got := lockStepPrompts(t, stores, orgID, userID, live); !slices.Equal(got, liveSteps) {
					t.Errorf("live blueprint's steps after %s = %v; want %v", name, got, liveSteps)
				}
				var stillLive *domain.Blueprint
				if err := stores.Tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
					var err error
					stillLive, err = tx.Blueprints.Get(ctx, orgID, live)
					return err
				}); err != nil || stillLive == nil {
					t.Errorf("live blueprint after %s = %+v, %v; want it live", name, stillLive, err)
				}
			})
		}
	})

	// The race the hold exists for: a merge of B into A, while a merge of A
	// into X holds A. The second waits for the first, then finds A retired,
	// and B's steps stay on B rather than on the retired A.
	t.Run("a_merge_into_a_blueprint_a_merge_absorbs_waits_and_finds_its_host_retired", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		x := seedLockChain(t, stores, orgID, userID, teamID, "x", 1)
		a := seedLockChain(t, stores, orgID, userID, teamID, "a", 2)
		b := seedLockChain(t, stores, orgID, userID, teamID, "b", 1)
		xSteps, aSteps, bSteps := lockStepPrompts(t, stores, orgID, userID, x), lockStepPrompts(t, stores, orgID, userID, a), lockStepPrompts(t, stores, orgID, userID, b)

		held, release := make(chan struct{}), make(chan struct{})
		free := releaseOnCleanup(t, release)
		absorb := make(chan error, 1)
		go func() {
			absorb <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				if err := tx.Blueprints.MergeInto(ctx, orgID, x, a); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		select {
		case <-held:
		case err := <-absorb:
			t.Fatalf("the merge of A into X ended before holding A: %v", err)
		}

		into := make(chan error, 1)
		go func() {
			into <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				return tx.Blueprints.MergeInto(ctx, orgID, a, b)
			})
		}()
		select {
		case err := <-into:
			t.Fatalf("the merge of B into A answered while the merge of A into X held A: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		free()
		if err := <-absorb; err != nil {
			t.Fatalf("merge A into X: %v", err)
		}
		select {
		case err := <-into:
			if !errors.Is(err, db.ErrNoSuchBlueprint) {
				t.Errorf("merge B into A after A was absorbed = %v; want db.ErrNoSuchBlueprint", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the merge of B into A never answered after the merge of A into X committed")
		}
		if got, want := lockStepPrompts(t, stores, orgID, userID, x), append(append([]string{}, xSteps...), aSteps...); !slices.Equal(got, want) {
			t.Errorf("X's steps = %v; want its own then A's, %v", got, want)
		}
		if got := lockStepPrompts(t, stores, orgID, userID, a); len(got) != 0 {
			t.Errorf("the retired A's steps = %v; want none", got)
		}
		if got := lockStepPrompts(t, stores, orgID, userID, b); !slices.Equal(got, bSteps) {
			t.Errorf("B's steps = %v; want its own, %v", got, bSteps)
		}
	})

	t.Run("a_merge_of_a_source_that_is_not_live_is_ErrNoSuchBlueprint", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()
		host := seedLockBlueprint(t, stores, orgID, userID, teamID, "host")
		source := seedLockBlueprint(t, stores, orgID, userID, teamID, "absorbed")
		merge := func() error {
			return stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				return tx.Blueprints.MergeInto(ctx, orgID, host, source)
			})
		}
		if err := merge(); err != nil {
			t.Fatalf("first merge: %v", err)
		}
		if err := merge(); !errors.Is(err, db.ErrNoSuchBlueprint) {
			t.Errorf("merge of a retired source = %v; want db.ErrNoSuchBlueprint", err)
		}
	})
}

// releaseOnCleanup returns what closes release, once, and closes it when the
// test ends too: a failed check must still end the transaction held on it,
// or its locks outlive the subtest.
func releaseOnCleanup(t *testing.T, release chan struct{}) func() {
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	return free
}

// seedLockChain creates a blueprint on teamID whose steps are n new prompts,
// under userID's claims, and returns its id.
func seedLockChain(t *testing.T, stores db.Stores, orgID, userID, teamID, name string, n int) string {
	t.Helper()
	id := seedLockBlueprint(t, stores, orgID, userID, teamID, name)
	if err := stores.Tx.WithTx(t.Context(), orgID, userID, func(tx db.TxStores) error {
		prompts := make([]string, n)
		for i := range prompts {
			p, err := tx.Prompts.Create(t.Context(), orgID, teamID, domain.Prompt{
				ID: uuid.NewString(), Name: name + " step", Body: "body", Source: "user",
			})
			if err != nil {
				return err
			}
			prompts[i] = p.ID
		}
		_, err := tx.Blueprints.ReplaceSteps(t.Context(), orgID, id, prompts, nil)
		return err
	}); err != nil {
		t.Fatalf("seed the steps of %s: %v", name, err)
	}
	return id
}

// lockStepPrompts reads a blueprint's step prompts in step order, whether or
// not it is live.
func lockStepPrompts(t *testing.T, stores db.Stores, orgID, userID, id string) []string {
	t.Helper()
	var out []string
	if err := stores.Tx.WithReadTx(t.Context(), orgID, userID, func(tx db.TxStores) error {
		steps, err := tx.Blueprints.ListSteps(t.Context(), orgID, id)
		for _, st := range steps {
			out = append(out, st.StepPromptID)
		}
		return err
	}); err != nil {
		t.Fatalf("read the steps of %s: %v", id, err)
	}
	return out
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
