package sqlite

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// deploymentCommitSignerStore is the SQLite stub for
// db.DeploymentCommitSignerStore. deployment_commit_signer is Postgres only:
// the deployment signer exists only beside the deployment App, which local
// mode never reads (githubapp.DeploymentAppFromEnv), so no code path reaches
// this store. Every method returns ErrNotApplicableInLocal; it is wired to keep
// db.Stores fully populated in both modes.
type deploymentCommitSignerStore struct{}

func newDeploymentCommitSignerStore() db.DeploymentCommitSignerStore {
	return deploymentCommitSignerStore{}
}

var _ db.DeploymentCommitSignerStore = deploymentCommitSignerStore{}

func (deploymentCommitSignerStore) GetSystem(context.Context, string, string) (*domain.DeploymentCommitSigner, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (deploymentCommitSignerStore) RegisterSystem(context.Context, string, string, string) (domain.DeploymentCommitSigner, error) {
	return domain.DeploymentCommitSigner{}, db.ErrNotApplicableInLocal
}

func (deploymentCommitSignerStore) RecordKeyCheckSystem(context.Context, domain.DeploymentCommitSigner, db.DeploymentSignerKeyCheck) (*domain.DeploymentCommitSigner, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (deploymentCommitSignerStore) ActivateSystem(context.Context, string, string, int64, string, string) (domain.DeploymentCommitSigner, error) {
	return domain.DeploymentCommitSigner{}, db.ErrNotApplicableInLocal
}

func (deploymentCommitSignerStore) BreakSystem(context.Context, string, string, string) (*domain.DeploymentCommitSigner, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (deploymentCommitSignerStore) NewestFailedVerificationSystem(context.Context, string, int64) (*domain.CommitSignature, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (deploymentCommitSignerStore) UsageSystem(context.Context, time.Time, time.Time) (db.DeploymentSignerUsage, error) {
	return db.DeploymentSignerUsage{}, db.ErrNotApplicableInLocal
}
