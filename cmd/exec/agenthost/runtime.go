package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// Runtime is the seam every DB-backed effect the exec verb trace performs
// routes through, so the SAME LocalClient logic runs in two placements:
//
//   - directRuntime (all/local, and the orchestrator serving a relayed op) —
//     the effect hits db.Stores in-process under the run's ConversationInfo, with the
//     event-vs-manual pool/tx branch it always had.
//   - relayRuntime (the capless per-run sidecar) — the effect is a narrow,
//     org-bound relay to the orchestrator over the supervision channel; the
//     sidecar holds no db.Stores and opens no DB connection.
//
// The split is deliberately at OP granularity, not store-method granularity:
// a write funnel that composes an artifact upsert + an external-action append
// inside one SyntheticClaimsWithTx (UpsertArtifact, Record) relays as ONE op,
// so the transaction stays orchestrator-side — a tx closure cannot cross the
// wire. Pure reads relay one-for-one.
type Runtime interface {
	Info() ConversationInfo

	// Reads.
	ListConversationArtifacts(ctx context.Context) ([]domain.Artifact, error)
	GetConversation(ctx context.Context) (*domain.Conversation, error)
	GetTask(ctx context.Context, taskID string) (*domain.Task, error)
	// ListRepos lists the repositories the run's team tracks — the set
	// TeamTracksRepo admits, so every repository `workspace list` offers is
	// one `workspace add` will materialize.
	ListRepos(ctx context.Context) ([]domain.Repository, error)

	// GetRepo takes the "owner/repo" the agent typed and resolves it against
	// the registry, or nil when nothing answers to that name. The agent's
	// surface speaks names throughout — this is where one stops being a name.
	GetRepo(ctx context.Context, slug string) (*domain.Repository, error)

	TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error)

	// TaskOwnRepo reports whether owner/repo is the run's own task repo — the
	// repo gate's supplement to the worktree ledger (see IsTaskOwnRepo).
	//
	// It is a relayed op rather than a field on ConversationInfo because it is
	// an AUTHORIZATION input and the sidecar is not authoritative for those:
	// AgentHostInfo is the run's non-secret identity as the sidecar reports it,
	// and the orchestrator deliberately re-binds every op's identity from its
	// own ConversationInfo so a sidecar cannot escalate by lying. A stamped
	// task repo would be exactly such a lie. The answer is asked for only on a
	// ledger miss, so the authorized path costs nothing.
	TaskOwnRepo(ctx context.Context, owner, repo string) (bool, error)
	GetConversationWorktreeByRepoRef(ctx context.Context, repositoryID, ref string) (*domain.ConversationWorktree, error)
	ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error)
	OrgJiraBaseURL(ctx context.Context) (string, error)

	// ReviewPosture resolves the review-posting decision inputs for owner/repo:
	// the run team's configured posture, and the identity of the credential that
	// would post the review. Both live where the stores and the
	// credential resolver do, so the capless sidecar relays for them — its own
	// gh clients speak to per-run REST proxies whose reported identity is
	// descriptive only, never the real App-vs-PAT tier.
	ReviewPosture(ctx context.Context, owner, repo string) (ReviewPostureResolution, error)

	// Writes.
	InsertConversationWorktree(ctx context.Context, row domain.ConversationWorktree) (inserted bool, winningPath string, err error)
	DeleteConversationWorktree(ctx context.Context, repositoryID, ref string) error
	UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error)
	// UpdateReviewDetailsIfPending persists a review draft's mutated
	// details_json, guarded on the draft still being state=pending. Returns
	// false when the draft was resolved (submitted/dismissed) since it was read
	// — the writer surfaces the lost race instead of resurrecting the resolved
	// review, which the unconditional UpsertArtifact (state last-writer-wins)
	// would do with its stale in-memory pending state.
	UpdateReviewDetailsIfPending(ctx context.Context, artifactID, detailsJSON string) (bool, error)
	// TransitionReviewState is the review artifact's compare-and-swap: the row
	// moves from → to only while it still holds `from`, optionally stamping the
	// resolved review's coordinates in the same write. It is the claim primitive
	// the auto-posting posture takes BEFORE calling GitHub — a finalized draft is
	// simultaneously reachable by a human in the approval queue, and both callers
	// submit the same review, so exactly one of them has to win a CAS or the
	// review posts twice. Returns false when the race was lost (or the row is
	// gone), never an error.
	TransitionReviewState(ctx context.Context, artifactID, from, to, externalID, url, detailsJSON string) (bool, error)
	// Record is the void, best-effort external-write funnel (RecordExternalWrite):
	// a recording failure is logged host-side and never fails the agent's
	// already-applied action, so it needs no error return. linearTeamID is the
	// Linear team of the issue a Linear write touched, read off the response
	// the verb acted on, and "" for every other write: the write's touch
	// creates the issue's entity when TF has none, and a Linear entity is
	// created with its team.
	Record(ctx context.Context, a *domain.Artifact, act *domain.ExternalAction, linearTeamID string)

	// RecordReadTouch persists a durable conversation→entity touch for an addressed read
	// (a verb targeting one entity by id/key/ts): it resolves-or-creates the
	// entity for (provider, target, url) and records a role='touched' row.
	// externalID is the provider id the read's response named, for a provider
	// whose entities are identified by one (a Jira issue's id), and "" for the
	// rest; a Jira read with none touches nothing. linearTeamID is the Linear
	// team the response named, for a Linear read, and "" for the rest, as on
	// Record. Void and best-effort like Record — a read never fails on its
	// touch — and, like Record, relayed to the orchestrator on the sidecar so
	// the write lands where the stores live. Set-returning reads never call it
	// (the touched-entity rule: addressed → touch, returned-in-a-set → never).
	RecordReadTouch(ctx context.Context, provider, target, externalID, linearTeamID, url string)

	// MemoryLoad resolves the entity for (source, sourceID) — by externalID,
	// the provider id the entity is identified by, when the caller resolved
	// one, else by its natural key — LOOKUP ONLY, it never mints an entity —
	// and, on a hit, returns that
	// entity's prior conversation memory (team-visibility-scoped to the conversation's team,
	// composed exactly as the spawn-time materializer composes it) capped at the
	// most recent `limit`, with Count the pre-limit scoped total. A hit records a
	// conversation→entity 'touched' row best-effort (loading IS an address); a miss
	// returns an empty result and records nothing. Relayed to the orchestrator on
	// the sidecar so the reads + the touch land where the stores live.
	MemoryLoad(ctx context.Context, source, sourceID, externalID string, limit int) (*MemoryLoadResult, error)

	// Relay / RelayNotify are the generic provider-op escape hatch: a provider
	// handler (Slack, future) reaches its own org-bound policy op by namespace
	// without a typed Runtime method per op. Core built-ins use the typed
	// methods above; providers use these. RelayNotify is for a provider's audit
	// record: never awaited, and on the sidecar delivered like every other
	// record — in order, resent until the orchestrator has applied it.
	Relay(ctx context.Context, namespace, op string, args, out any) error
	RelayNotify(ctx context.Context, namespace, op string, args any)

	// ProviderCredential returns the provider's sealed credential keyed set (its
	// own opaque JSON), for the handler to SELECT a member locally — never
	// asking the orchestrator for a secret. On the sidecar this reads the bundle
	// the brain sealed at provision time; on all/local it live-resolves through
	// the provider's registered resolver against the live secret store. Same
	// shape both ways, so a handler selects identically in either placement.
	// A set it cannot produce is a *SourceRefusal from the refusal table: the
	// bundle's manifest says why on the sidecar, and on all/local an empty set
	// is unconfigured and a failed resolve unresolved.
	ProviderCredential(ctx context.Context, namespace string) (json.RawMessage, error)

	// CheckEntitlement reports whether the run's org holds the given feature.
	// The extension dispatch gates on this BEFORE running a provider handler.
	// It must relay on the sidecar (whose process has no entitlements provider —
	// only the orchestrator's does), so it is a runtime method, not a direct
	// entitlements.For() call in callExtension.
	CheckEntitlement(ctx context.Context, feature string) (bool, error)

	// SourceDisabled reports whether an org admin has turned the named event
	// source off for the run's org. Every GitHub and Jira verb resolves its
	// credential behind this, which is what makes the switch reach an agent
	// that is ALREADY running rather than only the next one to start.
	//
	// It relays on the sidecar for the same reason CheckEntitlement does: the
	// policy lives in a database that process does not have. And it is read
	// per call rather than cached for the life of a run, because a cache is a
	// window in which a source an admin turned off still answers, and the read
	// is one indexed row against the API round trip it guards.
	SourceDisabled(ctx context.Context, kind string) (bool, error)

	// SourceManifest answers which sources the run can reach and, for each
	// it cannot, why not — what the exec help index and every source refusal
	// explain from. On the sidecar it is the held bundle's manifest, read
	// in-process (the brain sealed it beside the credentials, so the answer
	// is already here and nothing relays); a sealed credential past its expiry
	// reads as unresolved. The direct runtime has no bundle and builds the
	// same entries from a live availability resolve (LiveSourceManifest).
	SourceManifest(ctx context.Context) (domain.SourceManifest, error)
}

// ExtensionRuntime is the narrower runtime a provider's sidecar-half handler
// closes over — everything it needs (its identity, a relay to its org-bound
// policy ops, its sealed credential set, the audit funnel) and NOTHING
// host-specific (no db.Stores, no secret store). Runtime satisfies it, so the
// same handler runs over a direct runtime (all/local) or a relay runtime
// (sidecar) unchanged.
type ExtensionRuntime interface {
	Info() ConversationInfo
	Relay(ctx context.Context, namespace, op string, args, out any) error
	RelayNotify(ctx context.Context, namespace, op string, args any)
	ProviderCredential(ctx context.Context, namespace string) (json.RawMessage, error)
	Record(ctx context.Context, a *domain.Artifact, act *domain.ExternalAction, linearTeamID string)
	RecordReadTouch(ctx context.Context, provider, target, externalID, linearTeamID, url string)
}

// Core DB op names — the verb-trace reads/writes served under the "core"
// namespace, alongside the git ops (agentproc.Op*). Both the sidecar's
// relayRuntime (producer) and the orchestrator's RelayServer (consumer) live
// in this package, so these are package-local.
const (
	opGetConversation                  = "get_conversation"
	opGetTask                          = "get_task"
	opListRepos                        = "list_repos"
	opGetRepo                          = "get_repo"
	opTeamTracksRepo                   = "team_tracks_repo"
	opTaskOwnRepo                      = "task_own_repo"
	opGetConversationWorktreeByRepoRef = "get_conversation_worktree_by_repo_ref"
	opListConversationWorktrees        = "list_conversation_worktrees"
	opInsertConversationWorktree       = "insert_conversation_worktree"
	opDeleteConversationWorktree       = "delete_conversation_worktree"
	opListConversationArtifacts        = "list_conversation_artifacts"
	opOrgJiraBase                      = "org_jira_base"
	opUpsertArtifact                   = "upsert_artifact"
	opUpdateReviewDetails              = "update_review_details_if_pending"
	opTransitionReviewState            = "transition_review_state"
	opRecordExternalWrite              = "record_external_write"
	opRecordReadTouch                  = "record_read_touch"
	opMemoryLoad                       = "memory_load"
	opCheckEntitlement                 = "check_entitlement"
	opSourceDisabled                   = "source_disabled"
	opReviewPosture                    = "review_posture"
	// opCreateWorkspaceCheckout materializes a `workspace add` checkout. Unlike
	// the other core ops it is FS-bearing: the sidecar relays it because it owns
	// neither the shared bare cache nor the run-root; the orchestrator serves it.
	opCreateWorkspaceCheckout = "create_workspace_checkout"
)

// updateReviewDetailsArgs / updateReviewDetailsResult are the
// update_review_details_if_pending op's payloads — the draft's artifact id +
// its rewritten details_json, and whether the guarded write landed.
type updateReviewDetailsArgs struct {
	ArtifactID  string `json:"artifact_id"`
	DetailsJSON string `json:"details_json"`
}

type updateReviewDetailsResult struct {
	Updated bool `json:"updated"`
}

// transitionReviewStateArgs / transitionReviewStateResult are the
// transition_review_state op's payloads — the CAS's from/to states plus the
// optional coordinate stamp, and whether this caller won the transition.
type transitionReviewStateArgs struct {
	ArtifactID  string `json:"artifact_id"`
	From        string `json:"from"`
	To          string `json:"to"`
	ExternalID  string `json:"external_id,omitempty"`
	URL         string `json:"url,omitempty"`
	DetailsJSON string `json:"details_json,omitempty"`
}

type transitionReviewStateResult struct {
	Moved bool `json:"moved"`
}

// checkEntitlementArgs / checkEntitlementResult are the check_entitlement op's
// payloads — a feature name, and whether the run's org holds it.
type checkEntitlementArgs struct {
	Feature string `json:"feature"`
}

type checkEntitlementResult struct {
	Allowed bool `json:"allowed"`
}

// sourceDisabledArgs / sourceDisabledResult are the source_disabled op's
// payloads — a source kind, and whether an org admin has turned it off.
type sourceDisabledArgs struct {
	Kind string `json:"kind"`
}

type sourceDisabledResult struct {
	Disabled bool `json:"disabled"`
}

// sourceManifestResult is the SourceManifest IPC method's result. There are no
// args: identity is bound host-side from the run's ConversationInfo.
type sourceManifestResult struct {
	Sources domain.SourceManifest `json:"sources"`
}

// ReviewPostureResolution is what the review-posting decision reads from
// the team's configured posture (one of domain.ValidReviewPostures)
// and the acting credential's identity. It doubles as the review_posture op's
// result — Identity rides the wire as github.Identity's integer value, and its
// zero value IS IdentityUnknown, so a truncated or older peer decodes to the
// conservative case rather than a fabricated App.
type ReviewPostureResolution struct {
	Posture  string            `json:"posture"`
	Identity ghclient.Identity `json:"identity"`
}

// reviewPostureArgs is the review_posture op's payload — the repo whose acting
// credential is being classified. Team + org identity is bound orchestrator-side
// from the run's ConversationInfo, so the wire carries neither.
type reviewPostureArgs struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// listConversationArtifactsResult / orgJiraBaseResult / recordExternalWriteArgs are the
// relay payloads that don't already exist as an IPC arg/result in protocol.go.
type listConversationArtifactsResult struct {
	Artifacts []domain.Artifact `json:"artifacts"`
}

type orgJiraBaseResult struct {
	URL string `json:"url"`
}

type recordExternalWriteArgs struct {
	Artifact     *domain.Artifact       `json:"artifact,omitempty"`
	Action       *domain.ExternalAction `json:"action,omitempty"`
	LinearTeamID string                 `json:"linear_team_id,omitempty"`
}

// recordReadTouchArgs is the record_read_touch op's payload — the addressed
// read's entity coordinates. Identity (org, run) is bound orchestrator-side
// from the run's ConversationInfo, so the wire carries none. The IPC method of
// the same name shares the shape and ignores ExternalID: the provider id is
// read off a response by the daemon's own verbs, never named by the jailed
// CLI.
type recordReadTouchArgs struct {
	Provider     string `json:"provider"`
	Target       string `json:"target"`
	ExternalID   string `json:"external_id,omitempty"`
	LinearTeamID string `json:"linear_team_id,omitempty"`
	URL          string `json:"url,omitempty"`
}

// --- directRuntime: the in-process impl over db.Stores ---

// directRuntime is the all/local runtime AND the impl the orchestrator's
// RelayServer serves a relayed core op through — the same DB logic in both
// placements. Holds the run's stores + ConversationInfo; every method binds identity
// from info, never from a caller argument.
type directRuntime struct {
	stores db.Stores
	info   ConversationInfo

	// ghResolver classifies the acting GitHub credential for the review-posture
	// decision (ReviewPosture). Seeded by NewServer with the Server's shared
	// resolver so the App-coverage probe reuses its installation-token cache;
	// lazily built from stores when unset (the local-mode CLI's directRuntime,
	// which has no Server), and pre-set by tests.
	ghResolver ghclient.Resolver

	// ghCredential is the credential the branch-push row this runtime composes
	// into an artifact upsert is stamped with. Set through
	// LocalClient.SetGitHubCredential — from the sidecar's report on an
	// executor, from the resolved tier on every other placement. Empty falls
	// back to the resolver below, and to the App when even that can't answer.
	ghCredential string
}

func newDirectRuntime(stores db.Stores, info ConversationInfo) *directRuntime {
	return &directRuntime{stores: stores, info: info}
}

// githubResolver returns the seeded credential resolver, or one built from
// stores when nothing seeded it (the local-mode CLI's runtime, which has no
// Server to share one with).
func (r *directRuntime) githubResolver() ghclient.Resolver {
	if r.ghResolver != nil {
		return r.ghResolver
	}
	return ghclient.NewResolver(r.stores.Secrets, r.stores.GitHubApps, r.stores.Orgs, r.stores.Agents, nil)
}

func (r *directRuntime) Info() ConversationInfo { return r.info }

// ListConversationArtifacts respects the pool split: event-triggered runs read
// admin-pool (no JWT claims); manual runs read under the kicking-off user's
// synthetic claims. Mirrors withWriteInfo for the read side.
func (r *directRuntime) ListConversationArtifacts(ctx context.Context) ([]domain.Artifact, error) {
	if r.info.IsEventTriggered {
		return r.stores.Artifacts.ListByConversationSystem(ctx, r.info.OrgID, r.info.ConversationID)
	}
	var out []domain.Artifact
	err := r.stores.Tx.SyntheticClaimsWithReadTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
		a, e := ts.Artifacts.ListByConversation(ctx, r.info.OrgID, r.info.ConversationID)
		out = a
		return e
	})
	return out, err
}

func (r *directRuntime) GetConversation(ctx context.Context) (*domain.Conversation, error) {
	return r.stores.Conversations.GetSystem(ctx, r.info.OrgID, r.info.ConversationID)
}

func (r *directRuntime) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	return r.stores.Tasks.GetSystem(ctx, r.info.OrgID, taskID)
}

// githubHost is the org's current GitHub host — the host every repository a
// run lists, resolves or is gated on lives on. Read per call rather than held:
// the org can move hosts during a run, and a name the old host answered to may
// belong to another repository on the new one.
func (r *directRuntime) githubHost(ctx context.Context) (string, error) {
	return db.OrgGitHubHostSystem(ctx, r.stores.Orgs, r.info.OrgID)
}

func (r *directRuntime) ListRepos(ctx context.Context) ([]domain.Repository, error) {
	host, err := r.githubHost(ctx)
	if err != nil {
		return nil, err
	}
	repos, _, err := r.stores.Repos.ListTrackedByTeamSystem(ctx, r.info.OrgID, r.info.TeamID, host, db.Unwindowed)
	return repos, err
}

// GetRepo is the verb boundary for `tfac exec workspace add owner/repo`: the
// agent's argv arrives here as a name and is resolved to a row here, so the id
// is what everything past this point holds. A name nobody has a row for stays
// a nil rather than an error — the caller reports it as "repo is not
// configured in Triage Factory", which is the accurate answer. The name
// resolves on the org's current GitHub host only.
func (r *directRuntime) GetRepo(ctx context.Context, slug string) (*domain.Repository, error) {
	host, err := r.githubHost(ctx)
	if err != nil {
		return nil, err
	}
	return r.stores.Repos.GetByRefSystem(ctx, r.info.OrgID, domain.RepoRefFromSlug(host, slug))
}

func (r *directRuntime) TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error) {
	host, err := r.githubHost(ctx)
	if err != nil {
		return false, err
	}
	return r.stores.TeamGitHubRepos.TracksRepoSystem(ctx, r.info.TeamID, host, owner, repo)
}

func (r *directRuntime) TaskOwnRepo(ctx context.Context, owner, repo string) (bool, error) {
	host, err := r.githubHost(ctx)
	if err != nil {
		return false, err
	}
	return IsTaskOwnRepo(ctx, r.stores, r.info, host, owner, repo), nil
}

func (r *directRuntime) GetConversationWorktreeByRepoRef(ctx context.Context, repositoryID, ref string) (*domain.ConversationWorktree, error) {
	return r.stores.ConversationWorktrees.GetByRepoRefSystem(ctx, r.info.OrgID, r.info.ConversationID, repositoryID, ref)
}

func (r *directRuntime) ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error) {
	return r.stores.ConversationWorktrees.ListSystem(ctx, r.info.OrgID, r.info.ConversationID)
}

// OrgJiraBaseURL returns the org's configured Jira site URL (trailing slash
// trimmed), or "" when unset. Best-effort at the caller (jiraSiteBase swallows
// the error to ""), but the runtime propagates any store error so the relay
// path can distinguish a transient failure.
func (r *directRuntime) OrgJiraBaseURL(ctx context.Context) (string, error) {
	if r.stores.Orgs == nil {
		return "", nil
	}
	set, err := r.stores.Orgs.GetSettingsSystem(ctx, r.info.OrgID)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(set.JiraBaseURL, "/"), nil
}

// ReviewPosture reads the run team's posture and — only when the posture
// actually depends on it — classifies the credential that would post the
// review. The identity probe is skipped for the three fixed postures because it
// costs a resolver round-trip (and, on the App tier, a coverage probe) to
// produce a value nothing reads.
//
// A team with no settings row (or a column written before the default landed)
// reads as the empty string; it resolves to the default posture here rather
// than at every call site. A resolver failure is NOT an error: identity stays
// IdentityUnknown, which the decision treats as the conservative case, so a
// transient credential-backend blip stages a review instead of failing the
// agent's finalize outright.
func (r *directRuntime) ReviewPosture(ctx context.Context, owner, repo string) (ReviewPostureResolution, error) {
	out := ReviewPostureResolution{Posture: domain.DefaultReviewPosture}
	if r.stores.Teams != nil && r.info.TeamID != "" {
		set, err := r.stores.Teams.GetSettingsSystem(ctx, r.info.TeamID)
		if err != nil {
			return ReviewPostureResolution{}, err
		}
		if set.ReviewPosture != "" {
			out.Posture = set.ReviewPosture
		}
	}
	if out.Posture != domain.ReviewPostureIdentity {
		return out, nil
	}
	resolver := r.githubResolver()
	r.ghResolver = resolver
	ir, ok := resolver.(ghclient.RepoIdentityResolver)
	if !ok {
		return out, nil
	}
	_, identity, err := ir.ClientForRepoWithIdentity(ctx, r.info.OrgID, owner, repo)
	if err != nil {
		agenthostLog.Warn("review posture: credential identity unresolved; treating as unknown (review will be staged)",
			"conversation", r.info.ConversationID, "owner", owner, "repo", repo, "error", err)
		return out, nil
	}
	out.Identity = identity
	return out, nil
}

func (r *directRuntime) InsertConversationWorktree(ctx context.Context, row domain.ConversationWorktree) (bool, string, error) {
	// Identity comes from the server-bound ConversationInfo, never from the
	// wire. Besides owning the reservation row, this value also attributes the
	// cred_request doorbell emitted by the Postgres store on a fresh insert.
	row.ConversationID = r.info.ConversationID
	if r.info.IsEventTriggered {
		return r.stores.ConversationWorktrees.InsertSystem(ctx, r.info.OrgID, row)
	}
	var (
		inserted    bool
		winningPath string
	)
	err := r.stores.Tx.SyntheticClaimsWithTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
		i, w, ierr := ts.ConversationWorktrees.Insert(ctx, r.info.OrgID, row)
		inserted = i
		winningPath = w
		return ierr
	})
	return inserted, winningPath, err
}

func (r *directRuntime) DeleteConversationWorktree(ctx context.Context, repositoryID, ref string) error {
	return withWriteInfo(ctx, r.stores, r.info,
		func() error {
			return r.stores.ConversationWorktrees.DeleteByRepoRefSystem(ctx, r.info.OrgID, r.info.ConversationID, repositoryID, ref)
		},
		func(ts db.TxStores) error {
			return ts.ConversationWorktrees.DeleteByRepoRef(ctx, r.info.OrgID, r.info.ConversationID, repositoryID, ref)
		},
	)
}

// UpsertArtifact stamps the run identity and the org's current scope
// (stampScopeInfo) onto a and upserts it, composing the branch-push external
// action into the SAME write. Event-triggered runs route admin-pool; manual
// runs wrap in the kicking-off user's synthetic claims. Returns the stored
// row. Each write is retried on a retryable error like the recording funnel's
// (withRecordRetry) — the upsert is idempotent and the push row's dedup key is
// deterministic — but on the caller's own context, since the caller is waiting
// for the result. When the artifact write fails, the action is written on its own
// (recordActionAlone) before the artifact's error is returned, so a push whose
// artifact could not be recorded still leaves its ledger row. Inside a relayed
// record the artifact's failure is also the record's (dropRelayedArtifact).
func (r *directRuntime) UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error) {
	a.OrgID = r.info.OrgID
	a.TeamID = r.info.TeamID
	a.ConversationID = r.info.ConversationID
	act := branchPushActionInfo(a, r.info, r.githubCredential(ctx, a))
	if err := withRecordRetry(ctx, func() error { return stampScopeInfo(ctx, r.stores, r.info.OrgID, &a, act) }); err != nil {
		r.dropBranchPush(ctx, a, act, err)
		return domain.Artifact{}, err
	}
	if r.info.IsEventTriggered {
		var stored domain.Artifact
		err := withRecordRetry(ctx, func() error {
			var e error
			stored, e = r.stores.Artifacts.UpsertSystem(ctx, r.info.OrgID, a)
			return e
		})
		if err != nil {
			dropRelayedArtifact(ctx, a, err)
			r.recordBranchPushAlone(ctx, a, act)
			return domain.Artifact{}, err
		}
		if rerr := withRecordRetry(ctx, func() error { return recordActionSystemInfo(ctx, r.stores, r.info, act) }); rerr != nil {
			r.dropBranchPush(ctx, a, act, rerr)
		}
		return stored, nil
	}
	var out domain.Artifact
	var artifactErr error
	err := withRecordRetry(ctx, func() error {
		return r.stores.Tx.SyntheticClaimsWithTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
			artifactErr = nil
			stored, uerr := ts.Artifacts.Upsert(ctx, r.info.OrgID, a)
			if uerr != nil {
				artifactErr = uerr
				return uerr
			}
			out = stored
			return recordActionTx(ctx, ts, r.info.OrgID, act)
		})
	})
	if artifactErr != nil {
		dropRelayedArtifact(ctx, a, artifactErr)
		r.recordBranchPushAlone(ctx, a, act)
	} else if err != nil {
		r.dropBranchPush(ctx, a, act, err)
	}
	return out, err
}

// recordBranchPushAlone writes the branch-push row of an artifact whose own
// write failed, best-effort. A nil act (an artifact that is not a branch) has
// nothing to write.
func (r *directRuntime) recordBranchPushAlone(ctx context.Context, a domain.Artifact, act *domain.ExternalAction) {
	if act == nil {
		return
	}
	if err := recordActionAlone(ctx, r.stores, r.info, act); err != nil {
		r.dropBranchPush(ctx, a, act, err)
	}
}

// dropBranchPush accounts for a branch-push row that was not written, cause
// being the error that lost it. A nil act (an artifact that is not a branch)
// lost no row.
func (r *directRuntime) dropBranchPush(ctx context.Context, a domain.Artifact, act *domain.ExternalAction, cause error) {
	if act == nil {
		return
	}
	recordWriteDrop(ctx, recordDropOp(nil, act), cause)
	agenthostLog.Warn("branch external-action recording failed (push already applied)",
		"conversation", r.info.ConversationID, "target", a.Target, "error", cause)
}

// dropRelayedArtifact accounts for an artifact that a relayed record — a
// sidecar's record_push — failed to write, cause being the error that lost it.
// There the artifact is the record's loss like the ledger row beside it, as it
// is in the recording funnel: held until the record's outcome is known, and
// deferring the record when another attempt can land it, so the sidecar's
// resend retries the artifact too. Every other caller of UpsertArtifact is
// waiting on its result and gets the error back to handle, so outside a
// relayed record this does nothing.
func dropRelayedArtifact(ctx context.Context, a domain.Artifact, cause error) {
	if relayedRecordFrom(ctx) == nil {
		return
	}
	recordWriteDrop(ctx, recordDropOp(&a, nil), cause)
}

// githubCredential names the credential to stamp on the branch-push row for
// artifact a. A credential reported by whoever holds it wins outright — on an
// executor that is the sidecar, and nothing this process can read would confirm
// or contradict it.
//
// The resolver fallback exists for one caller: the local-mode pre-push hook,
// which upserts a branch artifact in a process that never resolved a gh client,
// so nothing has classified the tier by the time the row is built. It is asked
// only for a branch artifact — the sole kind that yields an action here — and
// answers from the same resolver that minted the token the push authenticated
// with. Failure is silent and reads as the App: a push that already landed must
// not be held up, or logged twice, over how its row is labeled.
func (r *directRuntime) githubCredential(ctx context.Context, a domain.Artifact) string {
	if r.ghCredential != "" {
		return r.ghCredential
	}
	if a.Kind != domain.ArtifactKindBranch {
		return domain.CredentialGitHubApp
	}
	owner, repo, ok := strings.Cut(a.Target, "/")
	if !ok {
		return domain.CredentialGitHubApp
	}
	ir, ok := r.githubResolver().(ghclient.RepoIdentityResolver)
	if !ok {
		return domain.CredentialGitHubApp
	}
	_, identity, err := ir.ClientForRepoWithIdentity(ctx, r.info.OrgID, owner, repo)
	if err != nil {
		return domain.CredentialGitHubApp
	}
	return identity.Credential()
}

// UpdateReviewDetailsIfPending routes the guarded draft write through the same
// event-vs-manual pool split UpsertArtifact uses: event-triggered runs write
// admin-pool (no JWT claims); manual runs write under the kicking-off user's
// synthetic claims.
func (r *directRuntime) UpdateReviewDetailsIfPending(ctx context.Context, artifactID, detailsJSON string) (bool, error) {
	if r.info.IsEventTriggered {
		return r.stores.Artifacts.UpdateReviewDetailsIfPendingSystem(ctx, r.info.OrgID, artifactID, detailsJSON)
	}
	var updated bool
	err := r.stores.Tx.SyntheticClaimsWithTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
		u, e := ts.Artifacts.UpdateReviewDetailsIfPending(ctx, r.info.OrgID, artifactID, detailsJSON)
		updated = u
		return e
	})
	return updated, err
}

// TransitionReviewState routes the review CAS through the same event-vs-manual
// pool split UpdateReviewDetailsIfPending uses: event-triggered runs claim on
// the admin pool (no JWT claims); manual runs claim under the kicking-off user's
// synthetic claims — the same pool the approve handler races on, which is what
// makes the two callers contend for one row rather than pass each other.
func (r *directRuntime) TransitionReviewState(ctx context.Context, artifactID, from, to, externalID, url, detailsJSON string) (bool, error) {
	if r.info.IsEventTriggered {
		return r.stores.Artifacts.TransitionReviewStateSystem(ctx, r.info.OrgID, artifactID, from, to, externalID, url, detailsJSON)
	}
	var moved bool
	err := r.stores.Tx.SyntheticClaimsWithTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
		m, e := ts.Artifacts.TransitionReviewState(ctx, r.info.OrgID, artifactID, from, to, externalID, url, detailsJSON)
		moved = m
		return e
	})
	return moved, err
}

func (r *directRuntime) Record(ctx context.Context, a *domain.Artifact, act *domain.ExternalAction, linearTeamID string) {
	recordExternalWrite(ctx, r.stores, r.info, a, act, linearTeamID)
}

func (r *directRuntime) RecordReadTouch(ctx context.Context, provider, target, externalID, linearTeamID, url string) {
	recordEntityTouch(ctx, r.stores, r.info, entityCoordinate{provider: provider, target: target, externalID: externalID, linearTeamID: linearTeamID, url: url})
}

func (r *directRuntime) MemoryLoad(ctx context.Context, source, sourceID, externalID string, limit int) (*MemoryLoadResult, error) {
	return loadEntityMemory(ctx, r.stores, r.info, source, sourceID, externalID, limit)
}

func (r *directRuntime) CheckEntitlement(_ context.Context, feature string) (bool, error) {
	return entitlements.For(r.info.OrgID).Has(entitlements.Feature(feature)), nil
}

// SourceDisabled reads the org's stored source policy. The nil-store arm is
// the test seam every other read here carries: a client assembled with partial
// stores has no policy to answer from, and inventing one would fail verbs that
// have nothing to do with this switch.
func (r *directRuntime) SourceDisabled(ctx context.Context, kind string) (bool, error) {
	if r.stores.OrgEventSources == nil {
		return false, nil
	}
	return eventsource.Disabled(ctx, r.stores.OrgEventSources, r.info.OrgID, kind)
}

// SourceManifest builds the run's manifest from a live system availability
// resolve: the direct runtime serves local mode, where no bundle exists and
// every verb resolves its credential live. The nil-store arm is the test seam
// every other read here carries; with no stores to resolve through it answers
// an error, and every caller degrades on one.
func (r *directRuntime) SourceManifest(ctx context.Context) (domain.SourceManifest, error) {
	if r.stores.Secrets == nil || r.stores.OrgEventSources == nil || r.stores.Orgs == nil {
		return nil, errors.New("agenthost: no stores to resolve source availability through")
	}
	avail, err := eventsource.ResolveSystem(ctx, r.stores, r.info.OrgID)
	if err != nil {
		return nil, err
	}
	return LiveSourceManifest(avail), nil
}

// Relay dispatches a provider op locally against db.Stores (all/local), the
// mirror of the sidecar relaying it to the orchestrator. Core built-ins never
// call this (they use the typed methods); a provider handler reaching its own
// policy op does.
func (r *directRuntime) Relay(ctx context.Context, namespace, op string, args, out any) error {
	return dispatchProviderOpLocal(ctx, r.stores, r.info, namespace, op, args, out)
}

func (r *directRuntime) RelayNotify(ctx context.Context, namespace, op string, args any) {
	if err := dispatchProviderOpLocal(ctx, r.stores, r.info, namespace, op, args, nil); err != nil {
		agenthostLog.Warn("local provider notify failed", "namespace", namespace, "op", op, "error", err)
	}
}

// ProviderCredential live-resolves the provider's keyed set against the live
// secret store (all/local) — the same resolver the brain runs at provision
// time, so the handler selects from an identically-shaped set in both
// placements.
func (r *directRuntime) ProviderCredential(ctx context.Context, namespace string) (json.RawMessage, error) {
	resolver, ok := providerCredentialResolvers[namespace]
	if !ok {
		return nil, fmt.Errorf("agenthost: no credential resolver for provider %q", namespace)
	}
	raw, err := resolver(ctx, r.stores, ProvisionScope{OrgID: r.info.OrgID, TeamID: r.info.TeamID, ConversationID: r.info.ConversationID})
	if err != nil {
		return nil, resolveFailureRefusal(namespace, r.info.ConversationID, err)
	}
	if len(raw) == 0 {
		return nil, refusalFor(namespace, domain.SourceEntry{State: domain.SourceUnconfigured})
	}
	return raw, nil
}

// --- relayRuntime: the sidecar impl over the supervision channel ---

// relayRuntime is the capless per-run sidecar's runtime: every effect is a
// narrow, org-bound relay to the orchestrator over the supervision channel. It
// holds no db.Stores and opens no DB connection; identity is bound
// orchestrator-side from the supervised run's ConversationInfo, so the args carry no
// org id and a sidecar cannot address another org's data.
type relayRuntime struct {
	conn relayConn
	info ConversationInfo
	// held reads the sidecar's current sealed bundle — a live view (not a
	// snapshot) so a mid-run brain re-seal is picked up. nil when the sidecar
	// was built with no bundle view.
	held HeldBundle
}

// HeldBundle is the sidecar's live view of the sealed bundle it holds, as much
// of it as the exec verbs read in-process: a provider's sealed keyed set, which
// only that provider's own handler selects from, and the source manifest
// sealed beside it. The GitHub, Jira and Linear credentials the proxies inject
// are not on it; they stay behind the proxies.
type HeldBundle interface {
	// ProviderCreds is a provider's sealed keyed set; ok=false when the bundle
	// carries none for that namespace, or no bundle has arrived.
	ProviderCreds(namespace string) (json.RawMessage, bool)
	// Sources is the bundle's source manifest as it stands now
	// (domain.SourceManifest.At); nil when no bundle has arrived.
	Sources() domain.SourceManifest
}

// relayConn is what the relayRuntime needs of the supervision channel —
// request/response calls, and audit records handed to the run's record sender —
// declared as an interface so a test can drive it without a real channel.
// (agentproc.CallRelay takes the concrete *sidecarproto.Conn, so the production
// relayRuntime wraps one; see newRelayRuntime.)
type relayConn interface {
	call(ctx context.Context, namespace, op string, args, out any) error
	record(namespace, op string, args any)
}

func newRelayRuntime(conn relayConn, info ConversationInfo, held HeldBundle) *relayRuntime {
	return &relayRuntime{conn: conn, info: info, held: held}
}

func (r *relayRuntime) Info() ConversationInfo { return r.info }

func (r *relayRuntime) ListConversationArtifacts(ctx context.Context) ([]domain.Artifact, error) {
	var res listConversationArtifactsResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opListConversationArtifacts, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Artifacts, nil
}

func (r *relayRuntime) GetConversation(ctx context.Context) (*domain.Conversation, error) {
	var res getConversationResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetConversation, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Conversation, nil
}

func (r *relayRuntime) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	var res taskResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetTask, getTaskArgs{TaskID: taskID}, &res); err != nil {
		return nil, err
	}
	return res.Task, nil
}

func (r *relayRuntime) ListRepos(ctx context.Context) ([]domain.Repository, error) {
	var res reposResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opListRepos, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Repos, nil
}

func (r *relayRuntime) GetRepo(ctx context.Context, slug string) (*domain.Repository, error) {
	var res repoResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetRepo, getRepoArgs{RepoID: slug}, &res); err != nil {
		return nil, err
	}
	return res.Repo, nil
}

func (r *relayRuntime) TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error) {
	var res teamTracksRepoResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opTeamTracksRepo, teamTracksRepoArgs{Owner: owner, Repo: repo}, &res); err != nil {
		return false, err
	}
	return res.Tracks, nil
}

func (r *relayRuntime) TaskOwnRepo(ctx context.Context, owner, repo string) (bool, error) {
	var res taskOwnRepoResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opTaskOwnRepo, taskOwnRepoArgs{Owner: owner, Repo: repo}, &res); err != nil {
		return false, err
	}
	return res.IsTaskRepo, nil
}

func (r *relayRuntime) GetConversationWorktreeByRepoRef(ctx context.Context, repositoryID, ref string) (*domain.ConversationWorktree, error) {
	var res conversationWorktreeResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetConversationWorktreeByRepoRef, conversationWorktreeByRepoRefArgs{RepositoryID: repositoryID, Ref: ref}, &res); err != nil {
		return nil, err
	}
	return res.Worktree, nil
}

func (r *relayRuntime) ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error) {
	var res conversationWorktreesResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opListConversationWorktrees, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Worktrees, nil
}

func (r *relayRuntime) OrgJiraBaseURL(ctx context.Context) (string, error) {
	var res orgJiraBaseResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opOrgJiraBase, emptyArgs{}, &res); err != nil {
		return "", err
	}
	return res.URL, nil
}

func (r *relayRuntime) ReviewPosture(ctx context.Context, owner, repo string) (ReviewPostureResolution, error) {
	var res ReviewPostureResolution
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opReviewPosture, reviewPostureArgs{Owner: owner, Repo: repo}, &res); err != nil {
		return ReviewPostureResolution{}, err
	}
	return res, nil
}

func (r *relayRuntime) InsertConversationWorktree(ctx context.Context, row domain.ConversationWorktree) (bool, string, error) {
	var res insertConversationWorktreeResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opInsertConversationWorktree, insertConversationWorktreeArgs{Row: row}, &res); err != nil {
		return false, "", err
	}
	return res.Inserted, res.WinningPath, nil
}

func (r *relayRuntime) DeleteConversationWorktree(ctx context.Context, repositoryID, ref string) error {
	return r.conn.call(ctx, agentproc.RelayNamespaceCore, opDeleteConversationWorktree, deleteConversationWorktreeByRepoRefArgs{RepositoryID: repositoryID, Ref: ref}, nil)
}

func (r *relayRuntime) UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error) {
	var res upsertArtifactResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opUpsertArtifact, upsertArtifactArgs{Artifact: a}, &res); err != nil {
		return domain.Artifact{}, err
	}
	return res.Artifact, nil
}

func (r *relayRuntime) UpdateReviewDetailsIfPending(ctx context.Context, artifactID, detailsJSON string) (bool, error) {
	var res updateReviewDetailsResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opUpdateReviewDetails, updateReviewDetailsArgs{ArtifactID: artifactID, DetailsJSON: detailsJSON}, &res); err != nil {
		return false, err
	}
	return res.Updated, nil
}

func (r *relayRuntime) TransitionReviewState(ctx context.Context, artifactID, from, to, externalID, url, detailsJSON string) (bool, error) {
	var res transitionReviewStateResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opTransitionReviewState, transitionReviewStateArgs{
		ArtifactID: artifactID, From: from, To: to, ExternalID: externalID, URL: url, DetailsJSON: detailsJSON,
	}, &res); err != nil {
		return false, err
	}
	return res.Moved, nil
}

func (r *relayRuntime) Record(_ context.Context, a *domain.Artifact, act *domain.ExternalAction, linearTeamID string) {
	// Queued, not awaited: the external write already landed upstream, so the
	// audit record must never block the op that follows it (mirrors the void
	// RecordExternalWrite). The run's record sender delivers it in order and
	// resends it until the orchestrator has applied it.
	r.conn.record(agentproc.RelayNamespaceCore, opRecordExternalWrite, recordExternalWriteArgs{Artifact: a, Action: act, LinearTeamID: linearTeamID})
}

func (r *relayRuntime) RecordReadTouch(_ context.Context, provider, target, externalID, linearTeamID, url string) {
	// Queued, mirroring Record: the read already returned, so the touch must
	// never block it.
	r.conn.record(agentproc.RelayNamespaceCore, opRecordReadTouch, recordReadTouchArgs{Provider: provider, Target: target, ExternalID: externalID, LinearTeamID: linearTeamID, URL: url})
}

// MemoryLoad relays as a call (not a record): unlike the queued touch, the
// agent waits on the returned memory. The best-effort touch it records
// happens orchestrator-side inside loadEntityMemory, so a relay round-trip
// carries only the read result back.
func (r *relayRuntime) MemoryLoad(ctx context.Context, source, sourceID, externalID string, limit int) (*MemoryLoadResult, error) {
	var res memoryLoadResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opMemoryLoad, memoryLoadArgs{Source: source, SourceID: sourceID, ExternalID: externalID, Limit: limit}, &res); err != nil {
		return nil, err
	}
	return res.Result, nil
}

func (r *relayRuntime) Relay(ctx context.Context, namespace, op string, args, out any) error {
	return r.conn.call(ctx, namespace, op, args, out)
}

func (r *relayRuntime) RelayNotify(_ context.Context, namespace, op string, args any) {
	r.conn.record(namespace, op, args)
}

// ProviderCredential returns the provider's sealed keyed set from the held
// bundle — the sidecar selects a member locally, never asking the orchestrator
// for a secret. A set the bundle does not carry is refused from the bundle's
// manifest, which says why the brain sealed none — the same table every Jira
// and Linear refusal reads, so a provider's verbs explain a missing credential
// the same way.
func (r *relayRuntime) ProviderCredential(_ context.Context, namespace string) (json.RawMessage, error) {
	if r.held == nil {
		return nil, refusalFor(namespace, domain.SourceManifest(nil).EntryFor(namespace))
	}
	raw, ok := r.held.ProviderCreds(namespace)
	if !ok || len(raw) == 0 {
		return nil, refusalFor(namespace, r.held.Sources().EntryFor(namespace))
	}
	return raw, nil
}

func (r *relayRuntime) CheckEntitlement(ctx context.Context, feature string) (bool, error) {
	var res checkEntitlementResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opCheckEntitlement, checkEntitlementArgs{Feature: feature}, &res); err != nil {
		return false, err
	}
	return res.Allowed, nil
}

func (r *relayRuntime) SourceDisabled(ctx context.Context, kind string) (bool, error) {
	var res sourceDisabledResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opSourceDisabled, sourceDisabledArgs{Kind: kind}, &res); err != nil {
		return false, err
	}
	return res.Disabled, nil
}

// SourceManifest reads the held bundle's manifest in-process: the brain sealed
// it beside the credentials, so the orchestrator is never asked.
func (r *relayRuntime) SourceManifest(context.Context) (domain.SourceManifest, error) {
	if r.held == nil {
		return nil, errors.New("agenthost: sidecar holds no credential bundle")
	}
	m := r.held.Sources()
	if m == nil {
		return nil, errors.New("agenthost: sidecar holds no credential bundle")
	}
	return m, nil
}

// sidecarRelayConn adapts a live supervision channel to relayConn — the
// production backing for a sidecar-hosted runtime: calls go straight over the
// channel (agentproc.CallRelay), records through the run's one record sender.
type sidecarRelayConn struct {
	conn    *sidecarproto.Conn
	records *agentproc.RecordSender
}

func (c sidecarRelayConn) call(ctx context.Context, namespace, op string, args, out any) error {
	return agentproc.CallRelay(ctx, c.conn, namespace, op, args, out)
}

func (c sidecarRelayConn) record(namespace, op string, args any) {
	// The sender is shared with everything else in the sidecar that records,
	// which is what keeps records in the order the acts happened. Never
	// awaited — the external write already landed, and nothing here may fail
	// the op that follows it. A record the sender refuses is one it has
	// already counted for the end-of-run drain.
	if err := c.records.Send(namespace, op, args); err != nil {
		agenthostLog.Warn("queueing an audit record failed", "namespace", namespace, "op", op, "error", err)
	}
}

// NewDirectRuntime builds the in-process runtime over db.Stores + ConversationInfo — the
// all/local runtime and the impl the orchestrator's RelayServer serves relayed
// core ops through.
func NewDirectRuntime(stores db.Stores, info ConversationInfo) Runtime {
	return newDirectRuntime(stores, info)
}

// NewRelayRuntime builds the sidecar's runtime over a live supervision channel:
// every DB effect relays to the orchestrator, the sidecar holds no stores.
// records is the run's one record sender on that channel, which the runtime's
// audit records go through. held reads the sidecar's bundle for a provider's
// sealed keyed set (nil when the run carries no provider credentials).
func NewRelayRuntime(conn *sidecarproto.Conn, records *agentproc.RecordSender, info ConversationInfo, held HeldBundle) Runtime {
	return newRelayRuntime(sidecarRelayConn{conn: conn, records: records}, info, held)
}
