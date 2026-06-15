package jira

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ErrNoAtlassianOAuthApp is returned by OAuthAppResolver.Resolve when no
// Atlassian OAuth app resolves for the org — no per-org override row and (in
// hosted) no deployment first-party app, or (in local) no user-supplied BYO
// app. Handlers map it to a "no Atlassian app configured" signal; it is the
// expected, non-error state for an org that hasn't set one up, NOT a backend
// failure (those propagate as wrapped errors instead).
var ErrNoAtlassianOAuthApp = errors.New("jira: no atlassian oauth app configured for org")

// Deployment-first-party env vars (hosted mode). These are operator config in
// the same class as the GoTrue signing material — they live in the operator's
// .env, NOT the keychain/vault, because TF registers ONE first-party Atlassian
// app for the whole deployment. A distributed local binary can't ship a shared
// client_secret, so local mode never reads them (FirstPartyOAuthAppFromEnv
// returns the zero app there) and the local user supplies a BYO app instead.
const (
	envAtlassianClientID     = "TF_ATLASSIAN_CLIENT_ID"
	envAtlassianClientSecret = "TF_ATLASSIAN_CLIENT_SECRET"
)

// OAuthApp is a resolved Atlassian OAuth app credential pair — the
// (client_id, client_secret) the authorize/token flow authenticates with.
type OAuthApp struct {
	ClientID     string
	ClientSecret string
}

// Configured reports whether both halves of the credential are present.
func (a OAuthApp) Configured() bool {
	return a.ClientID != "" && a.ClientSecret != ""
}

// FirstPartyOAuthAppFromEnv reads TF's first-party Atlassian OAuth app from
// the deployment env — but ONLY in hosted (multi) mode. In local mode there is
// no first-party default (a shared client_secret can't ship in a distributed
// binary), so this returns the zero app and the resolver falls back to the
// org's own BYO row. Whitespace is trimmed so a stray newline in .env doesn't
// poison the credential.
func FirstPartyOAuthAppFromEnv() OAuthApp {
	if runmode.Current() != runmode.ModeMulti {
		return OAuthApp{}
	}
	return OAuthApp{
		ClientID:     strings.TrimSpace(os.Getenv(envAtlassianClientID)),
		ClientSecret: strings.TrimSpace(os.Getenv(envAtlassianClientSecret)),
	}
}

// OAuthAppResolver resolves the Atlassian OAuth app the per-user Connect flow
// runs against, for a given org. Precedence:
//
//	tier 1  the org's own per-org override (org_jira_apps row + its
//	        client_secret in the secret store).
//	tier 2  the deployment first-party app (hosted only; the zero app in
//	        local).
//	else    ErrNoAtlassianOAuthApp.
//
// This is the credential-resolution half of Cloud per-user OAuth; the
// authorize/callback flow that consumes the resolved app is a separate
// concern. Mirrors internal/github.Resolver in shape (a system operation, so
// the store read uses the claims-free ...System door).
type OAuthAppResolver interface {
	Resolve(ctx context.Context, orgID string) (OAuthApp, error)
}

type oauthAppResolver struct {
	apps       db.JiraAppsStore
	secrets    db.SecretStore
	firstParty OAuthApp // deployment first-party (hosted); the zero app in local
}

// NewOAuthAppResolver builds an OAuthAppResolver. firstParty is the deployment
// first-party app (pass FirstPartyOAuthAppFromEnv() — the zero app in local
// mode, where there is no first-party default).
func NewOAuthAppResolver(apps db.JiraAppsStore, secrets db.SecretStore, firstParty OAuthApp) OAuthAppResolver {
	return &oauthAppResolver{apps: apps, secrets: secrets, firstParty: firstParty}
}

func (r *oauthAppResolver) Resolve(ctx context.Context, orgID string) (OAuthApp, error) {
	// Tier 1: the org's per-org override. A backend read error propagates
	// rather than being misreported as "not configured" — a transient
	// vault/DB outage must not silently fall through to the first-party app.
	app, err := r.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return OAuthApp{}, fmt.Errorf("read jira oauth app for org %s: %w", orgID, err)
	}
	if app != nil && app.ClientID != "" {
		secret, err := r.secrets.GetSystem(ctx, orgID, app.ClientSecretRef)
		if err != nil {
			return OAuthApp{}, fmt.Errorf("read jira oauth client secret for org %s: %w", orgID, err)
		}
		if secret != "" {
			return OAuthApp{ClientID: app.ClientID, ClientSecret: secret}, nil
		}
		// A row exists but its secret read clean-and-empty (the secret was
		// deleted out from under the row, or never landed). Treat the override
		// as not usable and fall through to the first-party app rather than
		// returning a half-credential the flow can't use.
	}

	// Tier 2: the deployment first-party app (hosted only). The zero app in
	// local mode, so this is skipped and the org falls to the not-configured
	// error unless it supplied a BYO row above.
	if r.firstParty.Configured() {
		return r.firstParty, nil
	}

	return OAuthApp{}, fmt.Errorf("%w: org=%s", ErrNoAtlassianOAuthApp, orgID)
}
