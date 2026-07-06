package pg_test

import (
	"context"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestChannelRegistry_Postgres_UpsertSighting_CreatedThenBumped: the first
// sighting of a channel creates the registry row (created=true); a second
// sighting for the same (org, channel) reports created=false and bumps
// workspace_id + last_mention_at while leaving first_seen_at untouched.
func TestChannelRegistry_Postgres_UpsertSighting_CreatedThenBumped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-sight")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	t1 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	created, err := slackstore.FromStores(stores).Channels.UpsertSightingSystem(ctx, orgID, "T0WS0001", "C0SIGHT01", t1)
	if err != nil {
		t.Fatalf("UpsertSightingSystem (first): %v", err)
	}
	if !created {
		t.Error("first sighting: created = false; want true")
	}

	got, err := slackstore.FromStores(stores).Channels.GetSystem(ctx, orgID, "C0SIGHT01")
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	if got == nil {
		t.Fatal("GetSystem after first sighting = nil")
	}
	firstSeen := got.FirstSeenAt
	if got.LastMentionAt == nil || !got.LastMentionAt.Equal(t1) {
		t.Errorf("last_mention_at = %v; want %v", got.LastMentionAt, t1)
	}
	if got.WorkspaceID != "T0WS0001" {
		t.Errorf("workspace_id = %q; want %q", got.WorkspaceID, "T0WS0001")
	}

	t2 := t1.Add(30 * time.Minute)
	created, err = slackstore.FromStores(stores).Channels.UpsertSightingSystem(ctx, orgID, "T0WS0002", "C0SIGHT01", t2)
	if err != nil {
		t.Fatalf("UpsertSightingSystem (second): %v", err)
	}
	if created {
		t.Error("second sighting: created = true; want false (row already existed)")
	}

	got, err = slackstore.FromStores(stores).Channels.GetSystem(ctx, orgID, "C0SIGHT01")
	if err != nil {
		t.Fatalf("GetSystem (after bump): %v", err)
	}
	if !got.FirstSeenAt.Equal(firstSeen) {
		t.Errorf("first_seen_at = %v; want unchanged %v", got.FirstSeenAt, firstSeen)
	}
	if got.LastMentionAt == nil || !got.LastMentionAt.Equal(t2) {
		t.Errorf("last_mention_at = %v; want bumped to %v", got.LastMentionAt, t2)
	}
	if got.WorkspaceID != "T0WS0002" {
		t.Errorf("workspace_id = %q; want bumped to %q", got.WorkspaceID, "T0WS0002")
	}
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
	ctx := context.Background()

	if err := slackstore.FromStores(stores).Channels.EnsureSystem(ctx, orgID, "T0WS0001", "C0ENSURE1", ""); err != nil {
		t.Fatalf("EnsureSystem: %v", err)
	}

	got, err := slackstore.FromStores(stores).Channels.GetSystem(ctx, orgID, "C0ENSURE1")
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	if got == nil {
		t.Fatal("GetSystem after EnsureSystem = nil")
	}
	if got.LastMentionAt != nil {
		t.Errorf("last_mention_at = %v; want nil (EnsureSystem records no sighting)", got.LastMentionAt)
	}
	if got.Name != "" {
		t.Errorf("name = %q; want \"\" (no name supplied)", got.Name)
	}
	firstSeen := got.FirstSeenAt

	if err := slackstore.FromStores(stores).Channels.EnsureSystem(ctx, orgID, "", "C0ENSURE1", "eng-alerts"); err != nil {
		t.Fatalf("EnsureSystem (name): %v", err)
	}
	got, err = slackstore.FromStores(stores).Channels.GetSystem(ctx, orgID, "C0ENSURE1")
	if err != nil {
		t.Fatalf("GetSystem (after name): %v", err)
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
	if !got.FirstSeenAt.Equal(firstSeen) {
		t.Errorf("first_seen_at = %v; want unchanged %v", got.FirstSeenAt, firstSeen)
	}
	if got.NameResolvedAt == nil {
		t.Error("name_resolved_at = nil; want set once a name is supplied")
	}
}

// TestChannelRegistry_Postgres_SetNameSystem resolves a channel's display
// name and stamps name_resolved_at on an existing row.
func TestChannelRegistry_Postgres_SetNameSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "chan-setname")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	if _, err := slackstore.FromStores(stores).Channels.UpsertSightingSystem(ctx, orgID, "T0WS0001", "C0NAME001", time.Now()); err != nil {
		t.Fatalf("seed sighting: %v", err)
	}
	if err := slackstore.FromStores(stores).Channels.SetNameSystem(ctx, orgID, "C0NAME001", "eng-alerts"); err != nil {
		t.Fatalf("SetNameSystem: %v", err)
	}

	got, err := slackstore.FromStores(stores).Channels.GetSystem(ctx, orgID, "C0NAME001")
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

// TestChannelRegistry_Postgres_ListForOrg_MemberScopedTwoOrgsIndependent:
// ListForOrg (app pool, member RLS) sees only the caller's org's rows, and
// the SAME channel_id sighted by two different orgs materializes as two
// wholly independent registry rows (org_id is part of the PRIMARY KEY).
func TestChannelRegistry_Postgres_ListForOrg_MemberScopedTwoOrgsIndependent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "chan-org-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "chan-org-b")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	const shared = "C0SHARED01"
	if _, err := slackstore.FromStores(stores).Channels.UpsertSightingSystem(ctx, orgA, "T0SHARED1", shared, time.Now()); err != nil {
		t.Fatalf("seed org A sighting: %v", err)
	}
	if _, err := slackstore.FromStores(stores).Channels.UpsertSightingSystem(ctx, orgB, "T0SHARED1", shared, time.Now()); err != nil {
		t.Fatalf("seed org B sighting: %v", err)
	}
	if err := slackstore.FromStores(stores).Channels.SetNameSystem(ctx, orgA, shared, "org-a-name"); err != nil {
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

	if len(listA) != 1 || listA[0].ChannelID != shared {
		t.Fatalf("org A ListForOrg = %+v; want its own single row", listA)
	}
	if listA[0].Name != "org-a-name" {
		t.Errorf("org A name = %q; want %q", listA[0].Name, "org-a-name")
	}
	if len(listB) != 1 || listB[0].ChannelID != shared {
		t.Fatalf("org B ListForOrg = %+v; want its own single row", listB)
	}
	if listB[0].Name != "" {
		t.Errorf("org B name = %q; want \"\" — independent row, org A's name must not leak", listB[0].Name)
	}
}
