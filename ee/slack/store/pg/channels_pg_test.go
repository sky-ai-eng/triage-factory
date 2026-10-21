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
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestChannelRegistry_Postgres_UpsertSighting_CreatedThenBumped: the first
// sighting of a channel creates the registry row with no name resolved yet;
// a second sighting for the same (org, channel) returns the same row and
// bumps workspace_id + last_mention_at while leaving first_seen_at untouched.
func TestChannelRegistry_Postgres_UpsertSighting_CreatedThenBumped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-sight")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	t1 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	first, err := channels.UpsertSightingSystem(ctx, orgID, "T0WS0001", "C0SIGHT01", t1)
	if err != nil {
		t.Fatalf("UpsertSightingSystem (first): %v", err)
	}
	if first.ID == "" || first.ChannelID != "C0SIGHT01" {
		t.Fatalf("first sighting = %+v; want a row for C0SIGHT01", first)
	}
	if first.NameResolvedAt != nil {
		t.Errorf("first sighting name_resolved_at = %v; want nil", first.NameResolvedAt)
	}
	if first.LastMentionAt == nil || !first.LastMentionAt.Equal(t1) {
		t.Errorf("last_mention_at = %v; want %v", first.LastMentionAt, t1)
	}
	if first.WorkspaceID != "T0WS0001" {
		t.Errorf("workspace_id = %q; want %q", first.WorkspaceID, "T0WS0001")
	}

	t2 := t1.Add(30 * time.Minute)
	second, err := channels.UpsertSightingSystem(ctx, orgID, "T0WS0002", "C0SIGHT01", t2)
	if err != nil {
		t.Fatalf("UpsertSightingSystem (second): %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second sighting row = %s; want the first's %s", second.ID, first.ID)
	}
	if !second.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Errorf("first_seen_at = %v; want unchanged %v", second.FirstSeenAt, first.FirstSeenAt)
	}
	if second.LastMentionAt == nil || !second.LastMentionAt.Equal(t2) {
		t.Errorf("last_mention_at = %v; want bumped to %v", second.LastMentionAt, t2)
	}
	if second.WorkspaceID != "T0WS0002" {
		t.Errorf("workspace_id = %q; want bumped to %q", second.WorkspaceID, "T0WS0002")
	}
	assertReturnedRowStored(t, channels, orgID, second)
}

// assertReturnedRowStored asserts a write's returned row equals a point read of it.
func assertReturnedRowStored(t *testing.T, channels slackstore.ChannelRegistryStore, orgID string, written slackstore.Channel) {
	t.Helper()
	read, err := channels.GetSystem(context.Background(), orgID, written.ID)
	if err != nil || read == nil {
		t.Fatalf("GetSystem(%s) = %+v, %v", written.ID, read, err)
	}
	if read.ChannelID != written.ChannelID || read.WorkspaceID != written.WorkspaceID || read.Name != written.Name ||
		!read.FirstSeenAt.Equal(written.FirstSeenAt) || !equalTimes(read.LastMentionAt, written.LastMentionAt) ||
		!equalTimes(read.NameResolvedAt, written.NameResolvedAt) {
		t.Errorf("returned row %+v; stored row %+v", written, *read)
	}
}

func equalTimes(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// TestChannelRegistry_Postgres_EnsureSystem_DoesNotTouchMentionTimestamps:
// EnsureSystem creates a bare registry row for a channel TF has never seen
// a mention in (the TF-first tracking flow) — last_mention_at stays NULL,
// unlike a sighting. A later EnsureSystem call with a name resolves it
// without disturbing first_seen_at, last_mention_at, or an already-set
// workspace_id (an empty value must never clobber).
func TestChannelRegistry_Postgres_EnsureSystem_DoesNotTouchMentionTimestamps(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-ensure")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	got, err := channels.EnsureSystem(ctx, orgID, "T0WS0001", "C0ENSURE1", "")
	if err != nil {
		t.Fatalf("EnsureSystem: %v", err)
	}
	if got.LastMentionAt != nil {
		t.Errorf("last_mention_at = %v; want nil (EnsureSystem records no sighting)", got.LastMentionAt)
	}
	if got.Name != "" {
		t.Errorf("name = %q; want \"\" (no name supplied)", got.Name)
	}
	first := got

	got, err = channels.EnsureSystem(ctx, orgID, "", "C0ENSURE1", "eng-alerts")
	if err != nil {
		t.Fatalf("EnsureSystem (name): %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("row = %s; want the first ensure's %s", got.ID, first.ID)
	}
	if got.Name != "eng-alerts" {
		t.Errorf("name = %q; want %q", got.Name, "eng-alerts")
	}
	if got.WorkspaceID != "T0WS0001" {
		t.Errorf("workspace_id = %q; want unchanged %q (empty supplied value must not clobber)", got.WorkspaceID, "T0WS0001")
	}
	if got.LastMentionAt != nil {
		t.Errorf("last_mention_at = %v; want still nil", got.LastMentionAt)
	}
	if !got.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Errorf("first_seen_at = %v; want unchanged %v", got.FirstSeenAt, first.FirstSeenAt)
	}
	if got.NameResolvedAt == nil {
		t.Error("name_resolved_at = nil; want set once a name is supplied")
	}
	assertReturnedRowStored(t, channels, orgID, got)
}

// TestChannelRegistry_Postgres_SetNameSystem resolves a channel's display
// name by its row and stamps name_resolved_at.
func TestChannelRegistry_Postgres_SetNameSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-setname")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	row, err := channels.UpsertSightingSystem(ctx, orgID, "T0WS0001", "C0NAME001", time.Now())
	if err != nil {
		t.Fatalf("seed sighting: %v", err)
	}
	if err := channels.SetNameSystem(ctx, orgID, row.ID, "eng-alerts"); err != nil {
		t.Fatalf("SetNameSystem: %v", err)
	}

	got, err := channels.GetSystem(ctx, orgID, row.ID)
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	if got.Name != "eng-alerts" {
		t.Errorf("name = %q; want %q", got.Name, "eng-alerts")
	}
	if got.NameResolvedAt == nil {
		t.Error("name_resolved_at = nil; want set by SetNameSystem")
	}
}

// TestChannelRegistry_Postgres_ResolveFollowsAChange: once Slack moves a
// channel to a new id, the old id resolves to the channel's row, which now
// carries the new id — and a sighting or an ensure under the old id lands on
// that row instead of registering the retired id as a channel of its own.
func TestChannelRegistry_Postgres_ResolveFollowsAChange(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-resolve")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	row, err := channels.UpsertSightingSystem(ctx, orgID, "T0WS0001", "G0RESOLVE1", time.Now())
	if err != nil {
		t.Fatalf("seed sighting: %v", err)
	}
	if miss, err := channels.ResolveSystem(ctx, orgID, "C0NEVERSEEN"); err != nil || miss != nil {
		t.Fatalf("ResolveSystem(unknown) = %+v, %v; want nil", miss, err)
	}
	if _, err := channels.MoveSystem(ctx, orgID, "G0RESOLVE1", "C0RESOLVE1"); err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}

	for _, id := range []string{"G0RESOLVE1", "C0RESOLVE1"} {
		got, err := channels.ResolveSystem(ctx, orgID, id)
		if err != nil {
			t.Fatalf("ResolveSystem(%s): %v", id, err)
		}
		if got == nil || got.ID != row.ID || got.ChannelID != "C0RESOLVE1" {
			t.Errorf("ResolveSystem(%s) = %+v; want row %s carrying C0RESOLVE1", id, got, row.ID)
		}
	}

	sighted, err := channels.UpsertSightingSystem(ctx, orgID, "T0WS0001", "G0RESOLVE1", time.Now())
	if err != nil {
		t.Fatalf("sighting under the retired id: %v", err)
	}
	ensured, err := channels.EnsureSystem(ctx, orgID, "", "G0RESOLVE1", "")
	if err != nil {
		t.Fatalf("ensure under the retired id: %v", err)
	}
	if sighted.ID != row.ID || ensured.ID != row.ID {
		t.Errorf("sighting row %s, ensure row %s; want both the channel's %s", sighted.ID, ensured.ID, row.ID)
	}
	var n int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM slack_channels WHERE org_id = $1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count slack_channels: %v", err)
	}
	if n != 1 {
		t.Errorf("slack_channels rows = %d; want 1 (a retired id registers nothing)", n)
	}
}

// TestChannelRegistry_Postgres_ListForOrg_MemberScopedTwoOrgsIndependent:
// ListForOrg (app pool, member RLS) sees only the caller's org's rows, and
// the SAME channel_id sighted by two different orgs materializes as two
// wholly independent registry rows (channel_id is unique per org).
func TestChannelRegistry_Postgres_ListForOrg_MemberScopedTwoOrgsIndependent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "chan-org-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "chan-org-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	const shared = "C0SHARED01"
	rowA, err := channels.UpsertSightingSystem(ctx, orgA, "T0SHARED1", shared, time.Now())
	if err != nil {
		t.Fatalf("seed org A sighting: %v", err)
	}
	rowB, err := channels.UpsertSightingSystem(ctx, orgB, "T0SHARED1", shared, time.Now())
	if err != nil {
		t.Fatalf("seed org B sighting: %v", err)
	}
	if rowA.ID == rowB.ID {
		t.Fatalf("both orgs got row %s; want one each", rowA.ID)
	}
	if err := channels.SetNameSystem(ctx, orgA, rowA.ID, "org-a-name"); err != nil {
		t.Fatalf("set org A name: %v", err)
	}

	var listA, listB []slackstore.Channel
	mustWithUser(t, stores, ctx, orgA, userA, func(tx db.TxStores) error {
		var e error
		listA, e = slackstore.FromTx(tx).Channels.ListForOrg(ctx, orgA)
		return e
	})
	mustWithUser(t, stores, ctx, orgB, userB, func(tx db.TxStores) error {
		var e error
		listB, e = slackstore.FromTx(tx).Channels.ListForOrg(ctx, orgB)
		return e
	})

	if len(listA) != 1 || listA[0].ID != rowA.ID {
		t.Fatalf("org A ListForOrg = %+v; want its own single row", listA)
	}
	if listA[0].Name != "org-a-name" {
		t.Errorf("org A name = %q; want %q", listA[0].Name, "org-a-name")
	}
	if len(listB) != 1 || listB[0].ID != rowB.ID {
		t.Fatalf("org B ListForOrg = %+v; want its own single row", listB)
	}
	if listB[0].Name != "" {
		t.Errorf("org B name = %q; want \"\" — independent row, org A's name must not leak", listB[0].Name)
	}
}

// TestChannelRegistry_Postgres_FindOrCreateThread: a thread entity is created
// once per thread, referencing its channel's row; a second call finds it and
// never rewrites its kind; the key names the id the channel has now, whichever
// id the call names it by; a channel with no row is ErrChannelNotRegistered;
// and the core entity store cannot create a Slack entity at all, since it has
// no channel row to reference.
func TestChannelRegistry_Postgres_FindOrCreateThread(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-thread")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	row, err := channels.EnsureSystem(ctx, orgID, "T0WS0001", "G0THREAD1", "")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	first, err := channels.FindOrCreateThreadSystem(ctx, orgID, "G0THREAD1", "1700000000.000100", "thread", "root", "https://x/p1")
	if err != nil || !first.Created {
		t.Fatalf("FindOrCreateThreadSystem (first) = %+v, %v; want created", first, err)
	}
	if first.Channel.ID != row.ID || first.Channel.ChannelID != "G0THREAD1" {
		t.Errorf("answer's channel = %+v; want the row %s", first.Channel, row.ID)
	}
	id := first.EntityID
	e, err := stores.Entities.GetSystem(ctx, orgID, id)
	if err != nil || e == nil {
		t.Fatalf("read thread entity: %+v, %v", e, err)
	}
	if e.Source != "slack" || e.Scope != domain.SlackScope || e.SourceID != "G0THREAD1/1700000000.000100" ||
		e.Kind != "thread" || e.Title != "root" || e.URL != "https://x/p1" || e.State != "active" {
		t.Errorf("thread entity = %+v", e)
	}
	if got := entityChannelRow(t, h, id); got != row.ID {
		t.Errorf("slack_channel_id = %q; want the channel's row %s", got, row.ID)
	}

	again, err := channels.FindOrCreateThreadSystem(ctx, orgID, "G0THREAD1", "1700000000.000100", "message", "ignored", "")
	if err != nil || again.Created || again.EntityID != id {
		t.Errorf("FindOrCreateThreadSystem (again) = %+v, %v; want %s found", again, err, id)
	}
	if e, _ := stores.Entities.GetSystem(ctx, orgID, id); e.Kind != "thread" {
		t.Errorf("kind = %q; want thread (unchanged by a later message-kind resolve)", e.Kind)
	}

	if _, err := channels.MoveSystem(ctx, orgID, "G0THREAD1", "C0THREAD1"); err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}
	for _, named := range []string{"G0THREAD1", "C0THREAD1"} {
		moved, err := channels.FindOrCreateThreadSystem(ctx, orgID, named, "1700000000.000200", "thread", "next", "")
		if err != nil || moved.Channel.ID != row.ID || moved.Channel.ChannelID != "C0THREAD1" {
			t.Fatalf("FindOrCreateThreadSystem (after move, by %s) = %+v, %v; want the row under its new id", named, moved, err)
		}
		if e, _ := stores.Entities.GetSystem(ctx, orgID, moved.EntityID); e.SourceID != "C0THREAD1/1700000000.000200" {
			t.Errorf("key after the move, by %s = %q; want it under the channel's new id", named, e.SourceID)
		}
	}

	if _, err := channels.FindOrCreateThreadSystem(ctx, orgID, "C0UNKNOWN", "1700000000.000100", "thread", "", ""); !errors.Is(err, slackstore.ErrChannelNotRegistered) {
		t.Errorf("FindOrCreateThreadSystem (unregistered) err = %v; want ErrChannelNotRegistered", err)
	}

	if _, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "slack", domain.SlackScope, "C0THREAD1/1700000000.000300", "", "message", "", ""); err == nil {
		t.Error("core FindOrCreateSystem created a slack entity with no channel row; want the CHECK to refuse it")
	}
}

// TestChannelRegistry_Postgres_ThreadIdentityIsRowAndTS: a thread is found by
// its channel's row and its root ts, never by the key it is displayed under:
// with the key rewritten to spell something else, both lookups still answer
// the thread, and the most recently closed entity answers once none is
// active. One thread has at most one active entity, and every Slack entity
// carries its root ts.
func TestChannelRegistry_Postgres_ThreadIdentityIsRowAndTS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-thread-id")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	channels := slackstore.FromStores(stores).Channels
	ctx := context.Background()

	row, err := channels.EnsureSystem(ctx, orgID, "T0WS0001", "C0IDENT01", "")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	first, err := channels.FindOrCreateThreadSystem(ctx, orgID, "C0IDENT01", "1700000000.000100", "thread", "root", "")
	if err != nil || !first.Created {
		t.Fatalf("FindOrCreateThreadSystem = %+v, %v; want created", first, err)
	}
	if got := entityThreadTS(t, h, first.EntityID); got != "1700000000.000100" {
		t.Errorf("slack_thread_ts = %q; want the root ts", got)
	}

	if _, err := h.AdminDB.Exec(`UPDATE entities SET source_id = 'C0ELSEWHERE/1' WHERE id = $1`, first.EntityID); err != nil {
		t.Fatalf("rewrite key: %v", err)
	}
	if again, err := channels.FindOrCreateThreadSystem(ctx, orgID, "C0IDENT01", "1700000000.000100", "message", "", ""); err != nil || again.Created || again.EntityID != first.EntityID {
		t.Errorf("FindOrCreateThreadSystem after the key changed = %+v, %v; want %s found", again, err, first.EntityID)
	}
	if got, err := channels.GetThreadSystem(ctx, orgID, row.ID, "1700000000.000100"); err != nil || got == nil ||
		*got != (slackstore.ThreadEntity{ID: first.EntityID, Kind: "thread", State: "active"}) {
		t.Errorf("GetThreadSystem = %+v, %v; want the active thread %s", got, err, first.EntityID)
	}
	if got, err := channels.GetThreadSystem(ctx, orgID, row.ID, "1700000000.000999"); err != nil || got != nil {
		t.Errorf("GetThreadSystem (no such thread) = %+v, %v; want nil", got, err)
	}

	var keyOnly string
	err = h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, scope, source_id, kind, slack_channel_id, slack_thread_ts)
		VALUES ($1, 'slack', $2, 'C0IDENT01/other', 'thread', $3, '1700000000.000100')
		RETURNING id::text
	`, orgID, domain.SlackScope, row.ID).Scan(&keyOnly)
	if err == nil {
		t.Error("a second active entity for one thread was accepted; want the index to refuse it")
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (org_id, source, scope, source_id, kind, slack_channel_id)
		VALUES ($1, 'slack', $2, 'C0IDENT01/2', 'thread', $3)
	`, orgID, domain.SlackScope, row.ID); err == nil {
		t.Error("a Slack entity with no root ts was accepted; want the CHECK to refuse it")
	}

	if _, err := stores.Entities.CloseSystem(ctx, orgID, first.EntityID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, err := channels.GetThreadSystem(ctx, orgID, row.ID, "1700000000.000100"); err != nil || got == nil ||
		*got != (slackstore.ThreadEntity{ID: first.EntityID, Kind: "thread", State: "closed"}) {
		t.Errorf("GetThreadSystem (closed) = %+v, %v; want the closed thread %s", got, err, first.EntityID)
	}
}

// entityThreadTS reads an entity's slack_thread_ts.
func entityThreadTS(t *testing.T, h *pgtest.Harness, entityID string) string {
	t.Helper()
	var ts string
	if err := h.AdminDB.QueryRow(`SELECT COALESCE(slack_thread_ts, '') FROM entities WHERE id = $1`, entityID).Scan(&ts); err != nil {
		t.Fatalf("read slack_thread_ts of %s: %v", entityID, err)
	}
	return ts
}

// TestChannelRegistry_Postgres_ThreadChannelPinnedToOrg: a thread entity's
// channel row must be its own org's — the composite foreign key refuses
// another org's channel.
func TestChannelRegistry_Postgres_ThreadChannelPinnedToOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-pin-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-pin-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	rowB, err := slackstore.FromStores(stores).Channels.EnsureSystem(ctx, orgB, "T0WS0001", "C0PIN0001", "")
	if err != nil {
		t.Fatalf("ensure org B channel: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (org_id, source, scope, source_id, kind, slack_channel_id, slack_thread_ts)
		VALUES ($1, 'slack', $2, 'C0PIN0001/1.0', 'thread', $3, '1.0')
	`, orgA, domain.SlackScope, rowB.ID); err == nil {
		t.Error("org A entity referencing org B's channel row was accepted; want the foreign key to refuse it")
	}
}

// entityChannelRow reads an entity's slack_channel_id.
func entityChannelRow(t *testing.T, h *pgtest.Harness, entityID string) string {
	t.Helper()
	var row string
	if err := h.AdminDB.QueryRow(`SELECT COALESCE(slack_channel_id::text, '') FROM entities WHERE id = $1`, entityID).Scan(&row); err != nil {
		t.Fatalf("read slack_channel_id of %s: %v", entityID, err)
	}
	return row
}
