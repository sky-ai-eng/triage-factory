package aead

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func mustKey(t *testing.T) Key {
	t.Helper()
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return k
}

func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	k := mustKey(t)
	plaintext := []byte("eyJhbGciOiJIUzI1NiJ9.fake.jwt")

	ct, nonce, err := k.Encrypt(plaintext, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext equals plaintext — encryption did nothing")
	}
	if len(nonce) != 12 {
		t.Fatalf("nonce length %d, want 12", len(nonce))
	}

	got, err := k.Decrypt(ct, nonce, nil)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("roundtrip mismatch:\n got %q\nwant %q", got, plaintext)
	}
}

func TestEncrypt_NoncesDistinct(t *testing.T) {
	// Sanity: two Encrypt calls with the same key + plaintext produce
	// distinct nonces (and thus distinct ciphertexts). Catastrophic nonce
	// reuse silently breaks AES-GCM confidentiality, so we assert against
	// it explicitly even though the implementation reads from crypto/rand.
	k := mustKey(t)
	plaintext := []byte("same input")
	_, n1, err := k.Encrypt(plaintext, nil)
	if err != nil {
		t.Fatalf("Encrypt #1: %v", err)
	}
	_, n2, err := k.Encrypt(plaintext, nil)
	if err != nil {
		t.Fatalf("Encrypt #2: %v", err)
	}
	if bytes.Equal(n1, n2) {
		t.Fatal("two encryptions reused the same nonce")
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	k1 := mustKey(t)
	k2 := mustKey(t)
	ct, nonce, err := k1.Encrypt([]byte("secret"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k2.Decrypt(ct, nonce, nil); err == nil {
		t.Fatal("decrypt with wrong key succeeded")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	k := mustKey(t)
	ct, nonce, err := k.Encrypt([]byte("authentic"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct[0] ^= 0x01
	if _, err := k.Decrypt(ct, nonce, nil); err == nil {
		t.Fatal("decrypt of tampered ciphertext succeeded")
	}
}

func TestDecrypt_WrongNonce(t *testing.T) {
	k := mustKey(t)
	ct, _, err := k.Encrypt([]byte("plaintext"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	wrongNonce := make([]byte, 12) // all-zeros
	if _, err := k.Decrypt(ct, wrongNonce, nil); err == nil {
		t.Fatal("decrypt with wrong nonce succeeded")
	}
}

func TestDecrypt_WrongNonceLength(t *testing.T) {
	k := mustKey(t)
	ct, _, err := k.Encrypt([]byte("plaintext"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k.Decrypt(ct, []byte{0x00}, nil); err == nil {
		t.Fatal("decrypt with 1-byte nonce succeeded")
	}
}

// TestEncryptDecrypt_AAD_Roundtrip pins the happy path for additional
// authenticated data: a ciphertext sealed under an aad decrypts
// only when the same aad is supplied. The aad is authenticated, not
// encrypted, so it never appears in the ciphertext.
func TestEncryptDecrypt_AAD_Roundtrip(t *testing.T) {
	k := mustKey(t)
	plaintext := []byte("ghp_token_value")
	aad := []byte("org-123|user-0|github_pat")

	ct, nonce, err := k.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Note: ciphertext is pseudorandom; we don't assert it cannot contain the AAD bytes by coincidence.

	got, err := k.Decrypt(ct, nonce, aad)
	if err != nil {
		t.Fatalf("Decrypt with matching aad: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("roundtrip mismatch:\n got %q\nwant %q", got, plaintext)
	}
}

// TestDecrypt_WrongAAD is the load-bearing relocation case: a ciphertext
// sealed under one aad must fail to open under a different one, even with
// the correct key and nonce. This is what makes a secret-row ciphertext
// non-portable to a different (org, user, key) identity.
func TestDecrypt_WrongAAD(t *testing.T) {
	k := mustKey(t)
	ct, nonce, err := k.Encrypt([]byte("secret"), []byte("orgA|github_pat"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k.Decrypt(ct, nonce, []byte("orgB|github_pat")); err == nil {
		t.Fatal("decrypt under a different aad succeeded — relocation guard broken")
	}
}

// TestDecrypt_MissingAAD: a ciphertext sealed WITH an aad must not open
// when the reader supplies none (nil). Guards a caller that forgets to
// thread the aad through on the read path.
func TestDecrypt_MissingAAD(t *testing.T) {
	k := mustKey(t)
	ct, nonce, err := k.Encrypt([]byte("secret"), []byte("bound-context"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k.Decrypt(ct, nonce, nil); err == nil {
		t.Fatal("decrypt of aad-bound ciphertext with nil aad succeeded")
	}
}

// TestDecrypt_UnexpectedAAD: the mirror of MissingAAD — a ciphertext
// sealed with NO aad must not open when the reader supplies one.
func TestDecrypt_UnexpectedAAD(t *testing.T) {
	k := mustKey(t)
	ct, nonce, err := k.Encrypt([]byte("secret"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k.Decrypt(ct, nonce, []byte("unexpected")); err == nil {
		t.Fatal("decrypt of no-aad ciphertext with a non-nil aad succeeded")
	}
}

// TestEncrypt_NilAndEmptyAADEquivalent pins the crypto/cipher contract the
// session + file backends rely on: nil and an empty (non-nil) aad are the
// same to GCM. So an explicit nil reader-side opens a writer-side empty
// slice and vice versa — the "no associated data" envelope is stable.
func TestEncrypt_NilAndEmptyAADEquivalent(t *testing.T) {
	k := mustKey(t)
	plaintext := []byte("plaintext")

	ct, nonce, err := k.Encrypt(plaintext, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := k.Decrypt(ct, nonce, []byte{})
	if err != nil {
		t.Fatalf("Decrypt nil-sealed with empty aad: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q want %q", got, plaintext)
	}

	ct2, nonce2, err := k.Encrypt(plaintext, []byte{})
	if err != nil {
		t.Fatalf("Encrypt empty aad: %v", err)
	}
	if _, err := k.Decrypt(ct2, nonce2, nil); err != nil {
		t.Fatalf("Decrypt empty-sealed with nil aad: %v", err)
	}
}

func TestLoadKeyFromEnv_Hex(t *testing.T) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv(testEnv, hex.EncodeToString(raw))
	k, err := LoadKeyFromEnv(testEnv)
	if err != nil {
		t.Fatalf("LoadKeyFromEnv (hex): %v", err)
	}
	if !bytes.Equal(k[:], raw) {
		t.Fatal("loaded key bytes don't match input")
	}
}

func TestLoadKeyFromEnv_Base64(t *testing.T) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv(testEnv, base64.StdEncoding.EncodeToString(raw))
	k, err := LoadKeyFromEnv(testEnv)
	if err != nil {
		t.Fatalf("LoadKeyFromEnv (base64): %v", err)
	}
	if !bytes.Equal(k[:], raw) {
		t.Fatal("loaded key bytes don't match input")
	}
}

func TestLoadKeyFromEnv_Empty(t *testing.T) {
	t.Setenv(testEnv, "")
	_, err := LoadKeyFromEnv(testEnv)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-env error, got %v", err)
	}
}

func TestLoadKeyFromEnv_RejectsShort(t *testing.T) {
	// 16 bytes hex (AES-128 size) — must be rejected
	t.Setenv(testEnv, hex.EncodeToString(make([]byte, 16)))
	_, err := LoadKeyFromEnv(testEnv)
	if err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("expected wrong-size error, got %v", err)
	}
}

func TestLoadKeyFromEnv_RejectsLong(t *testing.T) {
	t.Setenv(testEnv, hex.EncodeToString(make([]byte, 64)))
	_, err := LoadKeyFromEnv(testEnv)
	if err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("expected wrong-size error, got %v", err)
	}
}

func TestLoadKeyFromEnv_RejectsGarbage(t *testing.T) {
	t.Setenv(testEnv, "not-hex-or-base64-!!!")
	_, err := LoadKeyFromEnv(testEnv)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
}

// testEnv is a synthetic var name used by the loader tests. Decoupled
// from any production constant so a rename downstream doesn't force a
// test rewrite.
const testEnv = "TF_TEST_KEY_LOADER"
