package commitsigner

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

const (
	testOrg     = "00000000-0000-0000-0000-000000000001"
	activeKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rotationKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeSigners answers GetSystem with one org's row; nothing else is called.
type fakeSigners struct {
	db.OrgCommitSignerStore
	row *domain.OrgCommitSigner
	err error
}

func (f fakeSigners) GetSystem(context.Context, string) (*domain.OrgCommitSigner, error) {
	return f.row, f.err
}

// fakeGitHub answers the identity an org commits as without a signer; nothing
// else is called.
type fakeGitHub struct {
	ghclient.Resolver
	id ghclient.OrgIdentity
	ok bool
}

func (f fakeGitHub) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return f.id.Name, f.id.Email, f.ok
}

func (f fakeGitHub) OrgIdentityAccountFor(context.Context, string) (ghclient.OrgIdentity, bool) {
	return f.id, f.ok
}

func signer(status domain.CommitSignerStatus) *domain.OrgCommitSigner {
	return &domain.OrgCommitSigner{
		OrgID:            testOrg,
		Host:             "https://github.com",
		GitHubUserID:     583231,
		Login:            "octocat",
		Name:             "The Octocat",
		PublicKey:        activeKey,
		PendingPublicKey: rotationKey,
		Status:           status,
	}
}

// TestResolve_EveryCredentialClassAndSignerStatus walks every combination of
// the org's credential class and its signer's status. The class decides who
// the org commits as without a signer (the PAT's owner, the org's own App's
// bot, the deployment App's bot); a signer, in any status, replaces that
// identity in every class, and only an active one signs.
//
// With no deployment signer configured, an org on the deployment App
// with no signer of its own commits as the deployment's bot, unsigned
// (TestResolve_DeploymentSigner covers one configured). An org with its
// own signer, broken included, never resolves past it.
func TestResolve_EveryCredentialClassAndSignerStatus(t *testing.T) {
	classes := map[domain.GitHubCredentialClass]ghclient.OrgIdentity{
		domain.GitHubCredentialClassPAT:        {Name: "pat-owner", Email: "owner@example.com", AccountID: "42"},
		domain.GitHubCredentialClassBYOApp:     {Name: "acme[bot]", Email: "7+acme[bot]@users.noreply.github.com", AccountID: "7"},
		domain.GitHubCredentialClassManagedApp: {Name: "triage-factory[bot]", Email: "9+triage-factory[bot]@users.noreply.github.com", AccountID: "9"},
	}
	signerID := Identity{
		Name:      "The Octocat",
		Email:     "583231+octocat@users.noreply.github.com",
		AccountID: "583231",
		Login:     "octocat",
		Mode:      domain.CommitSigningNone,
	}
	signing := signerID
	signing.Mode = domain.CommitSigningOrg
	signing.Host = "https://github.com"
	signing.PublicKey = activeKey
	signing.PendingPublicKey = rotationKey

	for class, today := range classes {
		withoutSigner := Identity{Name: today.Name, Email: today.Email, AccountID: today.AccountID, Mode: domain.CommitSigningNone}
		for _, tc := range []struct {
			name   string
			signer *domain.OrgCommitSigner
			want   Identity
		}{
			{"no signer", nil, withoutSigner},
			{"pending signer", signer(domain.CommitSignerPending), signerID},
			{"active signer", signer(domain.CommitSignerActive), signing},
			{"broken signer", signer(domain.CommitSignerBroken), signerID},
		} {
			if tc.signer != nil {
				tc.want.Status, tc.want.StatusReason = tc.signer.Status, tc.signer.StatusReason
			}
			t.Run(string(class)+"/"+tc.name, func(t *testing.T) {
				r := Resolver{Signers: fakeSigners{row: tc.signer}, GitHub: fakeGitHub{id: today, ok: true}}
				got, err := r.Resolve(t.Context(), testOrg)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if got != tc.want {
					t.Errorf("Resolve =\n  %+v\nwant\n  %+v", got, tc.want)
				}
			})
		}
	}
}

// TestResolve_WithNoIdentityAtAll pins the change-nothing property for an org
// GitHub resolves no identity for: no signer, nothing to stamp, and still a
// mode the caller can read.
func TestResolve_WithNoIdentityAtAll(t *testing.T) {
	for name, gh := range map[string]ghclient.Resolver{
		"resolver answers none": fakeGitHub{},
		"no resolver":           nil,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Resolver{Signers: fakeSigners{}, GitHub: gh}.Resolve(t.Context(), testOrg)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != (Identity{Mode: domain.CommitSigningNone}) {
				t.Errorf("Resolve = %+v, want no identity and mode none", got)
			}
		})
	}
}

// TestResolve_SignerReadFailureIsAnError pins that a failed read never
// resolves to the identity a signer would have replaced: an org with a signer
// would commit as the bot, unsigned, with nothing saying why.
func TestResolve_SignerReadFailureIsAnError(t *testing.T) {
	boom := errors.New("connection reset")
	r := Resolver{Signers: fakeSigners{err: boom}, GitHub: fakeGitHub{id: ghclient.OrgIdentity{Name: "acme[bot]", Email: "a@b"}, ok: true}}
	if _, err := r.Resolve(t.Context(), testOrg); !errors.Is(err, boom) {
		t.Fatalf("Resolve = %v, want the read's error", err)
	}
}

// TestResolve_SignerNameAndEmail pins how a signer's commits are named and
// addressed: the profile name or else the login, and the override or else, on
// github.com only, the id form of the noreply address.
func TestResolve_SignerNameAndEmail(t *testing.T) {
	bot := fakeGitHub{id: ghclient.OrgIdentity{Name: "acme[bot]", Email: "7+acme[bot]@users.noreply.github.com", AccountID: "7"}, ok: true}
	for _, tc := range []struct {
		name      string
		mutate    func(*domain.OrgCommitSigner)
		wantName  string
		wantEmail string
	}{
		{"profile name and derived address", func(*domain.OrgCommitSigner) {}, "The Octocat", "583231+octocat@users.noreply.github.com"},
		{"no profile name", func(s *domain.OrgCommitSigner) { s.Name = "" }, "octocat", "583231+octocat@users.noreply.github.com"},
		// The name is what git writes for it, so the committer git records
		// is the one the signature check compares against.
		{"profile name git trims", func(s *domain.OrgCommitSigner) { s.Name = `"Acme";` }, "Acme", "583231+octocat@users.noreply.github.com"},
		{"profile name git drops brackets from", func(s *domain.OrgCommitSigner) { s.Name = "Acme <Bots>" }, "Acme Bots", "583231+octocat@users.noreply.github.com"},
		{"profile name git reduces to nothing", func(s *domain.OrgCommitSigner) { s.Name = `"<>"` }, "octocat", "583231+octocat@users.noreply.github.com"},
		{"override on github.com", func(s *domain.OrgCommitSigner) { s.EmailOverride = "agent@acme.example" }, "The Octocat", "agent@acme.example"},
		{"override on GHES", func(s *domain.OrgCommitSigner) {
			s.Host, s.EmailOverride = "https://ghe.acme.example", "agent@acme.example"
		}, "The Octocat", "agent@acme.example"},
		// No noreply address exists off github.com, so a signer there with
		// no address of its own cannot author a commit, and the org commits
		// as it would without a signer.
		{"GHES without an override", func(s *domain.OrgCommitSigner) { s.Host = "https://ghe.acme.example" }, "acme[bot]", "7+acme[bot]@users.noreply.github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := signer(domain.CommitSignerActive)
			tc.mutate(row)
			got, err := Resolver{Signers: fakeSigners{row: row}, GitHub: bot}.Resolve(t.Context(), testOrg)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Name != tc.wantName || got.Email != tc.wantEmail {
				t.Errorf("Resolve = %q <%s>, want %q <%s>", got.Name, got.Email, tc.wantName, tc.wantEmail)
			}
		})
	}
}

// fakeOrgs answers the org's GitHub credential class and host; nothing else
// is called.
type fakeOrgs struct {
	db.OrgsStore
	settings domain.OrgSettings
	err      error
}

func (f fakeOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return f.settings, f.err
}

func managedOrg() fakeOrgs {
	return fakeOrgs{settings: domain.OrgSettings{
		GitHubCredentialClass: domain.GitHubCredentialClassManagedApp,
		GitHubBaseURL:         "https://github.com",
	}}
}

// TestResolve_DeploymentSigner covers an org on the deployment App: with
// no signer of its own it resolves to the deployment signer, which signs while
// it is active and otherwise authors the commits unsigned; with a signer of its
// own, in any status, it resolves to that signer and never to the deployment's.
func TestResolve_DeploymentSigner(t *testing.T) {
	bot := fakeGitHub{id: ghclient.OrgIdentity{Name: "triage-factory[bot]", Email: "9+triage-factory[bot]@users.noreply.github.com", AccountID: "9"}, ok: true}
	asBot := Identity{Name: "triage-factory[bot]", Email: "9+triage-factory[bot]@users.noreply.github.com", AccountID: "9", Mode: domain.CommitSigningNone}
	d := newDeploymentSigner(t)
	deployment := func(status domain.CommitSignerStatus, reason string) Identity {
		id := Identity{
			Name: "Triage Factory", Email: "90001+TriageFactory@users.noreply.github.com", AccountID: "90001",
			Login: "TriageFactory", Mode: domain.CommitSigningNone, Status: status, StatusReason: reason,
		}
		if status == domain.CommitSignerActive {
			id.Mode, id.Host, id.PublicKey = domain.CommitSigningDeployment, "https://github.com", d.PublicKey
		}
		return id
	}
	own := func(status domain.CommitSignerStatus) Identity {
		id := Identity{
			Name: "The Octocat", Email: "583231+octocat@users.noreply.github.com", AccountID: "583231",
			Login: "octocat", Mode: domain.CommitSigningNone, Status: status,
		}
		if status == domain.CommitSignerActive {
			id.Mode, id.Host, id.PublicKey, id.PendingPublicKey = domain.CommitSigningOrg, "https://github.com", activeKey, rotationKey
		}
		return id
	}

	for _, tc := range []struct {
		name   string
		orgs   fakeOrgs
		own    *domain.OrgCommitSigner
		health *domain.DeploymentCommitSigner
		want   Identity
	}{
		{"no signer of its own, deployment signer active", managedOrg(), nil, rowPtr(deploymentRow(d, domain.CommitSignerActive, "")), deployment(domain.CommitSignerActive, "")},
		{"no signer of its own, deployment signer pending", managedOrg(), nil, rowPtr(deploymentRow(d, domain.CommitSignerPending, "")), deployment(domain.CommitSignerPending, "")},
		{"no signer of its own, deployment signer broken", managedOrg(), nil, rowPtr(deploymentRow(d, domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing)),
			deployment(domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing)},
		{"its own active signer", managedOrg(), signer(domain.CommitSignerActive), rowPtr(deploymentRow(d, domain.CommitSignerActive, "")), own(domain.CommitSignerActive)},
		{"its own broken signer never falls back to the deployment's", managedOrg(), signer(domain.CommitSignerBroken), rowPtr(deploymentRow(d, domain.CommitSignerActive, "")), own(domain.CommitSignerBroken)},
		// No check has read the account the login names, so there is no
		// account to commit as yet.
		{"deployment signer with no account yet", managedOrg(), nil, &domain.DeploymentCommitSigner{
			Host: d.Host, PublicKey: d.PublicKey, Login: "TriageFactory", Status: domain.CommitSignerPending,
		}, asBot},
		{"no health row yet", managedOrg(), nil, nil, asBot},
		{"its own App", fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: domain.GitHubCredentialClassBYOApp, GitHubBaseURL: "https://github.com"}},
			nil, rowPtr(deploymentRow(d, domain.CommitSignerActive, "")), asBot},
		{"the deployment App on another GitHub", fakeOrgs{settings: domain.OrgSettings{GitHubCredentialClass: domain.GitHubCredentialClassManagedApp, GitHubBaseURL: "https://ghe.acme.example"}},
			nil, rowPtr(deploymentRow(d, domain.CommitSignerActive, "")), asBot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := newMemDeploymentSigners()
			if tc.health != nil {
				health.set(*tc.health)
			}
			r := Resolver{
				Signers:    fakeSigners{row: tc.own},
				GitHub:     bot,
				Deployment: &DeploymentArm{Signer: d, Health: health, Orgs: tc.orgs},
			}
			got, err := r.Resolve(t.Context(), testOrg)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tc.want {
				t.Errorf("Resolve =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}

	t.Run("a configured email", func(t *testing.T) {
		withEmail := *d
		withEmail.EmailOverride = "agent@triage.example"
		health := newMemDeploymentSigners()
		health.set(deploymentRow(d, domain.CommitSignerActive, ""))
		got, err := Resolver{Signers: fakeSigners{}, Deployment: &DeploymentArm{Signer: &withEmail, Health: health, Orgs: managedOrg()}}.Resolve(t.Context(), testOrg)
		if err != nil || got.Email != "agent@triage.example" {
			t.Errorf("Resolve = %+v, %v; want the configured address", got, err)
		}
	})

	// A failed read never resolves to the bot the deployment signer would
	// have replaced.
	t.Run("read failures are errors", func(t *testing.T) {
		boom := errors.New("connection reset")
		for name, arm := range map[string]*DeploymentArm{
			"org settings": {Signer: d, Health: newMemDeploymentSigners(), Orgs: fakeOrgs{err: boom}},
			"health":       {Signer: d, Health: &memDeploymentSigners{err: boom}, Orgs: managedOrg()},
		} {
			if _, err := (Resolver{Signers: fakeSigners{}, GitHub: bot, Deployment: arm}).Resolve(t.Context(), testOrg); !errors.Is(err, boom) {
				t.Errorf("%s: Resolve = %v, want the read's error", name, err)
			}
		}
	})
}

func rowPtr(r domain.DeploymentCommitSigner) *domain.DeploymentCommitSigner { return &r }
