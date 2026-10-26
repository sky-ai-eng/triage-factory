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
// The deployment App row is the deployment arm: a workspace on it with no
// signer of its own commits as the deployment's bot, unsigned. A workspace
// with its own signer, broken included, never resolves past it.
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
