package pg_test

import (
	"context"
	"errors"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestDeliveryStore_Postgres_MarkDeliveredSystem_FreshThenDuplicate: the
// first delivery of an event_id is fresh; a redelivery (Slack retry) of the
// same (api_app_id, event_id) is not.
func TestDeliveryStore_Postgres_MarkDeliveredSystem_FreshThenDuplicate(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	fresh, err := slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "A0DELIV01", "Ev1")
	if err != nil {
		t.Fatalf("MarkDeliveredSystem (first): %v", err)
	}
	if !fresh {
		t.Error("first delivery: fresh = false; want true")
	}

	fresh, err = slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "A0DELIV01", "Ev1")
	if err != nil {
		t.Fatalf("MarkDeliveredSystem (duplicate): %v", err)
	}
	if fresh {
		t.Error("redelivered event_id: fresh = true; want false (dedup)")
	}

	// A different app with the same event_id is a distinct delivery — the
	// primary key is the (api_app_id, event_id) pair, not event_id alone.
	// This also covers two apps sharing a workspace: a shared workspace_id
	// never enters this key at all.
	fresh, err = slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "A0DELIV02", "Ev1")
	if err != nil {
		t.Fatalf("MarkDeliveredSystem (other app): %v", err)
	}
	if !fresh {
		t.Error("same event_id under a different api_app_id: fresh = false; want true")
	}
}

// TestDeliveryStore_Postgres_PruneDeliveredSystem: the prune sweeps
// deliveries older than 72 hours, regardless of app or event, and leaves
// recent ones.
func TestDeliveryStore_Postgres_PruneDeliveredSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	if _, err := h.AdminDB.Exec(`
		INSERT INTO slack_event_deliveries (api_app_id, event_id, received_at)
		VALUES ('A0STALE01', 'EvStale', now() - interval '100 hours')
	`); err != nil {
		t.Fatalf("seed stale delivery: %v", err)
	}
	if _, err := slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "A0FRESH01", "EvFresh"); err != nil {
		t.Fatalf("MarkDeliveredSystem: %v", err)
	}

	if err := slackstore.FromStores(stores).Deliveries.PruneDeliveredSystem(ctx); err != nil {
		t.Fatalf("PruneDeliveredSystem: %v", err)
	}

	if n := countDeliveries(t, h, "A0STALE01"); n != 0 {
		t.Errorf("stale delivery rows remaining = %d; want 0", n)
	}
	if n := countDeliveries(t, h, "A0FRESH01"); n != 1 {
		t.Errorf("recent delivery rows = %d; want 1 (the prune must not remove it)", n)
	}
}

// TestDeliveryStore_Postgres_MarkDeliveredSystem_CommitsWithTheSystemTx: a
// mark on the bundle a system transaction hands commits with it — a body
// that fails after the mark leaves no row, so Slack's redelivery is fresh,
// and one that commits makes the redelivery a duplicate.
func TestDeliveryStore_Postgres_MarkDeliveredSystem_CommitsWithTheSystemTx(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "slack-delivery-tx")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mark := func(fail error) (bool, error) {
		var fresh bool
		err := stores.Tx.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
			var err error
			if fresh, err = slackstore.FromSystemTx(tx).Deliveries.MarkDeliveredSystem(ctx, "A0DELTX01", "Ev1"); err != nil {
				return err
			}
			return fail
		})
		return fresh, err
	}

	later := errors.New("the delivery's work failed")
	if _, err := mark(later); !errors.Is(err, later) {
		t.Fatalf("SystemWithTx = %v; want the body's error", err)
	}
	if n := countDeliveries(t, h, "A0DELTX01"); n != 0 {
		t.Fatalf("delivery rows after a rolled-back mark = %d; want 0", n)
	}
	if fresh, err := mark(nil); err != nil || !fresh {
		t.Fatalf("redelivery after the rollback: fresh = %v, err = %v; want fresh", fresh, err)
	}
	if fresh, err := mark(nil); err != nil || fresh {
		t.Errorf("redelivery after the commit: fresh = %v, err = %v; want a duplicate", fresh, err)
	}
}

// TestDeliveryStore_Postgres_MarkDeliveredSystem_WaitsOnAnOpenMark: a second
// mark of a delivery whose first mark sits in an open transaction waits for
// that transaction, then answers by its outcome — fresh after a rollback, a
// duplicate after a commit. Answering before it ends is how one event would
// be processed twice.
func TestDeliveryStore_Postgres_MarkDeliveredSystem_WaitsOnAnOpenMark(t *testing.T) {
	for _, tc := range []struct {
		name      string
		commit    bool
		wantFresh bool
	}{
		{name: "rollback", commit: false, wantFresh: true},
		{name: "commit", commit: true, wantFresh: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := pgtest.Shared(t)
			h.Reset(t)
			orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "slack-delivery-wait-"+tc.name)
			stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
			ctx := context.Background()

			marked, release := make(chan struct{}), make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- stores.Tx.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
					if _, err := slackstore.FromSystemTx(tx).Deliveries.MarkDeliveredSystem(ctx, "A0DELWAIT", "Ev1"); err != nil {
						close(marked)
						return err
					}
					close(marked)
					<-release
					if tc.commit {
						return nil
					}
					return errors.New("the first delivery's work failed")
				})
			}()
			<-marked

			type result struct {
				fresh bool
				err   error
			}
			second := make(chan result, 1)
			go func() {
				fresh, err := slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "A0DELWAIT", "Ev1")
				second <- result{fresh, err}
			}()
			select {
			case r := <-second:
				t.Fatalf("second mark answered (fresh=%v, err=%v) while the first was uncommitted; want it to wait", r.fresh, r.err)
			case <-time.After(200 * time.Millisecond):
			}
			close(release)
			if err := <-firstDone; tc.commit && err != nil {
				t.Fatalf("first transaction: %v", err)
			}
			select {
			case r := <-second:
				if r.err != nil || r.fresh != tc.wantFresh {
					t.Errorf("second mark: fresh = %v, err = %v; want fresh = %v", r.fresh, r.err, tc.wantFresh)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("second mark never answered after the first transaction ended")
			}
		})
	}
}

func countDeliveries(t *testing.T, h *pgtest.Harness, apiAppID string) int {
	t.Helper()
	var n int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM slack_event_deliveries WHERE api_app_id = $1`, apiAppID).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return n
}

// TestWorkspaceStore_Postgres_GetByWorkspaceAppSystem: the admin-pool lookup
// by (workspace_id, api_app_id) alone (no org_id to scope by) is how the
// pre-auth webhook receiver resolves which org a delivery belongs to.
func TestWorkspaceStore_Postgres_GetByWorkspaceAppSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-getbyid")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspace(orgID, "T0GETSYS1"))
	})

	got, err := slackstore.FromStores(stores).Workspaces.GetByWorkspaceAppSystem(ctx, "T0GETSYS1", defaultAppID("T0GETSYS1"))
	if err != nil {
		t.Fatalf("GetByWorkspaceAppSystem: %v", err)
	}
	if got == nil || got.OrgID != orgID {
		t.Fatalf("GetByWorkspaceAppSystem = %+v; want a row bound to org %s", got, orgID)
	}

	missing, err := slackstore.FromStores(stores).Workspaces.GetByWorkspaceAppSystem(ctx, "T0GETSYS1", "A0WRONGAPP")
	if err != nil {
		t.Fatalf("GetByWorkspaceAppSystem (wrong app id): %v", err)
	}
	if missing != nil {
		t.Errorf("GetByWorkspaceAppSystem for the right workspace but wrong app id = %+v; want nil", missing)
	}

	missing, err = slackstore.FromStores(stores).Workspaces.GetByWorkspaceAppSystem(ctx, "T0NOPE0001", defaultAppID("T0NOPE0001"))
	if err != nil {
		t.Fatalf("GetByWorkspaceAppSystem (unknown): %v", err)
	}
	if missing != nil {
		t.Errorf("GetByWorkspaceAppSystem for an unconnected pair = %+v; want nil", missing)
	}
}
