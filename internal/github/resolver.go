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

// Identity classifies which credential tier a resolver call selected: a GitHub
// App installation token (a bot / service-account identity acting as itself)
// or a borrowed PAT (a real user's personal access token, lent to the org).
//
// The motivating consumer is the pending-review collision check (the agenthost
// GitHub bridge, TFAC-469): an existing pending review on a PR is safe for the
// bot to reuse only when the acting identity is the App (the review is the
// bot's own from a prior run); under a borrowed PAT the review might be a
// human's in-progress work, which must never be hijacked. The distinction is
// knowable only host-side, where the token resolves — hence it rides out of the
// resolver rather than being re-derived from the opaque bearer token (an App
// installation token and a PAT are indistinguishable strings to the client).
type Identity int

const (
	// IdentityUnknown is the zero value: no credential resolved, or the
	// resolver doesn't report identity. Callers that branch on identity for a
	// safety decision must treat it as the conservative case.
	IdentityUnknown Identity = iota
	// IdentityApp is a tier-1 GitHub App installation token — a service
	// account / bot acting as itself.
	IdentityApp
	// IdentityPAT is a tier-3 PAT-borrow — a real user's personal access token.
	IdentityPAT
)

// RepoIdentityResolver is the optional extension of Resolver, implemented by
// the production resolver, that reports the Identity (App installation vs
// borrowed PAT) of the credential ClientForRepo would resolve — in the same
// pass that builds the client, so the App-coverage probe runs once. A caller
// that must both make API calls AND branch on which credential is acting (the
// agenthost pending-review collision check) type-asserts for this; a Resolver
// that doesn't implement it leaves the caller to fall back to ClientForRepo and
// IdentityUnknown.
type RepoIdentityResolver interface {
	ClientForRepoWithIdentity(ctx context.Context, orgID, owner, repo string) (*Client, Identity, error)
}

// ScopedResolver is the optional extension, implemented by the production
// resolver, that mints a per-repo down-scoped credential and probes overall
// credential presence. Callers that need a least-privilege token narrowed to
// one repo (the multi-mode git proxy's TokenSource) type-assert for it; an
// implementation that doesn't satisfy it (a test fake) leaves the caller to
// disable the per-repo path. Kept off the base Resolver so the many existing
// Resolver fakes don't have to grow methods they never exercise.
type ScopedResolver interface {
	// TokenForRepoScoped mints a credential narrowed to exactly owner/repo
	// (and, when permissions is non-empty, that permission subset). App tier
	// only narrows — a PAT is returned unchanged (it cannot be scoped). See
	// the impl for the tier order and the 422-fall-through to PAT.
	TokenForRepoScoped(ctx context.Context, orgID, owner, repo string, permissions map[string]string) (githubapp.Token, error)
	// TokenForReposScoped mints ONE credential narrowed to the given set of
	// repos under a single owner (bare repo names, all under owner's
	// installation) — the real-gh channel's single team-set-scoped token. App
	// tier mints one installation token over the whole list; a PAT is returned
	// unscoped (it cannot be narrowed). repos beyond the installation's grant
	// 422 exactly like the single-repo path. Same App-XOR-PAT tiering as
	// TokenForRepoScoped.
	TokenForReposScoped(ctx context.Context, orgID, owner string, repos []string, permissions map[string]string) (githubapp.Token, error)
	// HasAnyCredential reports whether the org has ANY usable GitHub
	// credential (an App installation or a PAT) WITHOUT minting a repo-scoped
	// token — the run-start probe that decides whether a run gets a git proxy
	// at all (a Jira-only org with no GitHub credential gets none).
	HasAnyCredential(ctx context.Context, orgID string) (bool, error)
}

// ScopedRepoResolver is the optional extension that resolves a *Client backed
// by a per-repo down-scoped token (the multi-mode exec-gh channel). Separate
// from ScopedResolver because the gh channel wants a ready client + identity,
// not a raw token; a resolver that doesn't implement it falls back to the
// unscoped ClientForRepo path.
type ScopedRepoResolver interface {
	ClientForRepoScoped(ctx context.Context, orgID, owner, repo string, permissions map[string]string) (*Client, Identity, error)
}

// Resolver produces an authenticated *Client for a given org + GitHub
// target account, choosing the credential by tier:
//
//	tier 1  the org's own GitHub App installation token for the target
//	        account — short-lived (~1h), repo-scoped, revocable.
//	tier 2  a deployment-default (shared) App installation token. DEFERRED;
//	        the resolver leaves a numbered gap so that a future tier
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

	// OrgIdentityFor resolves the org's single GitHub commit identity — the
	// author + committer NAME and EMAIL stamped on every delegated-agent commit
	// (TFAC-452, TFAC-474). The App-vs-PAT email-form decision lives here, where
	// the data is. Two tiers, App-preferred to mirror ClientFor's own order:
	//
	//	App  → name "<slug>[bot]" (the org's App registration slug, resolved
	//	       live — installation-independent, one global bot identity). The
	//	       email is the numeric-id noreply form
	//	       "<bot_user_id>+<slug>[bot]@users.noreply.github.com" when the bot
	//	       user id is stored (org_github_apps.bot_user_id) — the only form
	//	       that links a bot's commits to its account on github.com — and falls
	//	       back to the plain "<slug>[bot]@users.noreply.github.com" when the id
	//	       is unknown (an App registered before TFAC-474, or a fetch miss;
	//	       self-heals on re-register). DB-only: no per-run GitHub API call.
	//	PAT  → name <agents.github_org_login> (the login the org PAT
	//	       authenticates as), email "<login>@users.noreply.github.com" — the
	//	       plain form, which links for a user account (the numeric form is
	//	       App-only). Returned ONLY while the org still has a PAT: the cached
	//	       login is gated on the same KeyGitHubPAT presence the credential
	//	       resolver uses, so a value left behind when the PAT was cleared can't
	//	       resurface as a stale identity for an org that no longer has it.
	//
	// ok=false when neither resolves — no App, or no live PAT / no stored PAT
	// login (an org bound before TFAC-452, or a read error). The caller then
	// leaves git identity unset and the agent inherits ambient config, never a
	// fabricated identity; it self-heals on the next PAT re-save / App re-register.
	OrgIdentityFor(ctx context.Context, orgID string) (name, email string, ok bool)
}

type resolver struct {
	secrets    db.SecretStore
	apps       db.GitHubAppsStore
	orgs       db.OrgsStore
	agents     db.AgentStore
	cache      TokenCache
	coverage   *repoCoverageCache
	rateLimits *rateLimitRegistry
}

// NewResolver builds a Resolver. A nil cache gets a fresh in-memory one.
// Each instance owns its own rate-limit registry (see RateLimitReader) —
// process-local, in-memory state, so there's nothing for a caller to
// inject or share.
func NewResolver(secrets db.SecretStore, apps db.GitHubAppsStore, orgs db.OrgsStore, agents db.AgentStore, cache TokenCache) Resolver {
	if cache == nil {
		cache = NewMemoryTokenCache()
	}
	return &resolver{secrets: secrets, apps: apps, orgs: orgs, agents: agents, cache: cache, coverage: newRepoCoverageCache(), rateLimits: newRateLimitRegistry()}
}

// The production resolver implements the optional per-repo-scoping extensions
// the multi-mode git proxy + exec-gh channel type-assert for. Pinned here so a
// dropped method is a compile error rather than a silent runtime fall-through
// to the unscoped path.
var (
	_ ScopedResolver     = (*resolver)(nil)
	_ ScopedRepoResolver = (*resolver)(nil)
	_ RateLimitReader    = (*resolver)(nil)
)

// RateLimitFor implements RateLimitReader, reading this resolver's
// process-wide, per-org registry (see newObservedClient).
func (r *resolver) RateLimitFor(orgID string) (RateLimitState, bool) {
	return r.rateLimits.get(orgID)
}

// newObservedClient is the single *Client construction path every
// resolver entry point (ClientFor, ClientForRepoWithIdentity,
// ClientForRepoScoped, tier3PATClient) uses, so every credential tier —
// App installation token or PAT — feeds the same per-org rate-limit
// registry without each call site remembering to wire it.
func (r *resolver) newObservedClient(orgID, base, token string) *Client {
	c := NewClient(base, token)
	c.SetRateLimitObserver(func(s RateLimitState) {
		r.rateLimits.record(orgID, s)
	})
	return c
}

func (r *resolver) ClientFor(ctx context.Context, orgID, target string) (*Client, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// App XOR PAT (see activeApp): an active App is the org's credential — mint
	// its installation token for target or fail; never borrow a PAT.
	if app := r.activeApp(ctx, orgID); app != nil {
		tok, terr := r.appToken(ctx, orgID, app, target, base)
		if terr != nil {
			return nil, terr
		}
		return r.newObservedClient(orgID, base, tok.Value), nil
	}

	// No active App → PAT-borrow. A backend read error propagates — there's no
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
	client, _, err := r.ClientForRepoWithIdentity(ctx, orgID, owner, repo)
	return client, err
}

// ClientForRepoWithIdentity is ClientForRepo plus the Identity of the
// credential it resolved (see RepoIdentityResolver). The credential decision IS
// the identity — an active App covering the repo → IdentityApp, a PAT-borrow →
// IdentityPAT — so reporting it costs nothing extra and the caller gets a client
// and an identity guaranteed to describe the same credential. ClientForRepo
// delegates here and discards the identity.
//
// App XOR PAT (see activeApp): an active App is resolved repo-aware (gated on
// coverage) or fails; only an org with no active App reaches the PAT. So
// IdentityApp/IdentityPAT also track which credential the org has, not a
// per-repo fallback choice.
func (r *resolver) ClientForRepoWithIdentity(ctx context.Context, orgID, owner, repo string) (*Client, Identity, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return nil, IdentityUnknown, err
	}

	// Active App → the App client for owner/repo (gated on coverage), or an
	// error. Never a PAT.
	if app := r.activeApp(ctx, orgID); app != nil {
		return r.appClientForRepo(ctx, orgID, app, owner, repo, base)
	}

	// No active App → PAT-borrow. A backend read error propagates — same
	// discipline as ClientFor.
	client, err := r.tier3PATClient(ctx, orgID, base)
	if err != nil {
		return nil, IdentityUnknown, err
	}
	if client != nil {
		return client, IdentityPAT, nil
	}

	return nil, IdentityUnknown, fmt.Errorf("%w: org=%s repo=%s/%s", ErrNoGitHubCredentials, orgID, owner, repo)
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

// activeApp returns the org's registered App when it is active (the org's git
// credential is an App), else nil. A read error is treated as "no active App"
// so resolution can still try the PAT tier — the same best-effort posture the
// resolver has always taken on the App read.
//
// This is the App-XOR-PAT gate. An org's git credential is its active App OR a
// borrowed PAT, never both stably: the migration flow deletes the PAT at App
// cutover and tears down the App on switch-to-PAT, so "active App + usable PAT"
// is only ever a transient anomaly (a half-finished cutover). So when an active
// App is configured it is the ONLY credential path — every resolver entry point
// mints from it or fails, and NONE falls through to a PAT (which for an
// active-App org would mean a decommissioning, unscoped credential). The PAT
// tier is reached only when there is no active App.
func (r *resolver) activeApp(ctx context.Context, orgID string) *domain.OrgGitHubApp {
	app, err := r.apps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		ghResolverLog.Warn("read app registration failed; treating org as no-active-App (PAT tier)",
			"org", orgID, "error", err)
		return nil
	}
	if app == nil || !app.Active {
		return nil
	}
	return app
}

// appToken resolves the org's App installation token for target (the account
// the work concerns), or an error — NO PAT fallback (the caller determined via
// activeApp that the org's credential is the App). Shares the installation
// TokenCache with ClientFor so the API client and the host-side git clone
// resolve identically.
func (r *resolver) appToken(ctx context.Context, orgID string, app *domain.OrgGitHubApp, target, base string) (githubapp.Token, error) {
	inst, ok := r.installationFor(ctx, orgID, target)
	if !ok {
		return githubapp.Token{}, fmt.Errorf("%w: org=%s: app has no unambiguous installation for %q", ErrNoGitHubCredentials, orgID, target)
	}
	tok, err := r.installationToken(ctx, orgID, app, inst, base)
	if err != nil {
		// A mint failure on an active-App org is a hard error (no PAT to fall
		// to) — usually a bad/rotated PEM or a revoked installation.
		ghResolverLog.Warn("mint installation token failed",
			"org", orgID, "target", target, "installation", inst.InstallationID, "error", err)
		return githubapp.Token{}, err
	}
	return tok, nil
}

// appClientForRepo resolves the App client for owner/repo, gated on coverage —
// or an error. No PAT fallback: a conclusive not-covered is surfaced (the App
// is the org's credential; a repo it can't reach is a misconfiguration to fix,
// not paper over). Coverage is probed once (a single GET /repos/{owner}/{repo}
// on the installation token) and a positive answer memoized (repoCoverageTTL);
// an indeterminate probe (5xx) fails open with the App client and is not cached;
// a negative is not cached, so a repo newly added to the grant is picked up on
// the next call.
func (r *resolver) appClientForRepo(ctx context.Context, orgID string, app *domain.OrgGitHubApp, owner, repo, base string) (*Client, Identity, error) {
	tok, err := r.appToken(ctx, orgID, app, owner, base)
	if err != nil {
		return nil, IdentityUnknown, err
	}
	client := r.newObservedClient(orgID, base, tok.Value)
	if r.coverage.covered(orgID, owner, repo) {
		return client, IdentityApp, nil // memoized: in the grant
	}
	reachable, conclusive := client.CheckRepoAccess(ctx, owner, repo)
	if !conclusive {
		return client, IdentityApp, nil // indeterminate (5xx) → fail open, don't cache
	}
	if reachable {
		r.coverage.markCovered(orgID, owner, repo)
		return client, IdentityApp, nil
	}
	ghResolverLog.Warn("app installed on account but repo not in its grant; no PAT fallback (app-xor-pat)",
		"org", orgID, "owner", owner, "repo", repo)
	return nil, IdentityUnknown, fmt.Errorf("%w: org=%s repo=%s/%s not in the app installation grant", ErrNoGitHubCredentials, orgID, owner, repo)
}

// appScopedToken mints a per-repo down-scoped App installation token, or an
// error — NO PAT fallback (the caller determined via activeApp that the org's
// credential is the App). Unlike appToken it bypasses the installation
// TokenCache: that cache is keyed by installation only, so a scoped token
// written there would later be served to an unscoped caller. A scoped mint is
// ~once per (run, repo); the gitproxy keeps its own per-repo cache.
func (r *resolver) appScopedToken(ctx context.Context, orgID string, app *domain.OrgGitHubApp, owner, repo, base string, permissions map[string]string) (githubapp.Token, error) {
	inst, ok := r.installationFor(ctx, orgID, owner)
	if !ok {
		return githubapp.Token{}, fmt.Errorf("%w: org=%s: app has no installation for %q", ErrNoGitHubCredentials, orgID, owner)
	}
	minter, err := r.minterFor(ctx, orgID, app, base)
	if err != nil {
		return githubapp.Token{}, err
	}
	installID, err := strconv.ParseInt(inst.InstallationID, 10, 64)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("parse installation id %q: %w", inst.InstallationID, err)
	}
	return minter.MintScopedInstallationToken(ctx, installID, []string{repo}, permissions)
}

// TokenFor mirrors ClientFor's credential resolution but returns the raw
// credential instead of a *Client — see the Resolver interface doc for why
// (the host-side git clone needs the token as an HTTPS auth header).
func (r *resolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return githubapp.Token{}, err
	}

	// App XOR PAT (see activeApp): an active App is the org's credential —
	// resolve its installation token (shared cache + mint path with ClientFor)
	// or fail; never fall to a PAT.
	if app := r.activeApp(ctx, orgID); app != nil {
		return r.appToken(ctx, orgID, app, target, base)
	}

	// No active App → PAT-borrow, returned as a Token with no mint expiry. A
	// backend read error propagates rather than being misreported as "not
	// configured".
	pat, err := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubPAT)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("resolve github pat for org %s: %w", orgID, err)
	}
	if pat != "" {
		// patBorrowUser does a DB read purely to label this trace line, so
		// skip it unless Debug is actually being emitted.
		if ghResolverLog.Enabled(ctx, slog.LevelDebug) {
			ghResolverLog.Debug("resolved pat",
				"org", orgID, "target", target, "user", r.patBorrowUser(ctx, orgID))
		}
		return githubapp.Token{Value: pat}, nil
	}

	return githubapp.Token{}, fmt.Errorf("%w: org=%s target=%s", ErrNoGitHubCredentials, orgID, target)
}

// TokenForRepoScoped mints a credential narrowed to owner/repo (see the
// ScopedResolver interface doc). App XOR PAT (see activeApp): an active App
// mints a fresh repo-scoped token or fails — NO PAT fallback (a transient mint
// blip surfaces as an error → the gitproxy 502s and the next request retries,
// rather than silently degrading to an unscoped, decommissioning PAT). Only an
// org with no active App uses the PAT, returned unscoped (a PAT can't be
// narrowed; Layers 2/3 enforce it). All-miss: ErrNoGitHubCredentials.
func (r *resolver) TokenForRepoScoped(ctx context.Context, orgID, owner, repo string, permissions map[string]string) (githubapp.Token, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return githubapp.Token{}, err
	}
	if app := r.activeApp(ctx, orgID); app != nil {
		return r.appScopedToken(ctx, orgID, app, owner, repo, base, permissions)
	}
	pat, err := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubPAT)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("resolve github pat for org %s: %w", orgID, err)
	}
	if pat != "" {
		return githubapp.Token{Value: pat}, nil
	}
	return githubapp.Token{}, fmt.Errorf("%w: org=%s repo=%s/%s", ErrNoGitHubCredentials, orgID, owner, repo)
}

// TokenForReposScoped mints ONE credential narrowed to a set of repos under a
// single owner (see the ScopedResolver interface doc) — the real-gh channel's
// single team-set-scoped token. App XOR PAT (see activeApp): an active App
// mints one installation token over the whole repo list or fails — NO PAT
// fallback (a transient mint blip surfaces as an error rather than silently
// degrading to an unscoped PAT); only a no-active-App org uses the PAT,
// returned unscoped (a PAT can't be narrowed). All-miss: ErrNoGitHubCredentials.
func (r *resolver) TokenForReposScoped(ctx context.Context, orgID, owner string, repos []string, permissions map[string]string) (githubapp.Token, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return githubapp.Token{}, err
	}
	if app := r.activeApp(ctx, orgID); app != nil {
		return r.appReposScopedToken(ctx, orgID, app, owner, repos, base, permissions)
	}
	pat, err := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubPAT)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("resolve github pat for org %s: %w", orgID, err)
	}
	if pat != "" {
		return githubapp.Token{Value: pat}, nil
	}
	return githubapp.Token{}, fmt.Errorf("%w: org=%s owner=%s", ErrNoGitHubCredentials, orgID, owner)
}

// appReposScopedToken mints one installation access token scoped to the given
// repo names under owner's installation. Mirror of appScopedToken but with a
// repo LIST (MintScopedInstallationToken already accepts one) — the real-gh
// channel's single token. Same no-cache rationale: a scoped mint bypasses the
// installation TokenCache (keyed by installation only) so a scoped token is
// never served to an unscoped caller.
func (r *resolver) appReposScopedToken(ctx context.Context, orgID string, app *domain.OrgGitHubApp, owner string, repos []string, base string, permissions map[string]string) (githubapp.Token, error) {
	inst, ok := r.installationFor(ctx, orgID, owner)
	if !ok {
		return githubapp.Token{}, fmt.Errorf("%w: org=%s: app has no installation for %q", ErrNoGitHubCredentials, orgID, owner)
	}
	minter, err := r.minterFor(ctx, orgID, app, base)
	if err != nil {
		return githubapp.Token{}, err
	}
	installID, err := strconv.ParseInt(inst.InstallationID, 10, 64)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("parse installation id %q: %w", inst.InstallationID, err)
	}
	return minter.MintScopedInstallationToken(ctx, installID, repos, permissions)
}

// ClientForRepoScoped resolves a *Client backed by the per-repo scoped
// credential TokenForRepoScoped mints, plus the acting Identity (App vs PAT) —
// so the exec-gh channel confines a jailbroken agent's API calls to the one
// repo the run is authorized for. Same App-XOR-PAT resolution as
// TokenForRepoScoped.
func (r *resolver) ClientForRepoScoped(ctx context.Context, orgID, owner, repo string, permissions map[string]string) (*Client, Identity, error) {
	base, err := r.githubBaseFor(ctx, orgID)
	if err != nil {
		return nil, IdentityUnknown, err
	}
	if app := r.activeApp(ctx, orgID); app != nil {
		tok, terr := r.appScopedToken(ctx, orgID, app, owner, repo, base, permissions)
		if terr != nil {
			return nil, IdentityUnknown, terr
		}
		return r.newObservedClient(orgID, base, tok.Value), IdentityApp, nil
	}
	client, err := r.tier3PATClient(ctx, orgID, base)
	if err != nil {
		return nil, IdentityUnknown, err
	}
	if client != nil {
		return client, IdentityPAT, nil
	}
	return nil, IdentityUnknown, fmt.Errorf("%w: org=%s repo=%s/%s", ErrNoGitHubCredentials, orgID, owner, repo)
}

// HasAnyCredential reports whether the org has any usable GitHub credential
// without minting a repo-scoped token (see the ScopedResolver interface doc).
// App XOR PAT (see activeApp): an active App's credential is usable iff it has
// at least one installation to mint from — a present PAT is NOT a fallback for
// an active-App org. Only a no-active-App org's usability turns on the PAT. A
// PAT-tier read error propagates so a transient secret-store outage isn't
// misreported as "no credential".
func (r *resolver) HasAnyCredential(ctx context.Context, orgID string) (bool, error) {
	if app := r.activeApp(ctx, orgID); app != nil {
		insts, err := r.apps.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			return false, fmt.Errorf("list installations for org %s: %w", orgID, err)
		}
		return len(insts) > 0, nil
	}
	pat, err := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubPAT)
	if err != nil {
		return false, fmt.Errorf("resolve github pat for org %s: %w", orgID, err)
	}
	return pat != "", nil
}

// BaseURLFor exposes githubBaseFor to callers outside the package that need
// the org's git host base (the multi-mode sandbox git proxy's upstream).
// Same resolution as ClientFor/TokenFor — org_settings, then the github_url
// secret, then github.com — so the proxy routes to the host the clone used.
func (r *resolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return r.githubBaseFor(ctx, orgID)
}

// OrgIdentityFor resolves the org's single GitHub commit identity. See the
// Resolver interface doc for the tier semantics. App is probed first (the bot
// account "<slug>[bot]") and PAT (agents.github_org_login) second, matching
// ClientFor's App-preferred order. Both reads use the System (claims-free)
// door like the rest of the resolver. A read error on either tier is
// non-fatal — it falls through, and an all-miss returns ok=false so the caller
// stamps no identity rather than a fabricated one.
func (r *resolver) OrgIdentityFor(ctx context.Context, orgID string) (name, email string, ok bool) {
	// App tier: the org's registered App acts as "<slug>[bot]". The slug comes
	// from the App registration, resolved live — no stored column, and
	// installation-independent (the bot is one global account however many
	// installations the App has). A staged/inactive App or a read error skips to
	// PAT rather than claiming an identity the org isn't acting as.
	if app, err := r.apps.GetForOrgSystem(ctx, orgID); err == nil && app != nil && app.Active && app.Slug != "" {
		name = app.Slug + "[bot]"
		// The numeric-id noreply form links a bot's commits to its account on
		// github.com (contribution graph + Verified co-author badge); the plain
		// "<slug>[bot]@..." form does NOT reliably link for bot accounts. Use the
		// numeric form when the bot user id is known (org_github_apps.bot_user_id,
		// fetched best-effort at registration), else fall back to the plain form —
		// today's behavior, never a worse state (TFAC-474).
		if app.BotUserID != 0 {
			email = strconv.FormatInt(app.BotUserID, 10) + "+" + name + "@users.noreply.github.com"
		} else {
			email = name + "@users.noreply.github.com"
		}
		return name, email, true
	}
	// PAT tier: the login the org PAT authenticates as. agents.github_org_login
	// is a CACHE of that login, written at PAT bind but deliberately NOT cleared
	// when the PAT is removed (clearing would mean chasing every scattered
	// credential-clear path — DELETE /api/integrations, the settings PAT/base-URL
	// clears, the App-switch teardown — and staying correct as new ones land).
	// Instead, gate the cached login on the SAME KeyGitHubPAT presence the
	// resolver's tier-3 uses (tier3PATClient / TokenFor): the login can only
	// resurface while the credential it describes still exists, so a stale value
	// left behind after a PAT clear (or a GitHub disconnect) never becomes a
	// commit identity for an org that no longer has a PAT. A read error is
	// conservative (treated as no PAT → no identity), never a fabricated one.
	if pat, err := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubPAT); err != nil || pat == "" {
		return "", "", false
	}
	// PAT present: trust the cached login. The plain "<login>@..." form links for
	// a user account, so the numeric form is App-only (locked decision 2). Empty
	// (an org bound before TFAC-452, or a read error) → ok=false; self-heals on
	// the next PAT re-save.
	if agent, err := r.agents.GetForOrgSystem(ctx, orgID); err == nil && agent != nil && agent.GitHubOrgLogin != "" {
		return agent.GitHubOrgLogin, agent.GitHubOrgLogin + "@users.noreply.github.com", true
	}
	return "", "", false
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

	minter, err := r.minterFor(ctx, orgID, app, base)
	if err != nil {
		return githubapp.Token{}, err
	}
	installID, err := strconv.ParseInt(inst.InstallationID, 10, 64)
	if err != nil {
		return githubapp.Token{}, fmt.Errorf("parse installation id %q: %w", inst.InstallationID, err)
	}
	tok, err := minter.MintInstallationToken(ctx, installID)
	if err != nil {
		return githubapp.Token{}, err
	}
	r.cache.Set(orgID, inst.InstallationID, tok)
	return tok, nil
}

// minterFor builds a githubapp.Minter from the org's stored App PEM + App ID,
// pinned at the org's API base. Shared by the cached full-install mint
// (installationToken) and the uncached repo-scoped mint (mintScopedToken).
func (r *resolver) minterFor(ctx context.Context, orgID string, app *domain.OrgGitHubApp, base string) (*githubapp.Minter, error) {
	pem, err := r.secrets.GetSystem(ctx, orgID, app.PEMRef)
	if err != nil {
		return nil, fmt.Errorf("read app pem: %w", err)
	}
	if pem == "" {
		return nil, fmt.Errorf("app pem secret %q not found", app.PEMRef)
	}
	key, err := githubapp.ParsePrivateKey([]byte(pem))
	if err != nil {
		return nil, err
	}
	appID, err := strconv.ParseInt(app.AppID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse app id %q: %w", app.AppID, err)
	}
	return githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID,
		APIBase:    APIBase(base),
	})
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
	return r.newObservedClient(orgID, base, pat), nil
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
