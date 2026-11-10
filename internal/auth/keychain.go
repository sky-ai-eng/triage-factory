package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
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
	GitHubPAT string
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
	// the api_key shape. LinearAPIKey is the personal key that shape stores.
	// An app install's credential is an envelope holding a refresh token,
	// which this bundle does not carry: the resolver reads it.
	// LinearAppInstalled says only whether that envelope is stored.
	LinearAPIKey       string
	LinearAuthMethod   string
	LinearAppInstalled bool
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
// Missing entries are not an error. Each key goes through the chunk-aware
// delete, so the chunks of an oversized value (or chunks orphaned by an
// interrupted write) are removed with it.
func SweepKeychain(keys []string) error {
	if !probeKeychain() {
		return nil
	}
	for _, k := range keys {
		if err := (keychainBackend{}).delete(k); err != nil {
			return err
		}
	}
	return nil
}

// --- keychain chunking ---
//
// Some keychains cap the size of one entry (Windows Credential Manager via
// go-keyring: 2560 bytes), and a GitHub App private key or an OAuth envelope
// can exceed that. A value longer than chunkLimit is split by bytes into
// entries "<key>#1".."<key>#N", and the base entry holds the record
// "tfchunks:v1:<N>:<sha256 of the whole value>" instead of the value. A value
// at or under the limit is stored exactly as given. chunkLimit 0 disables
// chunking, which keeps platforms without a practical cap on the plain path.

const chunkBasePrefix = "tfchunks:v1:"

// chunkLimit is the largest value stored in one keychain entry; 0 disables
// chunking. A var so tests can exercise the chunk logic on every OS.
var chunkLimit = platformChunkLimit

// keyringOps is the go-keyring surface the backend uses, held in a var so a
// test can inject a write failure.
type keyringOps struct {
	get    func(service, user string) (string, error)
	set    func(service, user, password string) error
	delete func(service, user string) error
}

var kr = keyringOps{get: keyring.Get, set: keyring.Set, delete: keyring.Delete}

// keychainMu serializes get/put/delete so a multi-entry write is never
// observed half-done by another goroutine in this process.
var keychainMu sync.Mutex

func chunkKey(key string, i int) string { return fmt.Sprintf("%s#%d", key, i) }

func chunkBaseRecord(n int, value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s%d:%s", chunkBasePrefix, n, hex.EncodeToString(sum[:]))
}

// parseChunkBase decodes a base record into its chunk count and digest.
func parseChunkBase(base string) (n int, digest string, err error) {
	parts := strings.Split(strings.TrimPrefix(base, chunkBasePrefix), ":")
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("malformed chunk record")
	}
	n, err = strconv.Atoi(parts[0])
	if err != nil || n < 1 {
		return 0, "", fmt.Errorf("malformed chunk count %q", parts[0])
	}
	return n, parts[1], nil
}

// deleteChunksFrom removes key#from, key#from+1, ... until one is not found.
// It probes rather than trusting a base record, so chunks orphaned by a crash
// between the chunk writes and the base write are removed too.
func deleteChunksFrom(key string, from int) error {
	for i := from; ; i++ {
		err := kr.delete(service, chunkKey(key, i))
		if err == keyring.ErrNotFound {
			return nil
		}
		if err != nil {
			return fmt.Errorf("keychain delete %s: %w", chunkKey(key, i), err)
		}
	}
}

// keychainBackend stores secrets in the OS keychain via go-keyring. The desktop
// default; selected whenever the keychain probe succeeds.
type keychainBackend struct{}

func (keychainBackend) get(key string) (string, error) {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	base, err := kr.get(service, key)
	if err == keyring.ErrNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if chunkLimit <= 0 || !strings.HasPrefix(base, chunkBasePrefix) {
		return base, nil
	}
	n, digest, err := parseChunkBase(base)
	if err != nil {
		return "", fmt.Errorf("keychain entry %s: %w", key, err)
	}
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		part, err := kr.get(service, chunkKey(key, i))
		if err == keyring.ErrNotFound {
			return "", fmt.Errorf("keychain entry %s: chunk %d of %d is missing", key, i, n)
		}
		if err != nil {
			return "", fmt.Errorf("keychain read %s: %w", chunkKey(key, i), err)
		}
		sb.WriteString(part)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	if hex.EncodeToString(sum[:]) != digest {
		return "", fmt.Errorf("keychain entry %s: reassembled value does not match its recorded digest", key)
	}
	return sb.String(), nil
}

func (keychainBackend) put(key, value string) error {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	if chunkLimit <= 0 {
		return kr.set(service, key, value)
	}
	if len(value) <= chunkLimit {
		if err := kr.set(service, key, value); err != nil {
			return err
		}
		// A previous longer value may have left chunks behind.
		return deleteChunksFrom(key, 1)
	}

	n := (len(value) + chunkLimit - 1) / chunkLimit
	for i := 1; i <= n; i++ {
		lo := (i - 1) * chunkLimit
		hi := min(lo+chunkLimit, len(value))
		if err := kr.set(service, chunkKey(key, i), value[lo:hi]); err != nil {
			return abandonChunkedPut(key, i, fmt.Errorf("keychain write %s: %w", chunkKey(key, i), err))
		}
	}
	if err := kr.set(service, key, chunkBaseRecord(n, value)); err != nil {
		return abandonChunkedPut(key, n, fmt.Errorf("keychain write %s: %w", key, err))
	}
	// A previous, longer chunked value may have left chunks past n.
	return deleteChunksFrom(key, n+1)
}

// abandonChunkedPut cleans up after a chunked write that failed: it removes the
// chunks the attempt may have written (1..attempted) and, because those
// overwrote the chunks of any earlier chunked value, the stale base record that
// would otherwise point at them. The original error is returned; a cleanup
// failure is joined to it.
func abandonChunkedPut(key string, attempted int, cause error) error {
	errs := []error{cause}
	for i := 1; i <= attempted; i++ {
		if err := kr.delete(service, chunkKey(key, i)); err != nil && err != keyring.ErrNotFound {
			errs = append(errs, fmt.Errorf("cleanup %s: %w", chunkKey(key, i), err))
		}
	}
	if base, err := kr.get(service, key); err == nil && strings.HasPrefix(base, chunkBasePrefix) {
		if err := kr.delete(service, key); err != nil && err != keyring.ErrNotFound {
			errs = append(errs, fmt.Errorf("cleanup %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

func (keychainBackend) delete(key string) error {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	if chunkLimit > 0 {
		if err := deleteChunksFrom(key, 1); err != nil {
			return err
		}
	}
	if err := kr.delete(service, key); err != nil && err != keyring.ErrNotFound {
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
