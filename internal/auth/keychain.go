package auth

import (
	"fmt"
	"os"
	"sort"
	"strings"
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
// The PAT vars name the org/bot access credential (PAT_1) — distinct from
// the per-user identity credential (PAT_2), which the headless bootstrap
// reads from TRIAGE_FACTORY_{GITHUB,JIRA}_USER_PAT (see internal/server's
// headless bootstrap). The URL vars carry the shared host and are not
// actor-specific, so they keep their plain names.
var envKeys = map[string]string{
	keyGitHubURL: "TRIAGE_FACTORY_GITHUB_URL",
	keyGitHubPAT: "TRIAGE_FACTORY_GITHUB_BOT_PAT",
	keyJiraURL:   "TRIAGE_FACTORY_JIRA_URL",
	keyJiraPAT:   "TRIAGE_FACTORY_JIRA_BOT_PAT",
}

// Credentials holds the stored ORG auth configuration (PAT_1, the bot
// credential). Identity facts that aren't secrets live in their own host-scoped
// tables, not here. Per-user identity — GitHub (this user's @login in
// user_github_identities) and Jira (account_id + display_name in
// user_jira_identities, plus the user's own stored credential) alike — is
// captured by its own surface: the setup wizard's User step / the Connect gate
// page → POST .../identity/{github,jira}*. It is never derived from these org
// credentials.
type Credentials struct {
	GitHubURL string
	GitHubPAT string
	JiraURL   string
	JiraPAT   string

	// Jira Cloud service credential (Basic auth, REST v3). JiraEmail +
	// JiraAPIToken are the Atlassian account email and API token; JiraPAT
	// above stays the Data Center service credential (Bearer, REST v2).
	// JiraAuthMethod is the jira.AuthMethod marker ("dc_pat" |
	// "cloud_api_token", carried as a string) recording which scheme the
	// stored credential uses, so the resolver knows which fields to read. An
	// empty marker is a pre-Cloud org and resolves as Data Center.
	JiraEmail      string
	JiraAPIToken   string
	JiraAuthMethod string
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

// GetSecret reads a single secret by key, returning "" (not an error) when no
// entry exists. For the four well-known credential keys (github_url,
// github_pat, jira_url, jira_pat) any matching TRIAGE_FACTORY_* env var
// overrides the stored value. Unknown keys read straight from the active
// backend.
//
// This is the read-path entry point for the local-mode SecretStore
// (internal/db/sqlite). The keyed shape lets multi-mode consumers
// (Postgres-backed SecretStore) and local-mode consumers share one interface in
// package db so callers don't have to branch on runmode. The active backend is
// the OS keychain when available, else an encrypted file (see resolveBackend).
func GetSecret(key string) (string, error) {
	if envName, ok := envKeys[key]; ok {
		if v := os.Getenv(envName); v != "" {
			logEnvOnce()
			return v, nil
		}
	}
	b, err := resolveBackend()
	if err != nil {
		return "", err
	}
	return b.get(key)
}

// PutSecret writes value under key in the active backend.
//
// # Asymmetry with GetSecret
//
// Env vars are read-only — GetSecret returns the env value when set, but
// PutSecret always writes to the backend. That means a rotation (Put
// new_value) is invisible to subsequent Get calls when a TRIAGE_FACTORY_* env
// var is set for the same key: Get continues to return the env value. The
// absence of a self-contained read-back is a real footgun for callers —
// surface the env-overlay state to the user when rotating a known key
// (Settings UI does this today via EnvProvided).
func PutSecret(key, value string) error {
	b, err := resolveBackend()
	if err != nil {
		return err
	}
	return b.put(key, value)
}

// HasStoredSecret reports whether the active backend currently has a value
// stored under key, bypassing the TRIAGE_FACTORY_* env overlay GetSecret
// applies. Use this when you need to know about the stored entry specifically —
// e.g. the SecretStore.Delete contract, which must report ok=false when only an
// env-supplied value exists (DeleteSecret can't remove env vars, so claiming
// "removed" would be a lie).
//
// Returns false on any backend error, including unavailability — callers
// treating absence the same as inaccessibility is the right posture here.
func HasStoredSecret(key string) bool {
	b, err := resolveBackend()
	if err != nil {
		return false
	}
	return b.has(key)
}

// DeleteSecret removes a single stored entry. Missing entries are not an error.
func DeleteSecret(key string) error {
	b, err := resolveBackend()
	if err != nil {
		return err
	}
	return b.delete(key)
}

// --- secret backend abstraction ---

// secretBackend is the storage seam the public secret functions delegate to.
// Two impls: keychainBackend (OS keychain, the desktop default) and fileBackend
// (an encrypted file, for headless boxes with no keychain). The env overlay
// lives above this seam in GetSecret, so backends are dumb key/value doors.
type secretBackend interface {
	get(key string) (string, error) // ("", nil) when absent — NOT an error
	put(key, value string) error
	delete(key string) error // no error when absent
	// has reports a real stored entry, bypassing the env overlay. An
	// empty-string value counts as absent (has == false) — both impls agree,
	// since the system only ever stores empty to mean "unset".
	has(key string) bool
}

type backendKind int

const (
	backendKeychain backendKind = iota
	backendFile
)

// envSecretsBackend forces the secret backend: "auto" (default — keychain if
// reachable, else file), "keychain" (force keychain; error if unreachable), or
// "file" (force the encrypted file). The override exists for ops control and so
// the file path is testable on a machine that has a working keychain.
const envSecretsBackend = "TF_SECRETS_BACKEND"

var (
	backendMu     sync.Mutex
	activeBackend secretBackend
)

// selectBackend resolves the backend kind from the TF_SECRETS_BACKEND value and
// the keychain probe result. Pure (no construction, no I/O) so the decision is
// unit-testable in isolation.
func selectBackend(backendEnv string, keychainOK bool) (backendKind, error) {
	switch strings.ToLower(strings.TrimSpace(backendEnv)) {
	case "", "auto":
		if keychainOK {
			return backendKeychain, nil
		}
		return backendFile, nil
	case "keychain":
		if !keychainOK {
			return backendKeychain, fmt.Errorf("%s=keychain but the OS keychain backend is unavailable", envSecretsBackend)
		}
		return backendKeychain, nil
	case "file":
		return backendFile, nil
	default:
		return backendKeychain, fmt.Errorf("%s=%q is invalid (want auto|keychain|file)", envSecretsBackend, backendEnv)
	}
}

// resolveBackend returns the process-wide active backend, constructing it once
// on first use and caching it. The file backend's construction can fail (a
// missing/invalid TF_SECRET_ENCRYPTION_KEY or an undecryptable file); on failure
// nothing is cached, so the error surfaces on every call until fixed — the boot
// path (InitLocalSecretBackend) turns the first such failure into a clean
// startup error.
func resolveBackend() (secretBackend, error) {
	backendMu.Lock()
	defer backendMu.Unlock()
	if activeBackend != nil {
		return activeBackend, nil
	}
	kind, err := selectBackend(os.Getenv(envSecretsBackend), probeKeychain())
	if err != nil {
		return nil, err
	}
	switch kind {
	case backendFile:
		fb, ferr := newFileBackend()
		if ferr != nil {
			return nil, ferr
		}
		activeBackend = fb
		logBackend(backendFile, fb.path)
	default:
		activeBackend = keychainBackend{}
		logBackend(backendKeychain, "")
	}
	return activeBackend, nil
}

// InitLocalSecretBackend eagerly resolves (and, for the file backend,
// constructs and validates) the active secret backend so a missing
// TF_SECRET_ENCRYPTION_KEY or an undecryptable file fails the server at boot
// rather than on the first credential read. Local mode only — the caller gates
// on runmode (multi mode uses the Postgres secret store and never touches this
// package).
func InitLocalSecretBackend() error {
	_, err := resolveBackend()
	return err
}

// SweepKeychain best-effort deletes the given keys directly from the OS
// keychain, bypassing the active-backend selection (and the
// TRIAGE_FACTORY_* env overlay). cmd/uninstall uses it to remove keychain
// entries regardless of which backend the running config reads/writes: a box
// may still hold keychain rows from an earlier keychain-backed run even while
// TF_SECRETS_BACKEND=file is set now. A no-op when the keychain is unreachable —
// there's then nothing in it to remove, and the file backend's bag lives under
// the state root, swept by uninstall's data-dir wipe instead (so uninstall
// never needs TF_SECRET_ENCRYPTION_KEY). Missing entries are not an error.
func SweepKeychain(keys []string) error {
	if !probeKeychain() {
		return nil
	}
	for _, k := range keys {
		if err := keyring.Delete(service, k); err != nil && err != keyring.ErrNotFound {
			return fmt.Errorf("keychain delete %s: %w", k, err)
		}
	}
	return nil
}

// keychainBackend stores secrets in the OS keychain via go-keyring. The desktop
// default; selected whenever the keychain probe succeeds.
type keychainBackend struct{}

func (keychainBackend) get(key string) (string, error) {
	val, err := keyring.Get(service, key)
	if err == keyring.ErrNotFound {
		return "", nil
	}
	return val, err
}

func (keychainBackend) put(key, value string) error {
	return keyring.Set(service, key, value)
}

func (keychainBackend) delete(key string) error {
	if err := keyring.Delete(service, key); err != nil && err != keyring.ErrNotFound {
		return fmt.Errorf("keychain delete %s: %w", key, err)
	}
	return nil
}

func (keychainBackend) has(key string) bool {
	val, err := keyring.Get(service, key)
	if err != nil {
		return false
	}
	return val != ""
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
		authLog.Info("credentials provided via environment", "names", names)
	})
}

var backendLogOnce sync.Once

func logBackend(kind backendKind, detail string) {
	backendLogOnce.Do(func() {
		if kind == backendFile {
			authLog.Info("secret backend selected", "backend", "file", "path", detail)
			return
		}
		authLog.Info("secret backend selected", "backend", "keychain")
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
			authLog.Warn("keychain backend unavailable", "error", err)
		}
	})
	return keychainOK
}
