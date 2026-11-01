package commitsigner

import (
	"context"
	"strings"
	"testing"
	"time"

	signing "github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner/gitsig"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// signerRow answers GetSystem with row; nothing else is called.
type signerRow struct {
	db.DeploymentCommitSignerStore
	row *domain.DeploymentCommitSigner
}

func (s signerRow) GetSystem(context.Context, string, string) (*domain.DeploymentCommitSigner, error) {
	return s.row, nil
}

// TestStatus pins what `commit-signer status` prints: a key no control pod
// has registered reads pending, with no account and the advice to verify; an
// active signer names its account and what its commits carry, and no advice.
func TestStatus(t *testing.T) {
	key, err := gitsig.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pem, err := gitsig.MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{signing.EnvDeploymentSignerLogin: "TriageFactory", signing.EnvDeploymentSignerKey: string(pem)}
	get := func(name string) string { return env[name] }
	signer, err := signing.ParseDeploymentSigner(get, get, "https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	active := &domain.DeploymentCommitSigner{
		Host: signer.Host, PublicKey: signer.PublicKey, GitHubUserID: 90001, Login: "TriageFactory", Name: "Triage Factory",
		Status: domain.CommitSignerActive, StatusSince: since, CheckedAt: &since,
	}
	for _, tc := range []struct {
		name          string
		row           *domain.DeploymentCommitSigner
		want, notWant []string
	}{
		{"never registered", nil,
			[]string{"login TriageFactory on https://github.com (account not read yet)", "status pending", "checked never",
				"public key " + signer.PublicKey, "run `triagefactory commit-signer verify`"},
			[]string{"commits as"}},
		{"active", active,
			[]string{"(account 90001)", "status active since 2026-10-27 12:00:00 UTC",
				"commits as Triage Factory <90001+TriageFactory@users.noreply.github.com>"},
			[]string{"commit unsigned", "configured login"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			h := &signing.DeploymentHealth{Signer: signer, Stores: db.Stores{DeploymentCommitSigners: signerRow{row: tc.row}}}
			if err := runStatus(t.Context(), &out, h); err != nil {
				t.Fatalf("status: %v", err)
			}
			got := strings.Join(strings.Fields(out.String()), " ")
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Errorf("status = %q, want it to say %q", got, s)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(got, s) {
					t.Errorf("status = %q, want it not to say %q", got, s)
				}
			}
		})
	}
}
