package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
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
	ListRepos(ctx context.Context) ([]domain.Repository, error)

	// GetRepo takes the "owner/repo" the agent typed and resolves it against
	// the registry, or nil when nothing answers to that name. The agent's
	// surface speaks names throughout — this is where one stops being a name.
	GetRepo(ctx context.Context, slug string) (*domain.Repository, error)

	TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error)
	GetConversationWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.ConversationWorktree, error)
	ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error)
	OrgJiraBaseURL(ctx context.Context) (string, error)
	AgentFooter(ctx context.Context, kind string) (string, error)

	// ReviewPosture resolves the review-posting decision inputs for owner/repo:
	// the run team's configured posture, and the identity of the credential that
	// would post the review. Both live where the stores and the
	// credential resolver do, so the capless sidecar relays for them — its own
	// gh clients speak to per-run REST proxies whose reported identity is
	// descriptive only, never the real App-vs-PAT tier.
	ReviewPosture(ctx context.Context, owner, repo string) (ReviewPostureResolution, error)

	// Writes.
	InsertConversationWorktree(ctx context.Context, row domain.ConversationWorktree) (inserted bool, winningPath string, err error)
	DeleteConversationWorktree(ctx context.Context, repoID, ref string) error
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
	// already-applied action, so it needs no error return.
	Record(ctx context.Context, a *domain.Artifact, act *domain.ExternalAction)

	// RecordReadTouch persists a durable conversation→entity touch for an addressed read
	// (a verb targeting one entity by id/key/ts): it resolves-or-creates the
	// entity for (provider, target, url) and records a role='touched' row. Void
	// and best-effort like Record — a read never fails on its touch — and, like
	// Record, relayed to the orchestrator on the sidecar so the write lands where
	// the stores live. Set-returning reads never call it (the touched-entity
	// rule: addressed → touch, returned-in-a-set → never).
	RecordReadTouch(ctx context.Context, provider, target, url string)

	// MemoryLoad resolves the entity for (source, sourceID) by its natural key
	// — LOOKUP ONLY, it never mints an entity — and, on a hit, returns that
	// entity's prior conversation memory (team-visibility-scoped to the conversation's team,
	// composed exactly as the spawn-time materializer composes it) capped at the
	// most recent `limit`, with Count the pre-limit scoped total. A hit records a
	// conversation→entity 'touched' row best-effort (loading IS an address); a miss
	// returns an empty result and records nothing. Relayed to the orchestrator on
	// the sidecar so the reads + the touch land where the stores live.
	MemoryLoad(ctx context.Context, source, sourceID string, limit int) (*MemoryLoadResult, error)

	// Relay / RelayNotify are the generic provider-op escape hatch: a provider
	// handler (Slack, future) reaches its own org-bound policy op by namespace
	// without a typed Runtime method per op. Core built-ins use the typed
	// methods above; providers use these.
	Relay(ctx context.Context, namespace, op string, args, out any) error
	RelayNotify(ctx context.Context, namespace, op string, args any)

	// ProviderCredential returns the provider's sealed credential keyed set (its
	// own opaque JSON), for the handler to SELECT a member locally — never
	// asking the orchestrator for a secret. On the sidecar this reads the bundle
	// the brain sealed at provision time; on all/local it live-resolves through
	// the provider's registered resolver against the live secret store. Same
	// shape both ways, so a handler selects identically in either placement.
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

	// AvailableSources answers which event-source kinds the run's org can
	// reach — the set the top-level exec help filters its command index on.
	// It relays on the sidecar (every availability input lives behind stores
	// that process does not hold); the direct runtime answers from the claim's
	// stamped tools manifest where one exists, else a live system resolve —
	// the same two-door rule the spawner's toolsReferenceFor reads, so a run's
	// help index and its <tools> section derive from one answer.
	AvailableSources(ctx context.Context) ([]string, error)
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
	Record(ctx context.Context, a *domain.Artifact, act *domain.ExternalAction)
	RecordReadTouch(ctx context.Context, provider, target, url string)
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
	opGetConversationWorktreeByRepoRef = "get_conversation_worktree_by_repo_ref"
	opListConversationWorktrees        = "list_conversation_worktrees"
	opInsertConversationWorktree       = "insert_conversation_worktree"
	opDeleteConversationWorktree       = "delete_conversation_worktree"
	opListConversationArtifacts        = "list_conversation_artifacts"
	opOrgJiraBase                      = "org_jira_base"
	// opBuildAgentFooter keeps "run": it builds the footer TF appends to
	// agent-authored PR and review bodies, which names the run in the
	// product's own vocabulary and links to /runs/{id}.
	opBuildAgentFooter      = "build_agent_run_footer"
	opUpsertArtifact        = "upsert_artifact"
	opUpdateReviewDetails   = "update_review_details_if_pending"
	opTransitionReviewState = "transition_review_state"
	opRecordExternalWrite   = "record_external_write"
	opRecordReadTouch       = "record_read_touch"
	opMemoryLoad            = "memory_load"
	opCheckEntitlement      = "check_entitlement"
	opSourceDisabled        = "source_disabled"
	opAvailableSources      = "available_sources"
	opReviewPosture         = "review_posture"
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

// availableSourcesResult is the available_sources op's (and the matching IPC
// method's) result — the org's reachable event-source kinds. There are no
// args: identity is bound host-side from the run's ConversationInfo.
type availableSourcesResult struct {
	Kinds []string `json:"kinds"`
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
	Artifact *domain.Artifact       `json:"artifact,omitempty"`
	Action   *domain.ExternalAction `json:"action,omitempty"`
}

// recordReadTouchArgs is the record_read_touch op's payload — the addressed
// read's entity coordinates. Identity (org, run) is bound orchestrator-side
// from the run's ConversationInfo, so the wire carries none.
type recordReadTouchArgs struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	URL      string `json:"url,omitempty"`
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
	err := r.stores.Tx.SyntheticClaimsWithTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
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

func (r *directRuntime) ListRepos(ctx context.Context) ([]domain.Repository, error) {
	return r.stores.Repos.ListSystem(ctx, r.info.OrgID)
}

// GetRepo is the verb boundary for `tfac exec workspace add owner/repo`: the
// agent's argv arrives here as a name and is resolved to a row here, so the id
// is what everything past this point holds. A name nobody has a row for stays
// a nil rather than an error — the caller reports it as "repo is not
// configured in Triage Factory", which is the accurate answer.
func (r *directRuntime) GetRepo(ctx context.Context, slug string) (*domain.Repository, error) {
	return r.stores.Repos.GetByRefSystem(ctx, r.info.OrgID, domain.RepoRefFromSlug(slug))
}

func (r *directRuntime) TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error) {
	return r.stores.TeamGitHubRepos.TracksRepoSystem(ctx, r.info.TeamID, owner, repo)
}

func (r *directRuntime) GetConversationWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.ConversationWorktree, error) {
	return r.stores.ConversationWorktrees.GetByRepoRefSystem(ctx, r.info.OrgID, r.info.ConversationID, repoID, ref)
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

func (r *directRuntime) AgentFooter(ctx context.Context, kind string) (string, error) {
	return agentmeta.Build(r.stores.Conversations, r.info.OrgID, r.info.ConversationID, kind), nil
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

func (r *directRuntime) DeleteConversationWorktree(ctx context.Context, repoID, ref string) error {
	return withWriteInfo(ctx, r.stores, r.info,
		func() error {
			return r.stores.ConversationWorktrees.DeleteByRepoRefSystem(ctx, r.info.OrgID, r.info.ConversationID, repoID, ref)
		},
		func(ts db.TxStores) error {
			return ts.ConversationWorktrees.DeleteByRepoRef(ctx, r.info.OrgID, r.info.ConversationID, repoID, ref)
		},
	)
}

// UpsertArtifact stamps the run identity onto a and upserts it, composing the
// branch-push external action into the SAME write (TFAC-483). Event-triggered
// runs route admin-pool; manual runs wrap in the kicking-off user's synthetic
// claims. Returns the stored row.
func (r *directRuntime) UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error) {
	a.OrgID = r.info.OrgID
	a.TeamID = r.info.TeamID
	a.ConversationID = r.info.ConversationID
	act := branchPushActionInfo(a, r.info, r.githubCredential(ctx, a))
	if r.info.IsEventTriggered {
		stored, err := r.stores.Artifacts.UpsertSystem(ctx, r.info.OrgID, a)
		if err != nil {
			return domain.Artifact{}, err
		}
		if rerr := recordActionSystemInfo(ctx, r.stores, r.info, act); rerr != nil {
			agenthostLog.Warn("branch external-action recording failed (push already applied)",
				"conversation", r.info.ConversationID, "target", a.Target, "error", rerr)
		}
		return stored, nil
	}
	var out domain.Artifact
	err := r.stores.Tx.SyntheticClaimsWithTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
		stored, uerr := ts.Artifacts.Upsert(ctx, r.info.OrgID, a)
		if uerr != nil {
			return uerr
		}
		out = stored
		return recordActionTx(ctx, ts, r.info.OrgID, act)
	})
	return out, err
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

func (r *directRuntime) Record(ctx context.Context, a *domain.Artifact, act *domain.ExternalAction) {
	RecordExternalWrite(ctx, r.stores, r.info, a, act)
}

func (r *directRuntime) RecordReadTouch(ctx context.Context, provider, target, url string) {
	recordEntityTouch(ctx, r.stores, r.info, provider, target, url)
}

func (r *directRuntime) MemoryLoad(ctx context.Context, source, sourceID string, limit int) (*MemoryLoadResult, error) {
	return loadEntityMemory(ctx, r.stores, r.info, source, sourceID, limit)
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

// AvailableSources resolves the org's reachable source kinds through the same
// two doors the spawner's toolsReferenceFor reads, in the same order: the
// claim's stamped tools manifest (claim_credentials.include_tools — the
// brain's own availability answer, and the only complete one an executor can
// produce, its secret store being disabled), else a live system resolve. A
// manifest read that fails hard is logged and falls through rather than
// failing the caller: on a placement where the live resolve can answer it
// will, and where it can't the caller's own degrade (an unfiltered help
// index) is the designed fallback. A nil manifest means the brain stamped no
// answer and also falls through; an empty non-nil one is a real answer.
func (r *directRuntime) AvailableSources(ctx context.Context) ([]string, error) {
	if r.stores.ClaimCredentials != nil {
		b, found, err := r.stores.ClaimCredentials.Get(ctx, r.info.OrgID, r.info.ConversationID)
		switch {
		case err != nil && !errors.Is(err, db.ErrNotApplicableInLocal):
			agenthostLog.Warn("read the claim's stamped tools manifest failed; falling back to the live availability resolve",
				"conversation", r.info.ConversationID, "error", err)
		case err == nil && found && b.IncludeTools != nil:
			return b.IncludeTools, nil
		}
	}
	return eventsource.AvailableKindsSystem(ctx, r.stores, r.info.OrgID)
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
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("agenthost: no %s credential configured for this run", namespace)
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
	// providerCreds reads the current sealed bundle's keyed set for a provider
	// namespace — a live accessor (not a snapshot) so a mid-run brain re-seal is
	// picked up. nil when the sidecar was built with no provider credentials.
	providerCreds providerCredsFunc
}

// providerCredsFunc reads the sidecar's held bundle for a provider's sealed
// keyed set. ok=false when the bundle carries none for that namespace.
type providerCredsFunc func(namespace string) (json.RawMessage, bool)

// relayConn is the slice of *sidecarproto.Conn the relayRuntime needs, declared
// as an interface so a test can drive it without a real supervision channel.
// (agentproc.CallRelay/NotifyRelay take the concrete *sidecarproto.Conn, so the
// production relayRuntime wraps one; see newRelayRuntime.)
type relayConn interface {
	call(ctx context.Context, namespace, op string, args, out any) error
	notify(namespace, op string, args any)
}

func newRelayRuntime(conn relayConn, info ConversationInfo, providerCreds providerCredsFunc) *relayRuntime {
	return &relayRuntime{conn: conn, info: info, providerCreds: providerCreds}
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

func (r *relayRuntime) GetConversationWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.ConversationWorktree, error) {
	var res conversationWorktreeResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetConversationWorktreeByRepoRef, conversationWorktreeByRepoRefArgs{RepoID: repoID, Ref: ref}, &res); err != nil {
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

func (r *relayRuntime) AgentFooter(ctx context.Context, kind string) (string, error) {
	var res buildAgentFooterResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opBuildAgentFooter, buildAgentFooterArgs{Kind: kind}, &res); err != nil {
		return "", err
	}
	return res.Footer, nil
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

func (r *relayRuntime) DeleteConversationWorktree(ctx context.Context, repoID, ref string) error {
	return r.conn.call(ctx, agentproc.RelayNamespaceCore, opDeleteConversationWorktree, deleteConversationWorktreeByRepoRefArgs{RepoID: repoID, Ref: ref}, nil)
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

func (r *relayRuntime) Record(_ context.Context, a *domain.Artifact, act *domain.ExternalAction) {
	// Fire-and-forget: the external write already landed upstream, so the audit
	// record must never block the op that follows it (mirrors the void
	// RecordExternalWrite). A dropped notify costs one audit row, never the
	// agent's action.
	r.conn.notify(agentproc.RelayNamespaceCore, opRecordExternalWrite, recordExternalWriteArgs{Artifact: a, Action: act})
}

func (r *relayRuntime) RecordReadTouch(_ context.Context, provider, target, url string) {
	// Fire-and-forget, mirroring Record: the read already returned, so the touch
	// must never block it. A dropped notify costs one touch row, re-established
	// on the next addressed hit or poll.
	r.conn.notify(agentproc.RelayNamespaceCore, opRecordReadTouch, recordReadTouchArgs{Provider: provider, Target: target, URL: url})
}

// MemoryLoad relays as a call (not a notify): unlike the fire-and-forget touch,
// the agent waits on the returned memory. The best-effort touch it records
// happens orchestrator-side inside loadEntityMemory, so a relay round-trip
// carries only the read result back.
func (r *relayRuntime) MemoryLoad(ctx context.Context, source, sourceID string, limit int) (*MemoryLoadResult, error) {
	var res memoryLoadResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opMemoryLoad, memoryLoadArgs{Source: source, SourceID: sourceID, Limit: limit}, &res); err != nil {
		return nil, err
	}
	return res.Result, nil
}

func (r *relayRuntime) Relay(ctx context.Context, namespace, op string, args, out any) error {
	return r.conn.call(ctx, namespace, op, args, out)
}

func (r *relayRuntime) RelayNotify(_ context.Context, namespace, op string, args any) {
	r.conn.notify(namespace, op, args)
}

// ProviderCredential returns the provider's sealed keyed set from the held
// bundle — the sidecar selects a member locally, never asking the orchestrator
// for a secret.
func (r *relayRuntime) ProviderCredential(_ context.Context, namespace string) (json.RawMessage, error) {
	if r.providerCreds == nil {
		return nil, fmt.Errorf("agenthost: sidecar holds no provider credentials")
	}
	raw, ok := r.providerCreds(namespace)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("agenthost: no %s credential in the sealed bundle", namespace)
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

func (r *relayRuntime) AvailableSources(ctx context.Context) ([]string, error) {
	var res availableSourcesResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opAvailableSources, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Kinds, nil
}

// sidecarRelayConn adapts a live supervision channel to relayConn — the
// production backing for a sidecar-hosted runtime (agentproc.CallRelay/NotifyRelay
// over *sidecarproto.Conn).
type sidecarRelayConn struct{ conn *sidecarproto.Conn }

func (c sidecarRelayConn) call(ctx context.Context, namespace, op string, args, out any) error {
	return agentproc.CallRelay(ctx, c.conn, namespace, op, args, out)
}

func (c sidecarRelayConn) notify(namespace, op string, args any) {
	// The audit sender, not the raw one: a record that never reaches the wire is
	// reported back over the same channel where that is still possible, so the
	// orchestrator can count it. Still fire-and-forget for the caller — the
	// external write already landed, and nothing here may fail the op that
	// follows it.
	if err := agentproc.NotifyRelayAudit(c.conn, namespace, op, args); err != nil {
		agenthostLog.Warn("relaying an audit record failed", "namespace", namespace, "op", op, "error", err)
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
// providerCreds reads the sidecar's held bundle for a provider's sealed keyed
// set (nil when the run carries no provider credentials).
func NewRelayRuntime(conn *sidecarproto.Conn, info ConversationInfo, providerCreds func(namespace string) (json.RawMessage, bool)) Runtime {
	return newRelayRuntime(sidecarRelayConn{conn: conn}, info, providerCreds)
}
