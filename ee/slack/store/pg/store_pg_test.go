package pg_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

func mustWithUser(t *testing.T, stores db.Stores, ctx context.Context, orgID, userID string, fn func(db.TxStores) error) {
	t.Helper()
	if err := stores.Tx.WithTx(ctx, orgID, userID, fn); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

// defaultAppID derives a deterministic api_app_id for callers that don't
// care which app id they get, only that Upsert/Get/etc agree on one — kept
// out of testWorkspace's signature so every single-workspace call site in
// this package (and delivery_pg_test.go) needs no change.
func defaultAppID(workspaceID string) string { return "A0" + workspaceID }

func testWorkspace(orgID, workspaceID string) slackstore.Workspace {
	return testWorkspaceApp(orgID, workspaceID, defaultAppID(workspaceID))
}

func testWorkspaceApp(orgID, workspaceID, apiAppID string) slackstore.Workspace {
	return slackstore.Workspace{
		WorkspaceID:      workspaceID,
		APIAppID:         apiAppID,
		OrgID:            orgID,
		WorkspaceName:    "Acme Corp",
		Transport:        "socket",
		BotUserID:        "U0BOT123",
		BotTokenRef:      "slack_ws_" + workspaceID + "_" + apiAppID + "_bot_token",
		AppTokenRef:      "slack_ws_" + workspaceID + "_" + apiAppID + "_app_token",
		SigningSecretRef: "",
	}
}

// TestWorkspaceStore_Postgres_UpsertCreatesAndReads: a fresh Upsert inserts
// the row; Get and ListForOrg both see it, with nullable columns round-
// tripping "" for the unset signing_secret_ref.
func TestWorkspaceStore_Postgres_UpsertCreatesAndReads(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-crud")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	ws := testWorkspace(orgID, "T0CRUD001")
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, ws)
	})

	var got *slackstore.Workspace
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = slackstore.FromTx(tx).Workspaces.Get(ctx, orgID, "T0CRUD001", defaultAppID("T0CRUD001"))
		return e
	})
	if got == nil {
		t.Fatal("Get returned nil for a just-upserted workspace")
	}
	if got.WorkspaceName != "Acme Corp" || got.Transport != "socket" || got.BotUserID != "U0BOT123" {
		t.Errorf("got = %+v; want name/transport/bot_user_id from the upsert", got)
	}
	if got.APIAppID != defaultAppID("T0CRUD001") {
		t.Errorf("api_app_id = %q; want %q", got.APIAppID, defaultAppID("T0CRUD001"))
	}
	if got.SigningSecretRef != "" {
		t.Errorf("signing_secret_ref = %q; want \"\" (never supplied -> NULL)", got.SigningSecretRef)
	}
	if got.AppTokenRef == "" {
		t.Error("app_token_ref empty; want the ref we upserted")
	}
	if got.RegisteredByUserID != "" {
		t.Errorf("registered_by_user_id = %q; want \"\" (never set in this fixture)", got.RegisteredByUserID)
	}

	var list []slackstore.Workspace
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		list, e = slackstore.FromTx(tx).Workspaces.ListForOrg(ctx, orgID)
		return e
	})
	if len(list) != 1 || list[0].WorkspaceID != "T0CRUD001" {
		t.Fatalf("ListForOrg = %d rows; want 1 (the upserted workspace)", len(list))
	}
}

// TestWorkspaceStore_Postgres_UpsertUpdatesInPlace: a second Upsert for the
// same (workspace_id, org) updates the existing row rather than creating a
// duplicate — the "re-submit the connect form" upsert contract.
func TestWorkspaceStore_Postgres_UpsertUpdatesInPlace(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-upsert")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	first := testWorkspace(orgID, "T0UPSERT1")
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, first)
	})

	second := first
	second.WorkspaceName = "Acme Corp (renamed)"
	second.Transport = "events_api"
	second.AppTokenRef = ""
	second.SigningSecretRef = "slack_ws_T0UPSERT1_" + defaultAppID("T0UPSERT1") + "_signing_secret"
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, second)
	})

	var list []slackstore.Workspace
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		list, e = slackstore.FromTx(tx).Workspaces.ListForOrg(ctx, orgID)
		return e
	})
	if len(list) != 1 {
		t.Fatalf("ListForOrg = %d rows; want 1 (upsert must not duplicate)", len(list))
	}
	if list[0].WorkspaceName != "Acme Corp (renamed)" || list[0].Transport != "events_api" {
		t.Errorf("row after re-upsert = %+v; want the updated name/transport", list[0])
	}
	if list[0].AppTokenRef != "" {
		t.Errorf("app_token_ref = %q; want cleared to \"\" by the second upsert", list[0].AppTokenRef)
	}
	if list[0].SigningSecretRef == "" {
		t.Error("signing_secret_ref empty; want the ref set by the second upsert")
	}
}

// TestWorkspaceStore_Postgres_Delete: Delete removes the row and only that org's row.
func TestWorkspaceStore_Postgres_Delete(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-delete")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspace(orgID, "T0DEL0001"))
	})
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Delete(ctx, orgID, "T0DEL0001", defaultAppID("T0DEL0001"))
	})

	var got *slackstore.Workspace
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = slackstore.FromTx(tx).Workspaces.Get(ctx, orgID, "T0DEL0001", defaultAppID("T0DEL0001"))
		return e
	})
	if got != nil {
		t.Errorf("Get after delete = %+v; want nil", got)
	}
}

// TestWorkspaceStore_Postgres_CrossOrgConflict: the (workspace_id,
// api_app_id) composite PRIMARY KEY binds a (workspace, app) pair to
// exactly one org, deployment-wide — a second org's Upsert for an
// already-bound (workspace_id, api_app_id) pair fails with a real
// constraint violation (not a silent steal, not a silent no-op). This is
// the SQL-level backstop; the app-single-org invariant itself (same
// api_app_id, any workspace_id) is enforced by the connect handler, not
// this constraint — see TestWorkspaceStore_Postgres_AppBoundToOtherOrgSystem
// and the handler acceptance tests.
func TestWorkspaceStore_Postgres_CrossOrgConflict(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-xorg-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "slack-xorg-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspace(orgA, "T0SHARED1"))
	})

	err := stores.Tx.WithTx(ctx, orgB, userB, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspace(orgB, "T0SHARED1"))
	})
	if err == nil {
		t.Fatal("org B upserted a (workspace_id, api_app_id) pair already bound to org A; want a constraint violation")
	}

	// Org A's row is untouched; org B still has none.
	var listA, listB []slackstore.Workspace
	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		var e error
		listA, e = slackstore.FromTx(tx).Workspaces.ListForOrg(ctx, orgA)
		return e
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		var e error
		listB, e = slackstore.FromTx(tx).Workspaces.ListForOrg(ctx, orgB)
		return e
	})
	if len(listA) != 1 || listA[0].OrgID != orgA {
		t.Errorf("org A rows = %+v; want exactly its own untouched row", listA)
	}
	if len(listB) != 0 {
		t.Errorf("org B rows = %+v; want 0 (its upsert failed)", listB)
	}
}

// TestWorkspaceStore_Postgres_ListAllSystem: the admin-pool read sees every
// org's workspace in one call — the future socket connection manager's
// boot-time enumeration.
func TestWorkspaceStore_Postgres_ListAllSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-sys-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "slack-sys-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspace(orgA, "T0SYSA001"))
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspace(orgB, "T0SYSB001"))
	})

	all, err := slackstore.FromStores(stores).Workspaces.ListAllSystem(ctx)
	if err != nil {
		t.Fatalf("ListAllSystem: %v", err)
	}
	seen := map[string]bool{}
	for _, w := range all {
		seen[w.WorkspaceID] = true
	}
	if !seen["T0SYSA001"] || !seen["T0SYSB001"] {
		t.Errorf("ListAllSystem = %v workspace ids; want both org A and org B's workspaces", keysOf(seen))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestWorkspaceStore_Postgres_TwoAppsSameWorkspaceDifferentOrgs: the schema
// itself imposes no workspace<->org cardinality limit — two different apps
// (distinct api_app_id) can both be bound to the same workspace_id under
// two different orgs (e.g. a prod TF org and an eval TF org each running
// their own bot in the same workspace). The app-single-org invariant that
// would forbid two ORGS sharing one APP lives one layer up, at the connect
// handler (LockApp + AppBoundToOtherOrgSystem) — not enforced here.
func TestWorkspaceStore_Postgres_TwoAppsSameWorkspaceDifferentOrgs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-2apps-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "slack-2apps-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspaceApp(orgA, "T0SHARED2", "A0FIRST001"))
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspaceApp(orgB, "T0SHARED2", "A0SECOND01"))
	})

	var listA, listB []slackstore.Workspace
	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		var e error
		listA, e = slackstore.FromTx(tx).Workspaces.ListForOrg(ctx, orgA)
		return e
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		var e error
		listB, e = slackstore.FromTx(tx).Workspaces.ListForOrg(ctx, orgB)
		return e
	})
	if len(listA) != 1 || listA[0].APIAppID != "A0FIRST001" || listA[0].WorkspaceID != "T0SHARED2" {
		t.Errorf("org A rows = %+v; want its own app row in the shared workspace", listA)
	}
	if len(listB) != 1 || listB[0].APIAppID != "A0SECOND01" || listB[0].WorkspaceID != "T0SHARED2" {
		t.Errorf("org B rows = %+v; want its own app row in the shared workspace", listB)
	}
}

// TestWorkspaceStore_Postgres_AppBoundToOtherOrgSystem: the app-single-org
// invariant's check primitive — true only when some row (ANY workspace_id)
// carries apiAppID under a DIFFERENT org; the owning org and unrelated app
// ids both report false.
func TestWorkspaceStore_Postgres_AppBoundToOtherOrgSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-appinv-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "slack-appinv-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspaceApp(orgA, "T0INVA0001", "A0SHARED01"))
	})

	bound, err := slackstore.FromStores(stores).Workspaces.AppBoundToOtherOrgSystem(ctx, "A0SHARED01", orgA)
	if err != nil {
		t.Fatalf("AppBoundToOtherOrgSystem (owning org): %v", err)
	}
	if bound {
		t.Error("AppBoundToOtherOrgSystem = true for the owning org; want false")
	}

	// A DIFFERENT workspace_id carrying the same api_app_id under org B
	// would also report bound — the invariant spans every workspace an app
	// is installed in, not just the one it was first seen on.
	bound, err = slackstore.FromStores(stores).Workspaces.AppBoundToOtherOrgSystem(ctx, "A0SHARED01", orgB)
	if err != nil {
		t.Fatalf("AppBoundToOtherOrgSystem (other org): %v", err)
	}
	if !bound {
		t.Error("AppBoundToOtherOrgSystem = false for a different org; want true (app-single-org invariant)")
	}

	bound, err = slackstore.FromStores(stores).Workspaces.AppBoundToOtherOrgSystem(ctx, "A0NEVERSEEN", orgB)
	if err != nil {
		t.Fatalf("AppBoundToOtherOrgSystem (unrelated app): %v", err)
	}
	if bound {
		t.Error("AppBoundToOtherOrgSystem = true for an unconnected app id; want false")
	}
}

// TestWorkspaceStore_Postgres_LockApp_SerializesSameAppID is the regression
// test for the app-single-org invariant's actual concurrency-safety
// mechanism: without a real lock, two concurrent connect transactions for
// the same api_app_id under different orgs could each run
// AppBoundToOtherOrgSystem before either commits its Upsert, both observe
// "not bound", and both succeed — since they write different workspace_id
// rows, nothing collides on a unique index to stop them. LockApp closes
// that gap with a transaction-scoped Postgres advisory lock keyed on
// api_app_id. This test proves the lock actually blocks: transaction A
// holds it across a deliberate delay; transaction B's LockApp call for the
// SAME api_app_id must not return until A commits (releasing the lock).
func TestWorkspaceStore_Postgres_LockApp_SerializesSameAppID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-lockapp-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "slack-lockapp-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const apiAppID = "A0LOCKTEST"
	const holdFor = 300 * time.Millisecond

	aHolding := make(chan struct{})
	var aReleasedAt, bAcquiredAt time.Time
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := stores.Tx.WithTx(ctx, orgA, userA, func(tx db.TxStores) error {
			if err := slackstore.FromTx(tx).Workspaces.LockApp(ctx, apiAppID); err != nil {
				return err
			}
			close(aHolding)
			time.Sleep(holdFor)
			aReleasedAt = time.Now()
			return nil
		}); err != nil {
			t.Errorf("transaction A: %v", err)
		}
	}()

	<-aHolding
	go func() {
		defer wg.Done()
		if err := stores.Tx.WithTx(ctx, orgB, userB, func(tx db.TxStores) error {
			if err := slackstore.FromTx(tx).Workspaces.LockApp(ctx, apiAppID); err != nil {
				return err
			}
			bAcquiredAt = time.Now()
			return nil
		}); err != nil {
			t.Errorf("transaction B: %v", err)
		}
	}()

	wg.Wait()
	if bAcquiredAt.Before(aReleasedAt) {
		t.Fatalf("B acquired LockApp at %v, before A released it at %v (held until commit) — LockApp did not serialize", bAcquiredAt, aReleasedAt)
	}
}

// TestWorkspaceStore_Postgres_LockApp_DoesNotSerializeDifferentAppIDs proves
// LockApp's key is actually api_app_id-scoped, not a single global lock: two
// concurrent transactions locking DIFFERENT api_app_id values must not block
// each other.
func TestWorkspaceStore_Postgres_LockApp_DoesNotSerializeDifferentAppIDs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-lockapp-diff-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "slack-lockapp-diff-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const holdFor = 300 * time.Millisecond

	aHolding := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		if err := stores.Tx.WithTx(ctx, orgA, userA, func(tx db.TxStores) error {
			if err := slackstore.FromTx(tx).Workspaces.LockApp(ctx, "A0DIFFERENTA"); err != nil {
				return err
			}
			close(aHolding)
			time.Sleep(holdFor)
			return nil
		}); err != nil {
			t.Errorf("transaction A: %v", err)
		}
	}()

	<-aHolding
	bStart := time.Now()
	if err := stores.Tx.WithTx(ctx, orgB, userB, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.LockApp(ctx, "A0DIFFERENTB")
	}); err != nil {
		t.Fatalf("transaction B: %v", err)
	}
	if elapsed := time.Since(bStart); elapsed >= holdFor {
		t.Errorf("B's LockApp for a different api_app_id took %v; want well under %v (must not block on A's key)", elapsed, holdFor)
	}
	wg.Wait()
}

// TestWorkspaceStore_Postgres_RLS_MemberReadsAdminWrites: any org member can
// SELECT (org_slack_workspaces_select mirrors org_jira_apps's member-visible
// read), but only an org admin can INSERT/UPDATE/DELETE.
func TestWorkspaceStore_Postgres_RLS_MemberReadsAdminWrites(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, ownerID, teamID := pgtest.SeedOrgWithUser(t, h, "slack-rls")
	memberID := pgtest.SeedUser(t, h, "slack-rls-member")
	pgtest.AddOrgMember(t, h, memberID, orgID, teamID, "member", "member")

	if _, err := h.AdminDB.Exec(`
		INSERT INTO org_slack_workspaces (workspace_id, api_app_id, org_id, transport, bot_token_ref)
		VALUES ('T0RLS0001', 'A0RLS0001', $1, 'socket', 'slack_ws_T0RLS0001_A0RLS0001_bot_token')
	`, orgID); err != nil {
		t.Fatalf("seed org_slack_workspaces: %v", err)
	}

	// Member: SELECT sees the row.
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM org_slack_workspaces`).Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			t.Errorf("member sees %d rows; want 1 (SELECT is member-visible)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("member select tx: %v", err)
	}

	// Member: INSERT/UPDATE/DELETE are all rejected/no-op.
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		_, e := tx.Exec(`
			INSERT INTO org_slack_workspaces (workspace_id, api_app_id, org_id, transport, bot_token_ref)
			VALUES ('T0RLS0002', 'A0RLS0002', $1, 'socket', 'x')
		`, orgID)
		return e
	}); err == nil {
		t.Error("member INSERT into org_slack_workspaces succeeded; want RLS rejection")
	}
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE org_slack_workspaces SET workspace_name = 'hijacked' WHERE workspace_id = 'T0RLS0001'`)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("member UPDATE affected %d rows; want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("member update tx: %v", err)
	}
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		res, e := tx.Exec(`DELETE FROM org_slack_workspaces WHERE workspace_id = 'T0RLS0001'`)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("member DELETE affected %d rows; want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("member delete tx: %v", err)
	}

	// Org admin (owner): UPDATE and DELETE both take effect.
	if err := h.WithUser(t, ownerID, orgID, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE org_slack_workspaces SET workspace_name = 'renamed' WHERE workspace_id = 'T0RLS0001'`)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("admin UPDATE affected %d rows; want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("admin update tx: %v", err)
	}
}
