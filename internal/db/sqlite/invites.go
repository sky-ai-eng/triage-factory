package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// invitesStore is the SQLite stub for db.InvitesStore. Org invites are a
// multi-mode concept (TFAC-416): local mode is N=1 and never mounts the
// invite routes, so every method returns ErrNotApplicableInLocal. The stub
// exists only to keep the db.Stores bundle fully wired in both modes — no
// local caller ever reaches it (the handlers 404 in local before touching
// the store).
type invitesStore struct{ q queryer }

func newInvitesStore(q, _ queryer) db.InvitesStore { return &invitesStore{q: q} }

var _ db.InvitesStore = (*invitesStore)(nil)

func (s *invitesStore) Create(context.Context, domain.CreateInviteParams) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *invitesStore) ListActive(context.Context, string, db.ListOpts) ([]domain.OrgInvite, int, error) {
	return nil, 0, db.ErrNotApplicableInLocal
}

func (s *invitesStore) GetActive(context.Context, string, string) (*domain.OrgInvite, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *invitesStore) Revoke(context.Context, string, string) (db.RevokeOutcome, error) {
	return db.RevokeNotFound, db.ErrNotApplicableInLocal
}

func (s *invitesStore) GetByTokenHashSystem(context.Context, []byte) (*domain.OrgInvite, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *invitesStore) IsOrgMemberSystem(context.Context, string, string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}
