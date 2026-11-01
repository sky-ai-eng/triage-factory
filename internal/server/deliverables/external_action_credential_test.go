package deliverables

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
)

// The approval lifecycle is org-executed but human-authorized, so its audit
// rows name the credential tier the resolver picked (filing a PAT org's rows
// under the App would name a service account the org does not have), and an
// unclassifiable tier reads as the App rather than inventing a PAT.

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

func (r identityResolver) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	panic("ClientForInstallation is not used by the credential classification")
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

func TestCredentialForRepo(t *testing.T) {
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
			got := ghclient.CredentialForRepo(t.Context(), tc.resolver, "org", "octo", "repo")
			if got != tc.want {
				t.Errorf("ghclient.CredentialForRepo = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGithubCredentialForPullRequest pins the pull-request-keyed wrapper: it
// classifies the resolved repository, and a pull request that did not resolve,
// or whose repository's row is retired, names no repository to classify.
func TestGithubCredentialForPullRequest(t *testing.T) {
	pat := identityResolver{identity: ghclient.IdentityPAT}
	pr := &db.DeliverablePullRequest{RepositoryID: "repo-1", Owner: "octo", Repo: "repo", Number: 7}
	if got := githubCredentialForPullRequest(t.Context(), pat, "org", pr); got != domain.CredentialGitHubPAT {
		t.Errorf("credential = %q, want github_pat", got)
	}
	if got := githubCredentialForPullRequest(t.Context(), pat, "org", nil); got != domain.CredentialGitHubApp {
		t.Errorf("credential for an unresolved pull request = %q, want the app fallback", got)
	}
	retired := *pr
	retired.Retired = true
	if got := githubCredentialForPullRequest(t.Context(), pat, "org", &retired); got != domain.CredentialGitHubApp {
		t.Errorf("credential for a retired repository = %q, want the app fallback", got)
	}
}
