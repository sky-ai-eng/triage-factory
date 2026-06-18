// The Spawner type — central coordinator for delegated agent runs — and
// the small cross-cutting helpers (status broadcasts, status updates,
// drainer/classification wiring) every other file in this package
// reaches for. The lifecycle methods (Delegate, Cancel, ResumeOpenRun)
// live in their own files; this one is the type definition + the bits
// that don't belong anywhere else.

package delegate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// shortRunID truncates a run UUID to 8 chars for toast messages — full UUIDs
// are noisy in a notification. Kept consistent so users can cross-reference
// the runs page listing.
func shortRunID(runID string) string {
	if len(runID) < 8 {
		return runID
	}
	return runID[:8]
}

// QueueDrainer is the interface the spawner uses to notify the per-entity
// firing queue that an auto run has reached a terminal state and the
// entity may be ready to drain its next pending firing. Implemented by
// the routing.Router. Manual runs do not call this — manual is fully
// decoupled from the queue per the SKY-189 design. orgID scopes the
// drain to the run's tenant so multi-mode lookups hit the right
// pending_firings rows.
type QueueDrainer interface {
	DrainEntity(orgID, entityID string)
}

// Spawner manages delegated agent runs.
type Spawner struct {
	database   *sql.DB
	prompts    db.PromptStore
	agents     db.AgentStore // resolves actor for run.actor_agent_id stamping
	blueprints db.BlueprintStore
	runQueue   db.RunQueueStore  // the run queue the dispatcher drains: enqueue a step, claim it, run it, react
	tasks      db.TaskStore      // re-read tasks for run lifecycle handlers
	agentRuns  db.AgentRunStore  // run lifecycle + transcript
	entities   db.EntityStore    // entity reads for project lookup + resume context
	reviews    db.ReviewStore    // pending review cleanup on discard / cancel paths
	pendingPRs db.PendingPRStore // pending PR lookup on processCompletion / cleanup paths
	events     db.EventStore     // admin-pool GetMetadataSystem for post-run prompt building
	// taskMemory routes the post-completion UpsertAgentMemorySystem
	// and the run-start GetMemoriesForEntitySystem through the dual-
	// pool store. Both fire inside the runAgent goroutine, which has
	// no JWT-claims context, so they hit the admin pool in Postgres.
	taskMemory db.TaskMemoryStore
	// runWorktrees serves the spawner's per-run cleanup defers (Jira
	// runs accumulate lazy worktrees via the agent's `workspace add`
	// CLI; the defer iterates and removes them). Goroutine-internal
	// callers, all routed through the admin-pool System variants.
	runWorktrees db.RunWorktreeStore
	// orgs reads per-org settings (GitHub clone protocol) from
	// org_settings during run setup. Post-internal/config deletion;
	// every per-org read goes through OrgsStore.GetSettingsSystem
	// (no JWT claims context on the run goroutine).
	orgs db.OrgsStore
	// jiraRules reads the team's per-project Jira status rules under the
	// admin pool (ListForTeamSystem) for the TFAC-300 board→Jira mirror's
	// system-context rule lookup. Populated from the Stores bundle in
	// NewSpawner — a plain store ref like s.tasks, set once and never
	// mutated, so it needs no mu guard (its sibling seam jiraResolver does).
	jiraRules db.JiraStatusRulesStore
	// teams reads per-team settings under the admin pool (GetSettingsSystem)
	// at spawn time — currently the TFAC-392 presence-gated absent-auto-deny
	// knobs (grace window + on/off toggle). Resolved once per run when the
	// permission handler is built, not per prompt. A plain store ref like
	// s.tasks/s.jiraRules; nil-safe (the helper falls back to defaults).
	teams db.TeamsStore
	// tx runs synthetic-claims write batches for manual runs (the
	// run's creator_user_id is the synthetic claim subject, so RLS
	// policies on the writes pass under tf_app). Event-triggered runs
	// don't construct a tx — their writes go through `...System`
	// admin-pool methods directly. Routing is inline at each call
	// site: `if triggerType == "manual" { s.tx.SyntheticClaimsWithTx
	// (..., creatorUserID, fn) } else { s.x.MethodSystem(...) }`.
	tx    db.TxRunner
	wsHub *websocket.Hub

	mu       sync.Mutex
	ghClient *ghclient.Client
	model    string
	// SKY-389 per-org run-credential seam, wired once at startup via
	// SetRunCredentialResolvers. When set (both modes in production) these
	// supersede the process-global ghClient/model above; tests leave them
	// nil and the resolver helpers fall back to ghClient/model.
	ghResolver ghclient.Resolver                            // per-(org, owner) GitHub client source (App token in multi, keychain PAT in local)
	runSecrets agentproc.SecretsReader                      // per-org LLM-credential reader (nil in local → ambient subscription; system-door reader in multi)
	modelFor   func(context.Context, string, string) string // per-(org, team) default-model resolver (prompt.Model still overrides per delegation)
	// jiraResolver routes the TFAC-300 board→Jira mirror under the org's
	// system/bot credential (ForSystem). Wired post-construction via
	// SetJiraResolver (the resolver is built in the app composition, not handed
	// to NewSpawner); nil disables the mirror (tests, local without Jira).
	// mu-guarded like the credential seam above it — creds hot-swap on config
	// change, so a client is resolved fresh per write and never cached here.
	jiraResolver jira.Resolver
	// jiraMirrorLocks serializes the TFAC-300 board→Jira mirror per ticket so a
	// slow in-progress mirror and the done mirror can't interleave or reorder
	// their writes against the same issue (which could drag a Done ticket back
	// to In Progress). Its own keyed lock, independent of mu; zero value ready.
	jiraMirrorLocks keyedMutex

	// blobs is the durable blob/object store handle for the blueprint
	// workspace seam: local mode → an on-disk store under the state root,
	// multi → an S3-compatible object store. Wired once at startup via
	// SetStorage, mirroring SetRunCredentialResolvers above; read through
	// Storage() by the snapshot/rehydrate consumer (a follow-up). Guarded
	// by mu like the credential seam it sits beside.
	blobs storage.Storage

	cancels               map[string]context.CancelFunc                     // runID → cancel the entire run
	dispatchWake          chan struct{}                                     // best-effort latency nudge for the run-queue dispatcher; non-blocking send on enqueue, buffered depth 1 so a missed wake only defers to the next scan tick
	drainer               QueueDrainer                                      // nil-safe; set post-construction via SetQueueDrainer
	waitForClassification func(ctx context.Context, orgID, entityID string) // SKY-220 hook: blocks until the project classifier has decided this entity, or a timeout/ctx-cancel elapses. orgID scopes the classification read to the run's tenant (SKY-392 — the read goes through the org-scoped admin-pool store, not a raw query). Nil-safe (test setups skip it). Wired in main.go via SetWaitForClassification — keeps internal/delegate from importing internal/projectclassify.

	// procs holds the live agent process handle for each run currently
	// executing as a LiveRun, keyed by run id. It survives across HTTP
	// turns because the spawner is the startup singleton, so a control op
	// (interrupt/steer/cancel) arriving on a later request can still
	// reach the process a delegation goroutine spawned. Distinct from
	// cancels (the hard-kill ctx) — a live run holds both a procs and a
	// cancels entry at once. Guarded by s.mu; see the
	// register/get/deregister accessors in process_registry.go.
	procs map[string]*liveRunHandle
	// controller routes the live-process control ops (interrupt, steer,
	// cancel-kill) to wherever the run's process lives. At N=1 it's the
	// in-process impl that resolves the handle from procs/cancels; the
	// seam horizontal scaling swaps for a DB-signal to the owning
	// executor. Set once in NewSpawner.
	controller RunController
	// permPending brokers the browser tool-permission round-trip: each
	// in-flight canUseTool prompt registers a pending entry here keyed by its
	// SDK request_id; the WS POST resolves it and the parked handler goroutine
	// receives the decision (or a bounded timeout denies it). In-memory only
	// (no schema); guarded by s.mu. The runLiveAndDrive call sites still pass
	// perms:nil, so the broker is dormant until a follow-up wires the handler
	// in alongside the browser prompt UI.
	permPending map[string]*pendingPermission
	// executorID is this spawner instance's executor identity, generated
	// once at construction and stamped onto runs.executor_id when a run
	// goes live. At N=1 there is one executor per process; on restart a
	// fresh id re-stamps re-claimed runs. The run→executor ownership hook
	// horizontal scaling builds the lease layer on.
	executorID string
	// runSem bounds how many runs execute off the dispatcher at once — a
	// process-wide cap so a burst of queued steps doesn't fan into an
	// unbounded number of agent subprocesses. Sized in NewSpawner
	// (DefaultMaxConcurrentRuns) and replaceable via SetMaxConcurrentRuns
	// before the dispatcher starts. Each drain acquires a slot before
	// claiming and the run goroutine releases it on terminal.
	runSem chan struct{}
	// idleHibernateTimeout is how long a live run may go quiet (no stream
	// activity) before it hibernates to a durable resume. Zero means use
	// DefaultIdleHibernateTimeout; tests inject a short value via
	// SetIdleHibernateTimeout. Read through idleTimeout().
	idleHibernateTimeout time.Duration
	// permPresencePoll is how often the TFAC-392 presence-gated permission wait
	// re-checks for an answer-capable, focused tab. Zero means use
	// defaultPresencePollInterval; tests inject a short value via
	// SetPresencePollInterval. Read through presencePollInterval().
	permPresencePoll time.Duration
	// snapshotRetentionTTL bounds how long a parked/aborted run's durable
	// workspace snapshot is kept before the retention reaper discards it. Zero
	// means use DefaultSnapshotRetentionTTL; tests inject a short value via
	// SetSnapshotRetentionTTL. Read through snapshotRetention().
	snapshotRetentionTTL time.Duration

	agentToolsOnce  sync.Once
	agentToolsCache string

	// stores is the full db.Stores bundle the per-run agenthost daemon
	// hands to its LocalClient at request dispatch. Set post-
	// construction via SetStores so we don't have to thread another
	// arg through every test fixture's NewSpawner call — the sandbox
	// branch is Linux+multi-mode only and unit tests never reach it.
	//
	// Pointer (rather than db.Stores value) so callers can branch on
	// `stores != nil` cleanly. The earlier `db.Stores{} != stores`
	// shape relied on every field being a comparable interface and
	// would runtime-panic the moment a future field landed with a
	// non-comparable concrete type (slice/map/func).
	stores *db.Stores
}

// NewSpawner constructs a Spawner from the per-resource store
// bundle plus the runtime knobs (GitHub client, WS hub, model). The
// Spawner keeps individual store fields rather than the full bundle
// so existing hot paths (s.tasks, s.entities, ...) stay put; New just
// unpacks once. Tests that only exercise a subset of stores can pass
// partial db.Stores{} — every field is a nil-safe interface.
func NewSpawner(database *sql.DB, stores db.Stores, ghClient *ghclient.Client, wsHub *websocket.Hub, model string) *Spawner {
	s := &Spawner{
		database:     database,
		prompts:      stores.Prompts,
		agents:       stores.Agents,
		blueprints:   stores.Blueprints,
		runQueue:     stores.RunQueue,
		tasks:        stores.Tasks,
		agentRuns:    stores.AgentRuns,
		entities:     stores.Entities,
		reviews:      stores.Reviews,
		pendingPRs:   stores.PendingPRs,
		events:       stores.Events,
		taskMemory:   stores.TaskMemory,
		runWorktrees: stores.RunWorktrees,
		orgs:         stores.Orgs,
		jiraRules:    stores.JiraStatusRules,
		teams:        stores.Teams,
		tx:           stores.Tx,
		ghClient:     ghClient,
		wsHub:        wsHub,
		model:        model,
		cancels:      make(map[string]context.CancelFunc),
		dispatchWake: make(chan struct{}, 1),
		procs:        make(map[string]*liveRunHandle),
		permPending:  make(map[string]*pendingPermission),
		executorID:   uuid.New().String(),
		runSem:       make(chan struct{}, DefaultMaxConcurrentRuns),
	}
	s.controller = inProcessController{s: s}
	return s
}

// useSSHCloneProtocol reports whether this run should clone over SSH. The
// ssh-vs-https decision is delegated to domain.EffectiveCloneProtocol so the
// "multi-mode is always HTTPS" invariant has a single home shared with the
// settings API view — an App installation token is an HTTPS bearer credential
// that can't be used over SSH, and the runtime container has no
// ssh-agent/key/known_hosts. orgs is nil-safe and any store failure logs +
// defaults to HTTPS, matching the prior config.Load() degrade path.
func (s *Spawner) useSSHCloneProtocol(ctx context.Context, orgID, runID string) bool {
	if s.orgs == nil {
		return false
	}
	settings, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		delegateLog.Warn("load org settings to pick clone protocol failed; defaulting to HTTPS", "run", runID, "error", err)
		return false
	}
	return domain.EffectiveCloneProtocol(settings.GitHubCloneProtocol, runmode.Current() == runmode.ModeMulti) == "ssh"
}

// SetStores hands the per-run agenthost daemon's store bundle to the
// spawner. The bundle is consulted only inside the sandbox branch
// (multi-mode + Linux); local-mode spawning ignores it entirely. main
// calls this once at startup post-NewSpawner; tests that don't
// exercise the sandbox path can leave it unset.
func (s *Spawner) SetStores(stores db.Stores) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stores = &stores
}

// getStores returns the configured store bundle and a bool indicating
// whether it was set. Callers branch on the bool rather than
// comparing the value against db.Stores{} so a future non-comparable
// field on db.Stores can't turn the check into a runtime panic.
func (s *Spawner) getStores() (db.Stores, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stores == nil {
		return db.Stores{}, false
	}
	return *s.stores, true
}

// SetQueueDrainer wires the firing-queue drainer into the spawner. Done
// post-construction because the router (which implements QueueDrainer)
// holds a reference to the spawner, so the spawner can't take it as a
// constructor arg without a circular dependency. Same post-construction
// injection pattern as SetRunCredentialResolvers. Safe to call once at
// startup; nil drainer disables the drain hook (used in tests).
func (s *Spawner) SetQueueDrainer(d QueueDrainer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drainer = d
}

// SetWaitForClassification wires the SKY-220 hook that blocks the
// spawner until the project classifier has decided the entity (or
// the timeout / ctx fires). main.go provides the implementation so
// this package doesn't import projectclassify. Nil-safe — tests and
// any configuration without a classifier skip the wait entirely.
func (s *Spawner) SetWaitForClassification(fn func(ctx context.Context, orgID, entityID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waitForClassification = fn
}

// awaitClassification calls the wait hook if one is configured. ctx
// is forwarded so the spawner's run cancellation / shutdown path
// breaks out of the wait early instead of blocking the full
// classifier timeout. orgID scopes the classification read to the
// run's tenant (SKY-392).
func (s *Spawner) awaitClassification(ctx context.Context, orgID, entityID string) {
	s.mu.Lock()
	fn := s.waitForClassification
	s.mu.Unlock()
	if fn != nil {
		fn(ctx, orgID, entityID)
	}
}

// notifyDrainer fires the QueueDrainer hook for an entity if a drainer is
// configured AND the run that just finished was an auto-fired one.
// Manual runs are fully decoupled from the queue per SKY-189 — they
// neither participate in the gate nor trigger drains. Runs in goroutine
// to keep run-teardown latency unaffected.
func (s *Spawner) notifyDrainer(orgID, triggerType, entityID string) {
	if triggerType == "manual" || entityID == "" {
		return
	}
	s.mu.Lock()
	d := s.drainer
	s.mu.Unlock()
	if d == nil {
		return
	}
	go d.DrainEntity(orgID, entityID)
}

// SetRunCredentialResolvers wires the per-org run-credential seam
// (SKY-389): both modes resolve a run's GitHub client + LLM key + default
// model through these, so credential resolution stops branching on mode.
//
//   - resolver: per-(org, owner) GitHub client — App-installation token in
//     multi, keychain PAT in local. Replaces the process-global ghClient
//     the retired UpdateCredentials used to hot-swap from main's local block.
//   - secrets: per-org LLM-credential reader. nil in local → the agent
//     inherits the host's ambient Claude subscription; the system-door
//     reader in multi.
//   - modelFor: per-(org, team) default model (the run's team default,
//     capped by the org max tier). The prompt's own Model still overrides
//     this per delegation.
//
// Set once at startup, post-NewSpawner. Any of the three may be nil; the
// resolver helpers fall back to the constructor-supplied ghClient/model
// (the test / no-seam path).
func (s *Spawner) SetRunCredentialResolvers(resolver ghclient.Resolver, secrets agentproc.SecretsReader, modelFor func(context.Context, string, string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ghResolver = resolver
	s.runSecrets = secrets
	s.modelFor = modelFor
}

// SetJiraResolver wires the Jira write-actor resolver so the TFAC-300
// board→Jira mirror can resolve the org's system/bot credential at each
// transition. Set once at startup, post-NewSpawner (the resolver is built in
// the app composition, like the GitHub one). Nil-safe: an unset resolver
// disables the mirror — tests and local-mode-without-Jira just skip the Jira
// write. Same post-construction injection shape as SetRunCredentialResolvers.
func (s *Spawner) SetJiraResolver(r jira.Resolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jiraResolver = r
}

// getJiraResolver returns the wired Jira resolver, or nil when the mirror is
// disabled. The lock keeps it race-free against a startup-time SetJiraResolver,
// matching the credential-seam getters.
func (s *Spawner) getJiraResolver() jira.Resolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jiraResolver
}

// SetStorage wires the durable blob store into the spawner. Done
// post-construction (like SetRunCredentialResolvers and SetStores) so the
// constructor signature — and every test fixture's NewSpawner call — stays
// put. Safe to call once at startup; nil is tolerated (tests that never
// touch the workspace seam leave it unset, and Storage() returns nil).
func (s *Spawner) SetStorage(blobs storage.Storage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs = blobs
}

// Storage returns the wired blob store, or nil if none was set. It is the
// read side of the seam for the blueprint-workspace snapshot/rehydrate
// path; the lock keeps it race-free against a startup-time SetStorage.
func (s *Spawner) Storage() storage.Storage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blobs
}

// resolveRunCredentials resolves the per-(org, owner) GitHub client and
// the run's team default model. Both modes call this identically;
// mode-awareness lives inside the resolver (App token vs PAT) and the
// secrets reader (system door vs nil), not at the call site. owner is the
// GitHub account the run targets — empty for Jira runs, which don't
// pre-clone. teamID is the task's owning team, so a multi-team org honors
// each team's model choice (SKY-389 review #2); empty falls back to the
// org default team.
func (s *Spawner) resolveRunCredentials(ctx context.Context, orgID, owner, teamID string) (*ghclient.Client, string) {
	return s.resolveGHClient(ctx, orgID, owner), s.resolveModel(ctx, orgID, teamID)
}

// resolveGHClient resolves the per-(org, owner) GitHub client via the
// SKY-389 resolver, falling back to the constructor-supplied client when
// no resolver is wired (test fixtures). A resolve failure returns nil:
// setupGitHub surfaces "GitHub credentials not configured" for GitHub
// tasks, and Jira runs don't need a client. The error is logged so a real
// backend failure (e.g. vault outage) isn't silent.
func (s *Spawner) resolveGHClient(ctx context.Context, orgID, owner string) *ghclient.Client {
	s.mu.Lock()
	resolver := s.ghResolver
	fallback := s.ghClient
	s.mu.Unlock()
	if resolver == nil {
		return fallback
	}
	client, err := resolver.ClientFor(ctx, orgID, owner)
	if err != nil {
		delegateLog.Warn("resolve GitHub client failed", "org", orgID, "target", owner, "error", err)
		return nil
	}
	return client
}

// resolveCloneToken resolves the App installation token for the host-side
// clone of a repo owned by owner, via the same SKY-389 resolver
// resolveGHClient uses — so the API client and the `git clone`/`git fetch`
// share one cached installation token. owner selects the
// installation.
//
// Multi-mode only by design: this ticket scopes host-side token injection to
// the hosted runtime (where SSH is unavailable and the App token is the only
// credential). Local clones deliberately keep their existing path — the
// operator's SSH key, or anonymous HTTPS — so local behavior is byte-for-byte
// unchanged. (CloneAuthFor independently no-ops on an SSH-form URL; the mode
// gate is what additionally leaves a local *HTTPS* clone uninjected, which is
// a separate future step per the ticket.)
//
// Returns "" when local, when no resolver is wired (test fixtures), or when
// resolution fails — the clone then proceeds with no injected credential. A
// resolve failure is logged so a real backend outage (e.g. vault down) isn't
// silent; the clone itself surfaces the auth error if the repo is private.
func (s *Spawner) resolveCloneToken(ctx context.Context, orgID, owner string) string {
	if runmode.Current() == runmode.ModeLocal {
		return ""
	}
	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return ""
	}
	tok, err := resolver.TokenFor(ctx, orgID, owner)
	if err != nil {
		delegateLog.Warn("resolve clone token failed", "org", orgID, "target", owner, "error", err)
		return ""
	}
	return tok.Value
}

// gitProxyConfigFor builds the per-run git-egress wiring for a run
// targeting a repo owned by owner: a gitproxy TokenSource backed by the
// same resolver resolveCloneToken uses, so the host-side clone and the
// in-sandbox push resolve one credential (App installation token or org
// PAT, App preferred — TokenFor selects the owner's installation and
// falls through to the PAT). agentproc holds the token host-side and
// routes the sandbox git at the proxy; the real credential never enters
// the box.
//
// Returns nil — disabling the git proxy — in local mode (the agent runs
// on the host with the operator's own git credentials), when no resolver
// is wired (test fixtures), or when owner is empty (Jira-only runs that
// pre-clone nothing). agentproc ignores a nil GitProxy.
//
// Upstream is resolved through the resolver's authoritative base
// resolution (org_settings → legacy github_url secret → github.com) so a
// GHES org's sandbox git routes to, and the insteadOf rewrite matches,
// its own host rather than github.com; a read error degrades to "" and
// agentproc defaults it to github.com. Failing safe: a wrong base only
// makes the insteadOf prefix miss the worktree remote, so the push is
// dropped closed at the egress allowlist — the credential is never sent
// to the wrong host.
//
// The closure maps the resolver's no-credentials sentinel to
// agentproc.ErrNoSandboxGitCredentials so a misconfigured org surfaces a
// clear admin-facing failure at run start rather than a confusing git
// error from inside the sandbox. The resolver is read under the same
// lock as resolveCloneToken so a startup-time credential hot-swap can't
// race it.
func (s *Spawner) gitProxyConfigFor(ctx context.Context, orgID, owner string) *agentproc.GitProxyConfig {
	if runmode.Current() == runmode.ModeLocal {
		return nil
	}
	if owner == "" {
		return nil
	}
	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		return nil
	}

	upstream := ""
	if base, err := resolver.BaseURLFor(ctx, orgID); err != nil {
		delegateLog.Warn("resolve git host base failed; leaving upstream empty; agentproc defaults to github.com", "org", orgID, "error", err)
	} else {
		upstream = base
	}

	return &agentproc.GitProxyConfig{
		Upstream: upstream,
		TokenSource: func(ctx context.Context) (gitproxy.Token, error) {
			tok, err := resolver.TokenFor(ctx, orgID, owner)
			if err != nil {
				if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
					return gitproxy.Token{}, fmt.Errorf("%w: org %s owner %s", agentproc.ErrNoSandboxGitCredentials, orgID, owner)
				}
				return gitproxy.Token{}, err
			}
			return gitproxy.Token{Value: tok.Value, ExpiresAt: tok.ExpiresAt}, nil
		},
	}
}

// resolveModel resolves the run's team default model via the SKY-389
// resolver, falling back to the constructor-supplied model when no resolver
// is wired (test fixtures) or the resolver returns empty. teamID is the
// run's owning team; empty falls back to the org default team inside the
// resolver.
func (s *Spawner) resolveModel(ctx context.Context, orgID, teamID string) string {
	s.mu.Lock()
	fn := s.modelFor
	fallback := s.model
	s.mu.Unlock()
	if fn != nil {
		if m := fn(ctx, orgID, teamID); m != "" {
			return m
		}
	}
	return fallback
}

// getRunSecrets returns the per-org LLM-credential reader threaded into
// RunOptions.Secrets: nil in local (ambient subscription fallback), the
// system-door reader in multi. SKY-389.
func (s *Spawner) getRunSecrets() agentproc.SecretsReader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runSecrets
}

func (s *Spawner) updateStatus(orgID, runID, status string) {
	// Transient progress states (fetching, cloning, agent_starting,
	// running) — no guard needed; the caller knows the prior row is
	// non-terminal. Goroutine-internal, no JWT claims in scope, so
	// admin pool.
	if err := s.agentRuns.SetStatusSystem(context.Background(), orgID, runID, status); err != nil {
		delegateLog.Warn("update status for run failed", "run", runID, "error", err)
	}
	s.broadcastRunUpdate(orgID, runID, status)
	// Board placement is no longer mirrored per-run from transient status here:
	// the blueprint orchestrator drives the aggregate column via
	// recomputeTaskBoardColumn at its transition points (blueprint start, step
	// start, park, resume). updateStatus stays a pure run-status + WS helper.
}

// recomputeTaskBoardColumn is the blueprint-era board placement rule: a task's
// live column (tasks.status) is a recomputed aggregate over its active
// blueprint_run's step runs, never a mirror of one run. For a bot-claimed task
// with an active blueprint_run it sets in_review ("needs 👀") if any of that
// run's runs is parked (open OR pending_approval — one column for both
// non-executing states), else in_progress, writing tasks.status only when it
// changes and pushing a WS nudge so peer boards follow.
//
// Terminal columns are NOT owned here — terminateBlueprint closes the task
// (done) on a clean finish and leaves it open for attention on abort/fail. This
// helper deliberately no-ops when there is no active blueprint_run.
//
// The rule has no step-count gate: a 1-step and a multi-step blueprint move
// identically, bouncing in_progress ↔ in_review across however many
// human-interaction points the blueprint has, and the aggregate-over-runs shape
// is parallel-ready by construction (more concurrent runs just feed it). A user
// claim is column-neutral: it flips the claim to the user, so the bot-claim guard
// below short-circuits and the column doesn't move.
//
// All failures are logged-not-fatal — the run state is already persisted and
// broadcast; a failed board write leaves a recoverable state the next transition
// reconciles.
func (s *Spawner) recomputeTaskBoardColumn(orgID, taskID string) {
	if s.tasks == nil || s.blueprints == nil {
		return
	}
	ctx := context.Background()
	task, err := s.tasks.GetSystem(ctx, orgID, taskID)
	if err != nil || task == nil {
		return
	}
	// Only place bot-claimed tasks. A user takeover flips the claim to the user,
	// who owns the lifecycle from then on — leave their card alone.
	if task.ClaimedByAgentID == "" {
		return
	}
	// A closed/dismissed task is terminal — a late transition must not reopen it.
	if task.Status == "done" || task.Status == "dismissed" {
		return
	}
	// The active blueprint_run is the unit the board tracks. None → no live work
	// to place; the terminal column belongs to terminateBlueprint, so leave the
	// task as-is. When the task was re-delegated, the latest running blueprint_run
	// drives the column, so a stale older run's transitions resolve against the
	// newer run's state rather than clobbering it.
	br, err := s.blueprints.ActiveRunForTaskSystem(ctx, orgID, taskID)
	if err != nil || br == nil {
		return
	}
	runs, err := s.blueprints.RunsForBlueprintSystem(ctx, orgID, br.ID)
	if err != nil {
		return
	}
	target := "in_progress"
	for _, r := range runs {
		if r.Status == "open" || r.Status == "pending_approval" {
			target = "in_review"
			break
		}
	}
	// Idempotent: skip the write + WS broadcast when already at the target.
	if task.Status == target {
		return
	}
	if err := s.tasks.SetStatusSystem(ctx, orgID, taskID, target); err != nil {
		delegateLog.Warn("set board column for task failed", "column", target, "task", taskID, "error", err)
		return
	}
	s.broadcastTaskUpdate(orgID, taskID, target)

	// TFAC-300: mirror the board move back onto the Jira ticket under the org's
	// system/bot credential. Past the idempotency guard, so this fires only on a
	// real column change — and both in_progress and in_review collapse to the
	// same InProgress bucket, so bouncing across human-interaction points makes
	// at most one real Jira move (the rest no-op in the mirror's own membership
	// skip). task is bot-claimed here (guarded above), so the write is always
	// bot-attributed by construction.
	s.mirrorJiraInProgress(orgID, task)
}

// broadcastTaskUpdate emits a SKY-330 task_updated WS event so the
// board can refetch / patch the card without polling. Payload
// matches the shared event shape (task_id + status) the other
// emitters use (handleSwipe, handleSnooze, handleTaskAdvance,
// finalizeRequeue), so the FE's typed WSEvent ('task_updated':
// {task_id, status}) holds across producers.
func (s *Spawner) broadcastTaskUpdate(orgID, taskID, status string) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]string{"task_id": taskID, "status": status},
	})
}

// updateBreakerCounter is a no-op stub. The breaker is now query-based
// (see routing.Router + db.CountConsecutiveFailedRuns). Kept as a call site
// placeholder until all callers are cleaned up.
func (s *Spawner) updateBreakerCounter(taskID, triggerType, status string) {
	// Breaker is query-based now — no per-task counter to update.
	// See internal/routing/router.go and internal/db/tasks.go.
}

// broadcastRunUpdate stamps the run's owning org on the event so the
// hub's per-connection scoping filter routes it only to clients
// authed against that tenant. Every caller is inside a goroutine
// that already has orgID in scope (the run's tenant, set at
// Delegate() entry and threaded through every helper).
func (s *Spawner) broadcastRunUpdate(orgID, runID, status string) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(websocket.Event{
		Type:  "agent_run_update",
		OrgID: orgID,
		RunID: runID,
		Data:  map[string]string{"status": status},
	})
}

func (s *Spawner) broadcastMessage(orgID, runID string, msg *domain.AgentMessage) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(websocket.Event{
		Type:  "agent_message",
		OrgID: orgID,
		RunID: runID,
		Data:  msg,
	})
}
