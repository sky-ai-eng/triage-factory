package commitsigner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// memDeploymentSigners is deployment_commit_signer over a map, for the tests
// here that run on SQLite, which has no such table. Its transitions are the
// Postgres store's, which internal/db/postgres's tests pin against the real
// table. NewestFailedVerificationSystem answers failed, which a test sets.
type memDeploymentSigners struct {
	mu     sync.Mutex
	rows   map[string]domain.DeploymentCommitSigner
	failed *domain.CommitSignature
	// err, when set, is every call's answer.
	err error
}

var _ db.DeploymentCommitSignerStore = (*memDeploymentSigners)(nil)

func newMemDeploymentSigners() *memDeploymentSigners {
	return &memDeploymentSigners{rows: map[string]domain.DeploymentCommitSigner{}}
}

func memKey(host, publicKey string) string { return db.NormalizeGitHubHost(host) + " " + publicKey }

func (m *memDeploymentSigners) GetSystem(_ context.Context, host, publicKey string) (*domain.DeploymentCommitSigner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	row, ok := m.rows[memKey(host, publicKey)]
	if !ok {
		return nil, nil
	}
	return &row, nil
}

func (m *memDeploymentSigners) RegisterSystem(_ context.Context, host, publicKey, login string) (domain.DeploymentCommitSigner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.DeploymentCommitSigner{}, m.err
	}
	k := memKey(host, publicKey)
	if row, ok := m.rows[k]; ok {
		return row, nil
	}
	now := time.Now().UTC()
	row := domain.DeploymentCommitSigner{
		Host: db.NormalizeGitHubHost(host), PublicKey: publicKey, Login: login,
		Status: domain.CommitSignerPending, StatusSince: now, CreatedAt: now, UpdatedAt: now,
	}
	m.rows[k] = row
	return row, nil
}

func (m *memDeploymentSigners) RecordKeyCheckSystem(_ context.Context, before domain.DeploymentCommitSigner, check db.DeploymentSignerKeyCheck) (*domain.DeploymentCommitSigner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	k := memKey(before.Host, before.PublicKey)
	row, ok := m.rows[k]
	if !ok || row.Status != before.Status || row.StatusReason != before.StatusReason ||
		(check.GitHubUserID > 0 && row.GitHubUserID > 0 && row.GitHubUserID != check.GitHubUserID) {
		return nil, nil
	}
	now := time.Now().UTC()
	if check.GitHubUserID > 0 {
		row.GitHubUserID, row.Login, row.Name = check.GitHubUserID, check.Login, check.Name
	}
	status, reason := db.DeploymentKeyCheckTransition(row.Status, row.StatusReason, check.KeyFound)
	if status != row.Status {
		row.StatusSince = now
	}
	row.Status, row.StatusReason, row.CheckedAt = status, reason, &now
	m.rows[k] = row
	return &row, nil
}

func (m *memDeploymentSigners) ActivateSystem(_ context.Context, host, publicKey string, userID int64, login, name string) (domain.DeploymentCommitSigner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.DeploymentCommitSigner{}, m.err
	}
	k := memKey(host, publicKey)
	row, ok := m.rows[k]
	if !ok || (row.GitHubUserID > 0 && row.GitHubUserID != userID) {
		return domain.DeploymentCommitSigner{}, db.ErrNoSuchDeploymentCommitSigner
	}
	now := time.Now().UTC()
	if row.Status != domain.CommitSignerActive {
		row.StatusSince = now
	}
	row.GitHubUserID, row.Login, row.Name = userID, login, name
	row.Status, row.StatusReason, row.CheckedAt = domain.CommitSignerActive, "", &now
	m.rows[k] = row
	return row, nil
}

func (m *memDeploymentSigners) BreakSystem(_ context.Context, host, publicKey, reason string) (*domain.DeploymentCommitSigner, error) {
	if err := db.ValidateBreakReason(reason); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	k := memKey(host, publicKey)
	row, ok := m.rows[k]
	takes := row.Status == domain.CommitSignerActive ||
		(row.Status == domain.CommitSignerBroken && row.StatusReason == domain.CommitSignerReasonKeyMissing && reason != domain.CommitVerificationUnknownKey)
	if !ok || !takes {
		return nil, nil
	}
	if row.Status != domain.CommitSignerBroken {
		row.StatusSince = time.Now().UTC()
	}
	row.Status, row.StatusReason = domain.CommitSignerBroken, reason
	m.rows[k] = row
	return &row, nil
}

func (m *memDeploymentSigners) NewestFailedVerificationSystem(context.Context, string, int64) (*domain.CommitSignature, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failed, m.err
}

// set stores row as the signer's health, keyed by its host and key.
func (m *memDeploymentSigners) set(row domain.DeploymentCommitSigner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[memKey(row.Host, row.PublicKey)] = row
}

const deploymentAccount int64 = 90001

// newDeploymentSigner is a deployment signer on github.com with a fresh key,
// as a control pod's config builds it.
func newDeploymentSigner(t *testing.T) *DeploymentSigner {
	t.Helper()
	key, public := newKey(t)
	return &DeploymentSigner{Host: "https://github.com", Login: "TriageFactory", PublicKey: public, key: key}
}

// deploymentRow is the health row of d's key, in status, with the account a
// check has read.
func deploymentRow(d *DeploymentSigner, status domain.CommitSignerStatus, reason string) domain.DeploymentCommitSigner {
	return domain.DeploymentCommitSigner{
		Host: d.Host, PublicKey: d.PublicKey, GitHubUserID: deploymentAccount,
		Login: "TriageFactory", Name: "Triage Factory", Status: status, StatusReason: reason,
	}
}
