// Package lite holds the SQLite stub implementations of the Enterprise
// Edition SSO stores and registers them for the "sqlite" dialect. SSO is a
// multi-mode concept: local mode is N=1 and never registers an IdP
// connection, so every method returns db.ErrNotApplicableInLocal. The
// stubs exist only so the SSO bundle is fully wired in both modes — no
// local caller ever reaches them (the SSO routes 404 in local before
// touching a store).
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
package lite

import (
	"context"

	ssostore "github.com/sky-ai-eng/triage-factory/ee/sso/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func init() {
	db.RegisterStoreExtension("sqlite", ssostore.ExtKey, func(app, admin db.Execer) any {
		return &ssostore.Bundle{
			Connections: newSSOConnectionStore(app, admin),
			Domains:     newSSODomainStore(app, admin),
			BreakGlass:  newSSOBreakGlassStore(app, admin),
		}
	})
}

type ssoConnectionStore struct{ q db.Execer }

func newSSOConnectionStore(q, _ db.Execer) ssostore.SSOConnectionStore {
	return &ssoConnectionStore{q: q}
}

var _ ssostore.SSOConnectionStore = (*ssoConnectionStore)(nil)

func (s *ssoConnectionStore) Create(context.Context, ssostore.CreateSSOConnectionParams) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) GetByID(context.Context, string, string) (*ssostore.SSOConnection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) ListByOrg(context.Context, string) ([]ssostore.SSOConnection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) Update(context.Context, string, string, ssostore.UpdateSSOConnectionParams) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) Delete(context.Context, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) GetByProviderID(context.Context, string) (*ssostore.SSOProviderBinding, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) SetEnforced(context.Context, string, string, bool) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) MarkTestedByProviderID(context.Context, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoConnectionStore) IdPsByProviderIDs(context.Context, []string) (map[string]string, error) {
	return nil, db.ErrNotApplicableInLocal
}

type ssoDomainStore struct{ q db.Execer }

func newSSODomainStore(q, _ db.Execer) ssostore.SSODomainStore {
	return &ssoDomainStore{q: q}
}

var _ ssostore.SSODomainStore = (*ssoDomainStore)(nil)

func (s *ssoDomainStore) Create(context.Context, ssostore.CreateSSODomainParams) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) ListByOrg(context.Context, string) ([]ssostore.SSODomain, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) GetByID(context.Context, string, string) (*ssostore.SSODomain, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) SetVerified(context.Context, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) Delete(context.Context, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *ssoDomainStore) GetVerifiedByDomain(context.Context, string) (*ssostore.SSODomainRoute, error) {
	return nil, db.ErrNotApplicableInLocal
}

type ssoBreakGlassStore struct{ q db.Execer }

func newSSOBreakGlassStore(q, _ db.Execer) ssostore.SSOBreakGlassStore {
	return &ssoBreakGlassStore{q: q}
}

var _ ssostore.SSOBreakGlassStore = (*ssoBreakGlassStore)(nil)

func (s *ssoBreakGlassStore) List(context.Context, string) ([]ssostore.SSOBreakGlassPrincipal, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *ssoBreakGlassStore) Add(context.Context, string, string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}

func (s *ssoBreakGlassStore) RemoveGuarded(context.Context, string, string) (bool, bool, error) {
	return false, false, db.ErrNotApplicableInLocal
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
