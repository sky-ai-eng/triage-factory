package server

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// The approval lifecycle is org-executed but human-authorized, and it used to
// file every one of its rows under the GitHub App. In a PAT org that named a
// service account the org does not have — so these pin that the row follows the
// tier the resolver picked, and that an unclassifiable one still reads as the
// App rather than inventing a PAT.

// identityResolver is a ghclient.Resolver that reports a fixed tier. Only the
// identity arm is exercised; the rest panic so a test that starts depending on
// them says so.
type identityResolver struct {
	identity ghclient.Identity
	err      error
}

func (r identityResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	panic("ClientFor is not used by the credential classification")
}

func (r identityResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	panic("ClientForRepo is not used by the credential classification")
}

func (r identityResolver) ClientForRepoWithIdentity(context.Context, string, string, string) (*ghclient.Client, ghclient.Identity, error) {
	if r.err != nil {
		return nil, ghclient.IdentityUnknown, r.err
	}
	return nil, r.identity, nil
}

func (r identityResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	panic("TokenFor is not used by the credential classification")
}

func (r identityResolver) BaseURLFor(context.Context, string) (string, error) {
	panic("BaseURLFor is not used by the credential classification")
}

func (r identityResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	panic("OrgIdentityFor is not used by the credential classification")
}

func TestGithubCredentialFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver ghclient.Resolver
		want     string
	}{
		{"app", identityResolver{identity: ghclient.IdentityApp}, domain.CredentialGitHubApp},
		{"pat", identityResolver{identity: ghclient.IdentityPAT}, domain.CredentialGitHubPAT},
		// A credential backend that just failed, and a tier that never resolved,
		// both keep the historical value rather than asserting a PAT.
		{"resolve failed", identityResolver{err: errors.New("vault down")}, domain.CredentialGitHubApp},
		{"unclassified", identityResolver{identity: ghclient.IdentityUnknown}, domain.CredentialGitHubApp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := githubCredentialFor(t.Context(), tc.resolver, "org", "octo", "repo")
			if got != tc.want {
				t.Errorf("githubCredentialFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGithubCredentialForArtifact pins the artifact-keyed wrapper: it reads the
// repo off the target, and a target it cannot parse names no repo to classify.
func TestGithubCredentialForArtifact(t *testing.T) {
	pat := identityResolver{identity: ghclient.IdentityPAT}
	art := &domain.Artifact{Target: "octo/repo#7"}
	if got := githubCredentialForArtifact(t.Context(), pat, "org", art); got != domain.CredentialGitHubPAT {
		t.Errorf("credential = %q, want github_pat", got)
	}
	malformed := &domain.Artifact{Target: "not-a-pr-target"}
	if got := githubCredentialForArtifact(t.Context(), pat, "org", malformed); got != domain.CredentialGitHubApp {
		t.Errorf("credential for an unparseable target = %q, want the app fallback", got)
	}
}

// TestCredentialForTarget pins the teardown's map lookup, including the miss a
// draft PR opened between the pre-pass and the tx would produce.
func TestCredentialForTarget(t *testing.T) {
	credentials := map[string]string{"octo/repo": domain.CredentialGitHubPAT}
	if got := credentialForTarget(credentials, "octo/repo#3"); got != domain.CredentialGitHubPAT {
		t.Errorf("hit = %q, want github_pat", got)
	}
	if got := credentialForTarget(credentials, "other/repo#3"); got != domain.CredentialGitHubApp {
		t.Errorf("miss = %q, want the app fallback", got)
	}
	if got := credentialForTarget(credentials, "garbage"); got != domain.CredentialGitHubApp {
		t.Errorf("unparseable target = %q, want the app fallback", got)
	}
}
