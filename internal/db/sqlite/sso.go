package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ssoConnectionStore / ssoDomainStore are the SQLite stubs for the SSO
// stores. SSO is a multi-mode concept (TFAC-425): local mode is N=1 and
// never registers an IdP connection, so every method returns
// ErrNotApplicableInLocal. The stubs exist only to keep the db.Stores
// bundle fully wired in both modes — no local caller ever reaches them (the
// SSO routes 404 in local before touching the store).

type ssoConnectionStore struct{ q queryer }

func newSSOConnectionStore(q, _ queryer) db.SSOConnectionStore {
	return &ssoConnectionStore{q: q}
}

var _ db.SSOConnectionStore = (*ssoConnectionStore)(nil)

func (s *ssoConnectionStore) Create(context.Context, domain.CreateSSOConnectionParams) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) GetByID(context.Context, string, string) (*domain.SSOConnection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) ListByOrg(context.Context, string) ([]domain.SSOConnection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) Update(context.Context, string, string, domain.UpdateSSOConnectionParams) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) Delete(context.Context, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) GetByProviderID(context.Context, string) (*domain.SSOProviderBinding, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) SetEnforced(context.Context, string, string, bool) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) MarkTestedByProviderID(context.Context, string) error {
	return db.ErrNotApplicableInLocal
}

type ssoDomainStore struct{ q queryer }

func newSSODomainStore(q, _ queryer) db.SSODomainStore {
	return &ssoDomainStore{q: q}
}

var _ db.SSODomainStore = (*ssoDomainStore)(nil)

func (s *ssoDomainStore) Create(context.Context, domain.CreateSSODomainParams) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) ListByOrg(context.Context, string) ([]domain.SSODomain, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) GetByID(context.Context, string, string) (*domain.SSODomain, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) SetVerified(context.Context, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) Delete(context.Context, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) GetVerifiedByDomain(context.Context, string) (*domain.SSODomainRoute, error) {
	return nil, db.ErrNotApplicableInLocal
}

// ssoBreakGlassStore is the SQLite stub for the break-glass store.
// SSO enforcement is multi-mode only; local mode (N=1) never enforces, so every
// method returns ErrNotApplicableInLocal — no local caller reaches it (the
// enforcement routes 404 in local).
type ssoBreakGlassStore struct{ q queryer }

func newSSOBreakGlassStore(q, _ queryer) db.SSOBreakGlassStore {
	return &ssoBreakGlassStore{q: q}
}

var _ db.SSOBreakGlassStore = (*ssoBreakGlassStore)(nil)

func (s *ssoBreakGlassStore) List(context.Context, string) ([]domain.SSOBreakGlassPrincipal, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoBreakGlassStore) Add(context.Context, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoBreakGlassStore) RemoveGuarded(context.Context, string, string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}

func (s *ssoBreakGlassStore) SeedOwnerIfEmpty(context.Context, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoBreakGlassStore) IsBreakGlass(context.Context, string, string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}

func (s *ssoBreakGlassStore) ResolveVerifiedEmailMember(context.Context, string, string) (string, error) {
	return "", db.ErrNotApplicableInLocal
}
