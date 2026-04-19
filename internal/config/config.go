package config

import (
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents ~/.triagefactory/config.yaml.
type Config struct {
	GitHub GitHubConfig `yaml:"github"`
	Jira   JiraConfig   `yaml:"jira"`
	Server ServerConfig `yaml:"server"`
	AI     AIConfig     `yaml:"ai"`
}

type GitHubConfig struct {
	BaseURL      string        `yaml:"base_url"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

type JiraConfig struct {
	BaseURL          string        `yaml:"base_url"`
	PollInterval     time.Duration `yaml:"poll_interval"`
	Projects         []string      `yaml:"projects"`
	PickupStatuses   []string      `yaml:"pickup_statuses"`
	InProgressStatus string        `yaml:"in_progress_status"`
	DoneStatus       string        `yaml:"done_status"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type AIConfig struct {
	Model                    string `yaml:"model"`
	ReprioritizeThreshold    int    `yaml:"reprioritize_threshold"`
	PreferenceUpdateInterval int    `yaml:"preference_update_interval"`
	AutoDelegateEnabled      bool   `yaml:"auto_delegate_enabled"`
}

// Ready returns true if GitHub credentials are configured.
// Repo count must be checked separately via the DB.
func (c GitHubConfig) Ready(pat, url string) bool {
	return pat != "" && url != ""
}

// Ready returns true if Jira is fully configured: credentials + at least one project.
func (c JiraConfig) Ready(pat, url string) bool {
	return pat != "" && url != "" && len(c.Projects) > 0
}

// Default returns a Config with sensible defaults matching the spec.
func Default() Config {
	return Config{
		GitHub: GitHubConfig{
			PollInterval: 60 * time.Second,
		},
		Jira: JiraConfig{
			PollInterval: 60 * time.Second,
		},
		Server: ServerConfig{
			Port: 3000,
		},
		AI: AIConfig{
			Model:                    "sonnet",
			ReprioritizeThreshold:    5,
			PreferenceUpdateInterval: 20,
			AutoDelegateEnabled:      true,
		},
	}
}

// configPath returns the path to ~/.triagefactory/config.yaml.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".triagefactory", "config.yaml"), nil
}

// Load reads the config from disk, falling back to defaults for missing fields.
func Load() (Config, error) {
	cfg := Default()

	path, err := configPath()
	if err != nil {
		return cfg, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // no config file yet, use defaults
		}
		return cfg, err
	}

	// Unmarshal on top of defaults — only overrides fields present in the file
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// Save writes the config to disk, creating the directory if needed.
func Save(cfg Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}
