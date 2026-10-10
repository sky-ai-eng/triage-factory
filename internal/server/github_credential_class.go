package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// ErrUnknownGitHubCredentialClass is returned by githubCredentialClass when the
// org's stored class is not one this build understands — a value written by a
// newer peer, or a hand-edited row.
//
// It exists so the handlers that branch on the class have something to refuse
// WITH. The failure this guards against is the one the class column was added
// to kill: a site that meets a credential system it doesn't know and quietly
// takes the PAT arm, acting on a credential the org does not have.
var ErrUnknownGitHubCredentialClass = fmt.Errorf("server: unknown github credential class")

// githubCredentialClass reads which credential system the org's GitHub access
// belongs to. It is the handler-side counterpart of the resolver's own class
// read, and the answer to the same question every one of these handlers used to
// answer by asking "is there an org_github_apps row?" — an inference that is
// right today only because PAT is the single rowless shape that exists.
//
// System (claims-free) read, matching the App-registration reads it sits
// alongside: the callers either already hold an authorized orgID from
// middleware, or (the webhook route) are pre-auth and have nothing else.
//
// An unrecognised class comes back as ErrUnknownGitHubCredentialClass rather
// than as a value, so a caller cannot accidentally switch it into a default
// arm; how to answer the request is the caller's decision, and the four sites
// legitimately differ (a picker 500s, a status probe reports "not configured",
// a webhook route 404s).
func (s *Server) githubCredentialClass(ctx context.Context, orgID string) (domain.GitHubCredentialClass, error) {
	if s.orgs == nil {
		return "", fmt.Errorf("read github credential class: orgs store not wired")
	}
	set, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("read github credential class: %w", err)
	}
	if !set.GitHubCredentialClass.Known() {
		return "", fmt.Errorf("%w: org=%s class=%q", ErrUnknownGitHubCredentialClass, orgID, set.GitHubCredentialClass)
	}
	return set.GitHubCredentialClass, nil
}

// errGitHubHostHasCredential refuses moving an org's GitHub host while a GitHub
// credential is connected. A token or an App works only on the GitHub that
// issued it, so the org's host is the only record of which GitHub a credential
// belongs to, and it cannot change under one.
var errGitHubHostHasCredential = errors.New("a GitHub credential only works on the GitHub that issued it — disconnect it before changing the GitHub URL")

// githubCredentialConnected names the GitHub credential org has connected, in
// the words an admin would use to go find it, or "" when it has none: a token,
// its own App registration (staged or active), or a binding to the
// deployment's App. The token is read through tx, so a caller inside a write
// sees the secret store that write sees.
func (s *Server) githubCredentialConnected(ctx context.Context, tx db.TxStores, orgID string) (string, error) {
	creds, err := integrations.Load(ctx, tx.Secrets, orgID)
	if err != nil {
		return "", fmt.Errorf("read github token: %w", err)
	}
	if creds.GitHubPAT != "" {
		return "this workspace's GitHub token", nil
	}
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("read github app registration: %w", err)
	}
	if app != nil {
		return "this workspace's GitHub App", nil
	}
	managed, err := s.managedInstallationsInTheWay(ctx, orgID)
	if err != nil {
		return "", err
	}
	if managed {
		return "the deployment's GitHub App", nil
	}
	return "", nil
}
