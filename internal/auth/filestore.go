package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// secretsFileName is the encrypted secret bag, written next to the SQLite
// DB under the state root (~/.triagefactory/secrets.enc in local mode).
const secretsFileName = "secrets.enc"

// secretsFileVersion is the on-disk envelope format version. load() rejects
// any other value up front with a clear error, so a future format bump (or a
// file written by a newer binary) fails actionably instead of as a confusing
// downstream decrypt/JSON error.
const secretsFileVersion = 1

// EnvSecretEncryptionKey names the env var holding the 32-byte AES-256-GCM
// key the file backend encrypts with. It intentionally matches
// postgres.EnvSecretEncryptionKey ("TF_SECRET_ENCRYPTION_KEY") so a single
// operator-generated key works in both modes — the local file backend (this
// package) and the multi-mode Postgres secret store are independent consumers
// of the same operator convention, and never run in the same process. auth
// must not import internal/db/postgres, so the constant is duplicated rather
// than shared.
const EnvSecretEncryptionKey = "TF_SECRET_ENCRYPTION_KEY"

// fileEnvelope is the on-disk JSON shape. The file is always ciphertext: the
// decrypted plaintext is a flat map[string]string of secret key → value, the
// same keys the keychain backend uses verbatim (per-user keys arrive already
// namespaced as "user/<userID>/<key>"). []byte fields marshal as base64.
type fileEnvelope struct {
	Version    int    `json:"version"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// fileBackend is the headless secret backend: an AES-256-GCM-encrypted file
// read into memory once at construction and rewritten atomically on every
// mutation. It exists because a headless box (Docker, a self-hosted Linux
// server) has no OS keychain — go-keyring's secret-service path needs a D-Bus
// session that isn't there, so the keychain backend can't be used. Selected by
// resolveBackend when the keychain probe fails (or TF_SECRETS_BACKEND=file).
type fileBackend struct {
	path string
	key  aead.Key

	mu   sync.RWMutex
	data map[string]string
}

// newFileBackend loads the encryption key from the environment and decrypts the
// existing secret bag into memory. A missing file is a fresh install (empty
// bag), not an error. A decrypt failure (wrong key or a corrupt/tampered file)
// IS an error and propagates to the boot path (InitLocalSecretBackend), which
// fails the server loudly — silently treating an undecryptable bag as empty
// would report a configured install's credentials as absent, which is worse
// than refusing to start.
func newFileBackend() (*fileBackend, error) {
	root, err := paths.StateRootErr()
	if err != nil {
		return nil, err
	}
	key, err := aead.LoadKeyFromEnv(EnvSecretEncryptionKey)
	if err != nil {
		return nil, err
	}
	fb := &fileBackend{
		path: filepath.Join(root, secretsFileName),
		key:  key,
		data: map[string]string{},
	}
	if err := fb.load(); err != nil {
		return nil, err
	}
	return fb, nil
}

func (fb *fileBackend) load() error {
	raw, err := os.ReadFile(fb.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // fresh install: empty bag
	}
	if err != nil {
		return fmt.Errorf("read secrets file %s: %w", fb.path, err)
	}
	var env fileEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse secrets file %s: %w", fb.path, err)
	}
	if env.Version != secretsFileVersion {
		return fmt.Errorf("secrets file %s has unsupported version %d (want %d) — written by a newer or incompatible build", fb.path, env.Version, secretsFileVersion)
	}
	plain, err := fb.key.Decrypt(env.Ciphertext, env.Nonce, nil)
	if err != nil {
		return fmt.Errorf("decrypt secrets file %s (wrong %s or corrupt file): %w", fb.path, EnvSecretEncryptionKey, err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		return fmt.Errorf("parse decrypted secrets from %s: %w", fb.path, err)
	}
	fb.data = m
	return nil
}

func (fb *fileBackend) get(key string) (string, error) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	return fb.data[key], nil // "" when absent — not an error, mirrors the keychain backend
}

func (fb *fileBackend) has(key string) bool {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	// Treat an empty stored value as absent, matching keychainBackend.has — so
	// the two backends agree on the secretBackend contract regardless of which
	// is active. (The system never stores empty strings; empty means absent.)
	return fb.data[key] != ""
}

func (fb *fileBackend) put(key, value string) error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.data[key] = value
	return fb.persist()
}

func (fb *fileBackend) delete(key string) error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if _, ok := fb.data[key]; !ok {
		return nil
	}
	delete(fb.data, key)
	return fb.persist()
}

// persist re-encrypts the whole bag with a fresh nonce and writes it atomically
// (temp file + fsync + rename) so a crash mid-write can't leave a truncated or
// half-encrypted file. The caller must hold fb.mu for writing.
func (fb *fileBackend) persist() error {
	plain, err := json.Marshal(fb.data)
	if err != nil {
		return fmt.Errorf("marshal secrets: %w", err)
	}
	// No AAD (nil): the file backend stores the whole secret bag as a
	// single encrypted blob with no per-row identity to bind to, so the
	// org_secrets row-identity AAD binding doesn't apply. Pass
	// nil explicitly so the shared aead-primitive change is a conscious
	// no-op here.
	ct, nonce, err := fb.key.Encrypt(plain, nil)
	if err != nil {
		return fmt.Errorf("encrypt secrets: %w", err)
	}
	blob, err := json.Marshal(fileEnvelope{Version: secretsFileVersion, Nonce: nonce, Ciphertext: ct})
	if err != nil {
		return fmt.Errorf("marshal secrets envelope: %w", err)
	}

	dir := filepath.Dir(fb.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "secrets-*.tmp") // 0600 by default
	if err != nil {
		return fmt.Errorf("create temp secrets file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp secrets file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp secrets file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp secrets file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp secrets file: %w", err)
	}
	if err := os.Rename(tmpName, fb.path); err != nil {
		return fmt.Errorf("rename secrets file into place: %w", err)
	}
	return nil
}
