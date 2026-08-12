package app

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/marketplacestats"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/projectclassify"
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

// buildAI constructs the three background-LLM Managers — the scorer, the
// repo-profiler, and the project classifier. All resolve per-run LLM
// credentials through the run-credential seam wired in buildRunCredentials,
// and all share one system-job concurrency limiter. None starts background
// work here: each is a per-org Manager that lazy-starts a runner on the first
// Trigger (driven by the system:poll: bus subscribers), never an explicit
// Start in startWorkers.
func (a *App) buildAI() {
	// Shared across the three headless LLM jobs: each records its per-call
	// cost + token breakdown into system_llm_runs (TFAC-451). One recorder
	// over the single org-scoped store; a nil store would make Record a
	// no-op, but the bundle always wires one.
	llmRecorder := systemllm.NewRecorder(a.stores.SystemLLMRuns)

	// Shared system-job sandbox cap: one limiter injected into all
	// three background Managers so their per-org runners can't fan out an
	// unbounded number of gVisor sandboxes across tenants in multi-mode. It
	// deliberately does NOT gate the curator, interactive sessions, or
	// delegated runs (delegated has its own cap). Threaded the same way
	// llmRecorder is.
	//
	// Applied in both modes (a real cap, not nil): in multi it bounds gVisor
	// sandboxes; in local — where agentproc.Run is a direct subprocess with no
	// sandbox — it's still a modest ceiling on concurrent background Haiku
	// processes. nil (unlimited) is reserved for callers that opt out (tests).
	sysLimiter := syslimit.New(syslimit.DefaultMaxConcurrentSystemRuns)

	a.scorer = ai.NewManager(a.stores.Scores, a.stores.Entities, a.runSecrets, llmcred.SystemEnvResolver(a.llmResolver, "tf-scorer"), llmRecorder, sysLimiter, ai.RunnerCallbacks{
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
			// this fires — deferred triggers, runs, claims — must finish,
			// while the cycle's values stay attached so the re-derive is
			// still recognizably part of the scoring that caused it.
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
	aiLog.Info("scorer manager ready (per-org runners)", "model", "haiku")

	// Repo-profiling manager: per-org Runners profiling configured repos off
	// the system:poll: "profiler" subscriber (TTL-gated per cycle) and the
	// explicit re-profile button (force). Sibling to the scorer — both react
	// to poll sentinels independently; scoring does NOT gate on profiling.
	a.profiler = repoprofile.NewManager(a.ghResolver, a.runSecrets, llmcred.SystemEnvResolver(a.llmResolver, "tf-profiler"), a.stores.Repos, a.stores.Orgs, llmRecorder, sysLimiter, a.wsHub)
	// Chain bare-clone warming off profile-cycle completion: profiling
	// populates repo_profiles.clone_url, which bootstrapBareClones reads.
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
	repoprofileLog.Info("repo-profiling manager ready (per-org runners)", "model", "haiku")

	// Project classifier: per-org Runners, classifying newly-
	// discovered entities against existing projects via per-project Haiku
	// quorum vote off the system:poll: subscriber. Sticky — only fires on
	// entities with classified_at IS NULL. Sibling to the scorer/profiler:
	// per-org isolation so a large org's backlog can't head-of-line-block
	// another tenant's classification.
	a.classifier = projectclassify.NewManager(a.stores.Entities, a.stores.Projects, a.runSecrets, llmcred.SystemEnvResolver(a.llmResolver, "tf-classifier"), llmRecorder, sysLimiter)
	classifyLog.Info("project classifier manager ready (per-org runners)", "model", "haiku")

	// Artifact reconciler: per-org Runners mirroring artifacts against live
	// GitHub state off the system:poll: GitHub sentinel (TFAC-464), a sibling
	// to the scorer/profiler/classifier — same per-org isolation so a slow
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
}

// buildExecution constructs the delegation spawner and the curator runtime,
// wiring each to the run-credential seam. Runs after buildAI (the spawner
// blocks on the classifier before KB injection) and before buildRouting
// (the router takes the spawner as its delegator). The spawner↔router
// back-edge is closed later in wire.
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
	// runs.executor_id on claimed rows must equal the registry id, and
	// RunInstanceHeartbeat's fenced renewal needs the matching boot_epoch.
	a.spawner.SetExecutorID(a.identity.ID, a.bootEpoch)
	// Dispatcher concurrency is a deployment decision: the default of 8 fits
	// ordinary hardware, while a provisioned multi-mode host handles
	// far more (memory-bound; see the TF_MAX_CONCURRENT_RUNS guidance in
	// .env.example for the sizing numbers). Resolved before RunDispatcher
	// starts — resizing later would strand semaphore tokens.
	rawMaxConcurrentRuns := os.Getenv("TF_MAX_CONCURRENT_RUNS")
	capRuns := delegate.DefaultMaxConcurrentRuns
	if n, clamped, err := delegate.ParseMaxConcurrentRuns(rawMaxConcurrentRuns); err != nil {
		appLog.Warn("max concurrent runs", "error", err)
	} else if clamped {
		// Distinct from the effective-cap log below: an operator asked for more
		// than the sandbox subnet allocator can ever honor, not just a value
		// above the default. Without this, a requested 1000 and an explicitly
		// set 256 would log identically and the operator sizing for a bigger
		// host would never see their setting got capped.
		capRuns = n
		a.spawner.SetMaxConcurrentRuns(n)
		appLog.Warn("max concurrent runs requested above sandbox ceiling; clamped", "requested", rawMaxConcurrentRuns, "cap", n)
	} else if n != delegate.DefaultMaxConcurrentRuns {
		capRuns = n
		a.spawner.SetMaxConcurrentRuns(n)
	}
	// Always name the effective cap, default included and in both modes — a
	// burst of delegations queues behind this number, and "runs sit queued"
	// must trace back to it from the boot log alone (local mode skips the
	// multi-only capacity advertisement below entirely).
	appLog.Info("run concurrency cap", "cap", capRuns, "env", "TF_MAX_CONCURRENT_RUNS")
	// Memory guardrail companion to the cap above: the cap bounds how many
	// runs may execute, the floor stops new claims when the host is out of
	// headroom regardless of the cap. Fails open off-Linux and when the
	// probe can't read /proc/meminfo. Resolved before the capacity warning
	// below so that warning can say whether the floor is actually armed,
	// rather than asserting a guardrail that TF_DISPATCH_MEM_FLOOR_MB=0
	// may have disabled.
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
			appLog.Info("host run capacity",
				"mem_total_mb", total,
				"budget_per_run_mb", delegate.DefaultRunMemoryBudgetMB,
				"platform_reserve_mb", reserve,
				"derived_capacity", derived)
			if capRuns > derived {
				msg := "max concurrent runs exceeds derived host capacity; the host may not have enough RAM to run the cap concurrently"
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
	// One kbstore over the same blob store, shared by every KB consumer
	// (handlers, executor syncer, classifier, project bundle). In multi mode
	// it is the KB source of truth; local mode never reads it (the handlers
	// stay on their byte-identical on-disk path).
	a.kbStore = kbstore.New(blobStore)
	// The classifier is built earlier (buildAI) than the KB store, so hand it
	// the store now. Nil-safe: only brain-capable roles build a classifier.
	if a.classifier != nil {
		a.classifier.SetKBStore(a.kbStore)
	}

	// Cross-pod run control (TFAC-585): the conversation_signals outbox is Postgres-
	// only, so this is the ONE gate that keeps local mode structurally free
	// of conversation_signals writes — s.controller stays the plain
	// inProcessController unless SetRunSignals is called, and it is only
	// ever called here, behind this mode check. Wired for every role in
	// multi mode (not just dispatcher-capable ones): a control pod's HTTP
	// handlers need the cross-pod controller to reach a run living on an
	// executor just as much as an executor needs it to apply signals
	// targeting itself.
	if runmode.Current() == runmode.ModeMulti {
		a.spawner.SetRunSignals(a.stores.RunSignals, a.database)
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

	// Before reading entity.project_id for KB injection, the spawner blocks
	// until classified_at is set (or the timeout elapses). Wired
	// unconditionally on every role, including an executor (TFAC-583):
	// WaitFor only ever needs a.stores.Entities (always present) and a
	// trigger func — a.triggerClassifier relays the kick over tf_ctl to
	// whichever pod currently holds the background-brain lease when this
	// process has no local classifier Manager (an executor, or a standby
	// control pod) to Trigger in-process. Previously this only ran on
	// brain roles, so an executor's delegated runs proceeded without ever
	// kicking a fresh classification — they just read whatever project_id
	// already existed.
	a.spawner.SetWaitForClassification(func(ctx context.Context, orgID, entityID string) {
		projectclassify.WaitFor(ctx, a.stores.Entities, a.triggerClassifier, orgID, entityID, projectclassify.DefaultWaitTimeout)
	})

	// Curator runtime — per-project chat sessions. Built on serveHTTP roles
	// (control/all: SendMessage forwards or runs in-process) and, in multi mode,
	// on executors (the home executor executes homed turns off the claim loop,
	// spec §6.3). buildCuratorRuntime no-ops on pods that run no curator.
	if err := a.buildCuratorRuntime(); err != nil {
		return err
	}

	// The server handle below only exists on serveHTTP roles. An executor has
	// no server to hand the spawner/curator to — its curator (if built) runs
	// headless off the claim loop.
	if !a.plan.serveHTTP {
		return nil
	}
	a.srv.SetSpawner(a.spawner)
	if a.curator != nil {
		a.srv.SetCurator(a.curator)
	}
	// KB blob seam for the Knowledge panel handlers. The handlers branch on
	// runmode, so this is inert in local mode; the doorbell that nudges the
	// home executor to materialize a panel upload mid-session only exists in
	// multi mode (tf_ctl NOTIFY has no local analogue).
	a.srv.SetKBStore(a.kbStore)
	if runmode.Current() == runmode.ModeMulti {
		a.srv.SetKBChangedDoorbell(a.publishKBDoorbell)
	}
	return nil
}

// buildCuratorRuntime constructs the per-project curator (chat sessions) and
// wires the homing role each pod plays (curator homing, spec §6.3):
//
//   - serveHTTP roles (control/all) always build it — that is where a chat POST
//     lands. A control pod additionally gets the Homer + doorbell in
//     buildPlacement (which runs after the placement resolver exists), so its
//     SendMessage forwards to the home executor. role=all keeps the unchanged
//     in-process path (no Homer wired).
//   - multi-mode executors build it too and run the claim loop, so a homed turn
//     executes on the executor that owns the project's warm cache. Executors
//     serve no HTTP, so their curator runs headless.
//
// Any other pod (there is none today) no-ops.
func (a *App) buildCuratorRuntime() error {
	multi := runmode.Current() == runmode.ModeMulti
	executorRuntime := multi && a.plan.dispatcher && a.plan.role == runmode.RoleExecutor
	if !a.plan.serveHTTP && !executorRuntime {
		return nil
	}

	// Boot recovery: cancel curator turns stranded by this pod's previous boot
	// (a restart killed every session goroutine + subprocess, so a non-terminal
	// row is stranded — cancelling lets the user re-send rather than wait for a
	// mystery reply). local / role=all owned every row, so it sweeps globally;
	// the multi-mode split roles sweep only turns homed to THEMSELVES, so a
	// control restart never cancels an executor's live turns (the leader reaper
	// covers turns homed to a *dead* executor).
	a.sweepStrandedCuratorTurns(multi)

	a.curator = curator.New(a.stores, a.wsHub, "")
	a.curator.SetRunCredentialResolvers(a.ghResolver, a.runSecrets, a.modelFor)
	// Dead-letter cap for poisoned turns, resolved the same way the reaper's
	// TF_RUN_MAX_ATTEMPTS is: parsed once at wiring, a malformed value fails
	// boot rather than silently running with a default.
	turnMaxAttempts, err := curator.ParseTurnMaxAttempts(os.Getenv("TF_CURATOR_TURN_MAX_ATTEMPTS"))
	if err != nil {
		return fmt.Errorf("curator turn max attempts: %w", err)
	}
	a.curator.SetTurnMaxAttempts(turnMaxAttempts)
	// Persistent instance-registry identity, stamped onto every claims row a
	// dispatch mints — the same identity the delegation spawner stamps, so
	// the ownership-scoped boot sweep finds this pod's own strays.
	a.curator.SetExecutorIdentity(a.identity.ID, a.bootEpoch)
	// KB blob seam: in multi mode the executor materializes the
	// project KB from the store at turn start and reconciles disk→store at
	// turn end, so the curator holds the same *kbstore.Store the handlers use.
	// Inert in local mode (the session brackets branch on runmode).
	a.curator.SetKBStore(a.kbStore)
	if a.llmResolver != nil {
		a.curator.SetLLMResolver(llmcred.SystemEnvResolver(a.llmResolver, "tf-curator"))
	}

	// One claim loop drives both surfaces: the dispatcher claims a curator
	// conversation exactly as it claims a delegated run, then hands it here.
	// Capacity comes with that — the loop holds its concurrency slot for the
	// whole turn, so a curator turn passes the same memory guardrail and
	// occupies the same semaphore (and the same heartbeat occupancy
	// snapshot) a delegated run does, with no separate admission gate.
	a.spawner.SetCuratorTurnDriver(a.curator.DriveClaimedTurn)
	// And the local doorbell: an enqueued turn nudges this pod's claim loop
	// instead of waiting out its backstop tick.
	a.curator.SetWake(a.spawner.WakeDispatcher)

	if executorRuntime {
		// Curator turns on an executor participate in the sealed-bundle
		// credential path: the spawner stands each turn's network +
		// credential sidecar up so the turn resolves LLM/GitHub/Jira through the
		// sidecar's proxies over the sealed bundle, never the disabled secret
		// store. The adapter converts the spawner's *runSidecar to the
		// curator.TurnSidecar interface, mapping a nil return to a nil interface
		// (avoiding a non-nil interface wrapping a nil pointer). Wired only here,
		// so control/all/local keep the in-process path.
		a.curator.SetTurnSidecar(func(ctx context.Context, orgID, conversationID, userID, teamID string, pinnedRepos []string) (curator.TurnSidecar, error) {
			sb, err := a.spawner.BringUpCuratorSidecar(ctx, orgID, conversationID, userID, teamID, pinnedRepos)
			if err != nil {
				return nil, err
			}
			if sb == nil {
				return nil, nil
			}
			return sb, nil
		})
	}
	return nil
}

// sweepStrandedCuratorTurns runs the boot recovery sweep over active curator
// CLAIMS: ownership-scoped in multi mode (always a split role —
// control/executor — so this pod releases only its own prior boots'
// engagements), global in local, where the single process owned every
// engagement. Queued turns are untouched either way: they are unowned
// undelivered messages now and survive to be re-claimed. See
// buildCuratorRuntime for the rationale.
func (a *App) sweepStrandedCuratorTurns(multi bool) {
	if multi {
		if n, err := a.stores.Curator.CancelStrandedTurnsForHomeSystem(context.Background(), a.identity.ID, a.bootEpoch, "process restarted"); err != nil {
			curatorLog.Error("sweep stranded homed turns failed", "error", err)
		} else if n > 0 {
			curatorLog.Info("cancelled stranded turns homed to this pod's prior boot", "count", n)
		}
		return
	}
	if n, err := a.stores.Curator.CancelOrphanedTurnsSystem(context.Background()); err != nil {
		curatorLog.Error("sweep stranded turns failed", "error", err)
	} else if n > 0 {
		curatorLog.Info("cancelled stranded turns from prior process", "count", n)
	}
}

// publishCuratorDoorbell publishes a curator homing tf_ctl notification
// (spec §6.3): "curator_new" nudges the home executor's claim loop, and
// "curator_cancel" routes a cross-pod cancel to whichever executor holds the
// project's live session. Best-effort — a publish error only costs the home
// executor's backstop-poll latency (curator_new) or the DB-level cancel flip's
// eventual convergence (curator_cancel). Wired onto the control-pod curator's
// doorbell seam in buildPlacement.
func (a *App) publishCuratorDoorbell(kind, orgID, projectID string) {
	if a.database == nil {
		return
	}
	msg := ctlbus.Message{Kind: kind, OrgID: orgID, ProjectID: projectID}
	if err := ctlbus.Publish(context.Background(), a.database, msg); err != nil {
		appLog.Warn("curator doorbell publish failed", "kind", kind, "project", projectID, "error", err)
	}
}

// publishKBDoorbell publishes a "kb_changed" tf_ctl notification:
// the KB upload/delete handlers ring it so the home executor materializes the
// panel write into a live session's dir, and op="project_deleted" tells the
// home executor to drop its materialized project dir. Best-effort — a publish
// error only costs the executor its turn-start materialize latency. Wired onto
// the server's kbChangedDoorbell seam in buildExecution (multi mode only).
func (a *App) publishKBDoorbell(op, orgID, projectID string) {
	if a.database == nil {
		return
	}
	msg := ctlbus.Message{Kind: "kb_changed", OrgID: orgID, ProjectID: projectID, Op: op}
	if err := ctlbus.Publish(context.Background(), a.database, msg); err != nil {
		appLog.Warn("kb doorbell publish failed", "op", op, "project", projectID, "error", err)
	}
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
	// spawner's run-queue self-sweep already uses (registerInstance minted it
	// at boot, above).
	a.router.SetExecutorID(a.identity.ID, a.bootEpoch)
}
