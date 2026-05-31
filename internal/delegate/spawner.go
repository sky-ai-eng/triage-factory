// The Spawner type — central coordinator for delegated agent runs — and
// the small cross-cutting helpers (status broadcasts, status updates,
// drainer/classification wiring) every other file in this package
// reaches for. The lifecycle methods (Delegate, Cancel, Takeover,
// Release, ResumeAfterYield) live in their own files; this one is the
// type definition + the bits that don't belong anywhere else.

package delegate

import (
	"context"
	"database/sql"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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
	chains     db.ChainStore
	tasks      db.TaskStore      // re-read tasks for run lifecycle handlers
	agentRuns  db.AgentRunStore  // run lifecycle + transcript + yields
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
	// takeoverDir is the raw instance_config.server_takeover_dir
	// value read once at boot — may be empty, "~/..." or an
	// absolute path. The Release path runs it through
	// ResolveTakeoverDir to produce the actual filesystem path
	// (and to apply the empty-string → ~/.triagefactory/takeovers
	// default); the value held here is the unresolved input.
	// Passed via the constructor so Release doesn't re-read
	// instance_config on every request.
	takeoverDir string
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
	ghResolver ghclient.Resolver                    // per-(org, owner) GitHub client source (App token in multi, keychain PAT in local)
	runSecrets agentproc.SecretsReader              // per-org LLM-credential reader (nil in local → ambient subscription; system-door reader in multi)
	modelFor   func(context.Context, string) string // per-org default-model resolver (prompt.Model still overrides per delegation)

	cancels               map[string]context.CancelFunc                     // runID → cancel the entire run
	drainer               QueueDrainer                                      // nil-safe; set post-construction via SetQueueDrainer
	takenOver             map[string]bool                                   // runIDs claimed by Takeover. Sticky-on for the rest of the goroutine's lifetime even after rollback — clearing the entry would let late-firing goroutine gates race the takeover/abort lifecycle. Suppresses every cleanup path in runAgent so Takeover/abortTakeover own the row's terminal state.
	chainRunIDs           map[string]bool                                   // chain_run IDs whose setup phase reuses the per-run status helpers but is not backed by a runs row. broadcastRunUpdate skips wsHub emission for these so clients don't fetch /api/runs/{id} and 404.
	waitForClassification func(ctx context.Context, orgID, entityID string) // SKY-220 hook: blocks until the project classifier has decided this entity, or a timeout/ctx-cancel elapses. orgID scopes the classification read to the run's tenant (SKY-392 — the read goes through the org-scoped admin-pool store, not a raw query). Nil-safe (test setups skip it). Wired in main.go via SetWaitForClassification — keeps internal/delegate from importing internal/projectclassify.

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
// bundle plus the runtime knobs (GitHub client, WS hub, model,
// takeover dir). The Spawner keeps individual store fields rather
// than the full bundle so existing hot paths (s.tasks, s.entities,
// ...) stay put; New just unpacks once. Tests that only exercise a
// subset of stores can pass partial db.Stores{} — every field is a
// nil-safe interface.
func NewSpawner(database *sql.DB, stores db.Stores, ghClient *ghclient.Client, wsHub *websocket.Hub, model, takeoverDir string) *Spawner {
	return &Spawner{
		database:     database,
		prompts:      stores.Prompts,
		agents:       stores.Agents,
		chains:       stores.Chains,
		tasks:        stores.Tasks,
		agentRuns:    stores.AgentRuns,
		entities:     stores.Entities,
		reviews:      stores.Reviews,
		pendingPRs:   stores.PendingPRs,
		events:       stores.Events,
		taskMemory:   stores.TaskMemory,
		runWorktrees: stores.RunWorktrees,
		orgs:         stores.Orgs,
		tx:           stores.Tx,
		ghClient:     ghClient,
		wsHub:        wsHub,
		model:        model,
		takeoverDir:  takeoverDir,
		cancels:      make(map[string]context.CancelFunc),
		takenOver:    make(map[string]bool),
		chainRunIDs:  make(map[string]bool),
	}
}

// useSSHCloneProtocol returns true when the per-org GitHub clone
// protocol is "ssh". orgs is nil-safe and any store failure logs +
// defaults to HTTPS, matching the prior config.Load() degrade path.
func (s *Spawner) useSSHCloneProtocol(ctx context.Context, orgID, runID string) bool {
	if s.orgs == nil {
		return false
	}
	settings, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		log.Printf("[delegate] load org settings to pick clone protocol for run %s: %v (defaulting to HTTPS)", runID, err)
		return false
	}
	return settings.GitHubCloneProtocol == "ssh"
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

// wasTakenOver reports whether Takeover() has claimed this run. The
// flag is set the moment Takeover validates state, BEFORE the worktree
// hand-over and the DB mark — that's intentional: every cleanup path
// in runAgent (worktree.Remove defers, RemoveClaudeProjectDir defer,
// the natural-completion block, failRun, handleCancelled) checks this
// and short-circuits, which is what keeps the source worktree on disk
// while the hand-over runs and prevents a concurrent natural completion
// from overwriting the taken_over status.
//
// The flag is sticky-on once set: neither successful takeovers nor
// failed takeovers (rolled back via abortTakeover) ever clear the
// entry. Clearing would let any late-firing gate in the runAgent
// goroutine re-read wasTakenOver and proceed with normal cleanup,
// racing whatever Takeover/abortTakeover is doing — the goroutine's
// unconditional db.CompleteAgentRun would overwrite our terminal
// stop_reason, and its RemoveClaudeProjectDir would run alongside
// ours. Leaving the flag set keeps every gate closed and Takeover
// /abortTakeover the sole writer of the row's terminal state.
func (s *Spawner) wasTakenOver(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takenOver[runID]
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
//   - modelFor: per-org default model (the org's team default, capped). The
//     prompt's own Model still overrides this per delegation.
//
// Set once at startup, post-NewSpawner. Any of the three may be nil; the
// resolver helpers fall back to the constructor-supplied ghClient/model
// (the test / no-seam path).
func (s *Spawner) SetRunCredentialResolvers(resolver ghclient.Resolver, secrets agentproc.SecretsReader, modelFor func(context.Context, string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ghResolver = resolver
	s.runSecrets = secrets
	s.modelFor = modelFor
}

// resolveRunCredentials resolves the per-(org, owner) GitHub client and
// the org's default model for a run. Both modes call this identically;
// mode-awareness lives inside the resolver (App token vs PAT) and the
// secrets reader (system door vs nil), not at the call site. owner is the
// GitHub account the run targets — empty for Jira runs, which don't
// pre-clone.
func (s *Spawner) resolveRunCredentials(ctx context.Context, orgID, owner string) (*ghclient.Client, string) {
	return s.resolveGHClient(ctx, orgID, owner), s.resolveModel(ctx, orgID)
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
		log.Printf("[delegate] resolve GitHub client for org %s target %q: %v", orgID, owner, err)
		return nil
	}
	return client
}

// resolveModel resolves the org's default model via the SKY-389 resolver,
// falling back to the constructor-supplied model when no resolver is wired
// (test fixtures) or the resolver returns empty.
func (s *Spawner) resolveModel(ctx context.Context, orgID string) string {
	s.mu.Lock()
	fn := s.modelFor
	fallback := s.model
	s.mu.Unlock()
	if fn != nil {
		if m := fn(ctx, orgID); m != "" {
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
		log.Printf("[delegate] warning: failed to update status for run %s: %v", runID, err)
	}
	s.broadcastRunUpdate(orgID, runID, status)
	s.advanceTaskFromRunStatus(orgID, runID, status)
}

// advanceTaskFromRunStatus is SKY-330's bot-side auto-progression:
// when a bot-claimed task's run transitions through lifecycle
// stages, mirror the change onto the task's status so the board
// column placement follows the work. User-claimed tasks transition
// manually — this helper short-circuits on non-bot claims.
//
// Mapping:
//   - any active stage (initializing/cloning/fetching/.../running) → in_progress
//   - pending_approval                                              → in_review
//   - completed                                                     → done (close_reason=run_completed)
//   - failed / cancelled / task_unsolvable / taken_over             → no transition (user decides)
//
// All failures here are logged-not-fatal — the run state has already
// been persisted and broadcast; failing the task transition would
// leave the system in a recoverable state (next poll / next event
// reconciles).
func (s *Spawner) advanceTaskFromRunStatus(orgID, runID, runStatus string) {
	if s.tasks == nil {
		return
	}
	if s.isChainRunID(runID) {
		return
	}
	target, isClose := targetTaskStatusForRunStatus(runStatus)
	if target == "" {
		return
	}
	ctx := context.Background()
	run, err := s.agentRuns.GetSystem(ctx, orgID, runID)
	if err != nil || run == nil || run.TaskID == "" {
		return
	}
	// Chain step: the chain orchestrator owns task lifecycle (it
	// closes the task only when the chain itself terminates). A
	// pending_approval / completed mid-chain step must not flip the
	// task to in_review / done — the next step is about to run.
	// processCompletion's inline 'completed' path has the same
	// guard at run.go:435.
	if run.ChainRunID != "" {
		return
	}
	task, err := s.tasks.GetSystem(ctx, orgID, run.TaskID)
	if err != nil || task == nil {
		return
	}
	// Only mirror onto bot-claimed tasks. A user takeover may have
	// flipped the claim while this run was in flight; in that case
	// the user owns the lifecycle and we leave their card alone.
	if task.ClaimedByAgentID == "" {
		return
	}
	// Idempotent: skip the write (and the WS broadcast) when the
	// task is already at the target state. Prevents repeat updateStatus
	// calls for the same run state from flooding the bus.
	if task.Status == target {
		return
	}
	// Terminal-already check: a closed task should never re-open
	// based on a late run event. Run completions that arrive after
	// a user-driven close are silently ignored.
	if task.Status == "done" || task.Status == "dismissed" {
		return
	}
	// Re-delegation guard: if a newer active run exists for the
	// same task (the user re-delegated while this run was in flight),
	// an older run reaching pending_approval / completed must not
	// flip the task — the newer run is still working. processCompletion's
	// inline 'completed' path at run.go:438 has the same check; the
	// helper mirrors it so the two paths don't drift. Active-stage
	// targets (in_progress) are idempotent against the newer run's
	// own writes so the guard is harmless there too.
	hasOtherActive, _ := s.agentRuns.HasOtherActiveRunForTaskSystem(ctx, orgID, task.ID, runID)
	if hasOtherActive {
		return
	}
	if isClose {
		if err := s.tasks.CloseSystem(ctx, orgID, task.ID, "run_completed", ""); err != nil {
			log.Printf("[delegate] warning: failed to close task %s for completed run %s: %v", task.ID, runID, err)
			return
		}
	} else {
		if err := s.tasks.SetStatusSystem(ctx, orgID, task.ID, target); err != nil {
			log.Printf("[delegate] warning: failed to advance task %s to %q for run %s: %v", task.ID, target, runID, err)
			return
		}
	}
	s.broadcastTaskUpdate(orgID, task.ID, target)
}

// targetTaskStatusForRunStatus returns the task status a given run
// status should produce, plus whether the transition is a close
// (callers route to Close for the close_reason metadata). Empty
// target means no transition (e.g. failed, cancelled — user picks
// next step).
func targetTaskStatusForRunStatus(runStatus string) (target string, isClose bool) {
	switch runStatus {
	case "initializing", "cloning", "fetching", "worktree_created", "agent_starting", "running":
		return "in_progress", false
	case "pending_approval":
		return "in_review", false
	case "completed":
		return "done", true
	default:
		return "", false
	}
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

// markChainRunID flags a chain_run id so broadcastRunUpdate skips
// wsHub emission for it. The setup phase of a chain reuses the per-run
// status helpers with the chain_run id (the first step's runs row
// doesn't exist yet) — those UPDATEs are harmless no-ops, but the
// matching WS event causes clients to fetch /api/runs/{id} and 404.
func (s *Spawner) markChainRunID(id string) {
	s.mu.Lock()
	s.chainRunIDs[id] = true
	s.mu.Unlock()
}

func (s *Spawner) unmarkChainRunID(id string) {
	s.mu.Lock()
	delete(s.chainRunIDs, id)
	s.mu.Unlock()
}

func (s *Spawner) isChainRunID(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chainRunIDs[id]
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
	if s.isChainRunID(runID) {
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
