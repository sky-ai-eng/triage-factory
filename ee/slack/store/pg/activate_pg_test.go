package pg_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// activeApps is the apps of orgID's active connections in workspaceID.
func activeApps(t *testing.T, stores db.Stores, ctx context.Context, orgID, userID, workspaceID string) []string {
	t.Helper()
	var out []string
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		conns, _, e := slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgID, db.Unwindowed)
		for _, c := range conns {
			if c.WorkspaceID == workspaceID && c.Active {
				out = append(out, c.APIAppID)
			}
		}
		return e
	})
	return out
}

// TestConnectionStore_Postgres_ActivateReturnsTheStoredRow: Activate sets the
// workspace's active connection inactive and the target active, returning the
// target as stored; a target the org does not have is ErrNoSuchConnection and
// changes nothing; and a workspace with no active connection gets the target
// as its one.
func TestConnectionStore_Postgres_ActivateReturnsTheStoredRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-activate")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	upsert(t, stores, orgID, userID, testWorkspaceApp(orgID, "T0ACT1", "A0OLD"))
	inactive := testWorkspaceApp(orgID, "T0ACT1", "A0NEW")
	inactive.Active = false
	upsert(t, stores, orgID, userID, inactive)

	activate := func(apiAppID string) (slackstore.Connection, error) {
		var got slackstore.Connection
		err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			var e error
			got, e = slackstore.FromTx(tx).Connections.Activate(ctx, orgID, "T0ACT1", apiAppID)
			return e
		})
		return got, err
	}

	if _, err := activate("A0NOPE"); !errors.Is(err, slackstore.ErrNoSuchConnection) {
		t.Fatalf("activate an unknown app: err = %v; want ErrNoSuchConnection", err)
	}
	if got := activeApps(t, stores, ctx, orgID, userID, "T0ACT1"); !slices.Equal(got, []string{"A0OLD"}) {
		t.Errorf("active after the refused activation = %v; want [A0OLD]", got)
	}

	got, err := activate("A0NEW")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	dbtest.AssertWriteReturnedStoredRow(t, "Activate", got, func() (*slackstore.Connection, error) {
		return get(t, stores, orgID, userID, "T0ACT1", "A0NEW"), nil
	})
	if !got.Active {
		t.Errorf("returned row = %+v; want active", got)
	}
	if got := activeApps(t, stores, ctx, orgID, userID, "T0ACT1"); !slices.Equal(got, []string{"A0NEW"}) {
		t.Errorf("active after the activation = %v; want [A0NEW]", got)
	}

	if _, err := h.AdminDB.Exec(`UPDATE org_slack_connections SET active = false WHERE org_id = $1`, orgID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := activate("A0OLD"); err != nil {
		t.Fatalf("activate into an empty workspace: %v", err)
	}
	if got := activeApps(t, stores, ctx, orgID, userID, "T0ACT1"); !slices.Equal(got, []string{"A0OLD"}) {
		t.Errorf("active after activating into an empty workspace = %v; want [A0OLD]", got)
	}
}

// TestConnectionStore_Postgres_LockWorkspace_Serializes: a second
// LockWorkspace on the workspace waits for the first transaction to commit,
// then reads the rows as that transaction left them; another workspace's rows
// are not held.
func TestConnectionStore_Postgres_LockWorkspace_Serializes(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "slack-lock-workspace")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	upsert(t, stores, orgID, userID, testWorkspaceApp(orgID, "T0LOCK1", "A0OLD"))
	inactive := testWorkspaceApp(orgID, "T0LOCK1", "A0NEW")
	inactive.Active = false
	upsert(t, stores, orgID, userID, inactive)
	upsert(t, stores, orgID, userID, testWorkspaceApp(orgID, "T0LOCK2", "A0ELSE"))

	const holdFor = 300 * time.Millisecond
	holding := make(chan struct{})
	var released time.Time
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			conns := slackstore.FromTx(tx).Connections
			rows, err := conns.LockWorkspace(ctx, orgID, "T0LOCK1")
			if err != nil {
				return err
			}
			if len(rows) != 2 || rows[0].APIAppID != "A0NEW" || rows[1].APIAppID != "A0OLD" {
				t.Errorf("locked rows = %+v; want A0NEW, A0OLD", rows)
			}
			close(holding)
			if _, err := conns.Activate(ctx, orgID, "T0LOCK1", "A0NEW"); err != nil {
				return err
			}
			time.Sleep(holdFor)
			released = time.Now()
			return nil
		}); err != nil {
			t.Errorf("first transaction: %v", err)
		}
	}()

	<-holding
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		start := time.Now()
		if _, err := slackstore.FromTx(tx).Connections.LockWorkspace(ctx, orgID, "T0LOCK2"); err != nil {
			return err
		}
		if waited := time.Since(start); waited >= holdFor {
			t.Errorf("locking another workspace waited %v; want no wait", waited)
		}
		return nil
	})
	var rows []slackstore.Connection
	var acquired time.Time
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var err error
		rows, err = slackstore.FromTx(tx).Connections.LockWorkspace(ctx, orgID, "T0LOCK1")
		acquired = time.Now()
		return err
	})
	wg.Wait()

	if acquired.Before(released) {
		t.Fatalf("second lock acquired at %v, before the first transaction finished at %v", acquired, released)
	}
	var active []string
	for _, c := range rows {
		if c.Active {
			active = append(active, c.APIAppID)
		}
	}
	if !slices.Equal(active, []string{"A0NEW"}) {
		t.Errorf("active among the rows read after the wait = %v; want [A0NEW], as the first transaction left them", active)
	}
}

// TestChannelRegistry_Postgres_ListWatchedInWorkspaceSystem: the read answers
// the workspace's channels some team watches, in name then channel id order,
// and not an unwatched channel, a channel in another workspace, or another
// org's.
func TestChannelRegistry_Postgres_ListWatchedInWorkspaceSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "slack-watched")
	otherOrg, otherUser, otherTeam := pgtest.SeedOrgWithUser(t, h, "slack-watched-other")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	channels := slackstore.FromStores(stores).Channels

	ensure := func(org, workspaceID, channelID, name string) {
		t.Helper()
		if _, err := channels.EnsureSystem(ctx, org, workspaceID, channelID, name); err != nil {
			t.Fatalf("ensure %s: %v", channelID, err)
		}
	}
	ensure(orgID, "T0W1", "C0ZED", "zed")
	ensure(orgID, "T0W1", "C0ALPHA", "alpha")
	ensure(orgID, "T0W1", "C0ALSO", "alpha")
	ensure(orgID, "T0W1", "C0UNWATCHED", "unwatched")
	ensure(orgID, "T0W2", "C0ELSEWHERE", "elsewhere")
	ensure(otherOrg, "T0W1", "C0OTHERORG", "other")
	replaceChannels(t, stores, ctx, orgID, userID, teamID, []string{"C0ZED", "C0ALSO", "C0ALPHA", "C0ELSEWHERE"})
	replaceChannels(t, stores, ctx, otherOrg, otherUser, otherTeam, []string{"C0OTHERORG"})

	got, err := channels.ListWatchedInWorkspaceSystem(ctx, orgID, "T0W1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ChannelID)
	}
	if want := []string{"C0ALPHA", "C0ALSO", "C0ZED"}; !slices.Equal(ids, want) {
		t.Errorf("watched = %v; want %v", ids, want)
	}

	if got, err := channels.ListWatchedInWorkspaceSystem(ctx, orgID, "T0NONE"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("a workspace with nothing watched = %#v (%v); want an empty list", got, err)
	}
}
