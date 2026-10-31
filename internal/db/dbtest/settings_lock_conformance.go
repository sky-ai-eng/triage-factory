package dbtest

import (
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// SettingsLockFactory hands RunSettingsLockConformance the wired stores,
// whose Tx opens the transactions, and the org, user and team to open them as.
// The user administers the team, as the callers that take the lock do.
type SettingsLockFactory func(t *testing.T) (stores db.Stores, orgID, userID, teamID string)

// RunSettingsLockConformance pins TeamsStore.LockSettings by what its callers
// rely on: a second write transaction on the team gets past the lock only once
// the first has committed, and then reads what the first stored; and outside a
// transaction, where nothing would hold it, it is refused.
func RunSettingsLockConformance(t *testing.T, mk SettingsLockFactory) {
	t.Run("refused_outside_a_transaction", func(t *testing.T) {
		stores, _, _, teamID := mk(t)
		if err := stores.Teams.LockSettings(t.Context(), teamID); err == nil {
			t.Error("LockSettings outside a transaction = nil; want a refusal")
		}
	})

	t.Run("second_writer_waits_for_the_first_to_commit", func(t *testing.T) {
		stores, orgID, userID, teamID := mk(t)
		ctx := t.Context()

		locked, release := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				if err := tx.Teams.LockSettings(ctx, teamID); err != nil {
					return err
				}
				if _, err := tx.Teams.SetLinearTeamOrder(ctx, teamID, []string{"written-by-first"}); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			})
		}()
		select {
		case <-locked:
		case err := <-first:
			t.Fatalf("first writer ended before holding the lock: %v", err)
		}

		type read struct {
			linearTeams []string
			err         error
		}
		second := make(chan read, 1)
		go func() {
			var r read
			r.err = stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				if err := tx.Teams.LockSettings(ctx, teamID); err != nil {
					return err
				}
				set, err := tx.Teams.GetSettings(ctx, teamID)
				r.linearTeams = set.LinearTeams
				return err
			})
			second <- r
		}()

		select {
		case r := <-second:
			t.Fatalf("second writer got past the lock while the first held it: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatalf("first writer: %v", err)
		}
		select {
		case r := <-second:
			if r.err != nil {
				t.Fatalf("second writer: %v", r.err)
			}
			if !slices.Equal(r.linearTeams, []string{"written-by-first"}) {
				t.Errorf("second writer read linear_teams = %v; want the first writer's", r.linearTeams)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("second writer never got past the lock after the first committed")
		}
	})
}
