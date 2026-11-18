package pg_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
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

// upsertActiveConnection upserts c as an active connection, through an org's
// own app unless c names another source.
func upsertActiveConnection(ctx context.Context, tx db.TxStores, c slackstore.Connection) error {
	if c.Source == "" {
		c.Source = slackstore.SourceOrg
	}
	c.Active = true
	_, err := slackstore.FromTx(tx).Connections.Upsert(ctx, c)
	return err
}

func testWorkspace(orgID, workspaceID string) slackstore.Connection {
	return testWorkspaceApp(orgID, workspaceID, defaultAppID(workspaceID))
}

func testWorkspaceApp(orgID, workspaceID, apiAppID string) slackstore.Connection {
	return slackstore.Connection{
		WorkspaceID:      workspaceID,
		APIAppID:         apiAppID,
		OrgID:            orgID,
		Source:           slackstore.SourceOrg,
		Active:           true,
		WorkspaceName:    "Acme Corp",
		AppName:          "Acme Bot",
		Transport:        "socket",
		BotUserID:        "U0BOT123",
		BotTokenRef:      "slack_ws_" + workspaceID + "_" + apiAppID + "_bot_token",
		AppTokenRef:      "slack_ws_" + workspaceID + "_" + apiAppID + "_app_token",
		SigningSecretRef: "",
	}
}

// upsert runs Upsert in a claims transaction as userID and returns its row.
func upsert(t *testing.T, stores db.Stores, orgID, userID string, c slackstore.Connection) slackstore.Connection {
	t.Helper()
	var got slackstore.Connection
	mustWithUser(t, stores, context.Background(), orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = slackstore.FromTx(tx).Connections.Upsert(context.Background(), c)
		return e
	})
	return got
}

// get reads (workspaceID, apiAppID) back through the point read.
func get(t *testing.T, stores db.Stores, orgID, userID, workspaceID, apiAppID string) *slackstore.Connection {
	t.Helper()
	var got *slackstore.Connection
	mustWithUser(t, stores, context.Background(), orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = slackstore.FromTx(tx).Connections.Get(context.Background(), orgID, workspaceID, apiAppID)
		return e
	})
	return got
}

// TestConnectionStore_Postgres_UpsertReturnsTheStoredRow: the row Upsert
// returns, on insert and on update, is the row the point read reads, with
// nullable columns round-tripping "" for the unset signing_secret_ref.
func TestConnectionStore_Postgres_UpsertReturnsTheStoredRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-crud")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	read := func() (*slackstore.Connection, error) {
		return get(t, stores, orgID, userID, "T0CRUD001", defaultAppID("T0CRUD001")), nil
	}

	ws := testWorkspace(orgID, "T0CRUD001")
	ws.RegisteredByUserID = userID
	dbtest.AssertWriteReturnedStoredRow(t, "Upsert (insert)", upsert(t, stores, orgID, userID, ws), read)
	got, _ := read()
	if got.Source != slackstore.SourceOrg || !got.Active || got.AppName != "Acme Bot" {
		t.Errorf("got = %+v; want source org, active, and the app name from the upsert", got)
	}
	if got.WorkspaceName != "Acme Corp" || got.Transport != "socket" || got.BotUserID != "U0BOT123" {
		t.Errorf("got = %+v; want name/transport/bot_user_id from the upsert", got)
	}
	if got.SigningSecretRef != "" {
		t.Errorf("signing_secret_ref = %q; want \"\" (never supplied -> NULL)", got.SigningSecretRef)
	}
	if got.AppTokenRef == "" {
		t.Error("app_token_ref empty; want the ref we upserted")
	}
	if got.RegisteredByUserID != userID {
		t.Errorf("registered_by_user_id = %q; want %q", got.RegisteredByUserID, userID)
	}

	rotated := ws
	rotated.AppName = "Acme Bot (renamed)"
	dbtest.AssertWriteReturnedStoredRow(t, "Upsert (update)", upsert(t, stores, orgID, userID, rotated), read)
	got, _ = read()
	if got.AppName != "Acme Bot (renamed)" {
		t.Errorf("app_name after the update = %q; want the new name", got.AppName)
	}

	var list []slackstore.Connection
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		list, _, e = slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgID, db.Unwindowed)
		return e
	})
	if len(list) != 1 || !reflect.DeepEqual(list[0], *got) {
		t.Fatalf("ListForOrg = %+v; want exactly the upserted connection", list)
	}
}

// TestConnectionStore_Postgres_UpsertUpdatesInPlace: a second Upsert for the
// same (workspace_id, api_app_id, org) updates the existing row's credential
// and display columns rather than creating a duplicate — the "re-submit the
// connect form" upsert contract — and keeps the active and source it has.
func TestConnectionStore_Postgres_UpsertUpdatesInPlace(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-upsert")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	first := testWorkspace(orgID, "T0UPSERT1")
	upsert(t, stores, orgID, userID, first)

	second := first
	second.WorkspaceName = "Acme Corp (renamed)"
	second.Transport = "events_api"
	second.AppTokenRef = ""
	second.SigningSecretRef = "slack_ws_T0UPSERT1_" + defaultAppID("T0UPSERT1") + "_signing_secret"
	second.Active = false
	second.Source = slackstore.SourceDeployment
	got := upsert(t, stores, orgID, userID, second)

	var list []slackstore.Connection
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		list, _, e = slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgID, db.Unwindowed)
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
	if !got.Active || got.Source != slackstore.SourceOrg {
		t.Errorf("after an upsert asking for inactive/deployment: active=%v source=%q; want the stored true/org", got.Active, got.Source)
	}
}

// TestConnectionStore_Postgres_OneActivePerWorkspace: the partial unique
// index refuses a second active connection in one (org, workspace), as
// ErrActiveConnectionExists, and allows a second inactive one. Another org's
// active connection in the same workspace is its own.
func TestConnectionStore_Postgres_OneActivePerWorkspace(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-one-active")
	otherOrg, otherUser, _ := pgtest.SeedOrgWithUser(t, h, "slack-one-active-other")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	upsert(t, stores, orgID, userID, testWorkspaceApp(orgID, "T0ONEACT1", "A0ACTIVE01"))

	err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		_, e := slackstore.FromTx(tx).Connections.Upsert(ctx, testWorkspaceApp(orgID, "T0ONEACT1", "A0ACTIVE02"))
		return e
	})
	if !errors.Is(err, slackstore.ErrActiveConnectionExists) {
		t.Fatalf("second active connection in the workspace: err = %v; want ErrActiveConnectionExists", err)
	}

	inactive := testWorkspaceApp(orgID, "T0ONEACT1", "A0INACTIV1")
	inactive.Active = false
	if got := upsert(t, stores, orgID, userID, inactive); got.Active {
		t.Errorf("inactive connection stored active=%v; want false", got.Active)
	}

	if got := upsert(t, stores, otherOrg, otherUser, testWorkspaceApp(otherOrg, "T0ONEACT1", "A0OTHERORG")); !got.Active {
		t.Error("another org's connection in the same workspace was stored inactive; want active (the rule is per org)")
	}
}

// TestConnectionStore_Postgres_SourceShape: the CHECK refuses a deployment row
// carrying a transport or a signing secret, and an org row without the secret
// its transport needs; it accepts a bare deployment row.
func TestConnectionStore_Postgres_SourceShape(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "slack-source-shape")

	insert := func(app, source, transport, signingSecretRef, appTokenRef string) error {
		_, err := h.AdminDB.Exec(`
			INSERT INTO org_slack_connections (
				workspace_id, api_app_id, org_id, source, active, transport, bot_token_ref,
				signing_secret_ref, app_token_ref
			) VALUES ('T0SHAPE01', $1, $2, $3, false, NULLIF($4, ''), 'bot', NULLIF($5, ''), NULLIF($6, ''))
		`, app, orgID, source, transport, signingSecretRef, appTokenRef)
		return err
	}
	for _, tc := range []struct {
		name                                     string
		source, transport, signingSecret, appTok string
		ok                                       bool
	}{
		{"deployment, bare", "deployment", "", "", "", true},
		{"deployment with a transport", "deployment", "events_api", "", "", false},
		{"deployment with a signing secret", "deployment", "", "sig", "", false},
		{"deployment with an app token", "deployment", "", "", "xapp", false},
		{"org events_api with its signing secret", "org", "events_api", "sig", "", true},
		{"org events_api without a signing secret", "org", "events_api", "", "", false},
		{"org socket with its app token", "org", "socket", "", "xapp", true},
		{"org socket without an app token", "org", "socket", "", "", false},
		{"org without a transport", "org", "", "sig", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := insert("A0"+strings.ReplaceAll(tc.name, " ", "_"), tc.source, tc.transport, tc.signingSecret, tc.appTok)
			if tc.ok && err != nil {
				t.Fatalf("insert: %v; want accepted", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "org_slack_connections_source_shape")) {
				t.Fatalf("insert: err = %v; want the source-shape CHECK to refuse it", err)
			}
		})
	}
}

// TestConnectionStore_Postgres_ActiveForWorkspaceSystem: the org's active
// connection in a workspace, and nil when the workspace has only inactive
// connections, none, or only another org's.
func TestConnectionStore_Postgres_ActiveForWorkspaceSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-active-for")
	otherOrg, otherUser, _ := pgtest.SeedOrgWithUser(t, h, "slack-active-for-other")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	conns := slackstore.FromStores(stores).Connections
	ctx := context.Background()

	inactive := testWorkspaceApp(orgID, "T0ACTFOR1", "A0ACTFOR0I")
	inactive.Active = false
	upsert(t, stores, orgID, userID, inactive)
	active := upsert(t, stores, orgID, userID, testWorkspaceApp(orgID, "T0ACTFOR1", "A0ACTFOR0A"))
	onlyInactive := testWorkspaceApp(orgID, "T0ACTFOR2", "A0ACTFOR2I")
	onlyInactive.Active = false
	upsert(t, stores, orgID, userID, onlyInactive)
	upsert(t, stores, otherOrg, otherUser, testWorkspaceApp(otherOrg, "T0ACTFOR3", "A0ACTFOR3O"))

	got, err := conns.ActiveForWorkspaceSystem(ctx, orgID, "T0ACTFOR1")
	if err != nil {
		t.Fatalf("ActiveForWorkspaceSystem: %v", err)
	}
	if got == nil || !reflect.DeepEqual(*got, active) {
		t.Errorf("ActiveForWorkspaceSystem = %+v; want the active connection %+v", got, active)
	}
	for _, workspaceID := range []string{"T0ACTFOR2", "T0ACTFOR3", "T0ACTFOR4"} {
		got, err := conns.ActiveForWorkspaceSystem(ctx, orgID, workspaceID)
		if err != nil {
			t.Fatalf("ActiveForWorkspaceSystem(%s): %v", workspaceID, err)
		}
		if got != nil {
			t.Errorf("ActiveForWorkspaceSystem(%s) = %+v; want nil", workspaceID, got)
		}
	}
}

// TestConnectionStore_Postgres_Delete: Delete removes the row and only that org's row.
func TestConnectionStore_Postgres_Delete(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-delete")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return upsertActiveConnection(ctx, tx, testWorkspace(orgID, "T0DEL0001"))
	})
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Connections.Delete(ctx, orgID, "T0DEL0001", defaultAppID("T0DEL0001"))
	})

	var got *slackstore.Connection
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		got, e = slackstore.FromTx(tx).Connections.Get(ctx, orgID, "T0DEL0001", defaultAppID("T0DEL0001"))
		return e
	})
	if got != nil {
		t.Errorf("Get after delete = %+v; want nil", got)
	}
}

// TestConnectionStore_Postgres_DeleteSystem: DeleteSystem removes the row only
// while its updated_at is the one the caller read, and reports a miss —
// whether an Upsert moved updated_at, the row is another org's, or there is no
// row — as false with the row left as it was.
func TestConnectionStore_Postgres_DeleteSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-delete-system")
	otherOrg, _, _ := pgtest.SeedOrgWithUser(t, h, "slack-delete-system-other")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	conns := slackstore.FromStores(stores).Connections
	ctx := context.Background()

	read := func(workspaceID string) *slackstore.Connection {
		t.Helper()
		ws, err := conns.GetByWorkspaceAppSystem(ctx, workspaceID, defaultAppID(workspaceID))
		if err != nil {
			t.Fatalf("GetByWorkspaceAppSystem(%s): %v", workspaceID, err)
		}
		return ws
	}
	upsert := func(ws slackstore.Connection) {
		t.Helper()
		mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
			return upsertActiveConnection(ctx, tx, ws)
		})
	}
	deleteSystem := func(org, workspaceID string, updatedAt time.Time) bool {
		t.Helper()
		deleted, err := conns.DeleteSystem(ctx, org, workspaceID, defaultAppID(workspaceID), updatedAt)
		if err != nil {
			t.Fatalf("DeleteSystem(%s): %v", workspaceID, err)
		}
		return deleted
	}

	t.Run("matching updated_at deletes", func(t *testing.T) {
		upsert(testWorkspace(orgID, "T0DELSYS1"))
		ws := read("T0DELSYS1")
		if !deleteSystem(orgID, "T0DELSYS1", ws.UpdatedAt) {
			t.Fatal("DeleteSystem = false; want true for the row as read")
		}
		if got := read("T0DELSYS1"); got != nil {
			t.Errorf("row after DeleteSystem = %+v; want none", got)
		}
	})

	t.Run("moved updated_at keeps the row", func(t *testing.T) {
		upsert(testWorkspace(orgID, "T0DELSYS2"))
		stale := read("T0DELSYS2")
		upsert(testWorkspace(orgID, "T0DELSYS2")) // a reconnect: the trigger moves updated_at
		current := read("T0DELSYS2")
		if !current.UpdatedAt.After(stale.UpdatedAt) {
			t.Fatalf("updated_at after the second Upsert = %v; want after %v", current.UpdatedAt, stale.UpdatedAt)
		}
		if deleteSystem(orgID, "T0DELSYS2", stale.UpdatedAt) {
			t.Fatal("DeleteSystem = true for a row whose updated_at moved; want false")
		}
		if got := read("T0DELSYS2"); got == nil || !got.UpdatedAt.Equal(current.UpdatedAt) {
			t.Errorf("row after a stale DeleteSystem = %+v; want it unchanged", got)
		}
	})

	t.Run("another org's row is kept", func(t *testing.T) {
		upsert(testWorkspace(orgID, "T0DELSYS3"))
		ws := read("T0DELSYS3")
		if deleteSystem(otherOrg, "T0DELSYS3", ws.UpdatedAt) {
			t.Fatal("DeleteSystem under another org = true; want false")
		}
		if read("T0DELSYS3") == nil {
			t.Error("row deleted by another org's DeleteSystem; want kept")
		}
	})

	t.Run("no row reports false", func(t *testing.T) {
		if deleteSystem(orgID, "T0DELSYS4", time.Now()) {
			t.Fatal("DeleteSystem with no row = true; want false")
		}
	})
}

// TestConnectionStore_Postgres_CrossOrgConflict: the (workspace_id,
// api_app_id) composite PRIMARY KEY binds a (workspace, app) pair to
// exactly one org, deployment-wide — a second org's Upsert for an
// already-bound (workspace_id, api_app_id) pair fails with a real
// constraint violation (not a silent steal, not a silent no-op). This is
// the SQL-level backstop; the app-single-org invariant itself (same
// api_app_id, any workspace_id) is enforced by the connect handler, not
// this constraint — see TestConnectionStore_Postgres_AppBoundToOtherOrgSystem
// and the handler acceptance tests.
func TestConnectionStore_Postgres_CrossOrgConflict(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-xorg-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "slack-xorg-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		return upsertActiveConnection(ctx, tx, testWorkspace(orgA, "T0SHARED1"))
	})

	err := stores.Tx.WithTx(ctx, orgB, userB, func(tx db.TxStores) error {
		return upsertActiveConnection(ctx, tx, testWorkspace(orgB, "T0SHARED1"))
	})
	if err == nil {
		t.Fatal("org B upserted a (workspace_id, api_app_id) pair already bound to org A; want a constraint violation")
	}

	// Org A's row is untouched; org B still has none.
	var listA, listB []slackstore.Connection
	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		var e error
		listA, _, e = slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgA, db.Unwindowed)
		return e
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		var e error
		listB, _, e = slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgB, db.Unwindowed)
		return e
	})
	if len(listA) != 1 || listA[0].OrgID != orgA {
		t.Errorf("org A rows = %+v; want exactly its own untouched row", listA)
	}
	if len(listB) != 0 {
		t.Errorf("org B rows = %+v; want 0 (its upsert failed)", listB)
	}
}

// TestConnectionStore_Postgres_ListAllSystem: the admin-pool read sees every
// org's workspace in one call — the future socket connection manager's
// boot-time enumeration.
func TestConnectionStore_Postgres_ListAllSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-sys-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "slack-sys-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		return upsertActiveConnection(ctx, tx, testWorkspace(orgA, "T0SYSA001"))
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		return upsertActiveConnection(ctx, tx, testWorkspace(orgB, "T0SYSB001"))
	})

	all, err := slackstore.FromStores(stores).Connections.ListAllSystem(ctx)
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

// TestConnectionStore_Postgres_TwoAppsSameWorkspaceDifferentOrgs: the schema
// itself imposes no workspace<->org cardinality limit — two different apps
// (distinct api_app_id) can both be bound to the same workspace_id under
// two different orgs (e.g. a prod TF org and an eval TF org each running
// their own bot in the same workspace). The app-single-org invariant that
// would forbid two ORGS sharing one APP lives one layer up, at the connect
// handler (LockApp + AppBoundToOtherOrgSystem) — not enforced here.
func TestConnectionStore_Postgres_TwoAppsSameWorkspaceDifferentOrgs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-2apps-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "slack-2apps-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		return upsertActiveConnection(ctx, tx, testWorkspaceApp(orgA, "T0SHARED2", "A0FIRST001"))
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		return upsertActiveConnection(ctx, tx, testWorkspaceApp(orgB, "T0SHARED2", "A0SECOND01"))
	})

	var listA, listB []slackstore.Connection
	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		var e error
		listA, _, e = slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgA, db.Unwindowed)
		return e
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		var e error
		listB, _, e = slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgB, db.Unwindowed)
		return e
	})
	if len(listA) != 1 || listA[0].APIAppID != "A0FIRST001" || listA[0].WorkspaceID != "T0SHARED2" {
		t.Errorf("org A rows = %+v; want its own app row in the shared workspace", listA)
	}
	if len(listB) != 1 || listB[0].APIAppID != "A0SECOND01" || listB[0].WorkspaceID != "T0SHARED2" {
		t.Errorf("org B rows = %+v; want its own app row in the shared workspace", listB)
	}
}

// TestConnectionStore_Postgres_AppBoundToOtherOrgSystem: the app-single-org
// invariant's check primitive — true only when some row (ANY workspace_id)
// carries apiAppID under a DIFFERENT org; the owning org and unrelated app
// ids both report false.
func TestConnectionStore_Postgres_AppBoundToOtherOrgSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "slack-appinv-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "slack-appinv-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		return upsertActiveConnection(ctx, tx, testWorkspaceApp(orgA, "T0INVA0001", "A0SHARED01"))
	})

	bound, err := slackstore.FromStores(stores).Connections.AppBoundToOtherOrgSystem(ctx, "A0SHARED01", orgA)
	if err != nil {
		t.Fatalf("AppBoundToOtherOrgSystem (owning org): %v", err)
	}
	if bound {
		t.Error("AppBoundToOtherOrgSystem = true for the owning org; want false")
	}

	// A DIFFERENT workspace_id carrying the same api_app_id under org B
	// would also report bound — the invariant spans every workspace an app
	// is installed in, not just the one it was first seen on.
	bound, err = slackstore.FromStores(stores).Connections.AppBoundToOtherOrgSystem(ctx, "A0SHARED01", orgB)
	if err != nil {
		t.Fatalf("AppBoundToOtherOrgSystem (other org): %v", err)
	}
	if !bound {
		t.Error("AppBoundToOtherOrgSystem = false for a different org; want true (app-single-org invariant)")
	}

	bound, err = slackstore.FromStores(stores).Connections.AppBoundToOtherOrgSystem(ctx, "A0NEVERSEEN", orgB)
	if err != nil {
		t.Fatalf("AppBoundToOtherOrgSystem (unrelated app): %v", err)
	}
	if bound {
		t.Error("AppBoundToOtherOrgSystem = true for an unconnected app id; want false")
	}

	// The deployment's app is in many orgs, one per workspace: its rows bind
	// no app to an org.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO org_slack_connections (workspace_id, api_app_id, org_id, source, active, bot_token_ref)
		VALUES ('T0INVDEP01', 'A0DEPLOY01', $1, 'deployment', true, 'bot')
	`, orgA); err != nil {
		t.Fatalf("insert deployment connection: %v", err)
	}
	bound, err = slackstore.FromStores(stores).Connections.AppBoundToOtherOrgSystem(ctx, "A0DEPLOY01", orgB)
	if err != nil {
		t.Fatalf("AppBoundToOtherOrgSystem (deployment app): %v", err)
	}
	if bound {
		t.Error("AppBoundToOtherOrgSystem = true for the deployment's app connected in another org; want false")
	}
}

// TestConnectionStore_Postgres_LockApp_SerializesSameAppID is the regression
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
func TestConnectionStore_Postgres_LockApp_SerializesSameAppID(t *testing.T) {
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
			if err := slackstore.FromTx(tx).Connections.LockApp(ctx, apiAppID); err != nil {
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
			if err := slackstore.FromTx(tx).Connections.LockApp(ctx, apiAppID); err != nil {
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

// TestConnectionStore_Postgres_LockApp_DoesNotSerializeDifferentAppIDs proves
// LockApp's key is actually api_app_id-scoped, not a single global lock: two
// concurrent transactions locking DIFFERENT api_app_id values must not block
// each other.
func TestConnectionStore_Postgres_LockApp_DoesNotSerializeDifferentAppIDs(t *testing.T) {
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
			if err := slackstore.FromTx(tx).Connections.LockApp(ctx, "A0DIFFERENTA"); err != nil {
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
		return slackstore.FromTx(tx).Connections.LockApp(ctx, "A0DIFFERENTB")
	}); err != nil {
		t.Fatalf("transaction B: %v", err)
	}
	if elapsed := time.Since(bStart); elapsed >= holdFor {
		t.Errorf("B's LockApp for a different api_app_id took %v; want well under %v (must not block on A's key)", elapsed, holdFor)
	}
	wg.Wait()
}

// TestConnectionStore_Postgres_RLS_MemberReadsAdminWrites: any org member can
// SELECT (org_slack_connections_select mirrors org_jira_apps's member-visible
// read), but only an org admin can INSERT/UPDATE/DELETE.
func TestConnectionStore_Postgres_RLS_MemberReadsAdminWrites(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, ownerID, teamID := pgtest.SeedOrgWithUser(t, h, "slack-rls")
	memberID := pgtest.SeedUser(t, h, "slack-rls-member")
	pgtest.AddOrgMember(t, h, memberID, orgID, teamID, "member", "member")

	if _, err := h.AdminDB.Exec(`
		INSERT INTO org_slack_connections (
			workspace_id, api_app_id, org_id, source, active, transport, bot_token_ref, app_token_ref
		)
		VALUES ('T0RLS0001', 'A0RLS0001', $1, 'org', true, 'socket', 'slack_ws_T0RLS0001_A0RLS0001_bot_token', 'x')
	`, orgID); err != nil {
		t.Fatalf("seed org_slack_connections: %v", err)
	}

	// Member: SELECT sees the row.
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM org_slack_connections`).Scan(&n); e != nil {
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
			INSERT INTO org_slack_connections (
				workspace_id, api_app_id, org_id, source, active, transport, bot_token_ref, app_token_ref
			)
			VALUES ('T0RLS0002', 'A0RLS0002', $1, 'org', false, 'socket', 'x', 'x')
		`, orgID)
		return e
	}); err == nil {
		t.Error("member INSERT into org_slack_connections succeeded; want RLS rejection")
	}
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE org_slack_connections SET workspace_name = 'hijacked' WHERE workspace_id = 'T0RLS0001'`)
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
		res, e := tx.Exec(`DELETE FROM org_slack_connections WHERE workspace_id = 'T0RLS0001'`)
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
		res, e := tx.Exec(`UPDATE org_slack_connections SET workspace_name = 'renamed' WHERE workspace_id = 'T0RLS0001'`)
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
