package app

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
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
// receiver publish verified deliveries onto it.
func (a *App) buildInfra() {
	a.bus = eventbus.New()
	a.srv.SetEventBus(a.bus)
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

	a.scorer = ai.NewManager(a.stores.Scores, a.stores.Entities, a.runSecrets, llmRecorder, sysLimiter, ai.RunnerCallbacks{
		OnScoringStarted: func(orgID string, taskIDs []string) {
			a.wsHub.Broadcast(websocket.Event{
				Type:  "scoring_started",
				OrgID: orgID,
				Data:  map[string]any{"task_ids": taskIDs},
			})
		},
		OnScoringCompleted: func(orgID string, taskIDs []string) {
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
			if a.router != nil {
				go a.router.ReDeriveAfterScoring(orgID, taskIDs)
			}
		},
		OnTasksSkipped: func(orgID string, skipped, total int) {
			toast.Warning(a.wsHub, orgID, fmt.Sprintf("AI scoring: %d of %d tasks skipped this cycle", skipped, total))
		},
		OnError: func(orgID string, err error) {
			toast.Error(a.wsHub, orgID, fmt.Sprintf("AI scoring cycle aborted: %v", err))
		},
	})
	a.srv.SetScorerTrigger(a.scorer.Trigger)
	aiLog.Info("scorer manager ready (per-org runners)", "model", "haiku")

	// Repo-profiling manager: per-org Runners profiling configured repos off
	// the system:poll: "profiler" subscriber (TTL-gated per cycle) and the
	// explicit re-profile button (force). Sibling to the scorer — both react
	// to poll sentinels independently; scoring does NOT gate on profiling.
	a.profiler = repoprofile.NewManager(a.ghResolver, a.runSecrets, a.stores.Repos, a.stores.Orgs, llmRecorder, sysLimiter, a.wsHub)
	// Chain bare-clone warming off profile-cycle completion: profiling
	// populates repo_profiles.clone_url, which bootstrapBareClones reads.
	// Local-only — the warm on-disk bare cache is an N=1 affordance; multi
	// clones per-run inside the sandbox, so there's nothing to warm here.
	if a.local() {
		a.profiler.SetOnCycleComplete(func(orgID string) {
			bootstrapBareClones(a.stores.Repos, a.stores.Secrets)
		})
	}
	a.srv.SetProfilerTrigger(a.profiler.Trigger)
	repoprofileLog.Info("repo-profiling manager ready (per-org runners)", "model", "haiku")

	// Project classifier: per-org Runners, classifying newly-
	// discovered entities against existing projects via per-project Haiku
	// quorum vote off the system:poll: subscriber. Sticky — only fires on
	// entities with classified_at IS NULL. Sibling to the scorer/profiler:
	// per-org isolation so a large org's backlog can't head-of-line-block
	// another tenant's classification.
	a.classifier = projectclassify.NewManager(a.stores.Entities, a.stores.Projects, a.runSecrets, llmRecorder, sysLimiter)
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
	// Dispatcher concurrency is a deployment decision: the default of 4 is
	// conservative for a laptop, while a provisioned multi-mode host handles
	// far more (memory-bound; see the TF_MAX_CONCURRENT_RUNS guidance in
	// .env.example for the sizing numbers). Resolved before RunDispatcher
	// starts — resizing later would strand semaphore tokens.
	rawMaxConcurrentRuns := os.Getenv("TF_MAX_CONCURRENT_RUNS")
	capRuns := delegate.DefaultMaxConcurrentRuns
	if n, clamped, err := delegate.ParseMaxConcurrentRuns(rawMaxConcurrentRuns); err != nil {
		appLog.Warn("max concurrent runs", "error", err)
	} else if clamped {
		// Distinct from the "configured" log below: an operator asked for more
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
		appLog.Info("max concurrent runs configured", "cap", n)
	}
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
			derived := delegate.DerivedRunCapacity(total)
			appLog.Info("host run capacity",
				"mem_total_mb", total,
				"budget_per_run_mb", delegate.DefaultRunMemoryBudgetMB,
				"platform_reserve_mb", delegate.DefaultPlatformReserveMB,
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
	a.spawner.SetStorage(blobStore)
	a.srv.SetSpawner(a.spawner)

	// Before reading entity.project_id for KB injection, the spawner blocks
	// until classified_at is set (or the timeout elapses).
	a.spawner.SetWaitForClassification(func(ctx context.Context, orgID, entityID string) {
		projectclassify.WaitFor(ctx, a.classifier, orgID, entityID, projectclassify.DefaultWaitTimeout)
	})

	// Curator runtime — per-project chat sessions. Sweep stranded
	// turns from a previous process first: a binary restart killed every
	// per-project goroutine + subprocess, so any queued/running row is by
	// definition stranded — cancelling it makes the user re-send rather than
	// wait for a delayed mystery reply.
	if n, err := a.stores.Curator.CancelOrphanedNonTerminalRequests(context.Background()); err != nil {
		curatorLog.Error("sweep stranded turns failed", "error", err)
	} else if n > 0 {
		curatorLog.Info("cancelled stranded turns from prior process", "count", n)
	}
	a.curator = curator.New(a.stores, a.wsHub, "")
	a.curator.SetRunCredentialResolvers(a.ghResolver, a.runSecrets, a.modelFor)
	a.srv.SetCurator(a.curator)
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

	a.pollerMgr = poller.NewManager(a.database, a.ingestor, a.stores.Users, a.stores.Tasks, a.stores.Entities, a.stores.Repos, a.stores.Orgs, a.stores.JiraStatusRules, a.stores.TeamGitHubGroups, a.stores.Secrets, a.stores.GitHubApps, a.ghResolver)
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
	a.router = routing.NewRouter(a.stores.Prompts, a.stores.Blueprints, a.stores.EventHandlers, a.stores.Agents, a.stores.TeamAgents, a.stores.Users, a.stores.Tasks, a.stores.AgentRuns, a.stores.Entities, a.stores.PendingFirings, a.stores.Events, a.stores.Orgs, a.stores.Teams, a.stores.TeamGitHubRepos, a.stores.JiraStatusRules, a.stores.TeamGitHubGroups, a.spawner, a.scorer, a.wsHub)
	a.router.SetEventQueue(a.stores.EventQueue)
}
