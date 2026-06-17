package app

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/projectclassify"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// buildInfra wires the in-process event bus and lets the GitHub webhook
// receiver publish verified deliveries onto it.
func (a *App) buildInfra() {
	a.bus = eventbus.New()
	a.srv.SetEventBus(a.bus)
}

// buildAI constructs the AI scoring manager and the project classifier.
// Both resolve per-run LLM credentials through the run-credential seam
// wired in buildRunCredentials. Neither starts background work here — the
// classifier is started in startWorkers; the scorer reacts to its trigger
// channel.
func (a *App) buildAI() {
	a.scorer = ai.NewManager(a.database, a.stores.Scores, a.stores.Entities, a.runSecrets, ai.RunnerCallbacks{
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
	log.Println("[ai] scorer manager ready (per-org runners, model: haiku)")

	// Project classifier: per-poll, classify newly-discovered entities
	// against existing projects via per-project Haiku quorum vote. Sticky —
	// only fires on entities with classified_at IS NULL.
	a.classifier = projectclassify.NewRunner(a.stores.Entities, a.stores.Projects, a.stores.Orgs, a.runSecrets)
	log.Println("[classify] project classifier ready (model: haiku)")
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
		log.Printf("[curator] sweep stranded turns: %v", err)
	} else if n > 0 {
		log.Printf("[curator] cancelled %d stranded turn(s) from prior process", n)
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

	a.pollerMgr = poller.NewManager(a.database, a.ingestor, a.stores.Users, a.stores.Tasks, a.stores.Entities, a.stores.Repos, a.stores.Orgs, a.stores.JiraStatusRules, a.stores.TeamGitHubGroups, a.stores.Secrets, a.stores.GitHubApps, a.ghResolver)
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
