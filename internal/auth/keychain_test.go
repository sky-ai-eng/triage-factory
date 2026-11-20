package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestKeychainBackend_PutTooLarge pins the mapping of the library's size
// refusal, which it returns on Windows and macOS, to ErrSecretTooLarge, and
// that any other refusal passes through as itself.
func TestKeychainBackend_PutTooLarge(t *testing.T) {
	t.Cleanup(keyring.MockInit)

	keyring.MockInitWithError(keyring.ErrSetDataTooBig)
	err := keychainBackend{}.put("github_app_pem", strings.Repeat("x", 3243))
	if !errors.Is(err, ErrSecretTooLarge) {
		t.Fatalf("put err = %v, want ErrSecretTooLarge", err)
	}
	if !strings.Contains(err.Error(), "github_app_pem") || !strings.Contains(err.Error(), "3243 bytes") {
		t.Errorf("put err = %q, want the key and the size for the log", err)
	}

	other := errors.New("no secret service on the session bus")
	keyring.MockInitWithError(other)
	err = keychainBackend{}.put("github_pat", "v")
	if !errors.Is(err, other) || errors.Is(err, ErrSecretTooLarge) {
		t.Errorf("put err = %v, want the library's own error", err)
	}
}
