package reachcache

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrUnknownCredentialClass is returned when the org's stored credential class
// is not one this build understands — a value written by a newer peer, or a
// hand-edited row.
//
// It exists so the callers that key on the class have something to refuse WITH.
// The failure it guards against is the one the class column was added to kill: a
// site that meets a credential system it doesn't know, quietly takes the PAT
// arm, and enumerates against a credential the org does not have.
var ErrUnknownCredentialClass = errors.New("reachcache: unknown github credential class")

// ClassResolver answers which credential class an org's reachable entries are
// keyed under. That is the org's stored class narrowed by the App-XOR-PAT gate
// where the org owns its App key, and the narrowing is not cosmetic:
//
// The resolver's own rule is that an org's git credential is its ACTIVE App or a
// borrowed PAT, never both — so an org in the BYO-App system whose App is
// registered but not yet active (one staged mid PAT→App cutover) reaches
// repositories through its PAT, and its reachable set is that PAT's set. Keying
// those entries 'byo_app' would be false twice over: the drift queries would
// read a PAT's reach as an App's grant, and the table's own CHECK would refuse
// the rows outright, since a byo_app entry must name the installation it came
// through and a PAT entry has none.
//
// One resolver, used by both the refresh that writes the entries and the reads
// that serve them, so writer and reader can never key the same org differently.
type ClassResolver struct {
	orgs db.OrgsStore
	apps db.GitHubAppsStore
}

// NewClassResolver builds a ClassResolver over the org settings (which carry the
// stored class) and the App registrations (which carry the active bit). apps may
// be nil — an org with no App-registration store is a PAT org, which is what a
// deployment without the BYO-App surface wired is.
func NewClassResolver(orgs db.OrgsStore, apps db.GitHubAppsStore) *ClassResolver {
	return &ClassResolver{orgs: orgs, apps: apps}
}

// For resolves the class orgID's reachable entries are keyed under. Both reads
// are system (claims-free) reads: every caller either already holds an
// authorized orgID or is a background pass with no claims at all.
func (c *ClassResolver) For(ctx context.Context, orgID string) (domain.GitHubCredentialClass, error) {
	set, err := c.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("read github credential class: %w", err)
	}
	class := set.GitHubCredentialClass
	switch class {
	case domain.GitHubCredentialClassPAT:
		return class, nil
	case domain.GitHubCredentialClassBYOApp:
		// Falls through to the App-XOR-PAT narrowing below.
	case domain.GitHubCredentialClassManagedApp:
		// Returned as it stands, and the narrowing below is skipped rather than
		// forgotten: it reads an org_github_apps row, and a managed workspace has
		// none — so there is no Active bit that could stage its App behind a PAT.
		// The workspace is on the deployment's App or it is not, and its reach is
		// keyed under that App's installations either way.
		return class, nil
	default:
		return "", fmt.Errorf("%w: org=%s class=%q", ErrUnknownCredentialClass, orgID, class)
	}
	if c.apps == nil {
		// No App-registration store wired at all, so no App can be active here
		// and the narrowing lands on the PAT arm — the same answer a registered
		// but inactive App gets. Defensive: every production bundle wires one.
		return domain.GitHubCredentialClassPAT, nil
	}
	app, err := c.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("read app registration: %w", err)
	}
	if app == nil || !app.Active {
		return domain.GitHubCredentialClassPAT, nil
	}
	return domain.GitHubCredentialClassBYOApp, nil
}
