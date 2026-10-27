package sqlite_test

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestDeploymentCommitSignerStore_SQLite_NotApplicable pins the local-mode
// posture: there is no deployment App in local mode and so no deployment
// signer, the SQLite schema carries no deployment_commit_signer table, and the
// stub refuses every call rather than answering for a signer that cannot exist.
func TestDeploymentCommitSignerStore_SQLite_NotApplicable(t *testing.T) {
	store := sqlitestore.New(openSQLiteForTest(t)).DeploymentCommitSigners
	ctx := t.Context()
	const host, key = "https://github.com", "ssh-ed25519 AAAA"

	errs := map[string]error{}
	_, errs["GetSystem"] = store.GetSystem(ctx, host, key)
	_, errs["RegisterSystem"] = store.RegisterSystem(ctx, host, key, "TriageFactory")
	_, errs["RecordKeyCheckSystem"] = store.RecordKeyCheckSystem(ctx, domain.DeploymentCommitSigner{Host: host, PublicKey: key}, db.DeploymentSignerKeyCheck{})
	_, errs["ActivateSystem"] = store.ActivateSystem(ctx, host, key, 1, "TriageFactory", "")
	_, errs["BreakSystem"] = store.BreakSystem(ctx, host, key, "no_user")
	_, errs["NewestFailedVerificationSystem"] = store.NewestFailedVerificationSystem(ctx, host, 1)
	for method, err := range errs {
		if !errors.Is(err, db.ErrNotApplicableInLocal) {
			t.Errorf("%s = %v, want ErrNotApplicableInLocal", method, err)
		}
	}
}
