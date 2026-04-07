package poller

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
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

// Restart stops any running pollers and starts new ones based on
// the current credentials and config. Safe to call multiple times.
func (m *Manager) Restart() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop existing pollers
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

	// Reload credentials and config
	cfg, _ := config.Load()
	creds, _ := auth.Load()

	// Start GitHub poller if configured
	if creds.GitHubPAT != "" && creds.GitHubURL != "" {
		ghUser, err := auth.ValidateGitHub(creds.GitHubURL, creds.GitHubPAT)
		if err != nil {
			log.Printf("[github] token validation failed, skipping poller: %v", err)
		} else {
			interval := cfg.GitHub.PollInterval
			if interval < 10*time.Second {
				interval = time.Minute
			}
			m.github = NewGitHubPoller(creds.GitHubURL, creds.GitHubPAT, ghUser.Login, m.database, interval, m.bus)
			m.github.Start()
			log.Printf("[github] poller started (interval: %s, user: %s)", interval, ghUser.Login)
		}
	}

	// Start Jira poller if configured
	if creds.JiraPAT != "" && creds.JiraURL != "" {
		interval := cfg.Jira.PollInterval
		if interval < 10*time.Second {
			interval = time.Minute
		}
		m.jira = NewJiraPoller(creds.JiraURL, creds.JiraPAT, cfg.Jira.Projects, cfg.Jira.PickupStatuses, m.database, interval, m.bus)
		m.jira.Start()
		log.Printf("[jira] poller started (interval: %s, projects: %v, statuses: %v)", interval, cfg.Jira.Projects, cfg.Jira.PickupStatuses)
	}
}
