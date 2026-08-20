package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// MarketplaceStoreFactory hands the conformance suite a wired
// MarketplaceStore plus the org/team/user ids every write in the suite
// addresses. userID must carry write role on teamID — the suite installs
// into teamID as well as publishing from it, and RLS write-role seeding is
// the caller's responsibility (schema- and auth-blind harness).
type MarketplaceStoreFactory func(t *testing.T) (store db.MarketplaceStore, orgID, teamID, userID string)

// marketplaceConformanceSnapshot is a minimal valid single-step prompt
// snapshot — content doesn't matter, only that it round-trips.
func marketplaceConformanceSnapshot(name string) domain.ListingSnapshot {
	return domain.ListingSnapshot{
		SchemaVersion: domain.ListingSnapshotSchemaVersion,
		Kind:          domain.ListingKindPrompt,
		Name:          name,
		Description:   "returned-row fixture",
		Steps: []domain.SnapshotStep{
			{StepIndex: 0, Name: name, Body: "mission body"},
		},
	}
}

// RunMarketplaceReturnedRowConformance pins the returned-row standard on
// PublishVersion/Delist/Relist/Vote/RecordInstall: what each write hands
// back is what a follow-up Get finds. All five run over ONE published
// listing in subtest declaration order (Go runs t.Run subtests
// sequentially — RunOrgsReturnedRowConformance's single-shared-row shape),
// since Delist/Relist are state flips on the same row PublishVersion just
// bumped.
func RunMarketplaceReturnedRowConformance(t *testing.T, mk MarketplaceStoreFactory) {
	t.Helper()
	ctx := context.Background()
	store, orgID, teamID, userID := mk(t)

	listingID, err := store.Publish(ctx, orgID, domain.MarketplaceListing{
		Kind: domain.ListingKindPrompt, Name: "Conformance Listing", Description: "d",
		PublisherTeamID: teamID,
	}, marketplaceConformanceSnapshot("Conformance Listing"))
	if err != nil {
		t.Fatalf("seed Publish: %v", err)
	}

	readListing := func() (*domain.MarketplaceListing, error) {
		d, err := store.Get(ctx, orgID, listingID, userID)
		if err != nil {
			return nil, err
		}
		return &d.MarketplaceListing, nil
	}
	readSummary := func() (*domain.ListingSummary, error) {
		d, err := store.Get(ctx, orgID, listingID, userID)
		if err != nil {
			return nil, err
		}
		return &d.ListingSummary, nil
	}

	t.Run("Vote_returns_the_stored_listing_summary", func(t *testing.T) {
		got, err := store.Vote(ctx, orgID, listingID, userID)
		if err != nil {
			t.Fatalf("Vote: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Vote", got, readSummary)
		if got.VoteCount != 1 || !got.ViewerVoted {
			t.Errorf("Vote returned %+v, want vote_count=1 viewer_voted=true", got)
		}
	})

	t.Run("RecordInstall_returns_the_stored_listing_summary", func(t *testing.T) {
		got, err := store.RecordInstall(ctx, orgID, listingID, 1, teamID, userID, "")
		if err != nil {
			t.Fatalf("RecordInstall: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "RecordInstall", got, readSummary)
		if got.InstallCount != 1 {
			t.Errorf("RecordInstall returned install_count=%d, want 1", got.InstallCount)
		}
	})

	t.Run("PublishVersion_returns_the_stored_row", func(t *testing.T) {
		got, err := store.PublishVersion(ctx, orgID, listingID, marketplaceConformanceSnapshot("Conformance Listing v2"),
			"Conformance Listing v2", "d2", nil)
		if err != nil {
			t.Fatalf("PublishVersion: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "PublishVersion", got, readListing)
		if got.CurrentVersion != 2 || got.Name != "Conformance Listing v2" {
			t.Errorf("PublishVersion returned %+v, want current_version=2 name=%q", got, "Conformance Listing v2")
		}
	})

	t.Run("Delist_returns_the_stored_row", func(t *testing.T) {
		got, err := store.Delist(ctx, orgID, listingID)
		if err != nil {
			t.Fatalf("Delist: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Delist", got, readListing)
		if got.Status != domain.ListingStatusDelisted || got.DelistedAt == nil {
			t.Errorf("Delist returned %+v, want status=delisted with delisted_at set", got)
		}
	})

	t.Run("Relist_returns_the_stored_row", func(t *testing.T) {
		got, err := store.Relist(ctx, orgID, listingID)
		if err != nil {
			t.Fatalf("Relist: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Relist", got, readListing)
		if got.Status != domain.ListingStatusPublished || got.DelistedAt != nil {
			t.Errorf("Relist returned %+v, want status=published with delisted_at cleared", got)
		}
	})

	t.Run("missing_id_reports_ErrNoSuchListing", func(t *testing.T) {
		const missing = "00000000-0000-0000-0000-0000000000ff"
		if _, err := store.Delist(ctx, orgID, missing); !errors.Is(err, db.ErrNoSuchListing) {
			t.Errorf("Delist on a missing id: got %v, want db.ErrNoSuchListing", err)
		}
		if _, err := store.Relist(ctx, orgID, missing); !errors.Is(err, db.ErrNoSuchListing) {
			t.Errorf("Relist on a missing id: got %v, want db.ErrNoSuchListing", err)
		}
		if _, err := store.PublishVersion(ctx, orgID, missing, marketplaceConformanceSnapshot("ghost"), "ghost", "", nil); !errors.Is(err, db.ErrNoSuchListing) {
			t.Errorf("PublishVersion on a missing id: got %v, want db.ErrNoSuchListing", err)
		}
	})
}
