// Package credprovision is the brain-side half of TFAC-614's sealed
// per-claim credential channel: it resolves a claimed conversation's
// LLM/GitHub/Jira credentials (using the real, key-bearing secret store only the brain
// holds), seals them to the claiming executor's published X25519 public
// key (credseal), and writes the result to claim_credentials for that
// executor to unseal.
//
// Manager is brain-unit member the same way the fleet reaper and drain
// sweeper are (internal/app's startBrain/stopBrain): constructed only for
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
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
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

// jiraSystemResolver is a structural (unexported, anonymous-interface-
// compatible) view of jira.Resolver's optional ResolveSystemCredential
// extension — mirrors internal/github's ScopedResolver pattern. Declared
// locally so this package can type-assert without jira exporting its own
// interface name.
type jiraSystemResolver interface {
	ResolveSystemCredential(ctx context.Context, orgID string) (jira.SystemCredential, error)
}

// Manager resolves and seals per-claim credential bundles. Holds the real,
// key-bearing db.Stores (brain-side only) plus the GitHub/Jira resolvers
// built against it.
type Manager struct {
	stores       db.Stores
	ghResolver   ghclient.Resolver
	jiraResolver jira.Resolver
	llm          llmResolver
}

// NewManager builds a Manager against stores — a brain-capable role's
// normal (secret-bearing) db.Stores, never the disabled-secrets bundle an
// executor holds. llm is the shared LLM-credential resolver
// (internal/llmcred): for a role-mode Bedrock org it mints a short-lived STS
// session credential (executor-bound: per-conversation session name + the executor
// egress network condition); for every other mode it passes the stored
// material through. nil falls back to agentproc's raw-secret resolution (no
// role support) — only for callers that don't wire llmcred.
func NewManager(stores db.Stores, llm llmResolver) *Manager {
	return &Manager{
		stores: stores,
		// Deployment App from the env, same as the API server's resolver: this is
		// the brain-side resolver that seals a run's git credential, so a managed
		// org's run gets a token minted from the shared App rather than nothing.
		ghResolver:   ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, nil, ghclient.WithDeploymentAppFromEnv()),
		jiraResolver: jira.NewResolver(stores.Secrets, stores.Orgs),
		llm:          llm,
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
		// this boot is waiting on this conversation; the reaper's stale-
		// heartbeat sweep is what recovers it (TFAC-586), not this
		// function. Sealing against the new epoch here would be wrong:
		// the executor compares a bundle's epoch against its OWN current
		// one, and inst.BootEpoch (the pubkey's owner) is authoritative,
		// but writing it under a claim that names an EARLIER epoch would
		// desync the executor's understanding of which claim this bundle
		// answers. Skip and let the reaper requeue.
		log.Warn("claiming executor's boot epoch has moved on since this conversation was claimed; skipping (reaper will requeue)",
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

	if gh, err := m.resolveGitHub(ctx, orgID, claim.TeamID, claim.TaskID, conversationID); err != nil {
		return fmt.Errorf("credprovision: resolve github credentials for conversation %s: %w", conversationID, err)
	} else {
		bundle.GitHub = gh
	}

	if jc, err := m.resolveJira(ctx, orgID); err != nil {
		return fmt.Errorf("credprovision: resolve jira credentials for org %s: %w", orgID, err)
	} else {
		bundle.Jira = jc
	}

	// Every first-class provider beyond the built-in GitHub/Jira (Slack, and
	// any future one) resolves its own sealed keyed set through its registered
	// resolver — the brain never imports the provider package, so core stays
	// free of provider-specific credential symbols. A provider with nothing
	// configured for this org/team is simply absent from the map.
	if providers, err := agenthost.ResolveProviderCredentials(ctx, m.stores, agenthost.ProvisionScope{
		OrgID: orgID, TeamID: claim.TeamID, TaskID: claim.TaskID, ConversationID: conversationID,
	}); err != nil {
		return fmt.Errorf("credprovision: resolve provider credentials for conversation %s: %w", conversationID, err)
	} else {
		bundle.Providers = providers
	}

	plaintext, err := bundle.Marshal()
	if err != nil {
		return fmt.Errorf("credprovision: marshal bundle for conversation %s: %w", conversationID, err)
	}
	sealed, err := credseal.Seal(pub, plaintext)
	if err != nil {
		return fmt.Errorf("credprovision: seal bundle for conversation %s: %w", conversationID, err)
	}
	if err := m.stores.ClaimCredentials.Put(ctx, orgID, conversationID, claim.ExecutorID, inst.BootEpoch, sealed, m.includeToolsFor(ctx, orgID, conversationID)); err != nil {
		return fmt.Errorf("credprovision: write bundle for conversation %s: %w", conversationID, err)
	}
	log.Debug("provisioned claim credential bundle", "conversation", conversationID, "org", orgID, "executor", claim.ExecutorID, "boot_epoch", inst.BootEpoch)
	span.SetAttributes(telemetry.Outcome("sealed"))
	return nil
}

// includeToolsFor resolves the claim's cleartext tools manifest — the
// event-source kinds available to the org right now — for the brain to stamp
// beside the sealed bundle (claim_credentials.include_tools). The executor
// composes the run's <tools> prompt section from it; it cannot resolve
// availability itself (disabled secret store, and no user identity on an
// event-triggered conversation), which is the whole reason the answer ships
// with the bundle.
//
// nil on a failed resolve, never a partial or empty answer standing in for
// one: the stamp is documentation, not a gate, and the executor reads an
// absent manifest as "fall back to the run's own sources" — the same
// degradation a failed resolve has always produced — while an empty one would
// read as "nothing is available". A failure therefore costs nothing that
// wasn't already the status quo, so it is logged and never fails the
// provision.
func (m *Manager) includeToolsFor(ctx context.Context, orgID, conversationID string) []string {
	// Nil-safe: a partially-wired Stores bundle cannot run the availability
	// probes, so it stamps no answer — the same degradation as a failed
	// resolve, without reaching a nil store.
	if m.stores.Secrets == nil || m.stores.OrgEventSources == nil {
		return nil
	}
	kinds, err := eventsource.AvailableKindsSystem(ctx, m.stores, orgID)
	if err != nil {
		log.Warn("resolve event-source availability for the claim's tools manifest failed; the executor will fall back to the run's own sources",
			"org", orgID, "conversation", conversationID, "error", err)
		return nil
	}
	if kinds == nil {
		kinds = []string{}
	}
	return kinds
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

	repoIDs, err := m.authorizedRepos(ctx, orgID, teamID, taskID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("resolve authorized repo set: %w", err)
	}

	gh := &credbundle.GitHubCreds{Mode: credbundle.GitHubModeApp, RepoTokens: map[string]credbundle.RepoToken{}}
	if base, err := m.ghResolver.BaseURLFor(ctx, orgID); err == nil {
		gh.BaseURL = base
	}
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
	for _, repoID := range repoIDs {
		owner, repo, ok := strings.Cut(repoID, "/")
		if !ok {
			continue
		}
		tok, err := ghclient.TokenForManagedGit(ctx, scoped, orgID, owner, repo)
		if err != nil {
			if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				continue
			}
			return nil, fmt.Errorf("mint token for %s: %w", repoID, err)
		}
		if tok.ExpiresAt.IsZero() {
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
	if owner, names := cliChannelScope(m.taskPrimaryRepo(ctx, orgID, taskID), repoIDs); owner != "" {
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

// authorizedRepos returns the conversation's authorized repo set as "owner/repo"
// strings: every distinct repo in the conversation's conversation_worktrees ledger, PLUS the
// task's own primary repo — both filtered to what the team tracks. The
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
// A conversation with no GitHub anchor — a Slack mention, a Jira issue, a taskless
// conversation — has neither a GitHub task-repo nor any worktree, so the set above is
// empty and its bundle would carry no GitHub credential at all: it couldn't
// read a PR diff, check a CI status, or `workspace add` a repo. Such a conversation
// falls back to the team's whole tracked set so it can reach the repos its
// team operates on. This widens READ/API reach, not push authority: the
// tokens stay per-repo scoped (resolveGitHub), and pushes remain gated by the
// conversation_worktrees ledger a `workspace add` creates. The boundary is the team's
// own tracked repos — never another team's, never another org's.
func (m *Manager) authorizedRepos(ctx context.Context, orgID, teamID, taskID, conversationID string) ([]string, error) {
	if m.stores.TeamGitHubRepos == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(repoID string) {
		key := strings.ToLower(repoID)
		if seen[key] {
			return
		}
		owner, repo, ok := strings.Cut(repoID, "/")
		if !ok {
			return
		}
		tracks, err := m.stores.TeamGitHubRepos.TracksRepoSystem(ctx, teamID, owner, repo)
		if err != nil || !tracks {
			return
		}
		seen[key] = true
		out = append(out, repoID)
	}

	if repoID := m.taskPrimaryRepo(ctx, orgID, taskID); repoID != "" {
		add(repoID)
	}
	if m.stores.ConversationWorktrees != nil {
		rows, err := m.stores.ConversationWorktrees.ListSystem(ctx, orgID, conversationID)
		if err != nil {
			return nil, err
		}
		for _, w := range rows {
			add(w.RepoID)
		}
	}

	// Unanchored conversation: no task-repo, no worktree. Grant the team's tracked set
	// (already the tracking source of truth, so no per-repo TracksRepoSystem
	// re-check) rather than shipping a bundle with no GitHub credential at all.
	if len(out) == 0 {
		tracked, err := m.stores.TeamGitHubRepos.ListForTeamSystem(ctx, teamID)
		if err != nil {
			return nil, fmt.Errorf("list team tracked repos: %w", err)
		}
		for _, t := range tracked {
			out = append(out, t.Slug())
		}
	}
	return out, nil
}

// taskPrimaryRepo resolves a GitHub task's own target repo as "owner/repo"
// from its entity source id ("owner/repo#42" for a PR/issue task) — the
// same parse delegate.ownerRepoForTask uses, duplicated here rather
// than exported since it's a two-line string split and importing
// internal/delegate from here would be a layering inversion (delegate is
// the executor-side consumer, not something the brain-side provisioner
// should depend on). "" for a non-GitHub task (e.g. Jira) or a malformed id.
func (m *Manager) taskPrimaryRepo(ctx context.Context, orgID, taskID string) string {
	if m.stores.Tasks == nil || taskID == "" {
		return ""
	}
	task, err := m.stores.Tasks.GetSystem(ctx, orgID, taskID)
	if err != nil || task == nil || task.EntitySource != "github" {
		return ""
	}
	repoStr := task.EntitySourceID
	if idx := strings.LastIndex(repoStr, "#"); idx >= 0 {
		repoStr = repoStr[:idx]
	}
	owner, repo, ok := strings.Cut(repoStr, "/")
	if !ok || owner == "" || repo == "" {
		return ""
	}
	return repoStr
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
