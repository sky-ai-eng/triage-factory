package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"

	"github.com/sky-ai-eng/todo-tinder/internal/ai"
	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/delegate"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
	"github.com/sky-ai-eng/todo-tinder/internal/eventbus"
	ghclient "github.com/sky-ai-eng/todo-tinder/internal/github"
	"github.com/sky-ai-eng/todo-tinder/internal/jira"
	"github.com/sky-ai-eng/todo-tinder/internal/poller"
	"github.com/sky-ai-eng/todo-tinder/internal/server"
	"github.com/sky-ai-eng/todo-tinder/internal/worktree"
	"github.com/sky-ai-eng/todo-tinder/pkg/websocket"

	"github.com/sky-ai-eng/todo-tinder/cmd/exec"
)

const defaultPort = 3000

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
	fmt.Printf("Todo Tinder running at http://localhost%s\n", addr)

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

	// Seed event type catalog
	if err := db.SeedEventTypes(database); err != nil {
		log.Fatalf("failed to seed event types: %v", err)
	}

	// Event bus — central pub/sub replacing direct callbacks
	bus := eventbus.New()

	wsHub := srv.WSHub()

	// Subscriber: WS broadcaster — forwards ALL events to the frontend
	bus.Subscribe(eventbus.Subscriber{
		Name: "ws-broadcast",
		Handle: func(evt domain.Event) {
			wsHub.Broadcast(websocket.Event{
				Type:  "event",
				Data:  evt,
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
		},
	})
	scorer.Start()
	log.Println("[ai] scorer started (model: haiku)")

	// Subscriber: scorer trigger — only reacts to poll-complete sentinels
	bus.Subscribe(eventbus.Subscriber{
		Name:   "scorer",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			scorer.Trigger()
		},
	})

	// Poller manager — uses event bus instead of direct callbacks
	pollerMgr := poller.NewManager(database, bus)

	// Spawner state — protected by mutex for hot-reload
	var spawnerMu sync.Mutex

	// Wire up credential change callback: restarts pollers + spawner
	srv.SetOnCredentialsChanged(func() {
		log.Println("[server] credentials changed, restarting pollers and spawner...")

		pollerMgr.Restart()

		// Recreate spawner with fresh credentials
		cfg, _ := config.Load()
		creds, _ := auth.Load()

		spawnerMu.Lock()
		defer spawnerMu.Unlock()

		if creds.GitHubPAT != "" && creds.GitHubURL != "" {
			ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
			spawner := delegate.NewSpawner(database, ghClient, wsHub, cfg.AI.Model)
			srv.SetSpawner(spawner)
			log.Println("[delegate] spawner restarted")
		} else {
			srv.SetSpawner(nil)
		}

		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT), cfg.Jira.InProgressStatus)
		} else {
			srv.SetJiraClient(nil, "")
		}
	})

	// Initial start with current credentials
	pollerMgr.Restart()

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	if creds.GitHubPAT != "" && creds.GitHubURL != "" {
		ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
		spawner := delegate.NewSpawner(database, ghClient, wsHub, cfg.AI.Model)
		srv.SetSpawner(spawner)
		log.Println("[delegate] spawner ready")
	}
	if creds.JiraPAT != "" && creds.JiraURL != "" {
		srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT), cfg.Jira.InProgressStatus)
	}

	// Score any tasks already in the DB without scores
	scorer.Trigger()

	if err := srv.ListenAndServe(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
