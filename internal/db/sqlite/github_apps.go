package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

type gitHubAppsStore struct{}

func newGitHubAppsStore() db.GitHubAppsStore { return &gitHubAppsStore{} }

var _ db.GitHubAppsStore = (*gitHubAppsStore)(nil)

func (*gitHubAppsStore) GetForOrg(_ context.Context, _ string) (*domain.OrgGitHubApp, error) {
	return nil, nil
}

func (*gitHubAppsStore) CreateForOrg(_ context.Context, _ domain.OrgGitHubApp) error {
	return db.ErrNotApplicableInLocal
}
