package linear

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
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

// The deployment app's env vars. They are operator config in the same class as
// the Atlassian pair: one Linear app is registered for the whole deployment,
// so it lives in the operator's .env rather than the secret store, which is
// keyed by org. A distributed local binary can ship no shared client secret,
// so local mode never reads them.
const (
	envDeploymentClientID     = "TF_LINEAR_CLIENT_ID"
	envDeploymentClientSecret = "TF_LINEAR_CLIENT_SECRET"
)

// DeploymentOAuthAppFromEnv is the deployment's own Linear OAuth app, the
// second tier of the precedence, read from the environment in multi mode and
// the zero app in local mode. Whitespace is trimmed so a stray newline in .env
// cannot poison the credential.
//
// The pair is all-or-none. One set without the other is logged and read as no
// deployment app rather than failing the boot: a deployment whose orgs bring
// their own apps must still start, and with no deployment app an org that has
// none of its own is offered no install, which fails closed.
func DeploymentOAuthAppFromEnv() OAuthApp {
	if runmode.Current() != runmode.ModeMulti {
		return OAuthApp{}
	}
	app := OAuthApp{
		ClientID:     strings.TrimSpace(os.Getenv(envDeploymentClientID)),
		ClientSecret: strings.TrimSpace(secretenv.Get(envDeploymentClientSecret)),
	}
	if app.Configured() || app == (OAuthApp{}) {
		return app
	}
	missing := envDeploymentClientSecret
	if app.ClientID == "" {
		missing = envDeploymentClientID
	}
	linearLog.Error("the deployment Linear app is partially configured, so the deployment runs without one; set both "+
		envDeploymentClientID+" and "+envDeploymentClientSecret+", or neither",
		"not_set", missing)
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
//
// ResolveClient is the app with clientID among those the org can still
// authenticate as — its own app or the deployment app — whichever tier
// Resolve would pick now. A token is accepted only with the secret of the app
// that minted it, so a caller holding a token another app minted refreshes or
// revokes it with this rather than with Resolve's answer. An app whose
// secret TF no longer holds (an org app since replaced or deleted) is
// ErrNoLinearOAuthApp.
type OAuthAppResolver interface {
	Resolve(ctx context.Context, orgID string) (OAuthApp, OAuthAppSource, error)
	ResolveClient(ctx context.Context, orgID, clientID string) (OAuthApp, error)
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
	own, err := r.orgApp(ctx, orgID)
	if err != nil {
		return OAuthApp{}, SourceNone, err
	}
	if own.Configured() {
		return own, SourceOrgOverride, nil
	}
	if r.deployment.Configured() {
		return r.deployment, SourceDeployment, nil
	}
	return OAuthApp{}, SourceNone, fmt.Errorf("%w: org=%s", ErrNoLinearOAuthApp, orgID)
}

func (r *oauthAppResolver) ResolveClient(ctx context.Context, orgID, clientID string) (OAuthApp, error) {
	own, err := r.orgApp(ctx, orgID)
	if err != nil {
		return OAuthApp{}, err
	}
	for _, app := range []OAuthApp{own, r.deployment} {
		if app.Configured() && clientID != "" && app.ClientID == clientID {
			return app, nil
		}
	}
	return OAuthApp{}, fmt.Errorf("%w: org=%s client_id=%s", ErrNoLinearOAuthApp, orgID, clientID)
}

// orgApp is the org's own app, the zero app when it has none. A failed read
// propagates rather than falling through: a store outage must not quietly
// put the org on the deployment app. A row whose secret reads empty cannot
// authenticate anything, so it reads as no app rather than half a credential.
func (r *oauthAppResolver) orgApp(ctx context.Context, orgID string) (OAuthApp, error) {
	app, err := r.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return OAuthApp{}, fmt.Errorf("read linear oauth app for org %s: %w", orgID, err)
	}
	if app == nil || app.ClientID == "" {
		return OAuthApp{}, nil
	}
	secret, err := r.secrets.GetSystem(ctx, orgID, app.ClientSecretRef)
	if err != nil {
		return OAuthApp{}, fmt.Errorf("read linear oauth client secret for org %s: %w", orgID, err)
	}
	return OAuthApp{ClientID: app.ClientID, ClientSecret: secret}, nil
}
