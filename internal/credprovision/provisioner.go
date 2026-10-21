// Package credprovision is the brain-side half of TFAC-614's sealed
// per-claim credential channel: it resolves a claimed conversation's
// LLM/GitHub/Jira/Linear credentials (using the real, key-bearing secret store only the brain
// holds), seals them to the claiming executor's published X25519 public
// key (credseal), and writes the result to claim_credentials for that
// executor to unseal, with the engagement's source manifest recorded on its
// claim in the same write.
//
// Manager is a brain-unit member the same way the registry GC and the drain
// worker are (internal/app's startBrain/stopBrain): constructed only for
// brain-capable roles in multi mode, nil-checked everywhere else.
package credprovision

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// llmResolver is the narrow slice of internal/llmcred the provisioner mints
// LLM material through — declared here so the provisioner's test can stub it
// without a real STS minter. The production impl is *llmcred.Resolver.
type llmResolver interface {
	ResolveForBundle(ctx context.Context, orgID, conversationID, model string) (llmcred.Material, error)
}

var log = slog.Default().With("component", "credprovision")

// DefaultAwaitingSweepInterval is the backstop-sweep cadence for
// conversations whose active claim is parked in phase='awaiting_credentials'
// — the fast path is the executor's cred_request tf_ctl notification; this
// recovers anything that notification dropped (the relay is lossy by
// design). Short, because it gates conversation START latency.
const DefaultAwaitingSweepInterval = 5 * time.Second

// DefaultRefreshSweepInterval is the cadence the refresh sweep runs at.
const DefaultRefreshSweepInterval = 5 * time.Minute

// DefaultRefreshAfter is how old an active claim's sealed bundle must be
// before the refresh sweep re-mints its GitHub tokens. GitHub installation
// tokens are hour-lived; this refreshes well within that window.
const DefaultRefreshAfter = 30 * time.Minute

// LinearTokenHeadroom is the least life a Linear app-install access token has
// left when it is sealed. A bundle goes at most DefaultRefreshAfter plus one
// sweep interval between seals (RefreshCadenceForTTL only ever shortens
// either), and the margin covers the re-seal reaching the sidecar, so a sealed
// token outlives the bundle that carries it.
const LinearTokenHeadroom = DefaultRefreshAfter + DefaultRefreshSweepInterval + 10*time.Minute

// jiraSystemResolver is a structural (unexported, anonymous-interface-
// compatible) view of jira.Resolver's optional ResolveSystemCredential
// extension — mirrors internal/github's ScopedResolver pattern. Declared
// locally so this package can type-assert without jira exporting its own
// interface name.
type jiraSystemResolver interface {
	ResolveSystemCredential(ctx context.Context, orgID string) (jira.SystemCredential, error)
}

// Manager resolves and seals per-claim credential bundles. Holds the real,
// key-bearing db.Stores (brain-side only) plus the GitHub/Jira/Linear
// resolvers built against it.
type Manager struct {
	stores         db.Stores
	ghResolver     ghclient.Resolver
	jiraResolver   jira.Resolver
	linearResolver linear.Resolver
	llm            llmResolver

	// sources overrides the availability resolve (availability) for a test
	// that drives a whole provision over fake stores. nil in production.
	sources func(ctx context.Context, orgID string) (eventsource.Availability, error)
}

// NewManager builds a Manager against stores — a brain-capable role's
// normal (secret-bearing) db.Stores, never the disabled-secrets bundle an
// executor holds. llm is the shared LLM-credential resolver
// (internal/llmcred): for a role-mode Bedrock org it mints a short-lived STS
// session credential (executor-bound: per-conversation session name + the executor
// egress network condition); for every other mode it passes the stored
// material through. nil falls back to agentproc's raw-secret resolution (no
// role support) — only for callers that don't wire llmcred.
//
// linearResolver resolves the org's Linear service credential. It is passed
// in rather than built here because an app-installed org's access token comes
// from a token cache (linearoauth.TokenCache) that refreshes it and rotates
// the install's refresh token, and the poller and this provisioner share one
// such cache, so an org's token is refreshed once for both rather than once
// each. It must serve the app_install shape (linear.NewResolverWithInstall);
// nil seals no Linear credential.
func NewManager(stores db.Stores, llm llmResolver, linearResolver linear.Resolver) *Manager {
	return &Manager{
		stores: stores,
		// Deployment App from the env, same as the API server's resolver: this is
		// the brain-side resolver that seals a run's git credential, so a managed
		// org's run gets a token minted from the shared App rather than nothing.
		ghResolver:     ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, nil, ghclient.WithDeploymentAppFromEnv()),
		jiraResolver:   jira.NewResolver(stores.Secrets, stores.Orgs),
		linearResolver: linearResolver,
		llm:            llm,
	}
}

// ProvisionForConversation resolves conversationID's credentials and seals them to its
// current claimant's published pubkey, writing claim_credentials. Called
// synchronously off the executor's cred_request notification (the fast
// path) and by both sweeps (the backstop / refresh paths) — idempotent and
// safe to call repeatedly for the same conversation.
//
// A no-op (nil error, no write) when the conversation isn't currently claimed by
// anyone, or the claiming instance hasn't published a pubkey — there is
// nothing to seal to. Not an error: the claim may have just been released
// (reaped, requeued) between the notification firing and this handler
// running; the executor's own timeout/requeue path is what recovers that
// window, not this function surfacing an error.
func (m *Manager) ProvisionForConversation(ctx context.Context, orgID, conversationID string) (err error) {
	// The brain's half of the credential handshake, as its OWN root. It is
	// deliberately not joined to the executor's awaiting-credentials span:
	// the tf_ctl doorbell that usually triggers this is lossy by design (the
	// sweeps are the real completion path), so a link would assert a 1:1
	// handoff that neither side can guarantee — and the two sweeps reach here
	// with no requester at all. conversation.id is the join; put the same id
	// in a Tempo query and both sides come back.
	ctx, span := tracer.Start(ctx, "credentials.provision", trace.WithNewRoot(),
		trace.WithAttributes(telemetry.ConversationID(conversationID), telemetry.OrgID(orgID)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	claim, ok, err := m.stores.ConversationQueue.GetClaim(ctx, orgID, conversationID)
	if err != nil {
		return fmt.Errorf("credprovision: read claim for conversation %s: %w", conversationID, err)
	}
	if !ok || claim.ExecutorID == "" {
		span.SetAttributes(telemetry.Outcome("unclaimed"))
		return nil
	}

	inst, err := m.stores.Instances.Get(ctx, claim.ExecutorID)
	if err != nil {
		return fmt.Errorf("credprovision: read claiming instance %s: %w", claim.ExecutorID, err)
	}
	if inst == nil {
		log.Warn("claiming instance not found; skipping (not yet registered)",
			"conversation", conversationID, "executor", claim.ExecutorID)
		span.SetAttributes(telemetry.Outcome("executor_unregistered"))
		return nil
	}
	// Seal to the claim's per-run sidecar key when the executor published one
	// for this claim — a bundle sealed to it opens only inside that claim's
	// sidecar process, so the orchestrator holds no unseal authority. The
	// per-instance key is the fallback for a claim whose sidecar hasn't
	// published (or a deployment still on the per-instance channel).
	sealPubKey := claim.CredPubKey
	if sealPubKey == "" {
		sealPubKey = inst.PubKey
	}
	if sealPubKey == "" {
		log.Warn("no sidecar or instance pubkey to seal to; skipping (not yet published, or not an executor)",
			"conversation", conversationID, "executor", claim.ExecutorID)
		span.SetAttributes(telemetry.Outcome("no_recipient_key"))
		return nil
	}
	if inst.BootEpoch != claim.BootEpoch {
		// The claiming executor has restarted since it claimed this conversation —
		// a new boot, a new ephemeral keypair, a new epoch. Nothing in
		// this boot is waiting on this conversation; the new boot's reset
		// releases the claim, or another dispatcher takes it over once its
		// lease lapses, not this function. Sealing against the new epoch
		// here would be wrong:
		// the executor compares a bundle's epoch against its OWN current
		// one, and inst.BootEpoch (the pubkey's owner) is authoritative,
		// but writing it under a claim that names an EARLIER epoch would
		// desync the executor's understanding of which claim this bundle
		// answers. Skip, and let the claim's release return it to the queue.
		log.Warn("claiming executor's boot epoch has moved on since this conversation was claimed; skipping (the claim is released by that executor's boot reset or taken over after its lease)",
			"conversation", conversationID, "executor", claim.ExecutorID, "claim_epoch", claim.BootEpoch, "instance_epoch", inst.BootEpoch)
		span.SetAttributes(telemetry.Outcome("stale_boot_epoch"))
		return nil
	}

	pubBytes, err := base64.StdEncoding.DecodeString(sealPubKey)
	if err != nil || len(pubBytes) != 32 {
		return fmt.Errorf("credprovision: conversation %s claim carries a malformed recipient pubkey", conversationID)
	}
	var pub [32]byte
	copy(pub[:], pubBytes)

	bundle := &credbundle.Bundle{BootEpoch: inst.BootEpoch}

	// Resolve LLM material through the shared llmcred seam — role-mode orgs
	// mint a short-lived STS session credential here (executor-bound: the
	// conversation id as RoleSessionName for per-conversation CloudTrail attribution, plus the
	// executor egress network condition), every other mode passes through.
	// A role-mode mint failure has nothing to fall back to (no raw key
	// stored), so it fails the provision per PS-H5: no bundle is written,
	// the executor's awaiting-credentials wait times out and requeues, and
	// the error names AssumeRole + the role ARN (surfaced by llmcred).
	model, err := m.conversationModel(ctx, orgID, conversationID)
	if err != nil {
		return err
	}
	if m.llm != nil {
		mat, err := m.llm.ResolveForBundle(ctx, orgID, conversationID, model)
		if err != nil && !llmcred.IsNoCredentials(err) {
			return fmt.Errorf("credprovision: resolve LLM credentials for org %s (conversation %s): %w", orgID, conversationID, err)
		}
		bundle.LLM = mat.Env
		if !mat.Expiry.IsZero() {
			bundle.LLMExpiryUnix = mat.Expiry.Unix()
		}
	} else {
		llm, err := agentproc.ResolveCredentialsForBundle(ctx, agentproc.NewSystemSecretsReader(m.stores.Secrets), orgID, model)
		if err != nil && !errors.Is(err, agentproc.ErrNoCredentialsConfigured) {
			return fmt.Errorf("credprovision: resolve LLM credentials for org %s: %w", orgID, err)
		}
		bundle.LLM = llm
	}

	sources, err := m.sealSources(ctx, orgID, claim, bundle)
	if err != nil {
		return err
	}
	bundle.Sources = sources

	plaintext, err := bundle.Marshal()
	if err != nil {
		return fmt.Errorf("credprovision: marshal bundle for conversation %s: %w", conversationID, err)
	}
	sealed, err := credseal.Seal(pub, plaintext)
	if err != nil {
		return fmt.Errorf("credprovision: seal bundle for conversation %s: %w", conversationID, err)
	}
	if err := m.stores.ClaimCredentials.Put(ctx, orgID, conversationID, claim.ExecutorID, inst.BootEpoch, sealed, sources); err != nil {
		return fmt.Errorf("credprovision: write bundle for conversation %s: %w", conversationID, err)
	}
	log.Debug("provisioned claim credential bundle", "conversation", conversationID, "org", orgID, "executor", claim.ExecutorID, "boot_epoch", inst.BootEpoch)
	span.SetAttributes(telemetry.Outcome("sealed"))
	return nil
}

// sealSources resolves every event source's credential into bundle and returns
// the engagement's source manifest: one entry per source the org's
// availability answer names, recording whether its credential is now in the
// bundle and, when it is not, why not. One pass writes both, so the manifest
// can never describe a bundle other than the one sealed beside it.
//
// A source an org admin turned off, or one the org is not licensed for, is
// not resolved at all: off means no credential is resolved for an agent. Every
// other source is resolved whatever its availability probe said, and its entry
// follows what the resolve produced — sealed when a credential came back,
// unconfigured when none did — so the entry is true of the bundle even where
// the probe and the resolver disagree.
//
// GitHub and Jira failures fail the seal, as they always have. A Linear failure
// degrades instead: an app install's credential can need a refresh from
// Linear's token endpoint, and an outage there would otherwise stop this org's
// claims from starting and its in-flight runs' GitHub tokens from being
// re-minted. Its entry records the source as unresolved, the sidecar still
// starts its proxy, and the refresh sweep re-seals the engagement until the
// credential resolves.
func (m *Manager) sealSources(ctx context.Context, orgID string, claim db.AwaitingCredentialsConversation, bundle *credbundle.Bundle) (domain.SourceManifest, error) {
	conversationID := claim.ConversationID
	avail, err := m.availability(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("credprovision: resolve event-source availability for org %s: %w", orgID, err)
	}

	manifest := domain.SourceManifest{}
	resolvable := map[string]bool{}
	for _, src := range avail {
		switch src.State {
		case eventsource.StateWIP:
			continue
		case eventsource.StateDisabled:
			manifest[src.Kind] = domain.SourceEntry{State: domain.SourceDisabled}
			continue
		case eventsource.StateUnlicensed:
			manifest[src.Kind] = domain.SourceEntry{State: domain.SourceUnlicensed}
			continue
		}
		resolvable[src.Kind] = true
	}
	// GitHub is required: it has no off switch and no licence, and every run
	// may clone, push and read a pull request, so it is resolved whatever the
	// availability answer says about it.
	resolvable[eventsource.KindGitHub] = true
	gh, err := m.resolveGitHub(ctx, orgID, claim.TeamID, claim.TaskID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("credprovision: resolve github credentials for conversation %s: %w", conversationID, err)
	}
	bundle.GitHub = gh
	manifest[eventsource.KindGitHub] = githubEntry(gh)

	if resolvable[eventsource.KindJira] {
		jc, err := m.resolveJira(ctx, orgID)
		if err != nil {
			return nil, fmt.Errorf("credprovision: resolve jira credentials for org %s: %w", orgID, err)
		}
		bundle.Jira = jc
		manifest[eventsource.KindJira] = presentEntry(jc != nil, nil)
	}

	if resolvable[eventsource.KindLinear] {
		lc, err := m.resolveLinear(ctx, orgID)
		switch {
		case err != nil:
			reason := agenthost.UnresolvedReason(err)
			log.Warn("resolve the org's Linear credential failed; sealing the bundle without it, and the run's Linear verbs report Linear unavailable until a re-seal resolves it",
				"org", orgID, "conversation", conversationID, "reason", reason, "error", err)
			manifest[eventsource.KindLinear] = domain.SourceEntry{State: domain.SourceUnresolved, Reason: reason}
		default:
			bundle.Linear = lc
			var expires *time.Time
			if lc != nil && lc.ExpiresUnix != 0 {
				t := time.Unix(lc.ExpiresUnix, 0).UTC()
				expires = &t
			}
			manifest[eventsource.KindLinear] = presentEntry(lc != nil, expires)
		}
	}

	// Every first-class provider beyond the built-in GitHub/Jira/Linear (Slack, and
	// any future one) resolves its own sealed keyed set through its registered
	// resolver — the brain never imports the provider package, so core stays
	// free of provider-specific credential symbols. A provider with nothing
	// configured for this org/team is simply absent from the map.
	providers, err := agenthost.ResolveProviderCredentials(ctx, m.stores, agenthost.ProvisionScope{
		OrgID: orgID, TeamID: claim.TeamID, TaskID: claim.TaskID, ConversationID: conversationID,
	}, func(namespace string) bool {
		// Skipped exactly when its source was recorded and not resolvable:
		// turned off or unlicensed. A provider whose source the availability
		// answer does not name at all (one with no system probe) has no entry
		// to record a state in, and resolves as it always has.
		_, recorded := manifest[namespace]
		return recorded && !resolvable[namespace]
	})
	if err != nil {
		return nil, fmt.Errorf("credprovision: resolve provider credentials for conversation %s: %w", conversationID, err)
	}
	bundle.Providers = providers
	for kind := range resolvable {
		if _, done := manifest[kind]; done || !agenthost.ProviderRegistered(kind) {
			continue
		}
		_, sealed := providers[kind]
		manifest[kind] = presentEntry(sealed, nil)
	}
	return manifest, nil
}

// availability is the org's per-source state, through the claims-free system
// door.
func (m *Manager) availability(ctx context.Context, orgID string) (eventsource.Availability, error) {
	if m.sources != nil {
		return m.sources(ctx, orgID)
	}
	return eventsource.ResolveSystem(ctx, m.stores, orgID)
}

// presentEntry is a resolved source's entry: sealed when the resolve produced
// a credential, with expires when that credential expires, and unconfigured
// when it produced none.
func presentEntry(present bool, expires *time.Time) domain.SourceEntry {
	if !present {
		return domain.SourceEntry{State: domain.SourceUnconfigured}
	}
	return domain.SourceEntry{State: domain.SourceSealed, ExpiresAt: expires}
}

// githubEntry is the GitHub half's entry. Its expiry is the earliest among the
// tokens sealed for it, the one the run loses first; a PAT carries none.
func githubEntry(gh *credbundle.GitHubCreds) domain.SourceEntry {
	if gh == nil {
		return presentEntry(false, nil)
	}
	var earliest time.Time
	note := func(t time.Time) {
		if !t.IsZero() && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}
	for _, tok := range gh.RepoTokens {
		note(tok.ExpiresAt)
	}
	if gh.CLIToken != nil {
		note(gh.CLIToken.ExpiresAt)
	}
	if earliest.IsZero() {
		return presentEntry(true, nil)
	}
	earliest = earliest.UTC()
	return presentEntry(true, &earliest)
}

// conversationModel reads the model the conversation runs on. It is the
// provider selector for every bundle this package seals: the model names its
// provider, so a conversation on a Bedrock key seals Bedrock material and one
// on an Anthropic key seals the Anthropic key — never both, and never the org's
// preferred provider over the conversation's own.
//
// A read failure is propagated rather than degraded to "no model". Resolving
// without one falls back to the org's configured provider, which for an org
// holding both would seal material this conversation may not be able to use.
func (m *Manager) conversationModel(ctx context.Context, orgID, conversationID string) (string, error) {
	conv, err := m.stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil {
		return "", fmt.Errorf("credprovision: read conversation %s: %w", conversationID, err)
	}
	if conv == nil {
		return "", fmt.Errorf("credprovision: conversation %s not found while resolving its model", conversationID)
	}
	return conv.Model, nil
}

// resolveGitHub resolves the conversation's GitHub credential: nil when the org has
// no usable GitHub credential at all (a Jira-only org — no regression, its
// conversations do no git). Otherwise mints a repo-scoped installation token (an
// active App) — or the org PAT, unscoped (a PAT can't be narrowed) — for
// every repo in the conversation's authorized set (the same team-tracked ∩
// conversation_worktrees intersection the git proxy's live authorize gate uses, see
// gitAuthorizeDecision in internal/delegate/spawner.go).
func (m *Manager) resolveGitHub(ctx context.Context, orgID, teamID, taskID, conversationID string) (*credbundle.GitHubCreds, error) {
	scoped, ok := m.ghResolver.(ghclient.ScopedResolver)
	if !ok {
		return nil, nil
	}
	has, err := scoped.HasAnyCredential(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("probe github credential: %w", err)
	}
	if !has {
		return nil, nil
	}
	// The sidecar sends the tokens below to this host and no other, so nothing
	// is minted until it is known. An org whose URL was cleared since the probe
	// has no credential left to resolve; any other failure fails the seal.
	base, err := m.ghResolver.BaseURLFor(ctx, orgID)
	if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve github host: %w", err)
	}
	if base == "" {
		return nil, fmt.Errorf("resolve github host: org %s resolved an empty GitHub URL", orgID)
	}

	authorized, err := m.authorizedRepos(ctx, orgID, teamID, taskID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("resolve authorized repo set: %w", err)
	}

	gh := &credbundle.GitHubCreds{Mode: credbundle.GitHubModeApp, BaseURL: base, RepoTokens: map[string]credbundle.RepoToken{}}
	if name, email, ok := m.ghResolver.OrgIdentityFor(ctx, orgID); ok {
		gh.IdentityName = name
		gh.IdentityEmail = email
	}

	// nil permissions = inherit the App installation's full granted set,
	// scoped to just this repo — narrower than today's ClientFor (which
	// mints across every repo the installation covers) while still
	// covering both the API-client uses (PR comments/reviews) and the git
	// proxy's contents-only push, a strict subset of whatever the
	// installation grants.
	//
	// RepoTokens is keyed by the repository's current "owner/repo": the
	// sidecar matches each upstream request's path against it, and that path
	// names the repository the way GitHub does now.
	repoIDs := make([]string, 0, len(authorized))
	for _, row := range authorized {
		owner, repo, repoID := row.Owner, row.Repo, row.Slug()
		repoIDs = append(repoIDs, repoID)
		tok, identity, err := ghclient.TokenForManagedGit(ctx, scoped, orgID, owner, repo)
		if err != nil {
			if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				continue
			}
			return nil, fmt.Errorf("mint token for %s: %w", repoID, err)
		}
		if identity == ghclient.IdentityPAT {
			gh.Mode = credbundle.GitHubModePAT
			gh.PAT = tok.Value
		}
		gh.RepoTokens[repoID] = credbundle.RepoToken{Token: tok.Value, ExpiresAt: tok.ExpiresAt}
	}

	// The real-gh channel's single team-set-scoped token: ONE installation
	// token over the conversation's authorized repos under the primary owner (App orgs),
	// or the org PAT (PAT orgs). The injector proxy injects this on every
	// request, so its own repo scope IS the policy — no per-request path
	// parsing. Minting is per-installation, so a conversation whose authorized set spans
	// multiple owners gets gh-channel coverage for the primary owner only;
	// repos under other owners surface GitHub's standard 404 on this channel
	// (the per-repo RepoTokens above still cover them for the exec/git channels
	// until P4).
	//
	// EVERY failure here is non-fatal, deliberately unlike the per-repo loop
	// above: this token is additive: a bundle without it still serves the
	// exec-verb channel and the git proxy from RepoTokens, so the conversation works with
	// only the gh channel degraded. Hard-failing would turn "gh unavailable"
	// into "conversation cannot start" — and the failure is reachable in normal
	// operation, not just on a blip: a "selected repositories" App install 422s
	// a mint naming a repo outside its grant, which would otherwise abort every
	// conversation for that org. The next refresh sweep re-mints.
	primary := ""
	if host, err := db.OrgGitHubHostSystem(ctx, m.stores.Orgs, orgID); err == nil {
		primary = m.taskPrimaryRepo(ctx, orgID, taskID, host)
	}
	if owner, names := cliChannelScope(primary, repoIDs); owner != "" {
		cliTok, err := scoped.TokenForReposScoped(ctx, orgID, owner, names, nil)
		switch {
		case err != nil:
			if !errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				log.Warn("gh-channel token mint failed; conversation continues without the gh channel",
					"org", orgID, "owner", owner, "repos", len(names), "error", err)
			}
		case cliTok.Value != "":
			gh.CLIToken = &credbundle.RepoToken{Token: cliTok.Value, ExpiresAt: cliTok.ExpiresAt}
		}
	}
	return gh, nil
}

// cliChannelScope picks the single owner the real-gh channel's team-set token
// is minted for, and the bare repo names under it. Preference: the owner of
// the conversation's primary repo (the repo a delegated conversation actually works on), else —
// for an unanchored conversation whose authorized set is the team's whole tracked set —
// the owner with the most authorized repos (alphabetically-first owner breaks a
// tie, for a deterministic seal). repoIDs are "owner/repo"; primaryRepo is
// "owner/repo" or "". Returns ("", nil) when repoIDs is empty.
func cliChannelScope(primaryRepo string, repoIDs []string) (owner string, repoNames []string) {
	byOwner := map[string][]string{}
	for _, id := range repoIDs {
		o, r, ok := strings.Cut(id, "/")
		if !ok || o == "" || r == "" {
			continue
		}
		byOwner[o] = append(byOwner[o], r)
	}
	if len(byOwner) == 0 {
		return "", nil
	}

	if po, _, ok := strings.Cut(primaryRepo, "/"); ok && po != "" {
		if names, present := byOwner[po]; present {
			return po, names
		}
	}

	best := ""
	for o, names := range byOwner {
		switch {
		case best == "":
			best = o
		case len(names) > len(byOwner[best]):
			best = o
		case len(names) == len(byOwner[best]) && o < best:
			best = o
		}
	}
	return best, byOwner[best]
}

// authorizedRepos returns the conversation's authorized repository set as
// registry rows: every distinct repository in the conversation_worktrees ledger
// of the conversation and of the rest of its task's conversations, PLUS the
// task's own primary repository — all filtered to what the team tracks. The
// conversation_worktrees half is the credential-minting mirror of
// gitAuthorizeDecision (internal/delegate/spawner.go), which enforces the
// same intersection live at the git proxy; minting outside that set would
// be pointless (the proxy would 403 it anyway). The task-repo half exists
// because provisioning happens BEFORE the conversation's very first clone —
// conversation_worktrees has no rows yet for a fresh claim, only for a resumed
// conversation or one that already cloned before a refresh — so without it, a
// fresh conversation's initial host-side clone (setupGitHub's clone auth,
// before any worktree exists) would get no token at all.
//
// Every input already names its repository by registry id — the task's entity
// (entities.repository_id), each ledger row (repository_id) — so the set is
// built, deduplicated and checked against the team's tracking
// (TracksRepositorySystem) by id. A row's name is read only where a token is
// minted for it, which is the name it carries now.
//
// A conversation with no GitHub anchor — a Slack mention, a Jira issue, a taskless
// conversation — has neither a GitHub task-repo nor any worktree, so the set above is
// empty and its bundle would carry no GitHub credential at all: it couldn't
// read a PR diff, check a CI status, or `workspace add` a repo. Such a conversation
// falls back to the team's whole tracked set so it can reach the repos its
// team operates on. This widens READ/API reach, not push authority: the
// tokens stay per-repo scoped (resolveGitHub), and pushes remain gated by the
// conversation_worktrees ledger a `workspace add` creates. The boundary is the team's
// own tracked repos — never another team's, never another org's.
//
// Every repository is checked against the team's tracking on the org's
// current GitHub host, the host the bundle's tokens are minted on: a
// repository the team tracked on a host the org has left is not one this
// bundle can reach. A task or checkout on another host is dropped, and it
// still anchors the conversation, so a conversation anchored only on another
// host gets no repositories rather than the team's whole tracked set. A
// retired repository is dropped the same way: a token minted for its name
// would reach the repository GitHub serves under that name now.
func (m *Manager) authorizedRepos(ctx context.Context, orgID, teamID, taskID, conversationID string) ([]domain.Repository, error) {
	if m.stores.TeamGitHubRepos == nil {
		return nil, nil
	}
	host, err := db.OrgGitHubHostSystem(ctx, m.stores.Orgs, orgID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []domain.Repository
	// anchoredElsewhere records an anchor this bundle cannot reach: a row on
	// another host, a retired row, or a row that cannot be read.
	anchoredElsewhere := false
	add := func(row *domain.Repository) {
		if row == nil || row.Host != host || row.RetiredAt != nil {
			anchoredElsewhere = true
			return
		}
		if seen[row.ID] {
			return
		}
		tracks, err := m.stores.TeamGitHubRepos.TracksRepositorySystem(ctx, teamID, row.ID)
		if err != nil || !tracks {
			return
		}
		seen[row.ID] = true
		out = append(out, *row)
	}

	if row := m.taskGitHubRepo(ctx, orgID, taskID); row != nil {
		add(row)
	}
	if m.stores.ConversationWorktrees != nil {
		rows, err := m.stores.ConversationWorktrees.ListSystem(ctx, orgID, conversationID)
		if err != nil {
			return nil, err
		}
		// The rest of the task's conversations' checkouts too: they share one
		// run tree, so this conversation can be the one that restores them from
		// the task's snapshot — whose rebuild runs through this bundle — and the
		// bundle is sealed before it records them as its own.
		if taskID != "" {
			taskRows, err := m.stores.ConversationWorktrees.ListForTaskSystem(ctx, orgID, taskID)
			if err != nil {
				return nil, err
			}
			rows = append(rows, taskRows...)
		}
		for _, w := range rows {
			add(m.repositoryRow(ctx, orgID, w.RepositoryID))
		}
	}

	// Unanchored conversation: no task-repo, no worktree. Grant the team's tracked set
	// (already the tracking source of truth, so no per-repo TracksRepositorySystem
	// re-check) rather than shipping a bundle with no GitHub credential at all.
	if len(out) == 0 && !anchoredElsewhere {
		tracked, err := m.stores.TeamGitHubRepos.ListForTeamSystem(ctx, teamID, host)
		if err != nil {
			return nil, fmt.Errorf("list team tracked repos: %w", err)
		}
		for _, t := range tracked {
			// A retired row's name belongs to another repository now, one the
			// team never chose to track.
			if t.RetiredAt != nil {
				continue
			}
			out = append(out, domain.Repository{ID: t.RepositoryID, ExternalID: t.ExternalID, Source: domain.RepoSourceGitHub, Host: host, Owner: t.Owner, Repo: t.Repo})
		}
	}
	return out, nil
}

// repositoryRow is the registry row id names, or nil when there is no id or
// the read fails: the caller treats an unreadable row as one it cannot reach.
func (m *Manager) repositoryRow(ctx context.Context, orgID, id string) *domain.Repository {
	if m.stores.Repos == nil || id == "" {
		return nil
	}
	row, err := m.stores.Repos.GetSystem(ctx, orgID, id)
	if err != nil {
		return nil
	}
	return row
}

// taskPrimaryRepo is the "owner/repo" of the task's own repository when its
// registry row is on host and not retired — a pull request from another GitHub
// host is about another repository, whatever its name — and "" otherwise.
func (m *Manager) taskPrimaryRepo(ctx context.Context, orgID, taskID, host string) string {
	row := m.taskGitHubRepo(ctx, orgID, taskID)
	if row == nil || row.Host != host || row.RetiredAt != nil {
		return ""
	}
	return row.Slug()
}

// taskGitHubRepo resolves a GitHub task's own repository: the registry row the
// task's entity references (entities.repository_id), read by that id, so the
// name is the repository's now whatever the entity's key still spells. nil for
// a non-GitHub task, whose entity references no repository, or when any read
// fails.
func (m *Manager) taskGitHubRepo(ctx context.Context, orgID, taskID string) *domain.Repository {
	if m.stores.Tasks == nil || m.stores.Entities == nil || taskID == "" {
		return nil
	}
	task, err := m.stores.Tasks.GetSystem(ctx, orgID, taskID)
	if err != nil || task == nil || task.EntitySource != "github" {
		return nil
	}
	entity, err := m.stores.Entities.GetSystem(ctx, orgID, task.EntityID)
	if err != nil || entity == nil {
		return nil
	}
	return m.repositoryRow(ctx, orgID, entity.RepositoryID)
}

// resolveJira resolves the org's Jira service credential to its raw,
// serializable fields — nil when the org has no Jira configured (not an
// error: most orgs are GitHub-only).
func (m *Manager) resolveJira(ctx context.Context, orgID string) (*credbundle.JiraCreds, error) {
	r, ok := m.jiraResolver.(jiraSystemResolver)
	if !ok {
		return nil, nil
	}
	cred, err := r.ResolveSystemCredential(ctx, orgID)
	if err != nil {
		if errors.Is(err, jira.ErrNoJiraSystemCredential) {
			return nil, nil
		}
		return nil, err
	}
	authMethod := "datacenter"
	if cred.Deployment == jira.DeploymentCloud {
		authMethod = "cloud"
	}
	return &credbundle.JiraCreds{
		URL:        cred.URL,
		AuthMethod: authMethod,
		Email:      cred.Email,
		APIToken:   cred.APIToken,
		PAT:        cred.PAT,
	}, nil
}

// resolveLinear resolves the org's Linear service credential to the fields
// the sidecar's Linear proxy injects: nil when the org has no Linear
// configured (not an error, as with Jira), and an error the caller degrades on
// for anything else. An app-installed org seals the
// access token the shared token cache holds, with that token's expiry; the
// cache refreshes first when the one it has has less than LinearTokenHeadroom
// left. The install's refresh token never enters the bundle. The refresh
// sweep's re-provision reaches here too, so a long run's bundle picks up each
// token the cache moves on to.
func (m *Manager) resolveLinear(ctx context.Context, orgID string) (*credbundle.LinearCreds, error) {
	if m.linearResolver == nil {
		return nil, nil
	}
	cred, err := m.linearResolver.ResolveSystemCredential(ctx, orgID)
	if err != nil {
		if errors.Is(err, linear.ErrNoLinearSystemCredential) {
			return nil, nil
		}
		return nil, err
	}
	out := &credbundle.LinearCreds{AuthMethod: string(cred.Method)}
	switch cred.Method {
	case linear.AuthMethodAPIKey:
		out.APIKey = cred.APIKey
	case linear.AuthMethodAppInstall:
		out.AccessToken = cred.AccessToken
		if !cred.ExpiresAt.IsZero() {
			out.ExpiresUnix = cred.ExpiresAt.Unix()
		}
	default:
		return nil, fmt.Errorf("linear credential for org %s has unknown auth method %q", orgID, cred.Method)
	}
	return out, nil
}
