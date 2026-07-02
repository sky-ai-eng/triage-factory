package pgtest

import (
	"database/sql"
	"errors"
	"testing"

	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// marketplaceSnapshot is a minimal valid domain.ListingSnapshot for the RLS
// tests below — content doesn't matter, only that it round-trips.
func marketplaceSnapshot(name string) domain.ListingSnapshot {
	return domain.ListingSnapshot{
		SchemaVersion: 1,
		Kind:          domain.ListingKindPrompt,
		Name:          name,
		Description:   "rls fixture",
		EventTypes:    []string{"github:pr:opened"},
		Steps: []domain.SnapshotStep{
			{StepIndex: 0, Name: name, Body: "mission body"},
		},
	}
}

// publishAsTeamWriter publishes a listing (with sourceID, may be "") as
// userID (a write-role member of teamID) inside its own claims-set tx,
// returning the new listing id.
func publishAsTeamWriter(t *testing.T, h *Harness, orgID, userID, teamID, name, sourceID string) string {
	t.Helper()
	var id string
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		got, err := pgstore.NewForTx(tx, SecretKey).Marketplace.Publish(t.Context(), orgID, domain.MarketplaceListing{
			Kind: domain.ListingKindPrompt, Name: name, Description: "rls fixture", PublisherTeamID: teamID, SourceID: sourceID,
		}, marketplaceSnapshot(name))
		if err != nil {
			return err
		}
		id = got
		return nil
	}); err != nil {
		t.Fatalf("publish %q as %s on team %s: %v", name, userID, teamID, err)
	}
	return id
}

// TestMarketplaceRLS_CrossTeamRead pins that every org member browses a
// published listing regardless of which team published it — the marketplace
// is the first org-wide read path for prompt-shaped content (TFAC-535).
func TestMarketplaceRLS_CrossTeamRead(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	teamB := SeedTeam(t, h, orgA, "teamB")
	AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Alice's Prompt", "")

	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		detail, err := pgstore.NewForTx(tx, SecretKey).Marketplace.Get(t.Context(), orgA, listingID, bob)
		if err != nil {
			return err
		}
		if detail.Name != "Alice's Prompt" {
			t.Errorf("bob's Get name = %q, want Alice's Prompt", detail.Name)
		}
		if len(detail.Versions) != 1 || detail.CurrentSnapshot.Name != "Alice's Prompt" {
			t.Errorf("bob's Get versions/snapshot = %+v / %+v, want v1 with the published snapshot", detail.Versions, detail.CurrentSnapshot)
		}
		listed, err := pgstore.NewForTx(tx, SecretKey).Marketplace.List(t.Context(), orgA, bob, domain.ListingFilter{})
		if err != nil {
			return err
		}
		if len(listed) != 1 || listed[0].ID != listingID {
			t.Errorf("bob's List = %+v, want just Alice's listing", listed)
		}
		return nil
	}); err != nil {
		t.Fatalf("bob reads teamA's listing: %v", err)
	}
}

// TestMarketplaceRLS_WriteRequiresPublisherTeam pins that only a
// write-role member of the *publishing* team can Delist/Relist/republish a
// listing — a sibling team's member reading it under RLS produces a
// zero-row UPDATE, not a visible mutation.
func TestMarketplaceRLS_WriteRequiresPublisherTeam(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	teamB := SeedTeam(t, h, orgA, "teamB")
	AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Guarded Prompt", "")

	// bob (teamB) cannot delist teamA's listing: the UPDATE runs (no error —
	// RLS USING silently filters the row) but affects zero rows.
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE marketplace_listings SET status = 'delisted', delisted_at = now() WHERE id = $1`, listingID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob's delist attempt affected %d rows, want 0 (RLS should block)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("bob's blocked delist: %v", err)
	}

	// The listing is untouched — still published.
	var status string
	if err := h.AdminDB.QueryRow(`SELECT status FROM marketplace_listings WHERE id = $1`, listingID).Scan(&status); err != nil {
		t.Fatalf("read listing status: %v", err)
	}
	if status != domain.ListingStatusPublished {
		t.Fatalf("listing status = %q after bob's blocked write, want still published", status)
	}

	// alice (teamA, the publisher) can delist it.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, SecretKey).Marketplace.Delist(t.Context(), orgA, listingID)
	}); err != nil {
		t.Fatalf("alice's delist: %v", err)
	}
	if err := h.AdminDB.QueryRow(`SELECT status FROM marketplace_listings WHERE id = $1`, listingID).Scan(&status); err != nil {
		t.Fatalf("read listing status after alice's delist: %v", err)
	}
	if status != domain.ListingStatusDelisted {
		t.Fatalf("listing status = %q after alice's delist, want delisted", status)
	}
}

// TestMarketplaceRLS_DelistedVisibility pins that a delisted listing drops
// out of a sibling team's Get (RLS: published OR own-team-write), while the
// publisher team's writer keeps resolving it (to relist/manage).
func TestMarketplaceRLS_DelistedVisibility(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	teamB := SeedTeam(t, h, orgA, "teamB")
	AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Soon Delisted", "")
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, SecretKey).Marketplace.Delist(t.Context(), orgA, listingID)
	}); err != nil {
		t.Fatalf("alice's delist: %v", err)
	}

	// bob (teamB, non-publisher): the listing is gone.
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, SecretKey).Marketplace.Get(t.Context(), orgA, listingID, bob)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("bob's Get(delisted) = %v, want sql.ErrNoRows", err)
		}
		listed, err := pgstore.NewForTx(tx, SecretKey).Marketplace.List(t.Context(), orgA, bob, domain.ListingFilter{})
		if err != nil {
			return err
		}
		if len(listed) != 0 {
			t.Errorf("bob's List after delist = %+v, want empty", listed)
		}
		return nil
	}); err != nil {
		t.Fatalf("bob's post-delist reads: %v", err)
	}

	// alice (teamA, the publisher) still resolves it, to relist/manage.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		detail, err := pgstore.NewForTx(tx, SecretKey).Marketplace.Get(t.Context(), orgA, listingID, alice)
		if err != nil {
			return err
		}
		if detail.Status != domain.ListingStatusDelisted {
			t.Errorf("alice's Get(delisted) status = %q, want delisted", detail.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("alice's post-delist Get: %v", err)
	}
}

// TestMarketplaceRLS_VotePKAndOwnerOnlyDelete pins the votes PK (one
// 'recommend' per user per listing — a second Vote from the same user is a
// no-op, not a duplicate row) and that a vote can only be deleted by the
// voter themselves (RLS: user_id = tf.current_user_id()), never by another
// org member.
func TestMarketplaceRLS_VotePKAndOwnerOnlyDelete(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	carol := SeedUser(t, h, "carol")
	dave := SeedUser(t, h, "dave")
	teamB := SeedTeam(t, h, orgA, "teamB")
	AddOrgMember(t, h, carol, orgA, teamB, "member", "member")
	AddOrgMember(t, h, dave, orgA, teamB, "member", "member")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Votable", "")

	// dave votes; a second Vote from dave is idempotent (PK dedupe).
	if err := h.WithUser(t, dave, orgA, func(tx *sql.Tx) error {
		m := pgstore.NewForTx(tx, SecretKey).Marketplace
		if err := m.Vote(t.Context(), orgA, listingID, dave); err != nil {
			return err
		}
		return m.Vote(t.Context(), orgA, listingID, dave)
	}); err != nil {
		t.Fatalf("dave's votes: %v", err)
	}
	var voteCount int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_votes WHERE listing_id = $1`, listingID).Scan(&voteCount); err != nil {
		t.Fatalf("count votes: %v", err)
	}
	if voteCount != 1 {
		t.Fatalf("vote count = %d after two Vote calls from the same user, want 1 (PK dedupe)", voteCount)
	}

	// carol cannot delete dave's vote: the DELETE runs (no error) but
	// affects zero rows under RLS (user_id = tf.current_user_id()).
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM marketplace_votes WHERE listing_id = $1 AND user_id = $2::uuid`, listingID, dave)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("carol's delete of dave's vote affected %d rows, want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("carol's blocked delete: %v", err)
	}
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_votes WHERE listing_id = $1`, listingID).Scan(&voteCount); err != nil {
		t.Fatalf("recount votes: %v", err)
	}
	if voteCount != 1 {
		t.Fatalf("vote count = %d after carol's blocked delete, want still 1 (dave's vote survives)", voteCount)
	}
}

// TestMarketplaceRLS_CrossOrgIsolation pins that a listing published in org
// A is invisible under org B's tf.current_org_id() — even when the caller
// explicitly passes org A's id as the orgID argument, RLS still scopes the
// underlying rows to the claims org.
func TestMarketplaceRLS_CrossOrgIsolation(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	orgB, dave, _ := SeedOrgWithUser(t, h, "dave")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "OrgA Only", "00000000-0000-0000-0000-0000000000cc")

	if err := h.WithUser(t, dave, orgB, func(tx *sql.Tx) error {
		m := pgstore.NewForTx(tx, SecretKey).Marketplace
		if _, err := m.Get(t.Context(), orgA, listingID, dave); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("dave's cross-org Get = %v, want sql.ErrNoRows", err)
		}
		listed, err := m.List(t.Context(), orgA, dave, domain.ListingFilter{})
		if err != nil {
			return err
		}
		if len(listed) != 0 {
			t.Errorf("dave's cross-org List = %+v, want empty", listed)
		}
		active, err := m.GetActiveBySource(t.Context(), orgA, "00000000-0000-0000-0000-0000000000cc")
		if err != nil {
			return err
		}
		if active != nil {
			t.Errorf("dave's cross-org GetActiveBySource = %+v, want nil", active)
		}
		return nil
	}); err != nil {
		t.Fatalf("dave's cross-org reads: %v", err)
	}
}

// TestMarketplaceRLS_InstallRequiresInstallingTeamWrite pins that
// RecordInstall gates on write role on the *installing* team, not the
// publishing team — a viewer on the installing team is blocked; a member
// (write role) succeeds.
func TestMarketplaceRLS_InstallRequiresInstallingTeamWrite(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	teamB := SeedTeam(t, h, orgA, "teamB")
	erin := SeedUser(t, h, "erin")
	AddOrgMember(t, h, erin, orgA, teamB, "member", "viewer")
	// teamB needs its own admin (tf.guard_team_admins, TFAC-444): erin's role
	// gets mutated below, and a team with zero admins trips "each team must
	// retain at least one admin role" on that write regardless of whether
	// the write itself removed the last admin.
	teamBAdmin := SeedUser(t, h, "teamBAdmin")
	AddOrgMember(t, h, teamBAdmin, orgA, teamB, "member", "admin")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Installable", "")

	const fakeRootObjectID = "00000000-0000-0000-0000-0000000000aa"

	// erin is a viewer on teamB: RecordInstall is blocked by RLS.
	installErr := h.WithUser(t, erin, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, SecretKey).Marketplace.RecordInstall(t.Context(), orgA, listingID, 1, teamB, erin, fakeRootObjectID)
	})
	AssertRLSViolation(t, installErr)

	var installCount int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_installs WHERE listing_id = $1`, listingID).Scan(&installCount); err != nil {
		t.Fatalf("count installs: %v", err)
	}
	if installCount != 0 {
		t.Fatalf("install count = %d after erin's blocked install, want 0", installCount)
	}

	// Promote erin to a write role; the same install now succeeds.
	MustExec(t, h.AdminDB, `UPDATE memberships SET role = 'member' WHERE user_id = $1 AND team_id = $2`, erin, teamB)
	if err := h.WithUser(t, erin, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, SecretKey).Marketplace.RecordInstall(t.Context(), orgA, listingID, 1, teamB, erin, fakeRootObjectID)
	}); err != nil {
		t.Fatalf("erin's install after promotion: %v", err)
	}
	var rootObjectID string
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(root_object_id::text), '') FROM marketplace_installs WHERE listing_id = $1`, listingID).Scan(&installCount, &rootObjectID); err != nil {
		t.Fatalf("recount installs: %v", err)
	}
	if installCount != 1 {
		t.Fatalf("install count = %d after erin's promoted install, want 1", installCount)
	}
	if rootObjectID != fakeRootObjectID {
		t.Errorf("root_object_id = %q, want %q (provenance survives the install write)", rootObjectID, fakeRootObjectID)
	}
}
