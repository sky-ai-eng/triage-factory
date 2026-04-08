package poller

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/eventbus"
)

// Manager manages the lifecycle of pollers, allowing them to be
// stopped and restarted when credentials or config change.
type Manager struct {
	database *sql.DB
	bus      *eventbus.Bus

	mu     sync.Mutex
	github *GitHubPoller
	jira   *JiraPoller
}

func NewManager(database *sql.DB, bus *eventbus.Bus) *Manager {
	return &Manager{
		database: database,
		bus:      bus,
	}
}

// RestartAll stops all pollers and restarts any that are fully configured.
func (m *Manager) RestartAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stopAllLocked()

	cfg, _ := config.Load()
	creds, _ := auth.Load()

	m.startGitHubLocked(cfg, creds)
	m.startJiraLocked(cfg, creds)
}

// RestartGitHub stops and restarts only the GitHub poller.
func (m *Manager) RestartGitHub() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.github != nil {
		m.github.Stop()
		m.github = nil
		log.Println("[github] poller stopped")
	}

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	m.startGitHubLocked(cfg, creds)
}

// RestartJira stops and restarts only the Jira poller.
func (m *Manager) RestartJira() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.jira != nil {
		m.jira.Stop()
		m.jira = nil
		log.Println("[jira] poller stopped")
	}

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	m.startJiraLocked(cfg, creds)
}

// StopAll stops all running pollers without restarting.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopAllLocked()
}

// Restart is a convenience alias for RestartAll (backwards compat).
func (m *Manager) Restart() {
	m.RestartAll()
}

func (m *Manager) stopAllLocked() {
	if m.github != nil {
		m.github.Stop()
		m.github = nil
		log.Println("[github] poller stopped")
	}
	if m.jira != nil {
		m.jira.Stop()
		m.jira = nil
		log.Println("[jira] poller stopped")
	}
}

func (m *Manager) startGitHubLocked(cfg config.Config, creds auth.Credentials) {
	if !cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) {
		log.Println("[github] credentials not configured, skipping poller")
		return
	}

	repos, err := db.GetConfiguredRepoNames(m.database)
	if err != nil {
		log.Printf("[github] error loading configured repos: %v", err)
		return
	}
	if len(repos) == 0 {
		log.Println("[github] no repos configured, skipping poller")
		return
	}

	ghUser, err := auth.ValidateGitHub(creds.GitHubURL, creds.GitHubPAT)
	if err != nil {
		log.Printf("[github] token validation failed, skipping poller: %v", err)
		return
	}

	interval := cfg.GitHub.PollInterval
	if interval < 10*time.Second {
		interval = time.Minute
	}
	m.github = NewGitHubPoller(creds.GitHubURL, creds.GitHubPAT, ghUser.Login, repos, m.database, interval, m.bus)
	m.github.Start()
	log.Printf("[github] poller started (interval: %s, user: %s, repos: %d)", interval, ghUser.Login, len(repos))
}

func (m *Manager) startJiraLocked(cfg config.Config, creds auth.Credentials) {
	if !cfg.Jira.Ready(creds.JiraPAT, creds.JiraURL) {
		log.Println("[jira] not fully configured, skipping poller")
		return
	}

	interval := cfg.Jira.PollInterval
	if interval < 10*time.Second {
		interval = time.Minute
	}
	m.jira = NewJiraPoller(creds.JiraURL, creds.JiraPAT, cfg.Jira.Projects, cfg.Jira.PickupStatuses, m.database, interval, m.bus)
	m.jira.Start()
	log.Printf("[jira] poller started (interval: %s, projects: %v)", interval, cfg.Jira.Projects)
}
