package auth

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

const service = "triagefactory"

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

	// Linear service credential. LinearAuthMethod is the linear.AuthMethod
	// marker ("api_key" | "app_install"); an empty marker with a key reads as
	// the api_key shape. LinearAPIKey is the personal key that shape stores. An app install's
	// credential is an envelope this bundle does not carry: the resolver reads
	// it.
	LinearAPIKey     string
	LinearAuthMethod string
}

// GetSecret reads a single secret by key, returning "" (not an error) when no
// entry exists.
//
// This is the read-path entry point for the local-mode SecretStore
// (internal/db/sqlite). The keyed shape lets multi-mode consumers
// (Postgres-backed SecretStore) and local-mode consumers share one interface in
// package db so callers don't have to branch on runmode. The active backend is
// the OS keychain when available, else an encrypted file (see resolveBackend).
func GetSecret(key string) (string, error) {
	b, err := resolveBackend()
	if err != nil {
		return "", err
	}
	return b.get(key)
}

// PutSecret writes value under key in the active backend.
func PutSecret(key, value string) error {
	b, err := resolveBackend()
	if err != nil {
		return err
	}
	return b.put(key, value)
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
// (an encrypted file, for hosts with no keychain). Backends are dumb key/value
// doors.
type secretBackend interface {
	get(key string) (string, error) // ("", nil) when absent — NOT an error
	put(key, value string) error
	delete(key string) error // no error when absent
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
// keychain, bypassing the active-backend selection. cmd/uninstall uses it to
// remove keychain entries regardless of which backend the running config
// reads/writes: a box may still hold keychain rows from an earlier
// keychain-backed run even while TF_SECRETS_BACKEND=file is set now. A no-op
// when the keychain is unreachable — there's then nothing in it to remove, and
// the file backend's bag lives under the state root, swept by uninstall's
// data-dir wipe instead (so uninstall never needs TF_SECRET_ENCRYPTION_KEY).
// Missing entries are not an error.
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
