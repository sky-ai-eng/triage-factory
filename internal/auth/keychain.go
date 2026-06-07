package auth

import (
	"fmt"
	"log"
	"os"
	"sort"
	"sync"

	"github.com/zalando/go-keyring"
)

const service = "triagefactory"

// Keychain keys
const (
	keyGitHubURL = "github_url"
	keyGitHubPAT = "github_pat"
	keyJiraURL   = "jira_url"
	keyJiraPAT   = "jira_pat"
)

// Environment variable names (TRIAGE_FACTORY_ prefix matches existing convention).
var envKeys = map[string]string{
	keyGitHubURL: "TRIAGE_FACTORY_GITHUB_URL",
	keyGitHubPAT: "TRIAGE_FACTORY_GITHUB_PAT",
	keyJiraURL:   "TRIAGE_FACTORY_JIRA_URL",
	keyJiraPAT:   "TRIAGE_FACTORY_JIRA_PAT",
}

// Credentials holds the stored ORG auth configuration (PAT_1, the bot
// credential). Identity facts that aren't secrets live on the users row, not
// here. Per-user identity — GitHub (this user's @login) and Jira
// (jira_account_id + jira_display_name, plus the user's own stored credential)
// alike — is captured by its own surface: the setup wizard's User step / the
// Connect gate page → POST .../identity/{github,jira}*. It is never derived
// from these org credentials.
type Credentials struct {
	GitHubURL string
	GitHubPAT string
	JiraURL   string
	JiraPAT   string
}

// EnvProvided returns which credential groups have values supplied by
// environment variables: "github" if URL+PAT are set, "jira" likewise.
func EnvProvided() []string {
	var out []string
	if os.Getenv(envKeys[keyGitHubURL]) != "" && os.Getenv(envKeys[keyGitHubPAT]) != "" {
		out = append(out, "github")
	}
	if os.Getenv(envKeys[keyJiraURL]) != "" && os.Getenv(envKeys[keyJiraPAT]) != "" {
		out = append(out, "jira")
	}
	return out
}

// get retrieves a value from the keychain, returning empty string if not found.
func get(key string) (string, error) {
	val, err := keyring.Get(service, key)
	if err == keyring.ErrNotFound {
		return "", nil
	}
	return val, err
}

// GetSecret reads a single keychain entry by key, returning "" (not an
// error) when no entry exists. For the four well-known credential keys
// (github_url, github_pat, jira_url, jira_pat) any matching
// TRIAGE_FACTORY_* env var overrides the keychain value. Unknown keys
// read straight from keychain.
//
// This is the read-path entry point for the local-mode SecretStore
// (internal/db/sqlite). The keyed shape lets multi-mode consumers
// (vault-backed SecretStore) and local-mode consumers share one
// interface in package db so callers don't have to branch on runmode.
func GetSecret(key string) (string, error) {
	if envName, ok := envKeys[key]; ok {
		if v := os.Getenv(envName); v != "" {
			logEnvOnce()
			return v, nil
		}
	}
	return get(key)
}

// PutSecret writes value under key in the keychain. If the keychain
// probe fails and the four well-known keys have env-var coverage,
// treat the write as a silent no-op (the env is the source of truth
// and the keychain write would just fail). For unknown keys there's
// no env fallback, so a keychain-unavailable error propagates.
//
// # Asymmetry with GetSecret
//
// Env vars are read-only — GetSecret returns the env value when set,
// but PutSecret always writes to the keychain. That means a rotation
// (Put new_value) is invisible to subsequent Get calls when a
// TRIAGE_FACTORY_* env var is set for the same key: Get continues to
// return the env value. The absence of a self-contained read-back is
// a real footgun for callers — surface the env-overlay state to the
// user when rotating a known key (Settings UI does this today via
// EnvProvided).
func PutSecret(key, value string) error {
	if !probeKeychain() {
		if _, known := envKeys[key]; known && len(EnvProvided()) > 0 {
			return nil
		}
		return fmt.Errorf("keychain backend unavailable")
	}
	return keyring.Set(service, key, value)
}

// HasKeychainEntry reports whether the keychain currently has a value
// stored under key, bypassing the TRIAGE_FACTORY_* env overlay
// GetSecret applies. Use this when you need to know about the
// keychain row specifically — e.g. the SecretStore.Delete contract,
// which must report ok=false when only an env-supplied value exists
// (DeleteSecret can't remove env vars, so claiming "removed" would be
// a lie).
//
// Returns false on any keychain error, including unavailability —
// callers treating absence the same as inaccessibility is the right
// posture here.
func HasKeychainEntry(key string) bool {
	if !probeKeychain() {
		return false
	}
	val, err := keyring.Get(service, key)
	if err != nil {
		return false
	}
	return val != ""
}

// DeleteSecret removes a single keychain entry. Missing entries are
// not an error. When the keychain is unavailable the call is a no-op.
func DeleteSecret(key string) error {
	if !probeKeychain() {
		return nil
	}
	if err := keyring.Delete(service, key); err != nil && err != keyring.ErrNotFound {
		return fmt.Errorf("keychain delete %s: %w", key, err)
	}
	return nil
}

// --- env var helpers ---

var envLogOnce sync.Once

func logEnvOnce() {
	envLogOnce.Do(func() {
		var names []string
		for _, envName := range envKeys {
			if os.Getenv(envName) != "" {
				names = append(names, envName)
			}
		}
		sort.Strings(names)
		log.Printf("[auth] credentials provided via environment: %v", names)
	})
}

// --- keychain availability probe ---

var (
	keychainProbeOnce sync.Once
	keychainOK        bool
)

func probeKeychain() bool {
	keychainProbeOnce.Do(func() {
		_, err := keyring.Get(service, "__probe__")
		keychainOK = err == nil || err == keyring.ErrNotFound
		if !keychainOK {
			log.Printf("[auth] keychain backend unavailable: %v", err)
		}
	})
	return keychainOK
}
