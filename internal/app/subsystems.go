package app

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/grantmirror"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/marketplacestats"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/reachcache"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// buildInfra wires the in-process event bus and lets the GitHub webhook
// receiver publish verified deliveries onto it. The bus is built in every
// role (the spawner's run-sentinel publisher and the router both attach to
// it); the server hook only fires on serveHTTP roles, where a server exists
// to receive webhook deliveries.
func (a *App) buildInfra() {
	a.bus = eventbus.New()
	if a.srv != nil {
		a.srv.SetEventBus(a.bus)
	}
}

// buildAI constructs the two background-LLM Managers — the scorer and the
// repo-profiler. Both resolve per-run LLM credentials through the
// run-credential seam wired in buildRunCredentials, and both share one
// system-job concurrency limiter. Neither starts background work here: each
// is a per-org Manager that lazy-starts a runner on the first Trigger
// (driven by the system:poll: bus subscribers), never an explicit Start in
// startWorkers.
func (a *App) buildAI() {
	// Shared system-job sandbox cap: one limiter injected into both
	// background Managers so their per-org runners can't fan out an
	// unbounded number of gVisor sandboxes across tenants in multi-mode. It
	// deliberately does NOT gate interactive sessions or delegated runs
	// (delegated has its own cap). Threaded the same way
	// a.llmRecorder is.
	//
	// Applied in both modes (a real cap, not nil): in multi it bounds gVisor
	// sandboxes; in local — where agentproc.Run is a direct subprocess with no
	// sandbox — it's still a modest ceiling on concurrent background LLM
	// processes. nil (unlimited) is reserved for callers that opt out (tests).
	sysLimiter := syslimit.New(syslimit.DefaultMaxConcurrentSystemRuns)

	// The org's background-jobs model, read per cycle from org_settings. One
	// resolver shared by the scorer; the profiler resolves from the settings
	// row it already reads for the clone protocol. A cycle whose org has no
	// usable model skips and says so — there is no fallback model to
	// substitute.
	systemJobModel := systemllm.NewModelFunc(a.stores.Orgs)

	a.scorer = ai.NewManager(a.stores.Scores, a.stores.Entities, a.runSecrets, llmcred.SystemEnvResolver(a.llmResolver, "tf-scorer"), a.llmRecorder, sysLimiter, systemJobModel, ai.RunnerCallbacks{
		OnScoringStarted: func(orgID string, taskIDs []string) {
			a.wsHub.Broadcast(websocket.Event{
				Type:  "scoring_started",
				OrgID: orgID,
				Data:  map[string]any{"task_ids": taskIDs},
			})
		},
		OnScoringCompleted: func(ctx context.Context, orgID string, taskIDs []string) {
			a.wsHub.Broadcast(websocket.Event{
				Type:  "scoring_completed",
				OrgID: orgID,
				Data:  map[string]any{"task_ids": taskIDs},
			})
			// Post-scoring re-derive: check deferred triggers whose
			// min_autonomy_suitability threshold the scored tasks now meet.
			// Async so it doesn't block the scorer clearing its running flag.
			// a.router is set in buildRouting (before Run), so it's non-nil
			// by the time any scoring cycle completes.
			//
			// WithoutCancel, not Background: the cycle that scheduled this
			// returns immediately (and its ctx dies with it), but the work
			// this fires — deferred triggers, conversations, claims — must
			// finish, while the cycle's values stay attached so the
			// re-derive is still recognizably part of the scoring that
			// caused it.
			if a.router != nil {
				go a.router.ReDeriveAfterScoring(context.WithoutCancel(ctx), orgID, taskIDs)
			}
		},
		OnReDeriveOwed: func(ctx context.Context, orgID string, taskIDs []string) {
			// The crash backstop for the callback above: tasks whose scores
			// committed while the process died before their re-derive ran.
			// Same pass, same WithoutCancel reasoning — a half-fired deferred
			// trigger is worse than a late one — but synchronous, because the
			// cycle must not start writing fresh scores while this decides
			// whether to clear the marks the last one left.
			if a.router != nil {
				a.router.ReDeriveAfterScoring(context.WithoutCancel(ctx), orgID, taskIDs)
			}
		},
		OnTasksSkipped: func(orgID string, skipped, total int) {
			toast.Warning(a.wsHub, orgID, fmt.Sprintf("AI scoring: %d of %d tasks skipped this cycle", skipped, total))
		},
		OnError: func(orgID string, err error) {
			toast.Error(a.wsHub, orgID, fmt.Sprintf("AI scoring cycle aborted: %v", err))
		},
	})
	// SetScorerTrigger wires the brain-lease-aware relay wrapper
	// (relay.go), not a.scorer.Trigger directly: the config-save handler
	// that calls this may run on a standby control pod, where a.scorer
	// exists (buildAI runs on every brain-capable role) but Triggering it
	// in-process would be silently pointless — this process's scorer
	// Runner never receives poll-completion sentinels because its own
	// poller isn't running. triggerScorer relays over tf_ctl instead when
	// this pod isn't the holder (TFAC-583).
	a.srv.SetScorerTrigger(a.triggerScorer)
	aiLog.Info("scorer manager ready (per-org runners)")

	// Repo-profiling manager: per-org Runners profiling configured repos off
	// the system:poll: "profiler" subscriber (TTL-gated per cycle) and the
	// explicit re-profile button (force). Sibling to the scorer — both react
	// to poll sentinels independently; scoring does NOT gate on profiling.
	a.profiler = repoprofile.NewManager(a.ghResolver, a.runSecrets, llmcred.SystemEnvResolver(a.llmResolver, "tf-profiler"), a.stores.Repos, a.stores.Orgs, a.llmRecorder, sysLimiter, a.wsHub)
	// Multi only: scope repository_updated delivery to each repo's REST
	// visibility (org admins + tracking-team members), resolved per
	// emission — the hub has no team axis, so an unscoped broadcast would
	// stream every team's repo profiles to the whole org. Local keeps the
	// org-wide broadcast: N=1, no team boundary.
	if !a.local() {
		a.profiler.SetRecipients(a.stores.TeamGitHubRepos.RepoUpdateRecipientsSystem)
	}
	// Chain bare-clone warming off profile-cycle completion: profiling
	// populates repositories.clone_url, which bootstrapBareClones reads.
	// Local-only — the warm on-disk bare cache is an N=1 affordance; multi
	// clones per-run inside the sandbox, so there's nothing to warm here.
	if a.local() {
		a.profiler.SetOnCycleComplete(func(orgID string) {
			bootstrapBareClones(a.stores.Repos, a.stores.Secrets)
		})
	}
	// SetProfilerTrigger: same relay-wrapper reasoning as SetScorerTrigger
	// above — the re-profile button may be clicked against a standby
	// control pod.
	a.srv.SetProfilerTrigger(a.triggerProfiler)
	repoprofileLog.Info("repo-profiling manager ready (per-org runners)")

	// Artifact reconciler: per-org Runners mirroring artifacts against live
	// GitHub state off the system:poll: GitHub sentinel (TFAC-464), a sibling
	// to the scorer/profiler — same per-org isolation so a slow
	// reconcile on one tenant can't head-of-line-block another. The shared
	// Reconciler is also handed to the server for the Tier-2 run-scoped refresh
	// endpoint, so foreground and background reconciliation run one code path.
	reconciler := reconcile.NewReconciler(a.ghResolver, a.stores.Artifacts, a.stores.TaskMemory, a.wsHub)
	a.reconciler = reconcile.NewManager(reconciler)
	a.srv.SetReconciler(reconciler)
	reconcileLog.Info("artifact reconciler ready (per-org runners)")

	// Marketplace run-derived listing stats (TFAC-540): multi-mode only —
	// the marketplace itself doesn't exist in local mode (db.Stores.Marketplace
	// is a stub there), so there is nothing to aggregate. a.marketplaceStats
	// stays nil in local mode; registerSubscribers checks that before wiring
	// the bus subscriber, matching the ticket's "gate its trigger on
	// runmode.Current()" instruction.
	if runmode.Current() == runmode.ModeMulti {
		a.marketplaceStats = marketplacestats.NewManager(marketplacestats.NewAggregator(a.stores.Marketplace))
		aiLog.Info("marketplace stats manager ready (per-org runners)")
	}

	// The App-installation grant reconcile, built here because two callers need
	// the same instance: buildRouting hands its RunOrg to the poller (once per
	// org per GitHub cycle, no TTL) and the reachable-cache refresher below
	// makes it the App tier's refresh (TTL-gated, forceable). One reconcile
	// object, two cadences — rather than a second enumeration path that could
	// disagree with the first about what an installation reaches.
	a.grantReconciler = grantmirror.NewReconciler(a.stores.GitHubApps, a.stores.ReachableRepos, a.ghResolver)

	// Reachable-repo cache: per-org Runners refreshing the mirror the repository
	// picker and the team-repos write gate both read. Sibling to the profiler in
	// shape (TTL + force, per-org single flight) and deliberately NOT a
	// system:poll: subscriber: its PAT arm is a full account enumeration, and
	// hanging that off every poll completion would spend the rate-limit budget
	// the pollers themselves need. It is kicked by the reads that care —
	// a stale-mirror picker open — and forced by every credential change.
	//
	// The class resolver is built from the same two stores the server builds its
	// own from: it is stateless, so two copies are two callers of one pair of
	// reads rather than two answers, and keeping the definition in one type is
	// what stops writer and reader from keying an org differently.
	a.reachCache = reachcache.NewManager(reachcache.NewRefresher(
		reachcache.NewClassResolver(a.stores.Orgs, a.stores.GitHubApps),
		a.stores.ReachableRepos, a.ghResolver, a.grantReconciler.RunOrg, a.wsHub,
	))
	// SetReachTrigger: same relay-wrapper reasoning as SetScorerTrigger above —
	// the picker read and the refresh control may land on a standby control pod,
	// which builds a Manager (buildAI runs on every brain-capable role) that
	// nothing would ever drive.
	a.srv.SetReachTrigger(a.triggerReach)
	reachLog.Info("reachable-repo cache manager ready (per-org runners)", "ttl", reachcache.TTL)
}

// buildExecution constructs the delegation spawner, wiring it to the
// run-credential seam. Runs after buildAI and before buildRouting (the
// router takes the spawner as its delegator). The spawner↔router back-edge
// is closed later in wire.
func (a *App) buildExecution() error {
	// Per-run credentials resolve through the run-credential seam, not a
	// process-global hot-swap.
	a.spawner = delegate.NewSpawner(a.database, a.stores, nil, a.wsHub, "")
	// Mirror run status/activity onto the bus (TFAC-592) so an EE
	// subscriber (ExtensionAPI.Bus()) can observe run lifecycle — the
	// bus is built in buildInfra, which runs before buildExecution.
	a.spawner.SetEventPublisher(a.bus)
	// Replace the constructor's random per-boot uuid with the persistent
	// instance-registry identity registerInstance minted above —
	// claims.executor_id on claimed rows must equal the registry id, and
	// RunInstanceHeartbeat's fenced renewal needs the matching boot_epoch.
	a.spawner.SetExecutorID(a.identity.ID, a.bootEpoch)
	// Dispatcher concurrency is a deployment decision: the default of 8 fits
	// ordinary hardware, while a provisioned multi-mode host handles
	// far more (memory-bound; see the TF_MAX_CONCURRENT_CLAIMS guidance in
	// .env.example for the sizing numbers). Resolved before RunDispatcher
	// starts — resizing later would strand semaphore tokens.
	rawMaxConcurrentClaims := os.Getenv("TF_MAX_CONCURRENT_CLAIMS")
	capRuns := delegate.DefaultMaxConcurrentClaims
	if n, clamped, err := delegate.ParseMaxConcurrentClaims(rawMaxConcurrentClaims); err != nil {
		appLog.Warn("max concurrent claims", "error", err)
	} else if clamped {
		// Distinct from the effective-cap log below: an operator asked for more
		// than the sandbox subnet allocator can ever honor, not just a value
		// above the default. Without this, a requested 1000 and an explicitly
		// set 256 would log identically and the operator sizing for a bigger
		// host would never see their setting got capped.
		capRuns = n
		a.spawner.SetMaxConcurrentClaims(n)
		appLog.Warn("max concurrent claims requested above sandbox ceiling; clamped", "requested", rawMaxConcurrentClaims, "cap", n)
	} else if n != delegate.DefaultMaxConcurrentClaims {
		capRuns = n
		a.spawner.SetMaxConcurrentClaims(n)
	}
	// Always name the effective cap, default included and in both modes — a
	// burst of delegations queues behind this number, and "runs sit queued"
	// must trace back to it from the boot log alone (local mode skips the
	// multi-only capacity advertisement below entirely).
	appLog.Info("claim concurrency cap", "cap", capRuns, "env", "TF_MAX_CONCURRENT_CLAIMS")
	// Memory guardrail companion to the cap above: the cap bounds how many
	// engagements may execute, the floor stops new claims when the host is out of
	// headroom regardless of the cap. Fails open off-Linux and when the
	// probe can't read /proc/meminfo. Resolved before the capacity warning
	// below so that warning can say whether the floor is actually armed,
	// rather than asserting a guardrail that TF_DISPATCH_MEM_FLOOR_MB=0
	// may have disabled.
	// How long a cold resume waits on a workspace persist that is still in
	// flight before falling back. Resolved here beside the other dispatcher
	// knobs; the default is the hung-writer backstop rather than an expected
	// wait, so an operator normally never touches it.
	if wait, werr := delegate.ParseSnapshotWaitTimeout(os.Getenv("TF_SNAPSHOT_WAIT_SEC")); werr != nil {
		appLog.Warn("workspace snapshot wait", "error", werr)
	} else if wait != delegate.DefaultSnapshotWait {
		a.spawner.SetSnapshotWaitTimeout(wait)
		appLog.Info("workspace snapshot wait configured", "wait", wait, "env", "TF_SNAPSHOT_WAIT_SEC")
	}
	floor, err := delegate.ParseDispatchMemFloorMB(os.Getenv("TF_DISPATCH_MEM_FLOOR_MB"))
	a.spawner.SetDispatchMemFloor(floor)
	if err != nil {
		appLog.Warn("dispatch memory floor", "error", err)
	} else if floor == 0 {
		appLog.Info("dispatch memory guardrail disabled (TF_DISPATCH_MEM_FLOOR_MB=0)")
	} else if floor != delegate.DefaultDispatchMemFloorMB {
		appLog.Info("dispatch memory floor configured", "floor_mb", floor)
	}
	// Advertise what this host's memory supports and flag an over-
	// provisioned cap at boot, when the operator is watching — a cap the
	// hardware can never honor deserves a loud line now, not a mystery
	// throttle under load. Multi mode only: capacity planning is a
	// deployment concern, and a laptop's numbers are just noise.
	if runmode.Current() == runmode.ModeMulti {
		if total := hostmem.TotalMB(); total != hostmem.Unknown {
			// Platform reserve is role-aware: a dedicated executor pod hosts
			// none of the co-resident platform stack (Postgres/GoTrue/object
			// store) the all-in-one default models, so it reserves far less —
			// otherwise a normal ~8 GB executor derives an advisory capacity of
			// 0. Env-tunable via TF_PLATFORM_RESERVE_MB.
			roleReserve := delegate.DefaultPlatformReserveMB
			if a.plan.role == runmode.RoleExecutor {
				roleReserve = delegate.DefaultExecutorPlatformReserveMB
			}
			reserve, rerr := delegate.ParsePlatformReserveMB(os.Getenv("TF_PLATFORM_RESERVE_MB"), roleReserve)
			if rerr != nil {
				appLog.Warn("platform reserve", "error", rerr)
			}
			derived := delegate.DerivedRunCapacityWithReserve(total, reserve)
			appLog.Info("host claim capacity",
				"mem_total_mb", total,
				"budget_per_claim_mb", delegate.DefaultClaimMemoryBudgetMB,
				"platform_reserve_mb", reserve,
				"derived_capacity", derived)
			if capRuns > derived {
				msg := "max concurrent claims exceeds derived host capacity; the host may not have enough RAM to run the cap concurrently"
				if floor > 0 {
					msg += " (the dispatch memory floor may throttle before the cap is reached, but is not guaranteed to be the first limiter)"
				} else {
					msg += " (TF_DISPATCH_MEM_FLOOR_MB=0 — the dispatch memory floor guardrail is disabled)"
				}
				appLog.Warn(msg, "cap", capRuns, "derived_capacity", derived, "mem_total_mb", total)
			}
		}
	}
	a.spawner.SetRunCredentialResolvers(a.ghResolver, a.runSecrets, a.modelFor)
	// Role-mode Bedrock minting for delegated runs on the all/local path
	// (TFAC-616). nil in local mode (ambient); on the executor role the
	// bundle path resolves LLM material, so this is only exercised at
	// TF_ROLE=all where a run resolves in-process.
	if a.llmResolver != nil {
		a.spawner.SetLLMResolver(a.llmResolver)
	}
	// TFAC-300: the board→Jira lifecycle mirror resolves the org's system/bot
	// Jira credential per write through this resolver (same construction the
	// server + poller use). A fresh instance is fine — the resolver is stateless,
	// reading creds fresh each call, so a config-change hot-swap is honored.
	a.spawner.SetJiraResolver(jira.NewResolver(a.stores.Secrets, a.stores.Orgs))
	// Hand the full Stores bundle so the sandbox-branch agenthost daemon can
	// serve the routing-sensitive RPCs the agent's `triagefactory exec`
	// invocations send. Nil-safe inside the spawner on local/non-sandbox.
	a.spawner.SetStores(a.stores)

	// Durable blob store for the blueprint workspace seam. Local → on-disk
	// under the state root (no new service, can't brick a local boot); multi
	// → an S3-compatible store from TF_BLOB_*, validated here so a
	// misconfigured deployment fails fast rather than on its first snapshot.
	blobStore, err := storage.New()
	if err != nil {
		return fmt.Errorf("storage init: %w", err)
	}
	a.blobStore = blobStore
	a.spawner.SetStorage(blobStore)

	// The team knowledge base: one process-wide seam over the same blob store
	// in multi, plain files under the state root in local. Both halves of the
	// product read it — the HTTP surface below, and the spawner, which stages
	// a run's knowledge from it at claim — so it is built once here and shared.
	a.teamKB = kbstore.New(blobStore)
	a.spawner.SetTeamKB(a.teamKB)

	// Cross-pod run control (TFAC-585): the conversation_signals outbox is Postgres-
	// only, so this is the ONE gate that keeps local mode structurally free
	// of conversation_signals writes — s.controller stays the plain
	// inProcessController unless SetConversationSignals is called, and it is only
	// ever called here, behind this mode check. Wired for every role in
	// multi mode (not just dispatcher-capable ones): a control pod's HTTP
	// handlers need the cross-pod controller to reach a run living on an
	// executor just as much as an executor needs it to apply signals
	// targeting itself.
	if runmode.Current() == runmode.ModeMulti {
		a.spawner.SetConversationSignals(a.stores.ConversationSignals, a.database)
		if ackTimeout, terr := delegate.ParseSignalAckTimeout(os.Getenv("TF_SIGNAL_ACK_TIMEOUT")); terr != nil {
			appLog.Warn("signal ack timeout", "error", terr)
		} else if ackTimeout != delegate.DefaultSignalAckTimeout {
			a.spawner.SetSignalAckTimeout(ackTimeout)
			appLog.Info("signal ack timeout configured", "timeout", ackTimeout)
		}
	}

	// Control pods build the spawner (the router enqueues runs through it,
	// and the run-control HTTP endpoints reach live runs through it) but
	// never run the dispatcher, so their registry row must not advertise
	// dispatcher capacity — leave those executor-only columns NULL.
	if !a.plan.dispatcher {
		a.spawner.SetReportCapacity(false)
	}

	// The server handle below only exists on serveHTTP roles.
	if !a.plan.serveHTTP {
		return nil
	}
	a.srv.SetSpawner(a.spawner)
	// The team knowledge base rides the same blob seam in multi and plain files
	// under the state root in local (internal/kbstore picks by mode). Wired
	// here rather than in buildServer because the blob store it is built over
	// is resolved with the rest of this subsystem.
	a.srv.SetTeamKB(a.teamKB)
	return nil
}

// buildRouting wires the durable ingest seam, the poller manager, and the
// event router. The poller/tracker emit through the ingestor (github:/jira:
// events are durably enqueued so the router can't drop them under burst),
// and the router drains the event_queue rather than the lossy in-memory bus.
func (a *App) buildRouting() {
	// Poll errors are toasted with per-source time-based throttling: the
	// poller fires OnError on every failure, but a persistent failure
	// (expired PAT, outage) would otherwise spam a sticky toast every cycle.
	const errorToastMinInterval = 5 * time.Minute
	var (
		errorThrottleMu sync.Mutex
		lastErrorToast  = map[string]time.Time{}
	)

	// eventWake is a best-effort, coalescing nudge to the router's drain
	// worker; a dropped wake only delays a drain to the worker's floor scan,
	// never loses an event.
	a.eventWake = make(chan struct{}, 1)
	wake := func() {
		select {
		case a.eventWake <- struct{}{}:
		default: // a wake is already pending; the drainer will see this event
		}
	}
	a.ingestor = ingest.New(a.bus, a.stores.EventQueue, wake)
	a.srv.SetIngestor(a.ingestor)

	a.pollerMgr = poller.NewManager(a.database, a.ingestor, a.stores.Users, a.stores.Tasks, a.stores.Entities, a.stores.Repos, a.stores.EventQueue, a.stores.Orgs, a.stores.JiraStatusRules, a.stores.TeamGitHubGroups, a.stores.Secrets, a.stores.GitHubApps, a.ghResolver)
	// TFAC-573: GET /readyz's poller-alive hard check + per-org poll-
	// staleness soft signal read through this method.
	a.srv.SetPollerManager(a.pollerMgr.Health)
	// The App-installation grant mirror, refreshed by pull at the head of every
	// GitHub cycle. Deliberately NOT a system:poll: subscriber like the scorer /
	// profiler / reconciler: those all hang off a poll COMPLETION,
	// and a cycle that finds no installations never emits one — so a subscriber
	// would go silent for precisely the org whose mirror needs correcting. Its
	// leader gating is the poller's own, which is the same brain lease every
	// other timer-driven pass sits behind.
	a.pollerMgr.ReconcileGrant = a.grantReconciler.RunOrg
	// Skip a cycle for a source an org admin turned off. This is what makes
	// "a turned-off source makes zero API calls" true, which is a promise
	// measurable from the other end; the router's drop beside it is what makes
	// "no tasks" true for every producer at once.
	a.pollerMgr.EventSources = a.stores.OrgEventSources
	a.pollerMgr.OnError = func(source, orgID string, err error) {
		// Throttle key includes orgID so a chronic failure on one tenant
		// doesn't suppress a fresh failure on another. Process-level errors
		// pass orgID="" and throttle together per source.
		throttleKey := source + ":" + orgID
		errorThrottleMu.Lock()
		if last, ok := lastErrorToast[throttleKey]; ok && time.Since(last) < errorToastMinInterval {
			errorThrottleMu.Unlock()
			return
		}
		lastErrorToast[throttleKey] = time.Now()
		errorThrottleMu.Unlock()

		label := "Jira"
		if source == "github" {
			label = "GitHub"
		}
		toast.ErrorTitled(a.wsHub, orgID, label, fmt.Sprintf("Poll failed: %v", err))
	}

	// Multi-mode only: let the server seed a bound user's trailing-window PR
	// history on identity bind / first dashboard load (TFAC-396). Local mode
	// leaves this unwired — its per-cycle Phase 1b backfill owns dashboard
	// history and is self-healing, so an on-bind seed would be redundant.
	if runmode.Current() == runmode.ModeMulti {
		a.srv.SetDashboardBackfiller(a.pollerMgr.BackfillUserDashboard)
	}

	// Event router — records events, creates/bumps tasks, auto-delegates on
	// matching triggers, runs inline close checks. It drains the durable
	// event_queue (not the bus); the ingestor enqueues there at emit time.
	a.router = routing.NewRouter(a.stores.Prompts, a.stores.Blueprints, a.stores.EventHandlers, a.stores.Agents, a.stores.TeamAgents, a.stores.Users, a.stores.Tasks, a.stores.Conversations, a.stores.Entities, a.stores.PendingFirings, a.stores.Events, a.stores.Orgs, a.stores.Teams, a.stores.TeamGitHubRepos, a.stores.JiraStatusRules, a.stores.TeamGitHubGroups, a.spawner, a.scorer, a.wsHub)
	a.router.SetEventQueue(a.stores.EventQueue)
	// The event-source pause. This is the single funnel every source's events
	// cross, so it is where a paused source stops minting tasks — poll-driven
	// and push-driven alike. Not optional in production, unlike the poller
	// skip beside it: the answer is cached per org behind a short TTL and
	// invalidated by the admin write (in process, or over the tf_ctl relay).
	a.router.SetEventSourceGate(a.stores.OrgEventSources)
	// The post-scoring re-derive discharges tasks.rederive_owed through the
	// score store — the clear half of the mark UpdateTaskScores raises with
	// every scores write.
	a.router.SetReDeriveLedger(a.stores.Scores)
	// Mirror the per-event routing disposition sentinel onto the bus
	// (TFAC-593) so an async event source (e.g. Slack) can learn
	// synchronously-unavailable routing outcomes. The bus is built in
	// buildInfra, which runs before buildRouting.
	a.router.SetEventPublisher(a.bus)
	// Ownership-scoped boot recovery (TFAC-578): the router's event_queue
	// self-sweep needs the same persistent instance-registry identity the
	// spawner's conversation-queue self-sweep already uses (registerInstance
	// minted it at boot, above).
	a.router.SetExecutorID(a.identity.ID, a.bootEpoch)
}
