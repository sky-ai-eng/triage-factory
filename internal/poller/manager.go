package poller

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/sky-ai-eng/todo-triage/internal/auth"
	"github.com/sky-ai-eng/todo-triage/internal/config"
	"github.com/sky-ai-eng/todo-triage/internal/db"
	"github.com/sky-ai-eng/todo-triage/internal/eventbus"
	ghclient "github.com/sky-ai-eng/todo-triage/internal/github"
	jiraclient "github.com/sky-ai-eng/todo-triage/internal/jira"
	"github.com/sky-ai-eng/todo-triage/internal/tracker"
)

// Manager manages the lifecycle of polling loops, allowing them to be
// stopped and restarted when credentials or config change.
type Manager struct {
	database *sql.DB
	bus      *eventbus.Bus
	tracker  *tracker.Tracker

	mu         sync.Mutex
	ghStop     chan struct{}
	jiraStop   chan struct{}
}

func NewManager(database *sql.DB, bus *eventbus.Bus) *Manager {
	return &Manager{
		database: database,
		bus:      bus,
		tracker:  tracker.New(database, bus),
	}
}

// RestartAll stops all polling loops and restarts any that are fully configured.
func (m *Manager) RestartAll() {
	m.stopAll()

	cfg, _ := config.Load()
	creds, _ := auth.Load()

	m.startGitHub(cfg, creds)
	m.startJira(cfg, creds)
}

// RestartGitHub stops and restarts only the GitHub polling loop.
func (m *Manager) RestartGitHub() {
	m.mu.Lock()
	if m.ghStop != nil {
		close(m.ghStop)
		m.ghStop = nil
		log.Println("[github] tracker stopped")
	}
	m.mu.Unlock()

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	m.startGitHub(cfg, creds)
}

// RestartJira stops and restarts only the Jira polling loop.
func (m *Manager) RestartJira() {
	m.mu.Lock()
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
	m.mu.Unlock()

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	m.startJira(cfg, creds)
}

// StopAll stops all running polling loops without restarting.
func (m *Manager) StopAll() {
	m.stopAll()
}

// Restart is a convenience alias for RestartAll.
func (m *Manager) Restart() {
	m.RestartAll()
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ghStop != nil {
		close(m.ghStop)
		m.ghStop = nil
		log.Println("[github] tracker stopped")
	}
	if m.jiraStop != nil {
		close(m.jiraStop)
		m.jiraStop = nil
		log.Println("[jira] tracker stopped")
	}
}

// startGitHub launches the GitHub tracking loop.
func (m *Manager) startGitHub(cfg config.Config, creds auth.Credentials) {
	if !cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) {
		log.Println("[github] credentials not configured, skipping tracker")
		return
	}

	repos, err := db.GetConfiguredRepoNames(m.database)
	if err != nil {
		log.Printf("[github] error loading configured repos: %v", err)
		return
	}
	if len(repos) == 0 {
		log.Println("[github] no repos configured, skipping tracker")
		return
	}

	if creds.GitHubUsername == "" {
		log.Println("[github] no username stored, skipping tracker")
		return
	}

	interval := cfg.GitHub.PollInterval
	if interval < 10*time.Second {
		interval = time.Minute
	}

	client := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
	stop := make(chan struct{})

	m.mu.Lock()
	m.ghStop = stop
	m.mu.Unlock()

	go func() {
		// Initial poll
		if _, err := m.tracker.RefreshGitHub(client, creds.GitHubUsername, repos); err != nil {
			log.Printf("[github] tracker error: %v", err)
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := m.tracker.RefreshGitHub(client, creds.GitHubUsername, repos); err != nil {
					log.Printf("[github] tracker error: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[github] tracker started (interval: %s, user: %s, repos: %d)", interval, creds.GitHubUsername, len(repos))
}

// startJira launches the Jira tracking loop.
func (m *Manager) startJira(cfg config.Config, creds auth.Credentials) {
	if !cfg.Jira.Ready(creds.JiraPAT, creds.JiraURL) {
		log.Println("[jira] not fully configured, skipping tracker")
		return
	}

	interval := cfg.Jira.PollInterval
	if interval < 10*time.Second {
		interval = time.Minute
	}

	client := jiraclient.NewClient(creds.JiraURL, creds.JiraPAT)
	stop := make(chan struct{})

	m.mu.Lock()
	m.jiraStop = stop
	m.mu.Unlock()

	go func() {
		// Initial poll
		if _, err := m.tracker.RefreshJira(client, creds.JiraURL, cfg.Jira.Projects, cfg.Jira.PickupStatuses); err != nil {
			log.Printf("[jira] tracker error: %v", err)
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := m.tracker.RefreshJira(client, creds.JiraURL, cfg.Jira.Projects, cfg.Jira.PickupStatuses); err != nil {
					log.Printf("[jira] tracker error: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()

	log.Printf("[jira] tracker started (interval: %s, projects: %v)", interval, cfg.Jira.Projects)
}
