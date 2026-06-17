package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// ClientForRepo resolves a client for a repo-scoped operation on
	// owner/repo. It differs from ClientFor in that tier 1 (the org's App
	// installation for owner) is chosen only when that installation's grant
	// actually covers owner/repo. A "Selected repositories" install mints a
	// token for any repo under the account — minting is per-installation, not
	// per-repo — so an owner-grain ClientFor would hand back a token that 403s
	// on a repo outside the grant, silently skipping the PAT that would have
	// worked. Deciding coverage up front (a single repo-access probe on the
	// installation token) lets the resolver fall through to tier 3 instead.
	// Genuinely account-grain callers (no single repo in view) keep using
	// ClientFor.
	ClientForRepo(ctx context.Context, orgID, owner, repo string) (*Client, error)

	// TokenFor returns the raw credential ClientFor would authenticate
	// with — the App installation token (tier 1) or the org PAT (tier 3) —
	// for callers that need to hand it to a subprocess rather than make API
	// calls through a *Client. The host-side `git clone` / `git fetch` in
	// internal/worktree is the motivating consumer: it injects the
	// token as an HTTPS auth header on a private-repo clone, and a *Client
	// gives it no way to reach the token (*Client.pat is private).
	//
	// Same tier order and cache as ClientFor, so the clone and the API
	// client share one minted installation token. The tier-1 githubapp.Token
	// carries the real ~1h expiry; the tier-3 PAT is returned as a Token with
	// a zero ExpiresAt (PATs have no mint lifetime we track). Returns
	// ErrNoGitHubCredentials when neither tier resolves.
	TokenFor(ctx context.Context, orgID, githubTarget string) (githubapp.Token, error)

	// BaseURLFor returns the org's user-facing GitHub host base — github.com
	// or a GHES host — resolved from org_settings.github_base_url, then the
	// legacy github_url secret, then the public default. It is the same
	// resolution ClientFor/TokenFor use internally, exposed for callers that
	// must route a subprocess at the org's git host rather than make API
	// calls — the multi-mode sandbox git proxy uses it for its upstream and
	// insteadOf rewrite, so the proxy forwards to (and the rewrite matches)
	// the same host the worktree was cloned from. A backend read error
	// propagates rather than silently defaulting to github.com: a GHES org
	// whose base can't be read must not be paired with the public host.
	BaseURLFor(ctx context.Context, orgID string) (string, error)
}

type resolver struct {
	secrets  db.SecretStore
	apps     db.GitHubAppsStore
	orgs     db.OrgsStore
	agents   db.AgentStore
	cache    TokenCache
	coverage *repoCoverageCache
}

// NewResolver builds a Resolver. A nil cache gets a fresh in-memory one.
func NewResolver(secrets db.SecretStore, apps db.GitHubAppsStore, orgs db.OrgsStore, agents db.AgentStore, cache TokenCache) Resolver {
	if cache == nil {
		cache = NewMemoryTokenCache()
	}
	return &resolver{secrets: secrets, apps: apps, orgs: orgs, agents: agents, cache: cache, coverage: newRepoCoverageCache()}
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

func (r *resolver) ClientForRepo(ctx context.Context, orgID, owner, repo string) (*Client, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// Tier 1, repo-aware: the org's App installation for owner, but only when
	// its grant covers owner/repo (see the ClientForRepo interface doc).
	// Coverage is probed up front rather than via a 403-retry — 403 is
	// ambiguous and the grant is knowable ahead of time. A single
	// GET /repos/{owner}/{repo} on the installation token answers it (200 →
	// covered, 404/403 → not in grant), far cheaper than paginating the whole
	// installation repo set; a positive answer is memoized (repoCoverageTTL) so
	// the per-card dashboard path doesn't re-probe every request. Negatives are
	// deliberately not cached — see repoCoverageCache.
	if client, ok := r.tier1AppClient(ctx, orgID, owner, base); ok {
		if r.coverage.covered(orgID, owner, repo) {
			return client, nil // memoized: in the grant
		}
		reachable, conclusive := client.CheckRepoAccess(ctx, owner, repo)
		if !conclusive {
			// Indeterminate (5xx / transport error) — fail open with the minted
			// App client (the same one owner-grain ClientFor would return) and
			// don't cache, so a transient outage can't pin a wrong answer.
			return client, nil
		}
		if reachable {
			r.coverage.markCovered(orgID, owner, repo)
			return client, nil
		}
		// Conclusively not covered: installed on this account but this repo
		// isn't in the grant. Fall through to the PAT, which may still reach it.
		ghResolverLog.Warn("app installed on account but repo not in grant; falling back to PAT",
			"org", orgID, "owner", owner, "repo", repo)
	}

	// Tier 2 (deployment-default shared App) slots in here when it lands,
	// same as in ClientFor.

	// Tier 3: PAT-borrow. A backend read error propagates — same discipline
	// as ClientFor.
	client, err := r.tier3PATClient(ctx, orgID, base)
	if err != nil {
		return nil, err
	}
	if client != nil {
		return client, nil
	}

	return nil, fmt.Errorf("%w: org=%s repo=%s/%s", ErrNoGitHubCredentials, orgID, owner, repo)
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
	tok, ok := r.tier1Token(ctx, orgID, target, base)
	if !ok {
		return nil, false
	}
	return NewClient(base, tok.Value), true
}

// tier1Token resolves the org's own App installation token for target, or
// (zero, false) when there's no usable App / installation / mintable token —
// in which case the caller falls through to the PAT tier. Shared by
// tier1AppClient (ClientFor) and TokenFor so the API client and the host-side
// git clone resolve identically and hit the same TokenCache.
func (r *resolver) tier1Token(ctx context.Context, orgID, target, base string) (githubapp.Token, bool) {
	app, err := r.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		ghResolverLog.Warn("read app registration failed; skipping tier1",
			"org", orgID, "error", err)
		return githubapp.Token{}, false
	}
	if app == nil || !app.Active {
		return githubapp.Token{}, false
	}

	inst, ok := r.installationFor(ctx, orgID, target)
	if !ok {
		return githubapp.Token{}, false
	}

	tok, err := r.installationToken(ctx, orgID, app, inst, base)
	if err != nil {
		// A mint failure on an org that HAS an App is worth a louder log —
		// it usually means a bad/rotated PEM or a revoked installation. Fall
		// through to PAT so the user isn't hard-blocked, but make it visible.
		ghResolverLog.Warn("mint installation token failed; falling back to PAT",
			"org", orgID, "target", target, "installation", inst.InstallationID, "error", err)
		return githubapp.Token{}, false
	}
	ghResolverLog.Debug("resolved tier1 app installation",
		"org", orgID, "target", target, "installation", inst.InstallationID, "account", inst.AccountLogin)
	return tok, true
}

// TokenFor mirrors ClientFor's tier resolution but returns the raw
// credential instead of a *Client — see the Resolver interface doc for why
// (the host-side git clone needs the token as an HTTPS auth header).
func (r *resolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return githubapp.Token{}, err
	}

	// Tier 1: the org's own App installation token (shared cache + mint
	// path with ClientFor). Best-effort — any failure falls through to PAT.
	if tok, ok := r.tier1Token(ctx, orgID, target, base); ok {
		return tok, nil
	}

	// Tier 2 (deployment-default shared App) is deferred to SKY-363.

	// Tier 3: PAT-borrow, returned as a Token with no mint expiry. A backend
	// read error propagates rather than being misreported as "not configured".
	pat, err := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubPAT)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("resolve github pat for org %s: %w", orgID, err)
	}
	if pat != "" {
		// patBorrowUser does a DB read purely to label this trace line, so
		// skip it unless Debug is actually being emitted.
		if ghResolverLog.Enabled(ctx, slog.LevelDebug) {
			ghResolverLog.Debug("resolved tier3 pat",
				"org", orgID, "target", target, "user", r.patBorrowUser(ctx, orgID))
		}
		return githubapp.Token{Value: pat}, nil
	}

	return githubapp.Token{}, fmt.Errorf("%w: org=%s target=%s", ErrNoGitHubCredentials, orgID, target)
}

// BaseURLFor exposes githubBaseFor to callers outside the package that need
// the org's git host base (the multi-mode sandbox git proxy's upstream).
// Same resolution as ClientFor/TokenFor — org_settings, then the github_url
// secret, then github.com — so the proxy routes to the host the clone used.
func (r *resolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return r.githubBaseFor(ctx, orgID)
}

// installationFor selects the App installation whose account matches target.
// Account logins are case-insensitive on GitHub, so the match is too. An
// empty target only resolves when there's exactly one installation (an
// unambiguous choice); otherwise it returns no match and the caller falls
// through to PAT.
func (r *resolver) installationFor(ctx context.Context, orgID, target string) (domain.OrgGitHubAppInstallation, bool) {
	insts, err := r.apps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		ghResolverLog.Warn("list installations failed; skipping tier1",
			"org", orgID, "error", err)
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
