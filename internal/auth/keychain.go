package auth

import (
	"fmt"

	"github.com/zalando/go-keyring"
)

const service = "todotriage"

// Keychain keys
const (
	keyGitHubURL      = "github_url"
	keyGitHubPAT      = "github_pat"
	keyGitHubUsername  = "github_username"
	keyJiraURL        = "jira_url"
	keyJiraPAT        = "jira_pat"
)

// Credentials holds the stored auth configuration.
type Credentials struct {
	GitHubURL      string
	GitHubPAT      string
	GitHubUsername  string
	JiraURL        string
	JiraPAT        string
}

// Store saves all credentials to the OS keychain.
func Store(creds Credentials) error {
	pairs := []struct{ key, val string }{
		{keyGitHubURL, creds.GitHubURL},
		{keyGitHubPAT, creds.GitHubPAT},
		{keyGitHubUsername, creds.GitHubUsername},
		{keyJiraURL, creds.JiraURL},
		{keyJiraPAT, creds.JiraPAT},
	}
	for _, p := range pairs {
		if p.val == "" {
			continue
		}
		if err := keyring.Set(service, p.key, p.val); err != nil {
			return fmt.Errorf("keychain store %s: %w", p.key, err)
		}
	}
	return nil
}

// Load retrieves all credentials from the OS keychain.
// Returns empty strings for missing keys (not an error).
func Load() (Credentials, error) {
	var creds Credentials
	var err error

	creds.GitHubURL, err = get(keyGitHubURL)
	if err != nil {
		return creds, err
	}
	creds.GitHubPAT, err = get(keyGitHubPAT)
	if err != nil {
		return creds, err
	}
	creds.GitHubUsername, err = get(keyGitHubUsername)
	if err != nil {
		return creds, err
	}
	creds.JiraURL, err = get(keyJiraURL)
	if err != nil {
		return creds, err
	}
	creds.JiraPAT, err = get(keyJiraPAT)
	if err != nil {
		return creds, err
	}

	return creds, nil
}

func deleteKeys(keys ...string) error {
	for _, key := range keys {
		if err := keyring.Delete(service, key); err != nil && err != keyring.ErrNotFound {
			return fmt.Errorf("keychain delete %s: %w", key, err)
		}
	}
	return nil
}

// Clear removes all credentials from the OS keychain.
func Clear() error {
	return deleteKeys(keyGitHubURL, keyGitHubPAT, keyGitHubUsername, keyJiraURL, keyJiraPAT)
}

// ClearGitHub removes GitHub credentials from the OS keychain.
func ClearGitHub() error {
	return deleteKeys(keyGitHubURL, keyGitHubPAT, keyGitHubUsername)
}

// ClearJira removes Jira credentials from the OS keychain.
func ClearJira() error {
	return deleteKeys(keyJiraURL, keyJiraPAT)
}

// IsConfigured returns true if at least one PAT is stored.
func IsConfigured() bool {
	creds, err := Load()
	if err != nil {
		return false
	}
	return creds.GitHubPAT != "" || creds.JiraPAT != ""
}

// get retrieves a value from the keychain, returning empty string if not found.
func get(key string) (string, error) {
	val, err := keyring.Get(service, key)
	if err == keyring.ErrNotFound {
		return "", nil
	}
	return val, err
}
