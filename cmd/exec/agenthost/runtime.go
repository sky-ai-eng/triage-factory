package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// Runtime is the seam every DB-backed effect the exec verb trace performs
// routes through, so the SAME LocalClient logic runs in two placements:
//
//   - directRuntime (all/local, and the orchestrator serving a relayed op) —
//     the effect hits db.Stores in-process under the run's RunInfo, with the
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
	Info() RunInfo

	// Reads.
	ListRunArtifacts(ctx context.Context) ([]domain.Artifact, error)
	GetConversation(ctx context.Context) (*domain.Conversation, error)
	GetTask(ctx context.Context, taskID string) (*domain.Task, error)
	ListRepos(ctx context.Context) ([]domain.RepoProfile, error)
	GetRepo(ctx context.Context, repoID string) (*domain.RepoProfile, error)
	TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error)
	GetRunWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.RunWorktree, error)
	ListRunWorktrees(ctx context.Context) ([]domain.RunWorktree, error)
	OrgJiraBaseURL(ctx context.Context) (string, error)
	AgentFooter(ctx context.Context, kind string) (string, error)

	// ReviewPosture resolves the review-posting decision inputs for owner/repo
	// (TFAC-680): the run team's configured posture, and the identity of the
	// credential that would post the review. Both live where the stores and the
	// credential resolver do, so the capless sidecar relays for them — its own
	// gh clients speak to per-run REST proxies whose reported identity is
	// descriptive only, never the real App-vs-PAT tier.
	ReviewPosture(ctx context.Context, owner, repo string) (ReviewPostureResolution, error)

	// Writes.
	InsertRunWorktree(ctx context.Context, row domain.RunWorktree) (inserted bool, winningPath string, err error)
	DeleteRunWorktree(ctx context.Context, repoID, ref string) error
	UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error)
	// UpdateReviewDetailsIfPending persists a review draft's mutated
	// details_json, guarded on the draft still being state=pending. Returns
	// false when the draft was resolved (submitted/dismissed) since it was read
	// — the writer surfaces the lost race instead of resurrecting the resolved
	// review, which the unconditional UpsertArtifact (state last-writer-wins)
	// would do with its stale in-memory pending state.
	UpdateReviewDetailsIfPending(ctx context.Context, artifactID, detailsJSON string) (bool, error)
	// Record is the void, best-effort external-write funnel (RecordExternalWrite):
	// a recording failure is logged host-side and never fails the agent's
	// already-applied action, so it needs no error return.
	Record(ctx context.Context, a *domain.Artifact, act *domain.ExternalAction)

	// RecordReadTouch persists a durable run→entity touch for an addressed read
	// (a verb targeting one entity by id/key/ts): it resolves-or-creates the
	// entity for (provider, target, url) and records a role='touched' row. Void
	// and best-effort like Record — a read never fails on its touch — and, like
	// Record, relayed to the orchestrator on the sidecar so the write lands where
	// the stores live. Set-returning reads never call it (the touched-entity
	// rule: addressed → touch, returned-in-a-set → never).
	RecordReadTouch(ctx context.Context, provider, target, url string)

	// MemoryLoad resolves the entity for (source, sourceID) by its natural key
	// — LOOKUP ONLY, it never mints an entity — and, on a hit, returns that
	// entity's prior run memory (team-visibility-scoped to the run's team,
	// composed exactly as the spawn-time materializer composes it) capped at the
	// most recent `limit`, with Count the pre-limit scoped total. A hit records a
	// run→entity 'touched' row best-effort (loading IS an address); a miss
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
}

// ExtensionRuntime is the narrower runtime a provider's sidecar-half handler
// closes over — everything it needs (its identity, a relay to its org-bound
// policy ops, its sealed credential set, the audit funnel) and NOTHING
// host-specific (no db.Stores, no secret store). Runtime satisfies it, so the
// same handler runs over a direct runtime (all/local) or a relay runtime
// (sidecar) unchanged.
type ExtensionRuntime interface {
	Info() RunInfo
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
	opGetConversation         = "get_conversation"
	opGetTask                 = "get_task"
	opListRepos               = "list_repos"
	opGetRepo                 = "get_repo"
	opTeamTracksRepo          = "team_tracks_repo"
	opGetRunWorktreeByRepoRef = "get_run_worktree_by_repo_ref"
	opListRunWorktrees        = "list_conversation_worktrees"
	opInsertRunWorktree       = "insert_run_worktree"
	opDeleteRunWorktree       = "delete_run_worktree"
	opListRunArtifacts        = "list_run_artifacts"
	opOrgJiraBase             = "org_jira_base"
	opBuildAgentFooter        = "build_agent_run_footer"
	opUpsertArtifact          = "upsert_artifact"
	opUpdateReviewDetails     = "update_review_details_if_pending"
	opRecordExternalWrite     = "record_external_write"
	opRecordReadTouch         = "record_read_touch"
	opMemoryLoad              = "memory_load"
	opCheckEntitlement        = "check_entitlement"
	opReviewPosture           = "review_posture"
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

// checkEntitlementArgs / checkEntitlementResult are the check_entitlement op's
// payloads — a feature name, and whether the run's org holds it.
type checkEntitlementArgs struct {
	Feature string `json:"feature"`
}

type checkEntitlementResult struct {
	Allowed bool `json:"allowed"`
}

// ReviewPostureResolution is what the review-posting decision reads from
// (TFAC-680): the team's configured posture (one of domain.ValidReviewPostures)
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
// from the run's RunInfo, so the wire carries neither.
type reviewPostureArgs struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// listRunArtifactsResult / orgJiraBaseResult / recordExternalWriteArgs are the
// relay payloads that don't already exist as an IPC arg/result in protocol.go.
type listRunArtifactsResult struct {
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
// from the run's RunInfo, so the wire carries none.
type recordReadTouchArgs struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	URL      string `json:"url,omitempty"`
}

// --- directRuntime: the in-process impl over db.Stores ---

// directRuntime is the all/local runtime AND the impl the orchestrator's
// RelayServer serves a relayed core op through — the same DB logic in both
// placements. Holds the run's stores + RunInfo; every method binds identity
// from info, never from a caller argument.
type directRuntime struct {
	stores db.Stores
	info   RunInfo

	// ghResolver classifies the acting GitHub credential for the review-posture
	// decision (ReviewPosture). Seeded by NewServer with the Server's shared
	// resolver so the App-coverage probe reuses its installation-token cache;
	// lazily built from stores when unset (the local-mode CLI's directRuntime,
	// which has no Server), and pre-set by tests.
	ghResolver ghclient.Resolver
}

func newDirectRuntime(stores db.Stores, info RunInfo) *directRuntime {
	return &directRuntime{stores: stores, info: info}
}

func (r *directRuntime) Info() RunInfo { return r.info }

// ListRunArtifacts respects the pool split: event-triggered runs read
// admin-pool (no JWT claims); manual runs read under the kicking-off user's
// synthetic claims. Mirrors withWriteInfo for the read side.
func (r *directRuntime) ListRunArtifacts(ctx context.Context) ([]domain.Artifact, error) {
	if r.info.IsEventTriggered {
		return r.stores.Artifacts.ListByRunSystem(ctx, r.info.OrgID, r.info.RunID)
	}
	var out []domain.Artifact
	err := r.stores.Tx.SyntheticClaimsWithTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
		a, e := ts.Artifacts.ListByRun(ctx, r.info.OrgID, r.info.RunID)
		out = a
		return e
	})
	return out, err
}

func (r *directRuntime) GetConversation(ctx context.Context) (*domain.Conversation, error) {
	return r.stores.Conversations.GetSystem(ctx, r.info.OrgID, r.info.RunID)
}

func (r *directRuntime) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	return r.stores.Tasks.GetSystem(ctx, r.info.OrgID, taskID)
}

func (r *directRuntime) ListRepos(ctx context.Context) ([]domain.RepoProfile, error) {
	return r.stores.Repos.ListSystem(ctx, r.info.OrgID)
}

func (r *directRuntime) GetRepo(ctx context.Context, repoID string) (*domain.RepoProfile, error) {
	return r.stores.Repos.GetSystem(ctx, r.info.OrgID, repoID)
}

func (r *directRuntime) TeamTracksRepo(ctx context.Context, owner, repo string) (bool, error) {
	return r.stores.TeamGitHubRepos.TracksRepoSystem(ctx, r.info.TeamID, owner, repo)
}

func (r *directRuntime) GetRunWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.RunWorktree, error) {
	return r.stores.RunWorktrees.GetByRepoRefSystem(ctx, r.info.OrgID, r.info.RunID, repoID, ref)
}

func (r *directRuntime) ListRunWorktrees(ctx context.Context) ([]domain.RunWorktree, error) {
	return r.stores.RunWorktrees.ListSystem(ctx, r.info.OrgID, r.info.RunID)
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
	return agentmeta.Build(r.stores.Conversations, r.info.OrgID, r.info.RunID, kind), nil
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
	resolver := r.ghResolver
	if resolver == nil {
		resolver = ghclient.NewResolver(r.stores.Secrets, r.stores.GitHubApps, r.stores.Orgs, r.stores.Agents, nil)
		r.ghResolver = resolver
	}
	ir, ok := resolver.(ghclient.RepoIdentityResolver)
	if !ok {
		return out, nil
	}
	_, identity, err := ir.ClientForRepoWithIdentity(ctx, r.info.OrgID, owner, repo)
	if err != nil {
		agenthostLog.Warn("review posture: credential identity unresolved; treating as unknown (review will be staged)",
			"run", r.info.RunID, "owner", owner, "repo", repo, "error", err)
		return out, nil
	}
	out.Identity = identity
	return out, nil
}

func (r *directRuntime) InsertRunWorktree(ctx context.Context, row domain.RunWorktree) (bool, string, error) {
	if r.info.IsEventTriggered {
		return r.stores.RunWorktrees.InsertSystem(ctx, r.info.OrgID, row)
	}
	var (
		inserted    bool
		winningPath string
	)
	err := r.stores.Tx.SyntheticClaimsWithTx(ctx, r.info.OrgID, r.info.UserID, func(ts db.TxStores) error {
		i, w, ierr := ts.RunWorktrees.Insert(ctx, r.info.OrgID, row)
		inserted = i
		winningPath = w
		return ierr
	})
	return inserted, winningPath, err
}

func (r *directRuntime) DeleteRunWorktree(ctx context.Context, repoID, ref string) error {
	return withWriteInfo(ctx, r.stores, r.info,
		func() error {
			return r.stores.RunWorktrees.DeleteByRepoRefSystem(ctx, r.info.OrgID, r.info.RunID, repoID, ref)
		},
		func(ts db.TxStores) error {
			return ts.RunWorktrees.DeleteByRepoRef(ctx, r.info.OrgID, r.info.RunID, repoID, ref)
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
	a.ConversationID = r.info.RunID
	act := branchPushActionInfo(a, r.info)
	if r.info.IsEventTriggered {
		stored, err := r.stores.Artifacts.UpsertSystem(ctx, r.info.OrgID, a)
		if err != nil {
			return domain.Artifact{}, err
		}
		if rerr := recordActionSystemInfo(ctx, r.stores, r.info, act); rerr != nil {
			agenthostLog.Warn("branch external-action recording failed (push already applied)",
				"run", r.info.RunID, "target", a.Target, "error", rerr)
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
	raw, err := resolver(ctx, r.stores, ProvisionScope{OrgID: r.info.OrgID, TeamID: r.info.TeamID, RunID: r.info.RunID})
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
// orchestrator-side from the supervised run's RunInfo, so the args carry no
// org id and a sidecar cannot address another org's data.
type relayRuntime struct {
	conn relayConn
	info RunInfo
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

func newRelayRuntime(conn relayConn, info RunInfo, providerCreds providerCredsFunc) *relayRuntime {
	return &relayRuntime{conn: conn, info: info, providerCreds: providerCreds}
}

func (r *relayRuntime) Info() RunInfo { return r.info }

func (r *relayRuntime) ListRunArtifacts(ctx context.Context) ([]domain.Artifact, error) {
	var res listRunArtifactsResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opListRunArtifacts, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Artifacts, nil
}

func (r *relayRuntime) GetConversation(ctx context.Context) (*domain.Conversation, error) {
	var res agentRunResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetConversation, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Run, nil
}

func (r *relayRuntime) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	var res taskResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetTask, getTaskArgs{TaskID: taskID}, &res); err != nil {
		return nil, err
	}
	return res.Task, nil
}

func (r *relayRuntime) ListRepos(ctx context.Context) ([]domain.RepoProfile, error) {
	var res reposResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opListRepos, emptyArgs{}, &res); err != nil {
		return nil, err
	}
	return res.Repos, nil
}

func (r *relayRuntime) GetRepo(ctx context.Context, repoID string) (*domain.RepoProfile, error) {
	var res repoResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetRepo, getRepoArgs{RepoID: repoID}, &res); err != nil {
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

func (r *relayRuntime) GetRunWorktreeByRepoRef(ctx context.Context, repoID, ref string) (*domain.RunWorktree, error) {
	var res runWorktreeResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opGetRunWorktreeByRepoRef, runWorktreeByRepoRefArgs{RepoID: repoID, Ref: ref}, &res); err != nil {
		return nil, err
	}
	return res.Worktree, nil
}

func (r *relayRuntime) ListRunWorktrees(ctx context.Context) ([]domain.RunWorktree, error) {
	var res runWorktreesResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opListRunWorktrees, emptyArgs{}, &res); err != nil {
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

func (r *relayRuntime) InsertRunWorktree(ctx context.Context, row domain.RunWorktree) (bool, string, error) {
	var res insertRunWorktreeResult
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, opInsertRunWorktree, insertRunWorktreeArgs{Row: row}, &res); err != nil {
		return false, "", err
	}
	return res.Inserted, res.WinningPath, nil
}

func (r *relayRuntime) DeleteRunWorktree(ctx context.Context, repoID, ref string) error {
	return r.conn.call(ctx, agentproc.RelayNamespaceCore, opDeleteRunWorktree, deleteRunWorktreeByRepoRefArgs{RepoID: repoID, Ref: ref}, nil)
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

// sidecarRelayConn adapts a live supervision channel to relayConn — the
// production backing for a sidecar-hosted runtime (agentproc.CallRelay/NotifyRelay
// over *sidecarproto.Conn).
type sidecarRelayConn struct{ conn *sidecarproto.Conn }

func (c sidecarRelayConn) call(ctx context.Context, namespace, op string, args, out any) error {
	return agentproc.CallRelay(ctx, c.conn, namespace, op, args, out)
}

func (c sidecarRelayConn) notify(namespace, op string, args any) {
	_ = agentproc.NotifyRelay(c.conn, namespace, op, args)
}

// NewDirectRuntime builds the in-process runtime over db.Stores + RunInfo — the
// all/local runtime and the impl the orchestrator's RelayServer serves relayed
// core ops through.
func NewDirectRuntime(stores db.Stores, info RunInfo) Runtime {
	return newDirectRuntime(stores, info)
}

// NewRelayRuntime builds the sidecar's runtime over a live supervision channel:
// every DB effect relays to the orchestrator, the sidecar holds no stores.
// providerCreds reads the sidecar's held bundle for a provider's sealed keyed
// set (nil when the run carries no provider credentials).
func NewRelayRuntime(conn *sidecarproto.Conn, info RunInfo, providerCreds func(namespace string) (json.RawMessage, bool)) Runtime {
	return newRelayRuntime(sidecarRelayConn{conn: conn}, info, providerCreds)
}
