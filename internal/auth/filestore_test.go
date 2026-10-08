package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// Two valid 64-char hex (32-byte) keys for the file backend.
const (
	testKeyHex  = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	testKeyHex2 = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
)

// resetBackend clears the cached backend so the next secret call re-resolves
// from the current env — used to simulate a process restart (reload from disk).
func resetBackend() {
	backendMu.Lock()
	activeBackend = nil
	backendMu.Unlock()
}

// useFileBackend points the state root at a temp dir, forces the file backend,
// supplies an encryption key, and clears the cached backend (now and on
// cleanup). Returns the secrets.enc path.
func useFileBackend(t *testing.T, key string) string {
	t.Helper()
	keyring.MockInit() // hermetic: probeKeychain hits the mock, never the host keychain
	root := t.TempDir()
	paths.SetForTest(t, root)
	t.Setenv(envSecretsBackend, "file")
	t.Setenv(EnvSecretEncryptionKey, key)
	resetBackend()
	t.Cleanup(resetBackend)
	return filepath.Join(root, secretsFileName)
}

func TestSelectBackend(t *testing.T) {
	cases := []struct {
		env        string
		keychainOK bool
		want       backendKind
		wantErr    bool
	}{
		{"", true, backendKeychain, false},
		{"", false, backendFile, false},
		{"auto", true, backendKeychain, false},
		{"auto", false, backendFile, false},
		{"AUTO", false, backendFile, false}, // case-insensitive
		{" file ", true, backendFile, false},
		{"file", false, backendFile, false},
		{"keychain", true, backendKeychain, false},
		{"keychain", false, backendKeychain, true}, // forced but unavailable
		{"bogus", true, backendKeychain, true},
	}
	for _, c := range cases {
		got, err := selectBackend(c.env, c.keychainOK)
		if (err != nil) != c.wantErr {
			t.Errorf("selectBackend(%q, %v) err=%v, wantErr=%v", c.env, c.keychainOK, err, c.wantErr)
		}
		if err == nil && got != c.want {
			t.Errorf("selectBackend(%q, %v) = %v, want %v", c.env, c.keychainOK, got, c.want)
		}
	}
}

func TestFileBackend_RoundTripAndPersistence(t *testing.T) {
	useFileBackend(t, testKeyHex)

	// Org key + a per-user-shaped key (the SQLite store namespaces these).
	const userKey = "user/11111111/jira_token/https://jira.example.com"
	if err := PutSecret(keyGitHubPAT, "ghp_filevalue"); err != nil {
		t.Fatalf("PutSecret org: %v", err)
	}
	if err := PutSecret(userKey, "jira-user-token"); err != nil {
		t.Fatalf("PutSecret user: %v", err)
	}

	if got, _ := GetSecret(keyGitHubPAT); got != "ghp_filevalue" {
		t.Errorf("GetSecret org = %q, want ghp_filevalue", got)
	}
	if !HasStoredSecret(userKey) {
		t.Error("HasStoredSecret(userKey) = false, want true")
	}

	// Simulate a restart: drop the cached backend, re-resolve from disk.
	resetBackend()
	if got, _ := GetSecret(userKey); got != "jira-user-token" {
		t.Errorf("after reload GetSecret(userKey) = %q, want jira-user-token", got)
	}

	// Delete contract.
	if err := DeleteSecret(keyGitHubPAT); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if HasStoredSecret(keyGitHubPAT) {
		t.Error("HasStoredSecret after delete = true, want false")
	}
	if got, _ := GetSecret(keyGitHubPAT); got != "" {
		t.Errorf("GetSecret after delete = %q, want empty", got)
	}
}

func TestFileBackend_MissingKeyAndDeleteAbsent(t *testing.T) {
	useFileBackend(t, testKeyHex)
	// Absent key: empty, not error; delete + has are no-ops.
	if got, err := GetSecret("never_set"); got != "" || err != nil {
		t.Errorf("GetSecret(absent) = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := DeleteSecret("never_set"); err != nil {
		t.Errorf("DeleteSecret(absent) = %v, want nil", err)
	}
	if HasStoredSecret("never_set") {
		t.Error("HasStoredSecret(absent) = true, want false")
	}
}

func TestFileBackend_CiphertextAtRest(t *testing.T) {
	path := useFileBackend(t, testKeyHex)
	const secret = "super-secret-plaintext-value"
	if err := PutSecret(keyGitHubPAT, secret); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read secrets file: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Error("plaintext secret found in secrets.enc — value is not encrypted at rest")
	}
}

func TestFileBackend_Perms(t *testing.T) {
	path := useFileBackend(t, testKeyHex)
	if err := PutSecret(keyGitHubPAT, "v"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("secrets.enc perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestInitLocalSecretBackend_MissingKeyFailsLoud(t *testing.T) {
	keyring.MockInit()
	paths.SetForTest(t, t.TempDir())
	t.Setenv(envSecretsBackend, "file")
	t.Setenv(EnvSecretEncryptionKey, "") // unset
	resetBackend()
	t.Cleanup(resetBackend)
	if err := InitLocalSecretBackend(); err == nil {
		t.Fatal("InitLocalSecretBackend with no key = nil, want error (fail loud at boot)")
	}
}

func TestFileBackend_WrongKeyFailsDecrypt(t *testing.T) {
	useFileBackend(t, testKeyHex)
	if err := PutSecret(keyGitHubPAT, "v"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	// Restart with a different key over the same file.
	resetBackend()
	t.Setenv(EnvSecretEncryptionKey, testKeyHex2)
	if err := InitLocalSecretBackend(); err == nil {
		t.Fatal("InitLocalSecretBackend with wrong key = nil, want decrypt error")
	}
}

func TestFileBackend_EnvOverlayWins(t *testing.T) {
	useFileBackend(t, testKeyHex)
	if err := PutSecret(keyGitHubPAT, "file-value"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	// The TRIAGE_FACTORY_* overlay shadows the stored value for the 4 well-known
	// keys, file backend or not.
	t.Setenv(envKeys[keyGitHubPAT], "env-value")
	if got, _ := GetSecret(keyGitHubPAT); got != "env-value" {
		t.Errorf("GetSecret with env overlay = %q, want env-value", got)
	}
}

func TestFileBackend_UnsupportedVersionFailsLoud(t *testing.T) {
	path := useFileBackend(t, testKeyHex)
	// A syntactically valid envelope with an unknown version must be rejected up
	// front, before any decrypt attempt.
	blob, err := json.Marshal(fileEnvelope{Version: 999, Nonce: []byte("n"), Ciphertext: []byte("c")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	resetBackend()
	if err := InitLocalSecretBackend(); err == nil {
		t.Fatal("InitLocalSecretBackend on unsupported version = nil, want error")
	}
}

func TestSweepKeychain(t *testing.T) {
	keyring.MockInit()
	if err := keyring.Set(service, "github_pat", "v"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	if err := SweepKeychain([]string{"github_pat", "never_stored"}); err != nil {
		t.Fatalf("SweepKeychain: %v", err)
	}
	if _, err := keyring.Get(service, "github_pat"); err != keyring.ErrNotFound {
		t.Errorf("after sweep, key still present (err=%v), want ErrNotFound", err)
	}
}

func TestFileBackend_ConcurrentPutsNoCorruption(t *testing.T) {
	useFileBackend(t, testKeyHex)
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + string(rune('A'+i%26)) + string(rune('0'+i/26))
			if err := PutSecret(key, "v"); err != nil {
				t.Errorf("concurrent PutSecret: %v", err)
			}
		}(i)
	}
	wg.Wait()
	// Re-resolve from disk: the file must still decrypt cleanly.
	resetBackend()
	if _, err := GetSecret("kA0"); err != nil {
		t.Errorf("after concurrent writes, reload GetSecret errored: %v", err)
	}
}
