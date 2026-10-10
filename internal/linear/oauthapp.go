package linear

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ErrNoLinearOAuthApp is OAuthAppResolver.Resolve finding no Linear OAuth app
// for the org: no app row of its own and no deployment app. It is the expected
// state for an org that has not set one up, not a backend failure; those
// return as wrapped errors of their own.
var ErrNoLinearOAuthApp = errors.New("linear: no oauth app configured for org")

// OAuthApp is a resolved Linear OAuth app's client credentials, what the
// install ceremony and every refresh authenticate with.
type OAuthApp struct {
	ClientID     string
	ClientSecret string
}

// Configured reports whether both halves of the credential are present.
func (a OAuthApp) Configured() bool {
	return a.ClientID != "" && a.ClientSecret != ""
}

// DeploymentOAuthAppFromEnv is the deployment's own Linear OAuth app, the
// second tier of the precedence.
//
// TODO(TFAC-1024): read TF_LINEAR_CLIENT_ID / TF_LINEAR_CLIENT_SECRET in multi
// mode. Until then there is no deployment app, and an org installs only
// through an app of its own.
func DeploymentOAuthAppFromEnv() OAuthApp {
	return OAuthApp{}
}

// OAuthAppSource names which tier of the precedence resolved the app, so the
// settings card reports what resolves rather than the raw row: a row whose
// secret has gone missing resolves to the deployment app, or to nothing.
type OAuthAppSource int

const (
	// SourceNone is nothing resolving (returned with ErrNoLinearOAuthApp).
	SourceNone OAuthAppSource = iota
	// SourceOrgOverride is the org's own app row, its secret present.
	SourceOrgOverride
	// SourceDeployment is the deployment app.
	SourceDeployment
)

// OAuthAppResolver resolves the Linear OAuth app an org's install ceremony and
// refreshes run against. Precedence:
//
//	tier 1  the org's own app (org_linear_apps row + its client secret).
//	tier 2  the deployment app.
//	else    ErrNoLinearOAuthApp.
//
// A system operation: the store reads use the ...System doors, and the orgID
// is already authorized by the caller.
type OAuthAppResolver interface {
	Resolve(ctx context.Context, orgID string) (OAuthApp, OAuthAppSource, error)
}

type oauthAppResolver struct {
	apps       db.LinearAppsStore
	secrets    db.SecretStore
	deployment OAuthApp
}

// NewOAuthAppResolver builds an OAuthAppResolver. deployment is the
// deployment app (DeploymentOAuthAppFromEnv), the zero app when there is none.
func NewOAuthAppResolver(apps db.LinearAppsStore, secrets db.SecretStore, deployment OAuthApp) OAuthAppResolver {
	return &oauthAppResolver{apps: apps, secrets: secrets, deployment: deployment}
}

func (r *oauthAppResolver) Resolve(ctx context.Context, orgID string) (OAuthApp, OAuthAppSource, error) {
	// A failed read propagates rather than falling through: a store outage
	// must not quietly put the org on the deployment app.
	app, err := r.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return OAuthApp{}, SourceNone, fmt.Errorf("read linear oauth app for org %s: %w", orgID, err)
	}
	if app != nil && app.ClientID != "" {
		secret, err := r.secrets.GetSystem(ctx, orgID, app.ClientSecretRef)
		if err != nil {
			return OAuthApp{}, SourceNone, fmt.Errorf("read linear oauth client secret for org %s: %w", orgID, err)
		}
		if secret != "" {
			return OAuthApp{ClientID: app.ClientID, ClientSecret: secret}, SourceOrgOverride, nil
		}
		// A row whose secret reads empty cannot authenticate anything, so the
		// org falls through to the deployment app rather than resolving half
		// a credential.
	}
	if r.deployment.Configured() {
		return r.deployment, SourceDeployment, nil
	}
	return OAuthApp{}, SourceNone, fmt.Errorf("%w: org=%s", ErrNoLinearOAuthApp, orgID)
}
