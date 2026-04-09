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
	m.stopAll()

	cfg, _ := config.Load()
	creds, _ := auth.Load()

	m.startGitHub(cfg, creds)
	m.startJira(cfg, creds)
}

// RestartGitHub stops and restarts only the GitHub poller.
func (m *Manager) RestartGitHub() {
	m.mu.Lock()
	if m.github != nil {
		m.github.Stop()
		m.github = nil
		log.Println("[github] poller stopped")
	}
	m.mu.Unlock()

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	m.startGitHub(cfg, creds)
}

// RestartJira stops and restarts only the Jira poller.
func (m *Manager) RestartJira() {
	m.mu.Lock()
	if m.jira != nil {
		m.jira.Stop()
		m.jira = nil
		log.Println("[jira] poller stopped")
	}
	m.mu.Unlock()

	cfg, _ := config.Load()
	creds, _ := auth.Load()
	m.startJira(cfg, creds)
}

// StopAll stops all running pollers without restarting.
func (m *Manager) StopAll() {
	m.stopAll()
}

// Restart is a convenience alias for RestartAll (backwards compat).
func (m *Manager) Restart() {
	m.RestartAll()
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

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

// startGitHub does all I/O (DB query, auth validation) outside the lock,
// then locks only to assign the poller pointer.
func (m *Manager) startGitHub(cfg config.Config, creds auth.Credentials) {
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

	poller := NewGitHubPoller(creds.GitHubURL, creds.GitHubPAT, ghUser.Login, repos, m.database, interval, m.bus)

	m.mu.Lock()
	m.github = poller
	m.mu.Unlock()

	poller.Start()
	log.Printf("[github] poller started (interval: %s, user: %s, repos: %d)", interval, ghUser.Login, len(repos))
}

// startJira does validation outside the lock, then locks only to assign.
func (m *Manager) startJira(cfg config.Config, creds auth.Credentials) {
	if !cfg.Jira.Ready(creds.JiraPAT, creds.JiraURL) {
		log.Println("[jira] not fully configured, skipping poller")
		return
	}

	interval := cfg.Jira.PollInterval
	if interval < 10*time.Second {
		interval = time.Minute
	}

	poller := NewJiraPoller(creds.JiraURL, creds.JiraPAT, cfg.Jira.Projects, cfg.Jira.PickupStatuses, m.database, interval, m.bus)

	m.mu.Lock()
	m.jira = poller
	m.mu.Unlock()

	poller.Start()
	log.Printf("[jira] poller started (interval: %s, projects: %v)", interval, cfg.Jira.Projects)
}
