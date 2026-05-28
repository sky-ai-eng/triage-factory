package github

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// ErrNoGitHubCredentials is returned by ClientFor when no credential tier
// resolves for the (org, target) pair — no usable App installation and no
// PAT. Handlers map it to a "GitHub not configured" response.
var ErrNoGitHubCredentials = errors.New("github: no credentials resolved for org")

// Resolver produces an authenticated *Client for a given org + GitHub
// target account, choosing the credential by tier:
//
//	tier 1  the org's own GitHub App installation token for the target
//	        account — short-lived (~1h), repo-scoped, revocable.
//	tier 2  a deployment-default (shared) App installation token. DEFERRED
//	        to SKY-363; the resolver leaves a numbered gap so that ticket
//	        slots in between tiers 1 and 3 without renumbering.
//	tier 3  PAT-borrow: the org's stored github_pat (keychain in local
//	        mode, Vault in multi mode). Identical to the pre-resolver path.
//
// githubTarget is the GitHub account login (org or user) the work concerns —
// e.g. the owner of the repo being acted on. It selects which installation's
// token to mint in tier 1. Empty target = "no specific account"; tier 1
// then only fires when the org has exactly one installation (otherwise it's
// ambiguous and the resolver falls through to PAT).
//
// All store reads use the ...System (claims-free) door: credential
// resolution is a system operation and the orgID is already authorized by
// upstream middleware (request path) or is the trusted local/poll-cycle org.
// This lets the same resolver serve both request handlers and background
// callers.
type Resolver interface {
	ClientFor(ctx context.Context, orgID, githubTarget string) (*Client, error)
}

type resolver struct {
	secrets db.SecretStore
	apps    db.GitHubAppsStore
	orgs    db.OrgsStore
	agents  db.AgentStore
	cache   TokenCache
}

// NewResolver builds a Resolver. A nil cache gets a fresh in-memory one.
func NewResolver(secrets db.SecretStore, apps db.GitHubAppsStore, orgs db.OrgsStore, agents db.AgentStore, cache TokenCache) Resolver {
	if cache == nil {
		cache = NewMemoryTokenCache()
	}
	return &resolver{secrets: secrets, apps: apps, orgs: orgs, agents: agents, cache: cache}
}

func (r *resolver) ClientFor(ctx context.Context, orgID, target string) (*Client, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// Tier 1: the org's own GitHub App installation token. Best-effort —
	// any failure here (no App, no matching installation, mint/PEM error)
	// falls through to PAT rather than propagating, because a working PAT is
	// a legitimate fallback.
	if client, ok := r.tier1AppClient(ctx, orgID, target, base); ok {
		return client, nil
	}

	// Tier 2 (deployment-default shared App) is deferred to SKY-363; it
	// slots in here between tier 1 and tier 3 without renumbering.

	// Tier 3: PAT-borrow. A backend read error here propagates — there's no
	// further fallback, so reporting "not configured" would misattribute a
	// secret-store outage to user misconfiguration.
	client, err := r.tier3PATClient(ctx, orgID, base)
	if err != nil {
		return nil, err
	}
	if client != nil {
		return client, nil
	}

	return nil, fmt.Errorf("%w: org=%s target=%s", ErrNoGitHubCredentials, orgID, target)
}

// githubBaseFor resolves the org's user-facing GitHub base URL (github.com
// or a GHES host). Precedence mirrors the pre-resolver dashboard/repos code:
// org_settings.github_base_url first, then the github_url secret, then the
// public default. The secret fallback is load-bearing for GHES orgs whose
// base predates the org_settings mirror (common in local mode, where the
// keychain holds the URL and the settings column can be empty).
//
// Backend read failures propagate rather than getting papered over: if a
// source that might hold a GHES host can't be read, we must NOT silently
// default to github.com — pairing a real (possibly GHES) PAT with the public
// host would route a tenant credential to the wrong server. Only when both
// sources are definitively readable AND empty do we treat the org as public
// github.com.
func (r *resolver) githubBaseFor(ctx context.Context, orgID string) (string, error) {
	set, setErr := r.orgs.GetSettingsSystem(ctx, orgID)
	if setErr == nil && set.GitHubBaseURL != "" {
		return ResolveBaseURL(set.GitHubBaseURL), nil
	}

	secretURL, secErr := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubURL)
	if secErr != nil {
		return "", fmt.Errorf("resolve github base for org %s: read %s secret: %w", orgID, integrations.KeyGitHubURL, secErr)
	}
	if secretURL != "" {
		return ResolveBaseURL(secretURL), nil
	}

	// Both sources read cleanly and the secret is empty. If the settings
	// read itself failed, we can't be sure the org isn't GHES — refuse to
	// guess github.com.
	if setErr != nil {
		return "", fmt.Errorf("resolve github base for org %s: read settings: %w", orgID, setErr)
	}
	return DefaultBaseURL, nil
}

func (r *resolver) tier1AppClient(ctx context.Context, orgID, target, base string) (*Client, bool) {
	app, err := r.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		log.Printf("[gh-resolver] org=%s: read App registration: %v (skipping tier1)", orgID, err)
		return nil, false
	}
	if app == nil || !app.Active {
		return nil, false
	}

	inst, ok := r.installationFor(ctx, orgID, target)
	if !ok {
		return nil, false
	}

	tok, err := r.installationToken(ctx, orgID, app, inst, base)
	if err != nil {
		// A mint failure on an org that HAS an App is worth a louder log —
		// it usually means a bad/rotated PEM or a revoked installation. Fall
		// through to PAT so the user isn't hard-blocked, but make it visible.
		log.Printf("[gh-resolver] org=%s target=%s: mint installation token (installation=%s): %v (falling back to PAT)", orgID, target, inst.InstallationID, err)
		return nil, false
	}
	log.Printf("[gh-resolver] org=%s target=%q → tier1 App installation=%s account=%s", orgID, target, inst.InstallationID, inst.AccountLogin)
	return NewClient(base, tok.Value), true
}

// installationFor selects the App installation whose account matches target.
// Account logins are case-insensitive on GitHub, so the match is too. An
// empty target only resolves when there's exactly one installation (an
// unambiguous choice); otherwise it returns no match and the caller falls
// through to PAT.
func (r *resolver) installationFor(ctx context.Context, orgID, target string) (domain.OrgGitHubAppInstallation, bool) {
	insts, err := r.apps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		log.Printf("[gh-resolver] org=%s: list installations: %v (skipping tier1)", orgID, err)
		return domain.OrgGitHubAppInstallation{}, false
	}
	if len(insts) == 0 {
		return domain.OrgGitHubAppInstallation{}, false
	}
	if target == "" {
		if len(insts) == 1 {
			return insts[0], true
		}
		return domain.OrgGitHubAppInstallation{}, false
	}
	for _, in := range insts {
		if strings.EqualFold(in.AccountLogin, target) {
			return in, true
		}
	}
	return domain.OrgGitHubAppInstallation{}, false
}

// installationToken returns a cached token for the installation or mints a
// fresh one (signing an App JWT with the org's PEM and exchanging it). The
// mint path is reached only on a cache miss (~once/hour per installation),
// so re-parsing the PEM each time is acceptable.
func (r *resolver) installationToken(ctx context.Context, orgID string, app *domain.OrgGitHubApp, inst domain.OrgGitHubAppInstallation, base string) (githubapp.Token, error) {
	if tok, ok := r.cache.Get(orgID, inst.InstallationID); ok {
		return tok, nil
	}

	pem, err := r.secrets.GetSystem(ctx, orgID, app.PEMRef)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("read app pem: %w", err)
	}
	if pem == "" {
		return githubapp.Token{}, fmt.Errorf("app pem secret %q not found", app.PEMRef)
	}
	key, err := githubapp.ParsePrivateKey([]byte(pem))
	if err != nil {
		return githubapp.Token{}, err
	}
	appID, err := strconv.ParseInt(app.AppID, 10, 64)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("parse app id %q: %w", app.AppID, err)
	}
	installID, err := strconv.ParseInt(inst.InstallationID, 10, 64)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("parse installation id %q: %w", inst.InstallationID, err)
	}
	minter, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID,
		APIBase:    APIBase(base),
	})
	if err != nil {
		return githubapp.Token{}, err
	}
	tok, err := minter.MintInstallationToken(ctx, installID)
	if err != nil {
		return githubapp.Token{}, err
	}
	r.cache.Set(orgID, inst.InstallationID, tok)
	return tok, nil
}

// tier3PATClient builds a PAT-backed client, or (nil, nil) when the org has
// no PAT configured (a genuine "not configured" → caller surfaces
// ErrNoGitHubCredentials). A secret-store read error is returned as an error
// so a transient Vault/DB outage isn't misreported as missing config.
func (r *resolver) tier3PATClient(ctx context.Context, orgID, base string) (*Client, error) {
	pat, err := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubPAT)
	if err != nil {
		return nil, fmt.Errorf("resolve github pat for org %s: %w", orgID, err)
	}
	if pat == "" {
		return nil, nil
	}
	log.Printf("[gh-resolver] org=%s → tier3 PAT user=%s", orgID, r.patBorrowUser(ctx, orgID))
	return NewClient(base, pat), nil
}

// patBorrowUser is a human-readable identity for the tier-3 log line — the
// agents.github_pat_user_id when set (multi mode), else "(unset)". Local
// mode leaves the FK NULL and keeps the PAT in the keychain, so tier 3 reads
// the secret directly and this is purely informational; it must NOT gate
// whether tier 3 fires, or local mode would never borrow its own PAT.
func (r *resolver) patBorrowUser(ctx context.Context, orgID string) string {
	agent, err := r.agents.GetForOrgSystem(ctx, orgID)
	if err != nil || agent == nil || agent.GitHubPATUserID == "" {
		return "(unset)"
	}
	return agent.GitHubPATUserID
}
