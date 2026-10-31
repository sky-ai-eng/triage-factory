package credprovision

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/cred/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// fakeSigners answers GetSystem with one org's signer row, or err.
type fakeSigners struct {
	db.OrgCommitSignerStore
	signer *domain.OrgCommitSigner
	err    error
}

func (f fakeSigners) GetSystem(context.Context, string) (*domain.OrgCommitSigner, error) {
	return f.signer, f.err
}

// signingSecrets holds an org's private signing keys, as the brain's secret
// store does, so a provisioner that read one into the bundle would have it to
// read.
type signingSecrets struct {
	db.SecretStore
	keys map[string]string
}

func (s signingSecrets) GetSystem(_ context.Context, _, name string) (string, error) {
	return s.keys[name], nil
}

// signerKeys generates a signer's key pair: the private half in OpenSSH form,
// as the secret store holds it, and the public half the signer row names.
func signerKeys(t *testing.T) (private []byte, public string) {
	t.Helper()
	key, err := commitsign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	private, err = commitsign.MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	public, err = commitsign.PublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return private, public
}

func signerManager(signers db.OrgCommitSignerStore, secrets db.SecretStore) *Manager {
	return &Manager{
		stores: db.Stores{
			Orgs:             &fakeOrgs{},
			TeamGitHubRepos:  &fakeTeamRepos{tracked: map[string]bool{"acme/widgets": true}},
			Tasks:            &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}},
			Entities:         &fakeEntities{scope: dbtest.TestGitHubHost},
			Repos:            fakeRepos{},
			OrgCommitSigners: signers,
			Secrets:          secrets,
		},
		ghResolver: &fakeScopedResolver{
			base:      "https://github.com",
			name:      "acme[bot]",
			email:     "316798692+acme[bot]@users.noreply.github.com",
			accountID: "316798692",
			hasCred:   true,
			token:     githubapp.Token{Value: "ghs_scoped", ExpiresAt: time.Now().Add(time.Hour)},
		},
	}
}

// TestResolveGitHub_ActiveSignerSealsItsPublicHalfOnly: an org whose signer is
// active seals the signer as the run's identity and its public keys as the
// run's signer, and the sealed bundle carries no private key, although the
// secret store beside the provisioner holds both of the signer's.
func TestResolveGitHub_ActiveSignerSealsItsPublicHalfOnly(t *testing.T) {
	private, public := signerKeys(t)
	pendingPrivate, pending := signerKeys(t)
	signer := &domain.OrgCommitSigner{
		Host: ghbase.GitHubCom, GitHubUserID: 4242, Login: "acme-signer", Name: "Acme Agent",
		PublicKey: public, PendingPublicKey: pending, Status: domain.CommitSignerActive,
	}
	m := signerManager(fakeSigners{signer: signer}, signingSecrets{keys: map[string]string{
		integrations.KeyGitHubCommitSigningKey:        string(private),
		integrations.KeyGitHubCommitSigningKeyPending: string(pendingPrivate),
	}})

	gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "conv-1")
	if err != nil {
		t.Fatalf("resolveGitHub: %v", err)
	}
	if gh.IdentityName != "Acme Agent" || gh.IdentityEmail != "4242+acme-signer@users.noreply.github.com" || gh.IdentityAccountID != "4242" {
		t.Errorf("identity = (%q, %q, %q), want the signer's", gh.IdentityName, gh.IdentityEmail, gh.IdentityAccountID)
	}
	want := credbundle.SignerCreds{Mode: string(domain.CommitSigningOrg), PublicKey: public, PendingPublicKey: pending}
	if gh.Signer == nil || *gh.Signer != want {
		t.Fatalf("Signer = %+v, want %+v", gh.Signer, want)
	}

	sealed, err := (&credbundle.Bundle{GitHub: gh}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Error("the bundle carries a private key's armor")
	}
	for _, key := range [][]byte{private, pendingPrivate} {
		// The armored body, line by line: any line of a private key in the
		// bundle is a leak, whatever encoding carried it there.
		for _, line := range strings.Split(string(key), "\n") {
			if len(line) > 20 && !strings.HasPrefix(line, "-----") && bytes.Contains(sealed, []byte(line)) {
				t.Fatalf("the bundle carries a line of a private signing key: %q", line)
			}
		}
	}
}

// TestResolveGitHub_InactiveSignerIsTheIdentityAndDoesNotSign: a pending or
// broken signer still authors the run's commits, unsigned, so the bundle names
// its identity and no signer.
func TestResolveGitHub_InactiveSignerIsTheIdentityAndDoesNotSign(t *testing.T) {
	_, public := signerKeys(t)
	for _, status := range []domain.CommitSignerStatus{domain.CommitSignerPending, domain.CommitSignerBroken} {
		t.Run(string(status), func(t *testing.T) {
			signer := &domain.OrgCommitSigner{
				Host: ghbase.GitHubCom, GitHubUserID: 4242, Login: "acme-signer",
				PublicKey: public, Status: status,
			}
			gh, err := signerManager(fakeSigners{signer: signer}, nil).resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "conv-1")
			if err != nil {
				t.Fatalf("resolveGitHub: %v", err)
			}
			if gh.IdentityName != "acme-signer" || gh.IdentityAccountID != "4242" {
				t.Errorf("identity = (%q, %q), want the signer's login and account", gh.IdentityName, gh.IdentityAccountID)
			}
			if gh.Signer != nil {
				t.Errorf("Signer = %+v, want nil for a %s signer", gh.Signer, status)
			}
		})
	}
}

// TestResolveGitHub_NoSignerSealsTheOrgIdentityUnsigned: an org with no signer
// commits as its GitHub identity, as before signing existed.
func TestResolveGitHub_NoSignerSealsTheOrgIdentityUnsigned(t *testing.T) {
	gh, err := signerManager(fakeSigners{}, nil).resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "conv-1")
	if err != nil {
		t.Fatalf("resolveGitHub: %v", err)
	}
	if gh.IdentityName != "acme[bot]" || gh.IdentityAccountID != "316798692" || gh.Signer != nil {
		t.Errorf("identity = (%q, %q), signer %+v; want the App's bot, unsigned", gh.IdentityName, gh.IdentityAccountID, gh.Signer)
	}
}

// TestResolveGitHub_UnreadableSignerFailsTheSeal: a signer that cannot be read
// fails the seal, rather than sealing the bot the signer would have replaced.
func TestResolveGitHub_UnreadableSignerFailsTheSeal(t *testing.T) {
	readErr := errors.New("signer read failed")
	if _, err := signerManager(fakeSigners{err: readErr}, nil).resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "conv-1"); !errors.Is(err, readErr) {
		t.Fatalf("resolveGitHub = %v, want the signer read's error", err)
	}
}

// fakeDeploymentHealth answers the deployment signer's health row.
type fakeDeploymentHealth struct {
	db.DeploymentCommitSignerStore
	row *domain.DeploymentCommitSigner
}

func (f fakeDeploymentHealth) GetSystem(context.Context, string, string) (*domain.DeploymentCommitSigner, error) {
	return f.row, nil
}

// TestResolveGitHub_ManagedOrgSealsTheDeploymentSigner: an org on
// the deployment App with no signer of its own seals the deployment signer as
// its identity and its public key as the run's signer, and the bundle carries
// no line of the deployment's private key, which this control pod holds.
func TestResolveGitHub_ManagedOrgSealsTheDeploymentSigner(t *testing.T) {
	private, public := signerKeys(t)
	env := map[string]string{
		commitsigner.EnvDeploymentSignerLogin: "TriageFactory",
		commitsigner.EnvDeploymentSignerKey:   string(private),
	}
	d, err := commitsigner.ParseDeploymentSigner(func(k string) string { return env[k] }, func(k string) string { return env[k] }, ghbase.GitHubCom)
	if err != nil {
		t.Fatalf("ParseDeploymentSigner: %v", err)
	}
	for _, status := range []domain.CommitSignerStatus{domain.CommitSignerActive, domain.CommitSignerBroken} {
		t.Run(string(status), func(t *testing.T) {
			m := signerManager(fakeSigners{}, nil)
			m.stores.Orgs = &fakeOrgs{class: domain.GitHubCredentialClassManagedApp}
			m.stores.DeploymentCommitSigners = fakeDeploymentHealth{row: &domain.DeploymentCommitSigner{
				Host: ghbase.GitHubCom, PublicKey: public, GitHubUserID: 90001, Login: "TriageFactory", Name: "Triage Factory", Status: status,
			}}
			m.SetDeploymentSigner(d)

			gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "conv-1")
			if err != nil {
				t.Fatalf("resolveGitHub: %v", err)
			}
			if gh.IdentityName != "Triage Factory" || gh.IdentityEmail != "90001+TriageFactory@users.noreply.github.com" || gh.IdentityAccountID != "90001" {
				t.Errorf("identity = (%q, %q, %q), want the deployment signer's", gh.IdentityName, gh.IdentityEmail, gh.IdentityAccountID)
			}
			if status != domain.CommitSignerActive {
				if gh.Signer != nil {
					t.Errorf("Signer = %+v, want nil for a %s deployment signer", gh.Signer, status)
				}
				return
			}
			want := credbundle.SignerCreds{Mode: string(domain.CommitSigningDeployment), PublicKey: public}
			if gh.Signer == nil || *gh.Signer != want {
				t.Fatalf("Signer = %+v, want %+v", gh.Signer, want)
			}
			sealed, err := (&credbundle.Bundle{GitHub: gh}).Marshal()
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(private), "\n") {
				if len(line) > 20 && !strings.HasPrefix(line, "-----") && bytes.Contains(sealed, []byte(line)) {
					t.Fatalf("the bundle carries a line of the deployment's private key: %q", line)
				}
			}
		})
	}
}
