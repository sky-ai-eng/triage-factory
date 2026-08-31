package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgMembershipsStore is the SQLite impl of db.OrgMembershipsStore. The org
// People roster is a multi-only surface (its handlers 404 in local mode),
// so every method here is multi-only: it exists solely so the local store
// bundle satisfies the interface. All three return ErrNotApplicableInLocal —
// they should never be reached at N=1, and a clear error beats a misleading
// empty roster if some future caller escapes the runmode gate.
type orgMembershipsStore struct{}

func newOrgMembershipsStore() db.OrgMembershipsStore {
	return &orgMembershipsStore{}
}

var _ db.OrgMembershipsStore = (*orgMembershipsStore)(nil)

func (*orgMembershipsStore) ListWithIdentity(_ context.Context, _, _, _ string, _ db.ListOpts) ([]domain.OrgMember, int, error) {
	return nil, 0, db.ErrNotApplicableInLocal
}

// RoleFor is the one method on this store local mode answers rather than
// refusing: N=1 has exactly one user, who owns the org. Every other method
// backs a multi-only surface whose handlers 404 before reaching the store.
func (*orgMembershipsStore) RoleFor(_ context.Context, _, _ string) (string, error) {
	return "owner", nil
}

func (*orgMembershipsStore) UpdateRole(_ context.Context, _, _, _ string) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (*orgMembershipsStore) Remove(_ context.Context, _, _ string) error {
	return db.ErrNotApplicableInLocal
}

func (*orgMembershipsStore) TransferOwnership(_ context.Context, _, _, _ string) error {
	return db.ErrNotApplicableInLocal
}
