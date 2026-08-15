package server

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
