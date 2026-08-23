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
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// ErrNoGitHubCredentials is returned by ClientFor when no credential tier
// resolves for the (org, target) pair — no usable App installation and no
// PAT. Handlers map it to a "GitHub not configured" response.
var ErrNoGitHubCredentials = errors.New("github: no credentials resolved for org")

// ErrAmbiguousInstallation is returned when resolution reaches an App with
// more than one installation and the call named no account to choose between
// them. It deliberately does NOT wrap ErrNoGitHubCredentials: the workspace's
// credential is present and working, and telling a user their GitHub is
// disconnected when the real fault is a caller that didn't say which account it
// meant sends them to re-authorize something that was never broken.
//
// One installation per workspace is what a private BYO App produces, so the
// empty target went unnoticed for as long as that was the only shape. An
// enterprise-owned internal App installs once per GitHub org in the
// enterprise, and from then on "no specific account" has no answer.
var ErrAmbiguousInstallation = errors.New("github: app has multiple installations and no target account")

// ErrUnknownCredentialClass is returned when the org's stored credential class
// is not one this build understands — a class written by a newer peer, or a
// hand-edited row. Like ErrAmbiguousInstallation it deliberately does NOT wrap
// ErrNoGitHubCredentials: the org's credential may be perfectly present, and
// this build simply cannot tell which system to reach for.
//
// Refusing is the point. The alternative — treating an unrecognised class as
// PAT, which is what inferring from a missing org_github_apps row amounted to —
// resolves the wrong credential system silently, in the direction nobody
// watches. An error surfaces at the call site instead.
var ErrUnknownCredentialClass = errors.New("github: unknown credential class for org")

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

// Credential names the audit-log credential (a domain.Credential* value) this
// identity acts under, so the external-action row records the tier that made
// the write rather than the one the deployment is assumed to have.
//
// IdentityUnknown reports the App: it means the tier was never established, and
// the App is what this column has said since it existed, so an unclassified
// write keeps reading as it always has instead of asserting a PAT nobody saw.
func (i Identity) Credential() string {
	if i == IdentityPAT {
		return domain.CredentialGitHubPAT
	}
	return domain.CredentialGitHubApp
}

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
// one repo (the managed Git proxy's TokenSource) type-assert for it; an
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

// TokenForManagedGit resolves the credential injected by a managed Git proxy.
// A nil permission subset is intentional: an App token inherits the
// installation's granted permissions, so a read-only installation can still
// clone and fetch while GitHub rejects pushes. Repository and ref policy remain
// the proxy gate's responsibility. PATs are returned unchanged because GitHub
// cannot narrow them.
//
// Both local's live token source and multi's sealed-bundle provisioner use this
// helper so their App permission request cannot drift.
func TokenForManagedGit(ctx context.Context, resolver ScopedResolver, orgID, owner, repo string) (githubapp.Token, error) {
	return resolver.TokenForRepoScoped(ctx, orgID, owner, repo, nil)
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
// token to mint in tier 1. Empty target = "no specific account", which only one
// installation can answer for: with exactly one there is nothing to choose and
// tier 1 fires, and past that resolution fails with ErrAmbiguousInstallation
// rather than guessing an account. Every caller acting on a repository can name
// the account, because the repository determines it — the owner is the target.
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
	// calls — the managed Git proxy uses it for its upstream and
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
	//	PAT  → name <agents.github_org_login> and email
	//	       <agents.github_org_email>, captured from the PAT owner together.
	//	       Returned ONLY while the org still has a PAT: the cached
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
// the managed Git proxy + exec-gh channel type-assert for. Pinned here so a
// dropped method is a compile error rather than a silent runtime fall-through
// to the unscoped path.
var (
	_ ScopedResolver          = (*resolver)(nil)
	_ ScopedRepoResolver      = (*resolver)(nil)
	_ RateLimitReader         = (*resolver)(nil)
	_ RepoCoverageInvalidator = (*resolver)(nil)
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
	oc, err := r.orgContextFor(ctx, orgID)
	if err != nil {
		return nil, err
	}
	base := oc.base

	// App XOR PAT (see activeApp): an active App is the org's credential — mint
	// its installation token for target or fail; never borrow a PAT.
	app, err := r.activeApp(ctx, orgID, oc.class)
	if err != nil {
		return nil, err
	}
	if app != nil {
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
	oc, err := r.orgContextFor(ctx, orgID)
	if err != nil {
		return nil, IdentityUnknown, err
	}
	base := oc.base

	// Active App → the App client for owner/repo (gated on coverage), or an
	// error. Never a PAT.
	app, err := r.activeApp(ctx, orgID, oc.class)
	if err != nil {
		return nil, IdentityUnknown, err
	}
	if app != nil {
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
	return r.baseFrom(ctx, orgID, set, setErr)
}

// baseFrom is githubBaseFor's precedence logic over an ALREADY-READ settings
// row, so a caller that needs the host and the credential class gets both from
// one read of one row. setErr is the error from that read, threaded in rather
// than swallowed because the fallback order turns on it.
func (r *resolver) baseFrom(ctx context.Context, orgID string, set domain.OrgSettings, setErr error) (string, error) {
	if setErr == nil && set.GitHubBaseURL != "" {
		return ghbase.ResolveBaseURL(set.GitHubBaseURL), nil
	}

	secretURL, secErr := r.secrets.GetSystem(ctx, orgID, integrations.KeyGitHubURL)
	if secErr != nil {
		return "", fmt.Errorf("resolve github base for org %s: read %s secret: %w", orgID, integrations.KeyGitHubURL, secErr)
	}
	if secretURL != "" {
		return ghbase.ResolveBaseURL(secretURL), nil
	}

	// Both sources read cleanly and the secret is empty. If the settings
	// read itself failed, we can't be sure the org isn't GHES — refuse to
	// guess github.com.
	if setErr != nil {
		return "", fmt.Errorf("resolve github base for org %s: read settings: %w", orgID, setErr)
	}
	return ghbase.DefaultBaseURL, nil
}

// orgGitHubContext is the pair of facts a resolution needs from the org's
// settings row: which host to talk to, and which credential system the org is
// in. They travel together because they are read together — see orgContextFor.
type orgGitHubContext struct {
	base  string
	class domain.GitHubCredentialClass
}

// orgContextFor reads org_settings ONCE and derives both the host and the
// credential class from that single row.
//
// One read, for two reasons. It halves the per-resolution settings round-trips
// on the polling-heavy paths, where every entry point needs both. And it makes
// the pair a consistent snapshot: two reads could straddle a credential
// transition and hand back one state's host with another state's class.
//
// The class is stated on org_settings, never inferred from the presence of an
// org_github_apps row — see domain.GitHubCredentialClass for why a missing row
// cannot mean one thing. A settings read error therefore fails the resolution
// even when the base survived it via the github_url secret fallback: the host
// has a second source and the class does not, and a source that decides which
// credential to use must never be guessed at.
func (r *resolver) orgContextFor(ctx context.Context, orgID string) (orgGitHubContext, error) {
	set, setErr := r.orgs.GetSettingsSystem(ctx, orgID)
	base, err := r.baseFrom(ctx, orgID, set, setErr)
	if err != nil {
		return orgGitHubContext{}, err
	}
	if setErr != nil {
		return orgGitHubContext{}, fmt.Errorf("resolve github credential class for org %s: %w", orgID, setErr)
	}
	return orgGitHubContext{base: base, class: set.GitHubCredentialClass}, nil
}

// credentialClassFor reads the credential class alone, for the two entry points
// that resolve no host (HasAnyCredential, OrgIdentityFor). Resolving a base they
// would discard can cost an extra secret read, so they don't go through
// orgContextFor.
func (r *resolver) credentialClassFor(ctx context.Context, orgID string) (domain.GitHubCredentialClass, error) {
	set, err := r.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("resolve github credential class for org %s: %w", orgID, err)
	}
	return set.GitHubCredentialClass, nil
}

// activeApp returns the org's registered App when the org is in the BYO-App
// credential system AND that App is active (so the App is the live git
// credential), else nil — meaning the PAT tier.
//
// This is the App-XOR-PAT gate. An org's git credential is its active App OR a
// borrowed PAT, never both stably: the migration flow deletes the PAT at App
// cutover and tears down the App on switch-to-PAT, so "active App + usable PAT"
// is only ever a transient anomaly (a half-finished cutover). So when an active
// App is configured it is the ONLY credential path — every resolver entry point
// mints from it or fails, and NONE falls through to a PAT (which for an
// active-App org would mean a decommissioning, unscoped credential). The PAT
// tier is reached only when there is no active App.
//
// Which arm runs is decided by the org's credential CLASS, not by whether an
// App row happens to exist. The distinction is the entire point: "no row" is
// the shape of a PAT org today and would equally be the shape of an org riding
// a deployment-level shared App, so inferring PAT from a missing row is a guess
// that is right only by accident. A class this build doesn't know is refused
// outright rather than resolved as PAT — an org must fail where someone sees
// it, not quietly act on a credential it does not have.
//
// class arrives as a resolved parameter rather than a read of its own, so it is
// the same value the caller resolved its base URL alongside — one settings row,
// one snapshot, one decision.
func (r *resolver) activeApp(ctx context.Context, orgID string, class domain.GitHubCredentialClass) (*domain.OrgGitHubApp, error) {
	switch class {
	case domain.GitHubCredentialClassPAT:
		return nil, nil

	case domain.GitHubCredentialClassBYOApp:
		app, err := r.apps.GetForOrgSystem(ctx, orgID)
		if err != nil {
			// Best-effort on the App READ specifically, unchanged: the org whose
			// registration can't be read may be mid-switch, with a staged App and a
			// live PAT that resolves perfectly well, and a store blip shouldn't take
			// that org's polling down. A cutover-completed org has no PAT left, so
			// this path ends in ErrNoGitHubCredentials either way.
			ghResolverLog.Warn("read app registration failed; falling back to the PAT tier for this resolution",
				"org", orgID, "error", err)
			return nil, nil
		}
		// nil or staged (active=false) is the PAT tier: during a PAT→App switch
		// the org is already in the App system while the PAT is still live.
		if app == nil || !app.Active {
			return nil, nil
		}
		return app, nil

	default:
		return nil, fmt.Errorf("%w: org=%s class=%q", ErrUnknownCredentialClass, orgID, class)
	}
}

// appToken resolves the org's App installation token for target (the account
// the work concerns), or an error — NO PAT fallback (the caller determined via
// activeApp that the org's credential is the App). Shares the installation
// TokenCache with ClientFor so the API client and the host-side git clone
// resolve identically.
func (r *resolver) appToken(ctx context.Context, orgID string, app *domain.OrgGitHubApp, target, base string) (githubapp.Token, error) {
	inst, err := r.installationFor(ctx, orgID, accountByLogin(target))
	if err != nil {
		return githubapp.Token{}, err
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
	inst, err := r.installationFor(ctx, orgID, accountByLogin(owner))
	if err != nil {
		return githubapp.Token{}, err
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
	oc, err := r.orgContextFor(ctx, orgID)
	if err != nil {
		return githubapp.Token{}, err
	}
	base := oc.base

	// App XOR PAT (see activeApp): an active App is the org's credential —
	// resolve its installation token (shared cache + mint path with ClientFor)
	// or fail; never fall to a PAT.
	app, err := r.activeApp(ctx, orgID, oc.class)
	if err != nil {
		return githubapp.Token{}, err
	}
	if app != nil {
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
	oc, err := r.orgContextFor(ctx, orgID)
	if err != nil {
		return githubapp.Token{}, err
	}
	base := oc.base
	app, err := r.activeApp(ctx, orgID, oc.class)
	if err != nil {
		return githubapp.Token{}, err
	}
	if app != nil {
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
	oc, err := r.orgContextFor(ctx, orgID)
	if err != nil {
		return githubapp.Token{}, err
	}
	base := oc.base
	app, err := r.activeApp(ctx, orgID, oc.class)
	if err != nil {
		return githubapp.Token{}, err
	}
	if app != nil {
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
	inst, err := r.installationFor(ctx, orgID, accountByLogin(owner))
	if err != nil {
		return githubapp.Token{}, err
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
	oc, err := r.orgContextFor(ctx, orgID)
	if err != nil {
		return nil, IdentityUnknown, err
	}
	base := oc.base
	app, err := r.activeApp(ctx, orgID, oc.class)
	if err != nil {
		return nil, IdentityUnknown, err
	}
	if app != nil {
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
	// Class alone: this probe resolves no host, so it skips orgContextFor and
	// the base resolution (and possible secret read) it would discard.
	class, err := r.credentialClassFor(ctx, orgID)
	if err != nil {
		return false, err
	}
	app, err := r.activeApp(ctx, orgID, class)
	if err != nil {
		return false, err
	}
	if app != nil {
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

// appForIdentity returns the App registration OrgIdentityFor should consider
// for class, or nil when the class has no App behind it. Split out so the
// class switch is a switch — the arm a future shared-App class needs is a new
// case here, not a condition threaded into the identity expression.
func (r *resolver) appForIdentity(ctx context.Context, orgID string, class domain.GitHubCredentialClass) (*domain.OrgGitHubApp, error) {
	switch class {
	case domain.GitHubCredentialClassPAT:
		return nil, nil
	case domain.GitHubCredentialClassBYOApp:
		return r.apps.GetForOrgSystem(ctx, orgID)
	default:
		// Unreachable: the caller gates on Known() first. Kept explicit so an
		// added class fails here rather than silently resolving as PAT.
		return nil, fmt.Errorf("%w: org=%s class=%q", ErrUnknownCredentialClass, orgID, class)
	}
}

// BaseURLFor exposes githubBaseFor to callers outside the package that need
// the org's git host base (the managed Git proxy's upstream).
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
//
// Which tier is even eligible follows the org's credential CLASS, the same gate
// activeApp uses, so the identity stamped on a commit always describes the
// credential system that produced it. A class this build doesn't know (or that
// can't be read) yields no identity at all rather than the PAT tier's login:
// the whole posture of this function is to refuse a fabricated identity, and
// under an unrecognised class the PAT login would be exactly that.
func (r *resolver) OrgIdentityFor(ctx context.Context, orgID string) (name, email string, ok bool) {
	class, err := r.credentialClassFor(ctx, orgID)
	if err != nil || !class.Known() {
		ghResolverLog.Warn("resolve credential class failed or unknown; stamping no org identity",
			"org", orgID, "class", class, "error", err)
		return "", "", false
	}

	// App tier: the org's registered App acts as "<slug>[bot]". The slug comes
	// from the App registration, resolved live — no stored column, and
	// installation-independent (the bot is one global account however many
	// installations the App has). A staged/inactive App or a read error skips to
	// PAT rather than claiming an identity the org isn't acting as — during a
	// PAT→App switch the org is in the App system but still committing as its PAT.
	if app, err := r.appForIdentity(ctx, orgID, class); err == nil && app != nil && app.Active && app.Slug != "" {
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
	// PAT tier: the identity the org PAT authenticates as. The agents columns
	// cache both values at PAT bind but are deliberately not cleared
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
	// PAT present: trust the captured pair. A partial identity cannot stamp a
	// commit; it self-heals on the next PAT save.
	if agent, err := r.agents.GetForOrgSystem(ctx, orgID); err == nil && agent != nil && agent.GitHubOrgLogin != "" && agent.GitHubOrgEmail != "" {
		return agent.GitHubOrgLogin, agent.GitHubOrgEmail, true
	}
	return "", "", false
}

// accountRef names the GitHub account whose installation a caller needs. The
// two fields are the two halves of an account's identity and they age
// differently: Login is the handle, which its owner can rename at any time,
// and ID is GitHub's numeric account id, which they cannot. A caller that
// holds only a repo owner supplies the login (accountByLogin); a caller that
// knows the account's id supplies both.
type accountRef struct {
	ID    string
	Login string
}

// accountByLogin is the ref for a caller that knows an account only by its
// handle — every public resolver entry point today, since ClientFor and
// friends take a GitHub target login. Such a ref matches exactly as the
// resolver always has.
func accountByLogin(login string) accountRef { return accountRef{Login: login} }

// String names the account a resolution error is about, in whichever halves the
// caller held. The empty ref renders as the absence it is, so a failure that
// came of naming no account reads that way in a log line.
func (a accountRef) String() string {
	switch {
	case a.Login != "" && a.ID != "":
		return strconv.Quote(a.Login) + " (id " + a.ID + ")"
	case a.Login != "":
		return strconv.Quote(a.Login)
	case a.ID != "":
		return "id " + a.ID
	default:
		return "(no account)"
	}
}

// installationFor selects the App installation serving target.
//
// The numeric account id decides whenever both the ref and the mirrored row
// carry one: it is the half of the account's identity that a rename leaves
// alone, so an installation whose account_login has not yet caught up with a
// rename still resolves, and one that merely inherited a freed login can never
// answer for the account that gave it up. When either side has no id — a row
// mirrored before ids were captured, or a caller that knows only a handle —
// the match falls back to the case-insensitive login compare (GitHub logins
// are case-insensitive, so the compare is too), which is the whole of the
// behaviour for a ref with no id.
//
// An empty ref means "no specific account", which one installation can answer
// for and several cannot: with exactly one there is nothing to choose between,
// so it is returned; with more than one the call is ambiguous and the error
// says so (ErrAmbiguousInstallation), because the fault is a caller that didn't
// name an account rather than a workspace missing a credential. Guessing is not
// an option on offer — a token minted from the wrong account either 422s at
// GitHub or, worse, succeeds against somebody else's repositories.
func (r *resolver) installationFor(ctx context.Context, orgID string, target accountRef) (domain.OrgGitHubAppInstallation, error) {
	insts, err := r.apps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		// A read failure is not a missing credential, and saying so would be
		// the same misreport the ambiguous case is: it sends a user to
		// reconnect a GitHub that was never disconnected, when what happened
		// was a backend blip that will be gone next call. Propagate it with the
		// cause intact — the same posture githubBaseFor and the PAT tier take,
		// and what HasAnyCredential already does with this very read.
		ghResolverLog.Warn("list installations failed", "org", orgID, "error", err)
		return domain.OrgGitHubAppInstallation{}, fmt.Errorf("list installations for org %s: %w", orgID, err)
	}
	if target.ID == "" && target.Login == "" {
		if len(insts) == 1 {
			return insts[0], nil
		}
		if len(insts) > 1 {
			return domain.OrgGitHubAppInstallation{}, fmt.Errorf("%w: org=%s: %d installations to choose from", ErrAmbiguousInstallation, orgID, len(insts))
		}
	}
	if target.ID != "" {
		for _, in := range insts {
			if in.AccountID == target.ID {
				return in, nil
			}
		}
	}
	for _, in := range insts {
		// A row whose id is known and differs is a different account, whatever
		// it is currently called — skip it rather than let a colliding login
		// answer for the wrong one. An empty login matches nothing for the same
		// reason: two accounts neither side can name are two unknowns, not a
		// pair, and the empty target was already answered above.
		if target.ID != "" && in.AccountID != "" && in.AccountID != target.ID {
			continue
		}
		if target.Login != "" && strings.EqualFold(in.AccountLogin, target.Login) {
			return in, nil
		}
	}
	return domain.OrgGitHubAppInstallation{}, fmt.Errorf("%w: org=%s: app has no installation for %s", ErrNoGitHubCredentials, orgID, target)
}

// installationToken returns a cached token for the installation or mints a
// fresh one (signing an App JWT with the org's PEM and exchanging it). The
// mint path is reached only on a cache miss (~once/hour per installation),
// so re-parsing the PEM each time is acceptable.
//
// A suspended installation is never served from cache. GitHub refuses every
// token minted from one, so a cached token that outlives the suspension is a
// credential that has already stopped working — and the webhook that would
// have dropped it may never have arrived (GitHub does not re-deliver, and
// local mode receives nothing), leaving the mirrored row as the only place
// that knows. Dropping the entry and minting fresh keeps the failure honest:
// GitHub answers for its own suspension, and the moment it is lifted the next
// mint succeeds with nothing stale in the way.
func (r *resolver) installationToken(ctx context.Context, orgID string, app *domain.OrgGitHubApp, inst domain.OrgGitHubAppInstallation, base string) (githubapp.Token, error) {
	if inst.Suspended() {
		r.cache.Invalidate(orgID, inst.InstallationID)
	} else if tok, ok := r.cache.Get(orgID, inst.InstallationID); ok {
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
// (installationToken) and the uncached repo-scoped mint (appScopedToken).
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
		APIBase:    ghbase.APIBase(base),
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
