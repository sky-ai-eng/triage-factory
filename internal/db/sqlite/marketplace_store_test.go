package sqlite_test

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMarketplaceStore_SQLite_EveryMethodReturnsErrNotApplicableInLocal pins
// the multi-mode-only stub (TFAC-535, mirrors the invites precedent): the
// marketplace lives in the Postgres baseline only, so every SQLite method
// must fail loudly rather than silently no-op against a table that doesn't
// exist locally.
func TestMarketplaceStore_SQLite_EveryMethodReturnsErrNotApplicableInLocal(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	m := stores.Marketplace

	const orgID = runmode.LocalDefaultOrgID
	const listingID = "some-listing"
	const userID = runmode.LocalDefaultUserID

	if _, err := m.Publish(ctx, orgID, domain.MarketplaceListing{}, domain.ListingSnapshot{}); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("Publish = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := m.PublishVersion(ctx, orgID, listingID, domain.ListingSnapshot{}, "name", "desc", nil); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("PublishVersion = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := m.Delist(ctx, orgID, listingID); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("Delist = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := m.Relist(ctx, orgID, listingID); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("Relist = %v, want ErrNotApplicableInLocal", err)
	}
	if _, _, err := m.List(ctx, orgID, userID, domain.ListingFilter{}, db.ListOpts{Limit: 50}); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("List = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := m.Get(ctx, orgID, listingID, userID); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("Get = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := m.GetActiveBySource(ctx, orgID, "some-source"); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("GetActiveBySource = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := m.Vote(ctx, orgID, listingID, userID); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("Vote = %v, want ErrNotApplicableInLocal", err)
	}
	if err := m.Unvote(ctx, orgID, listingID, userID); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("Unvote = %v, want ErrNotApplicableInLocal", err)
	}
	if _, err := m.RecordInstall(ctx, orgID, listingID, 1, runmode.LocalDefaultTeamID, userID, "some-root-object"); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("RecordInstall = %v, want ErrNotApplicableInLocal", err)
	}
	if _, _, err := m.MaterializeListing(ctx, orgID, runmode.LocalDefaultTeamID, domain.ListingSnapshot{}, listingID, 1, userID); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Errorf("MaterializeListing = %v, want ErrNotApplicableInLocal", err)
	}
}
