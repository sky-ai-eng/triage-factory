package pg_test

import (
	"context"
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestDeliveryStore_Postgres_MarkDeliveredSystem_FreshThenDuplicate: the
// first delivery of an event_id is fresh; a redelivery (Slack retry) of the
// same (workspace_id, event_id) is not.
func TestDeliveryStore_Postgres_MarkDeliveredSystem_FreshThenDuplicate(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	fresh, err := slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "T0DELIV01", "Ev1")
	if err != nil {
		t.Fatalf("MarkDeliveredSystem (first): %v", err)
	}
	if !fresh {
		t.Error("first delivery: fresh = false; want true")
	}

	fresh, err = slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "T0DELIV01", "Ev1")
	if err != nil {
		t.Fatalf("MarkDeliveredSystem (duplicate): %v", err)
	}
	if fresh {
		t.Error("redelivered event_id: fresh = true; want false (dedup)")
	}

	// A different workspace with the same event_id is a distinct delivery —
	// the primary key is the (workspace_id, event_id) pair, not event_id alone.
	fresh, err = slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "T0DELIV02", "Ev1")
	if err != nil {
		t.Fatalf("MarkDeliveredSystem (other workspace): %v", err)
	}
	if !fresh {
		t.Error("same event_id under a different workspace_id: fresh = false; want true")
	}
}

// TestDeliveryStore_Postgres_MarkDeliveredSystem_PrunesStaleRows: the
// opportunistic prune piggybacked on every insert sweeps deliveries older
// than 72 hours, regardless of which workspace/event triggered the call.
func TestDeliveryStore_Postgres_MarkDeliveredSystem_PrunesStaleRows(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	if _, err := h.AdminDB.Exec(`
		INSERT INTO slack_event_deliveries (workspace_id, event_id, received_at)
		VALUES ('T0STALE01', 'EvStale', now() - interval '100 hours')
	`); err != nil {
		t.Fatalf("seed stale delivery: %v", err)
	}

	if _, err := slackstore.FromStores(stores).Deliveries.MarkDeliveredSystem(ctx, "T0FRESH01", "EvFresh"); err != nil {
		t.Fatalf("MarkDeliveredSystem: %v", err)
	}

	var n int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM slack_event_deliveries WHERE workspace_id = 'T0STALE01'`).Scan(&n); err != nil {
		t.Fatalf("count stale rows: %v", err)
	}
	if n != 0 {
		t.Errorf("stale delivery rows remaining = %d; want 0 (pruned by the piggybacked delete)", n)
	}

	var freshCount int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM slack_event_deliveries WHERE workspace_id = 'T0FRESH01'`).Scan(&freshCount); err != nil {
		t.Fatalf("count fresh rows: %v", err)
	}
	if freshCount != 1 {
		t.Errorf("fresh delivery rows = %d; want 1 (the prune must not remove the row this same call just inserted)", freshCount)
	}
}

// TestWorkspaceStore_Postgres_GetByWorkspaceIDSystem: the admin-pool lookup
// by workspace_id alone (no org_id to scope by) is how the pre-auth webhook
// receiver resolves which org a delivery belongs to.
func TestWorkspaceStore_Postgres_GetByWorkspaceIDSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-getbyid")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspace(orgID, "T0GETSYS1"))
	})

	got, err := slackstore.FromStores(stores).Workspaces.GetByWorkspaceIDSystem(ctx, "T0GETSYS1")
	if err != nil {
		t.Fatalf("GetByWorkspaceIDSystem: %v", err)
	}
	if got == nil || got.OrgID != orgID {
		t.Fatalf("GetByWorkspaceIDSystem = %+v; want a row bound to org %s", got, orgID)
	}

	missing, err := slackstore.FromStores(stores).Workspaces.GetByWorkspaceIDSystem(ctx, "T0NOPE0001")
	if err != nil {
		t.Fatalf("GetByWorkspaceIDSystem (unknown): %v", err)
	}
	if missing != nil {
		t.Errorf("GetByWorkspaceIDSystem for an unconnected workspace_id = %+v; want nil", missing)
	}
}
