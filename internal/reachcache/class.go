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

// ErrClassNotMirrored is returned for a credential class the mirror has no
// place to key entries under. It is a different fault from the one above: the
// class is one this build knows, and the mirror still cannot hold it.
//
// reachable_repositories.credential_class is CHECK-constrained to 'pat' and
// 'byo_app' on both dialects, and each value names the scope column its rows
// must carry — an installation for one, a host for the other. So a workspace on
// the deployment's shared App has nowhere for its reachable entries to live,
// which is not the same as having none yet.
//
// Refusing is the same posture as the unknown arm beside it, for the same
// reason: the alternative is keying a managed workspace's reach under another
// credential system's key, and every read filters on the class — so those rows
// would be served to, or as, a credential system that did not observe them.
var ErrClassNotMirrored = errors.New("reachcache: credential class has no reachable-repo scope")

// ClassResolver answers which credential class an org's reachable entries are
// keyed under. That is the org's stored class NARROWED BY THE APP-XOR-PAT GATE,
// and the narrowing is not cosmetic:
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
		return "", fmt.Errorf("%w: org=%s class=%q", ErrClassNotMirrored, orgID, class)
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
