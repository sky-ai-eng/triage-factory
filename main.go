package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"

	"github.com/sky-ai-eng/triage-factory/cmd/exec"
)

const defaultPort = 3000

// pluralize picks the singular or plural form of a noun based on count.
// Used for toast copy where "1 entity tracked" vs "5 entities tracked"
// reads nicer than a naive "(s)" suffix.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func main() {
	// Dual-mode dispatch: exec/status commands are CLI-only (used by Claude Code agent)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "exec":
			exec.Handle(os.Args[2:])
			return
		case "status":
			exec.HandleStatus(os.Args[2:])
			return
		}
	}

	// Server mode: start HTTP server + pollers
	port := defaultPort
	noBrowser := false

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--port":
			if i+1 < len(os.Args) {
				p, err := strconv.Atoi(os.Args[i+1])
				if err != nil {
					log.Fatalf("invalid port: %s", os.Args[i+1])
				}
				port = p
				i++
			}
		case "--no-browser":
			noBrowser = true
		}
	}

	database, err := db.Open()
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Triage Factory running at http://localhost%s\n", addr)

	if !noBrowser {
		openBrowser(fmt.Sprintf("http://localhost%s", addr))
	}

	srv := server.New(database)

	distFS, err := frontendDist()
	if err != nil {
		log.Fatalf("failed to load embedded frontend: %v", err)
	}
	srv.SetStatic(distFS)

	// Clean up any orphaned worktrees from crashed runs
	worktree.Cleanup()

	// Seed event type catalog, task_rules defaults, and default prompts.
	// Order matters: task_rules FK to events_catalog(id), so catalog must be
	// seeded first.
	if err := db.SeedEventTypes(database); err != nil {
		log.Fatalf("failed to seed event types: %v", err)
	}
	if err := db.SeedTaskRules(database); err != nil {
		log.Fatalf("failed to seed task rules: %v", err)
	}
	seedDefaultPrompts(database)

	// Auto-import Claude Code skill files as prompts
	skills.ImportAll(database)

	// Event bus — central pub/sub replacing direct callbacks
	bus := eventbus.New()

	wsHub := srv.WSHub()

	// Subscriber: WS broadcaster — forwards ALL events to the frontend
	bus.Subscribe(eventbus.Subscriber{
		Name: "ws-broadcast",
		Handle: func(evt domain.Event) {
			wsHub.Broadcast(websocket.Event{
				Type: "event",
				Data: evt,
			})
			// Also send the legacy "tasks_updated" for backward compat
			if evt.EventType == domain.EventSystemPollCompleted {
				wsHub.Broadcast(websocket.Event{
					Type: "tasks_updated",
					Data: map[string]any{},
				})
			}
		},
	})

	// Start AI scoring runner
	// Profile gate — scorer waits for this before running
	profileGate := repoprofile.NewProfileGate(database)

	// Declare eventRouter early so the scorer callback can reference it.
	// Actual initialization happens below after the spawner is created.
	var eventRouter *routing.Router

	scorer := ai.NewRunner(database, ai.RunnerCallbacks{
		OnScoringStarted: func(taskIDs []string) {
			wsHub.Broadcast(websocket.Event{
				Type: "scoring_started",
				Data: map[string]any{"task_ids": taskIDs},
			})
		},
		OnScoringCompleted: func(taskIDs []string) {
			wsHub.Broadcast(websocket.Event{
				Type: "scoring_completed",
				Data: map[string]any{"task_ids": taskIDs},
			})
			// Post-scoring re-derive: check deferred triggers whose
			// min_autonomy_suitability threshold the scored tasks now meet.
			// Runs async so it doesn't block the scorer from clearing its
			// running flag and handling subsequent Trigger() calls.
			if eventRouter != nil {
				go eventRouter.ReDeriveAfterScoring(taskIDs)
			}
		},
		OnTasksSkipped: func(skipped, total int) {
			toast.Warning(wsHub, fmt.Sprintf("AI scoring: %d of %d tasks skipped this cycle", skipped, total))
		},
		OnError: func(err error) {
			toast.Error(wsHub, fmt.Sprintf("AI scoring cycle aborted: %v", err))
		},
	})
	scorer.SetProfileGate(profileGate.Ready)
	scorer.Start()
	srv.SetScorerTrigger(scorer.Trigger)
	log.Println("[ai] scorer started (model: haiku)")

	// Subscriber: scorer trigger — only reacts to poll-complete sentinels
	bus.Subscribe(eventbus.Subscriber{
		Name:   "scorer",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			scorer.Trigger()
		},
	})

	// Poller manager — uses event bus instead of direct callbacks.
	// Poll errors are toasted with per-source time-based throttling: the
	// poller fires OnError on every failure (raw signal), but we only
	// refresh the user-facing toast every errorToastMinInterval. Without
	// throttling, a persistent failure (expired PAT, network outage) would
	// generate a sticky error toast every poll cycle (default 60s) until
	// the user manually dismissed each one — badly spammy on the UI.
	const errorToastMinInterval = 5 * time.Minute
	var (
		errorThrottleMu sync.Mutex
		lastErrorToast  = map[string]time.Time{}
	)
	pollerMgr := poller.NewManager(database, bus)
	pollerMgr.OnError = func(source string, err error) {
		errorThrottleMu.Lock()
		if last, ok := lastErrorToast[source]; ok && time.Since(last) < errorToastMinInterval {
			errorThrottleMu.Unlock()
			return
		}
		lastErrorToast[source] = time.Now()
		errorThrottleMu.Unlock()

		label := "Jira"
		if source == "github" {
			label = "GitHub"
		}
		toast.ErrorTitled(wsHub, label, fmt.Sprintf("Poll failed: %v", err))
	}

	// Create spawner once — credentials are hot-swapped in place
	spawner := delegate.NewSpawner(database, nil, wsHub, "")
	srv.SetSpawner(spawner)

	// Event router — records events, creates/bumps tasks, auto-delegates on
	// matching triggers, runs inline close checks. Also handles post-scoring
	// re-derive via the scorer callback wired above.
	eventRouter = routing.NewRouter(database, spawner, scorer, wsHub)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "router",
		Filter: []string{"github:", "jira:"},
		Handle: eventRouter.HandleEvent,
	})

	// Tracks per-source "announce next poll completion as a toast". Set when
	// a config change triggers a poller restart; cleared after the first
	// post-restart completion fires the toast. Prevents every-minute spam
	// while still giving users explicit feedback that their config took
	// effect.
	var (
		announceMu      sync.Mutex
		announcePending = map[string]bool{}
	)
	setAnnouncePending := func(source string) {
		announceMu.Lock()
		announcePending[source] = true
		announceMu.Unlock()
	}
	shouldAnnounce := func(source string) bool {
		announceMu.Lock()
		defer announceMu.Unlock()
		if announcePending[source] {
			announcePending[source] = false
			return true
		}
		return false
	}

	// GitHub changed: invalidate profiles → stop all → re-profile → restart all
	srv.SetOnGitHubChanged(func() {
		log.Println("[server] GitHub config changed, full restart...")
		setAnnouncePending("github")
		setAnnouncePending("jira")

		profileGate.Invalidate()
		pollerMgr.StopAll()

		cfg, _ := config.Load()
		creds, _ := auth.Load()

		if cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) {
			ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
			spawner.UpdateCredentials(ghClient, cfg.AI.Model)
			srv.SetGitHubClient(ghClient)

			// Re-profile, then signal ready and restart all pollers
			go func() {
				profiler := repoprofile.NewProfiler(ghClient, database, wsHub)
				repos, _ := db.GetConfiguredRepoNames(database)
				if err := profiler.Run(context.Background(), repos, true); err != nil {
					log.Printf("[repoprofile] profiling failed: %v", err)
				}
				profileGate.Signal()
				pollerMgr.RestartAll()
				scorer.Trigger()
			}()
		} else {
			spawner.UpdateCredentials(nil, "")
			srv.SetGitHubClient(nil)
			pollerMgr.RestartAll()
		}

		// Also refresh Jira client in case it's configured
		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT), cfg.Jira.InProgress)
		} else {
			srv.SetJiraClient(nil, config.JiraStatusRule{})
		}
	})

	// Jira changed: restart only the Jira poller
	srv.SetOnJiraChanged(func() {
		log.Println("[server] Jira config changed, restarting Jira poller...")
		setAnnouncePending("jira")

		cfg, _ := config.Load()
		creds, _ := auth.Load()

		pollerMgr.RestartJira()

		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT), cfg.Jira.InProgress)
		} else {
			srv.SetJiraClient(nil, config.JiraStatusRule{})
		}
	})

	// Subscriber: track Jira/GitHub poll completions.
	// Jira: gates /api/jira/stock so it knows when snapshots are ready.
	// Both: surface a one-shot "first poll complete after config change"
	// toast so users can see their settings change actually took effect.
	bus.Subscribe(eventbus.Subscriber{
		Name:   "poll-tracker",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			if evt.EventType != domain.EventSystemPollCompleted {
				return
			}
			var meta struct {
				Source    string `json:"source"`
				StartedAt int64  `json:"started_at"`
				Entities  int    `json:"entities"`
			}
			if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
				log.Printf("[poll-tracker] warning: failed to parse poll completion metadata: %v; raw metadata=%q", err, evt.MetadataJSON)
				return
			}
			if meta.Source == "jira" {
				// Pass the poll's started_at so MarkJiraPollComplete can ignore
				// stale sentinels from pre-restart poll goroutines that finish
				// late — RestartJira doesn't cancel in-flight RefreshJira calls.
				// A missing field yields StartedAt=0; pass a zero time.Time so
				// MarkJiraPollComplete treats it as "unknown generation" and
				// accepts it rather than getting stuck on {status:"polling"}.
				var startedAt time.Time
				if meta.StartedAt != 0 {
					startedAt = time.Unix(0, meta.StartedAt)
				}
				srv.MarkJiraPollComplete(startedAt)
			}
			if shouldAnnounce(meta.Source) {
				label := "GitHub"
				if meta.Source == "jira" {
					label = "Jira"
				}
				toast.Info(wsHub, fmt.Sprintf(
					"First %s poll complete — %d %s tracked",
					label, meta.Entities, pluralize(meta.Entities, "entity", "entities"),
				))
			}
		},
	})

	// Initial start with current credentials
	cfg, _ := config.Load()
	creds, _ := auth.Load()
	repoCount, _ := db.CountConfiguredRepos(database)

	if cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) && repoCount > 0 {
		ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
		spawner.UpdateCredentials(ghClient, cfg.AI.Model)
		srv.SetGitHubClient(ghClient)
		log.Printf("[delegate] spawner ready (%d repos configured)", repoCount)

		// Profile repos, then signal ready, start pollers, and trigger scoring
		go func() {
			profiler := repoprofile.NewProfiler(ghClient, database, wsHub)
			repos, _ := db.GetConfiguredRepoNames(database)
			if err := profiler.Run(context.Background(), repos, false); err != nil {
				log.Printf("[repoprofile] initial profiling failed: %v", err)
			}
			profileGate.Signal()
			pollerMgr.RestartAll()
			scorer.Trigger()
		}()
	} else {
		// Not fully configured — start pollers immediately (may be empty)
		pollerMgr.RestartAll()
	}

	if creds.JiraPAT != "" && creds.JiraURL != "" {
		srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT), cfg.Jira.InProgress)
	}

	if err := srv.ListenAndServe(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
