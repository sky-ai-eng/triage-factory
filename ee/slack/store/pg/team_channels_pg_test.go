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

// channelRow returns the registry row of the Slack channel channelID,
// registering it — the reference every tracking row holds.
func channelRow(t *testing.T, stores db.Stores, ctx context.Context, orgID, channelID string) string {
	t.Helper()
	ch, err := slackstore.FromStores(stores).Channels.EnsureSystem(ctx, orgID, "", channelID, "")
	if err != nil {
		t.Fatalf("ensure slack channel %s: %v", channelID, err)
	}
	return ch.ID
}

func channelRows(t *testing.T, stores db.Stores, ctx context.Context, orgID string, channelIDs []string) []string {
	t.Helper()
	out := make([]string, 0, len(channelIDs))
	for _, id := range channelIDs {
		out = append(out, channelRow(t, stores, ctx, orgID, id))
	}
	return out
}

// replaceChannels registers the Slack channels channelIDs and tracks them for
// teamID.
func replaceChannels(t *testing.T, stores db.Stores, ctx context.Context, orgID, userID, teamID string, channelIDs []string) {
	t.Helper()
	channelRows(t, stores, ctx, orgID, channelIDs)
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		_, err := slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(ctx, orgID, teamID, channelIDs)
		return err
	})
}

func listForTeamChannels(t *testing.T, stores db.Stores, ctx context.Context, orgID, userID, teamID string) []slackstore.TeamChannel {
	t.Helper()
	var out []slackstore.TeamChannel
	mustWithUser(t, stores, ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		out, e = slackstore.FromTx(tx).TeamChannels.ListForTeam(ctx, orgID, teamID)
		return e
	})
	return out
}

// TestTeamChannel_Postgres_ReplaceForTeam_PreservesCreatedAtAndPrimary:
// insert-then-prune keeps a kept row's created_at AND is_primary untouched
// across a save; a channel newly added to the tracked set always inserts
// is_primary=false, even once a sibling channel already has a promoted
// primary — ReplaceForTeam never writes is_primary=true itself.
func TestTeamChannel_Postgres_ReplaceForTeam_PreservesCreatedAtAndPrimary(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "tc-replace")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	replaceChannels(t, stores, ctx, orgID, owner, teamID, []string{"C0KEEP001", "C0DROP001"})
	if err := slackstore.FromStores(stores).TeamChannels.ReconcilePrimariesSystem(ctx, orgID, channelRows(t, stores, ctx, orgID, []string{"C0KEEP001", "C0DROP001"})); err != nil {
		t.Fatalf("ReconcilePrimariesSystem: %v", err)
	}

	before := listForTeamChannels(t, stores, ctx, orgID, owner, teamID)
	var keepBefore slackstore.TeamChannel
	for _, tc := range before {
		if tc.ChannelID == "C0KEEP001" {
			keepBefore = tc
		}
	}
	if !keepBefore.IsPrimary {
		t.Fatal("C0KEEP001 not primary after Reconcile; want true (sole tracker promoted)")
	}

	// Drop C0DROP001, add a new C0NEW0001, keep C0KEEP001.
	replaceChannels(t, stores, ctx, orgID, owner, teamID, []string{"C0KEEP001", "C0NEW0001"})

	after := listForTeamChannels(t, stores, ctx, orgID, owner, teamID)
	byChannel := map[string]slackstore.TeamChannel{}
	for _, tc := range after {
		byChannel[tc.ChannelID] = tc
	}
	if _, ok := byChannel["C0DROP001"]; ok {
		t.Error("C0DROP001 still present after prune; want removed")
	}
	kept, ok := byChannel["C0KEEP001"]
	if !ok {
		t.Fatal("C0KEEP001 missing after replace; want kept")
	}
	if !kept.CreatedAt.Equal(keepBefore.CreatedAt) {
		t.Errorf("C0KEEP001 created_at = %v; want unchanged %v", kept.CreatedAt, keepBefore.CreatedAt)
	}
	if !kept.IsPrimary {
		t.Error("C0KEEP001 is_primary = false after replace; want preserved true")
	}
	added, ok := byChannel["C0NEW0001"]
	if !ok {
		t.Fatal("C0NEW0001 missing after replace; want inserted")
	}
	if added.IsPrimary {
		t.Error("C0NEW0001 is_primary = true on insert; want false — ReplaceForTeam never writes true")
	}
}

// TestTeamChannel_Postgres_ReplaceForTeam_ResolvesTheIDsItIsGiven: the
// tracking write resolves Slack ids itself, on the app pool. A row registered
// for a save and then absorbed by a move before the write (the channels PUT's
// race) tracks the surviving row; an id Slack has since replaced tracks the
// channel's row; and an id with no row is ErrChannelNotRegistered, leaving the
// set as it was.
func TestTeamChannel_Postgres_ReplaceForTeam_ResolvesTheIDsItIsGiven(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "tc-resolve")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	survivor := channelRow(t, stores, ctx, orgID, "G0RESOLV1")
	absorbed := channelRow(t, stores, ctx, orgID, "C0RESOLV1")
	if _, err := channels.MoveSystem(ctx, orgID, "G0RESOLV1", "C0RESOLV1"); err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}
	if row, _ := channels.GetSystem(ctx, orgID, absorbed); row != nil {
		t.Fatalf("row %s survived the move; want it absorbed", absorbed)
	}

	for _, named := range []string{"C0RESOLV1", "G0RESOLV1"} {
		var tracked []string
		mustWithUser(t, stores, ctx, orgID, owner, func(tx db.TxStores) error {
			var err error
			tracked, err = slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(ctx, orgID, teamID, []string{named})
			return err
		})
		if len(tracked) != 1 || tracked[0] != survivor {
			t.Errorf("ReplaceForTeam(%s) tracked %v; want the surviving row %s", named, tracked, survivor)
		}
	}

	err := stores.Tx.WithTx(ctx, orgID, owner, func(tx db.TxStores) error {
		_, err := slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(ctx, orgID, teamID, []string{"C0NOROW01"})
		return err
	})
	if !errors.Is(err, slackstore.ErrChannelNotRegistered) {
		t.Errorf("ReplaceForTeam(unregistered) err = %v; want ErrChannelNotRegistered", err)
	}
	if got := listForTeamChannels(t, stores, ctx, orgID, owner, teamID); len(got) != 1 || got[0].SlackChannelID != survivor {
		t.Errorf("tracked set after the refused write = %+v; want the surviving row alone", got)
	}
}

// TestTeamChannel_Postgres_ReplaceForTeam_WaitsForAMoveInProgress: a tracking
// write naming the new id while a move absorbs that id's row waits for the
// move, then tracks the surviving row. Resolving before the move commits would
// pick the row being deleted, and the insert would fail its foreign key.
func TestTeamChannel_Postgres_ReplaceForTeam_WaitsForAMoveInProgress(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "tc-move-wait")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	survivor := channelRow(t, stores, ctx, orgID, "G0WAIT001")
	absorbed := channelRow(t, stores, ctx, orgID, "C0WAIT001")

	// The move's own steps, uncommitted: the lock, the change, the absorbed
	// row deleted and the channel's row moved onto the new id.
	move, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin move: %v", err)
	}
	defer func() { _ = move.Rollback() }()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`SELECT pg_advisory_xact_lock(hashtextextended($1, $2))`, []any{orgID, int64(0x43484944)}},
		{`INSERT INTO slack_channel_id_changes (org_id, old_channel_id, new_channel_id) VALUES ($1, 'G0WAIT001', 'C0WAIT001')`, []any{orgID}},
		{`DELETE FROM slack_channels WHERE id = $1`, []any{absorbed}},
		{`UPDATE slack_channels SET channel_id = 'C0WAIT001' WHERE id = $1`, []any{survivor}},
	} {
		if _, err := move.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("move step %q: %v", stmt.sql, err)
		}
	}

	type result struct {
		tracked []string
		err     error
	}
	wrote := make(chan result, 1)
	go func() {
		var r result
		r.err = stores.Tx.WithTx(ctx, orgID, owner, func(tx db.TxStores) error {
			var err error
			r.tracked, err = slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(ctx, orgID, teamID, []string{"C0WAIT001"})
			return err
		})
		wrote <- r
	}()
	select {
	case r := <-wrote:
		t.Fatalf("ReplaceForTeam returned %+v while the move held its lock; want it to wait", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := move.Commit(); err != nil {
		t.Fatalf("commit move: %v", err)
	}
	select {
	case r := <-wrote:
		if r.err != nil || len(r.tracked) != 1 || r.tracked[0] != survivor {
			t.Errorf("ReplaceForTeam after the move = %v, %v; want the surviving row %s", r.tracked, r.err, survivor)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReplaceForTeam still waiting after the move committed")
	}
}

// TestTeamChannel_Postgres_RLS_MemberWriteRefused: a plain (non-admin) team
// member's ReplaceForTeam write is refused by RLS (team_slack_channels_insert
// requires team-admin) — the call errors and the existing row set survives
// untouched, mirroring TestJiraStatusRulesStore_Postgres_ReplaceForTeam_TeamAdminGated.
func TestTeamChannel_Postgres_RLS_MemberWriteRefused(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "tc-rls-gate")
	member := pgtest.SeedUser(t, h, "tc-rls-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	replaceChannels(t, stores, ctx, orgID, owner, teamID, []string{"C0RLS0001"})

	channelRows(t, stores, ctx, orgID, []string{"C0RLS0001", "C0RLS0002"})
	err := stores.Tx.WithTx(ctx, orgID, member, func(tx db.TxStores) error {
		_, err := slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(ctx, orgID, teamID, []string{"C0RLS0001", "C0RLS0002"})
		return err
	})
	if err == nil {
		t.Fatal("member ReplaceForTeam succeeded; want RLS refusal (team-admin gate)")
	}

	got := listForTeamChannels(t, stores, ctx, orgID, owner, teamID)
	if len(got) != 1 || got[0].ChannelID != "C0RLS0001" {
		t.Fatalf("after refused member write: rows = %+v; want only the owner-seeded C0RLS0001", got)
	}
}

// TestTeamChannel_Postgres_RLS_CrossOrgTeamInvisible: a caller's claims
// scope team_slack_channels reads to their own org — a different org's team
// (even referenced by ID directly) returns zero rows, and ListForTeam
// naturally sees only the caller's own org's tracking.
func TestTeamChannel_Postgres_RLS_CrossOrgTeamInvisible(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, ownerA, teamA := pgtest.SeedOrgWithUser(t, h, "tc-xorg-a")
	orgB, ownerB, teamB := pgtest.SeedOrgWithUser(t, h, "tc-xorg-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	replaceChannels(t, stores, ctx, orgA, ownerA, teamA, []string{"C0XORG001"})
	replaceChannels(t, stores, ctx, orgB, ownerB, teamB, []string{"C0XORG002"})

	gotB := listForTeamChannels(t, stores, ctx, orgB, ownerB, teamB)
	if len(gotB) != 1 || gotB[0].ChannelID != "C0XORG002" {
		t.Fatalf("orgB ListForTeam = %+v; want only its own row", gotB)
	}

	// orgB's claims trying to read teamA (a different org's team) directly.
	var crossOrgRows []slackstore.TeamChannel
	mustWithUser(t, stores, ctx, orgB, ownerB, func(tx db.TxStores) error {
		var e error
		crossOrgRows, e = slackstore.FromTx(tx).TeamChannels.ListForTeam(ctx, orgB, teamA)
		return e
	})
	if len(crossOrgRows) != 0 {
		t.Errorf("orgB claims reading teamA (a different org's team) = %+v; want empty (cross-org leak)", crossOrgRows)
	}
}

// TestTeamChannel_Postgres_OnePrimaryPartialIndex_Enforced: the partial
// unique index on slack_channel_id WHERE is_primary rejects a second primary
// row for the same channel, even via a direct admin-pool INSERT that bypasses
// RLS entirely.
func TestTeamChannel_Postgres_OnePrimaryPartialIndex_Enforced(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamA := pgtest.SeedOrgWithUser(t, h, "tc-onepri")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	row := channelRow(t, stores, context.Background(), orgID, "C0ONEPRI1")

	if _, err := h.AdminDB.Exec(`
		INSERT INTO team_slack_channels (org_id, team_id, slack_channel_id, is_primary)
		VALUES ($1, $2, $3, true)
	`, orgID, teamA, row); err != nil {
		t.Fatalf("seed first primary: %v", err)
	}

	_, err := h.AdminDB.Exec(`
		INSERT INTO team_slack_channels (org_id, team_id, slack_channel_id, is_primary)
		VALUES ($1, $2, $3, true)
	`, orgID, teamB, row)
	if err == nil {
		t.Fatal("second primary INSERT for the same channel succeeded; want the partial unique index to reject it")
	}
}

// TestTeamChannel_Postgres_TrackingReferencesTheRegistry: a tracking row
// names a registry row of its own org — the composite foreign key refuses a
// row id that does not exist, and one of another org.
func TestTeamChannel_Postgres_TrackingReferencesTheRegistry(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, teamA := pgtest.SeedOrgWithUser(t, h, "tc-fk-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "tc-fk-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	rowB := channelRow(t, stores, context.Background(), orgB, "C0FK00001")

	for name, row := range map[string]string{"unregistered": "00000000-0000-0000-0000-00000000c0de", "other org's": rowB} {
		if _, err := h.AdminDB.Exec(`
			INSERT INTO team_slack_channels (org_id, team_id, slack_channel_id) VALUES ($1, $2, $3)
		`, orgA, teamA, row); err == nil {
			t.Errorf("tracking an %s channel row was accepted; want the foreign key to refuse it", name)
		}
	}
}

// TestTeamChannel_Postgres_ReconcilePrimariesSystem exercises all three
// documented behaviors: promoting a sole tracker, no-op when a primary
// already exists (including once a second tracker joins), and promoting the
// oldest remaining tracker once the primary is removed.
func TestTeamChannel_Postgres_ReconcilePrimariesSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, owner, teamA := pgtest.SeedOrgWithUser(t, h, "tc-reconcile")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, owner, teamB)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	primaryFor := func(channelID string) string {
		t.Helper()
		p, err := slackstore.FromStores(stores).TeamChannels.PrimaryTeamForChannelSystem(ctx, orgID, channelRow(t, stores, ctx, orgID, channelID))
		if err != nil {
			t.Fatalf("PrimaryTeamForChannelSystem(%s): %v", channelID, err)
		}
		return p
	}
	reconcile := func(channelIDs ...string) {
		t.Helper()
		if err := slackstore.FromStores(stores).TeamChannels.ReconcilePrimariesSystem(ctx, orgID, channelRows(t, stores, ctx, orgID, channelIDs)); err != nil {
			t.Fatalf("ReconcilePrimariesSystem(%v): %v", channelIDs, err)
		}
	}

	// Sole tracker is promoted.
	replaceChannels(t, stores, ctx, orgID, owner, teamA, []string{"C0HASPRI1"})
	reconcile("C0HASPRI1")
	if p := primaryFor("C0HASPRI1"); p != teamA {
		t.Fatalf("primary for C0HASPRI1 = %q; want %q (sole tracker promoted)", p, teamA)
	}

	// Re-reconciling a channel that already has a primary is a no-op.
	reconcile("C0HASPRI1")
	if p := primaryFor("C0HASPRI1"); p != teamA {
		t.Errorf("primary changed on re-reconcile: got %q, want unchanged %q", p, teamA)
	}

	// A second tracker joins; the existing primary is untouched.
	replaceChannels(t, stores, ctx, orgID, owner, teamB, []string{"C0HASPRI1"})
	reconcile("C0HASPRI1")
	if p := primaryFor("C0HASPRI1"); p != teamA {
		t.Errorf("primary after second tracker joins = %q; want unchanged %q", p, teamA)
	}

	// teamA (the primary) untracks — no primary until Reconcile runs again,
	// then the remaining oldest tracker (teamB) succeeds it.
	replaceChannels(t, stores, ctx, orgID, owner, teamA, nil)
	if p := primaryFor("C0HASPRI1"); p != "" {
		t.Fatalf("primary right after teamA untracks = %q; want \"\" until Reconcile runs", p)
	}
	reconcile("C0HASPRI1")
	if p := primaryFor("C0HASPRI1"); p != teamB {
		t.Errorf("primary after succession = %q; want %q (the only remaining tracker)", p, teamB)
	}
}

// TestTeamChannel_Postgres_SetPrimarySystem: org-admin reassignment clears
// the old primary and sets the new one atomically, and refuses to promote a
// team that does not track the channel.
func TestTeamChannel_Postgres_SetPrimarySystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, owner, teamA := pgtest.SeedOrgWithUser(t, h, "tc-setpri")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, owner, teamB)
	teamC := pgtest.SeedTeam(t, h, orgID, "team-c")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	replaceChannels(t, stores, ctx, orgID, owner, teamA, []string{"C0SETPRI1"})
	replaceChannels(t, stores, ctx, orgID, owner, teamB, []string{"C0SETPRI1"})
	row := channelRow(t, stores, ctx, orgID, "C0SETPRI1")
	if err := slackstore.FromStores(stores).TeamChannels.ReconcilePrimariesSystem(ctx, orgID, []string{row}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	primaryFor := func() string {
		t.Helper()
		p, err := slackstore.FromStores(stores).TeamChannels.PrimaryTeamForChannelSystem(ctx, orgID, row)
		if err != nil {
			t.Fatalf("PrimaryTeamForChannelSystem: %v", err)
		}
		return p
	}
	if p := primaryFor(); p != teamA {
		t.Fatalf("primary before reassignment = %q; want %q", p, teamA)
	}

	if err := slackstore.FromStores(stores).TeamChannels.SetPrimarySystem(ctx, orgID, row, teamB); err != nil {
		t.Fatalf("SetPrimarySystem: %v", err)
	}
	if p := primaryFor(); p != teamB {
		t.Fatalf("primary after reassignment = %q; want %q", p, teamB)
	}
	rowsA := listForTeamChannels(t, stores, ctx, orgID, owner, teamA)
	if len(rowsA) != 1 || rowsA[0].IsPrimary {
		t.Errorf("teamA row after reassignment = %+v; want present with is_primary=false", rowsA)
	}

	err := slackstore.FromStores(stores).TeamChannels.SetPrimarySystem(ctx, orgID, row, teamC)
	if !errors.Is(err, slackstore.ErrTeamNotTrackingChannel) {
		t.Fatalf("SetPrimarySystem for a non-tracking team = %v; want ErrTeamNotTrackingChannel", err)
	}
	if p := primaryFor(); p != teamB {
		t.Errorf("primary after failed reassignment = %q; want unchanged %q", p, teamB)
	}
}

// TestTeamChannel_Postgres_TwoOrgsBothHoldPrimaryOnSameChannelID: each org
// registers its own row for a channel id, so two different orgs each
// tracking the SAME global channel_id (the Slack Connect / shared-channel
// scenario) can each independently hold their own primary on it.
func TestTeamChannel_Postgres_TwoOrgsBothHoldPrimaryOnSameChannelID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, ownerA, teamA := pgtest.SeedOrgWithUser(t, h, "tc-2org-a")
	orgB, ownerB, teamB := pgtest.SeedOrgWithUser(t, h, "tc-2org-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	const shared = "C0SHARED99"
	replaceChannels(t, stores, ctx, orgA, ownerA, teamA, []string{shared})
	replaceChannels(t, stores, ctx, orgB, ownerB, teamB, []string{shared})

	rowA, rowB := channelRow(t, stores, ctx, orgA, shared), channelRow(t, stores, ctx, orgB, shared)
	if err := slackstore.FromStores(stores).TeamChannels.ReconcilePrimariesSystem(ctx, orgA, []string{rowA}); err != nil {
		t.Fatalf("Reconcile org A: %v", err)
	}
	if err := slackstore.FromStores(stores).TeamChannels.ReconcilePrimariesSystem(ctx, orgB, []string{rowB}); err != nil {
		t.Fatalf("Reconcile org B: %v", err)
	}

	pA, err := slackstore.FromStores(stores).TeamChannels.PrimaryTeamForChannelSystem(ctx, orgA, rowA)
	if err != nil || pA != teamA {
		t.Fatalf("org A primary for shared channel = (%q, %v); want (%q, nil)", pA, err, teamA)
	}
	pB, err := slackstore.FromStores(stores).TeamChannels.PrimaryTeamForChannelSystem(ctx, orgB, rowB)
	if err != nil || pB != teamB {
		t.Fatalf("org B primary for shared channel = (%q, %v); want (%q, nil)", pB, err, teamB)
	}
}
