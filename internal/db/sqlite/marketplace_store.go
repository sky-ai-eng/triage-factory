package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// marketplaceStore is the SQLite stub for db.MarketplaceStore. The
// marketplace is a multi-mode concept (TFAC-535): every marketplace_* table
// lives in the Postgres baseline only, so local mode has nothing to store
// against. Every method returns ErrNotApplicableInLocal. The stub exists
// only to keep the db.Stores bundle fully wired in both modes — no local
// caller ever reaches it (the handlers 404 in local before touching the
// store). Mirrors internal/db/sqlite/invites.go.
type marketplaceStore struct{ q queryer }

func newMarketplaceStore(q, _ queryer) db.MarketplaceStore { return &marketplaceStore{q: q} }

var _ db.MarketplaceStore = (*marketplaceStore)(nil)

func (s *marketplaceStore) Publish(context.Context, string, domain.MarketplaceListing, domain.ListingSnapshot) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) PublishVersion(context.Context, string, string, domain.ListingSnapshot, string, string, []string) (domain.MarketplaceListing, error) {
	return domain.MarketplaceListing{}, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) Delist(context.Context, string, string) (domain.MarketplaceListing, error) {
	return domain.MarketplaceListing{}, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) Relist(context.Context, string, string) (domain.MarketplaceListing, error) {
	return domain.MarketplaceListing{}, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) List(context.Context, string, string, domain.ListingFilter, db.ListOpts) ([]domain.ListingSummary, int, error) {
	return nil, 0, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) Get(context.Context, string, string, string) (domain.ListingDetail, error) {
	return domain.ListingDetail{}, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) GetActiveBySource(context.Context, string, string) (*domain.MarketplaceListing, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) GetBySource(context.Context, string, string) (*domain.ListingSummary, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) Vote(context.Context, string, string, string) (domain.ListingSummary, error) {
	return domain.ListingSummary{}, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) Unvote(context.Context, string, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) RecordInstall(context.Context, string, string, int, string, string, string) (domain.ListingSummary, error) {
	return domain.ListingSummary{}, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) MaterializeListing(context.Context, string, string, domain.ListingSnapshot, string, int, string) (string, []string, error) {
	return "", nil, db.ErrNotApplicableInLocal
}

func (s *marketplaceStore) RecomputeStatsSystem(context.Context, string) error {
	return db.ErrNotApplicableInLocal
}
