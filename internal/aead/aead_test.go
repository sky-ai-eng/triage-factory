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

	ct, nonce, err := k.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext equals plaintext — encryption did nothing")
	}
	if len(nonce) != 12 {
		t.Fatalf("nonce length %d, want 12", len(nonce))
	}

	got, err := k.Decrypt(ct, nonce)
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
	_, n1, err := k.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt #1: %v", err)
	}
	_, n2, err := k.Encrypt(plaintext)
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
	ct, nonce, err := k1.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k2.Decrypt(ct, nonce); err == nil {
		t.Fatal("decrypt with wrong key succeeded")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	k := mustKey(t)
	ct, nonce, err := k.Encrypt([]byte("authentic"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct[0] ^= 0x01
	if _, err := k.Decrypt(ct, nonce); err == nil {
		t.Fatal("decrypt of tampered ciphertext succeeded")
	}
}

func TestDecrypt_WrongNonce(t *testing.T) {
	k := mustKey(t)
	ct, _, err := k.Encrypt([]byte("plaintext"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	wrongNonce := make([]byte, 12) // all-zeros
	if _, err := k.Decrypt(ct, wrongNonce); err == nil {
		t.Fatal("decrypt with wrong nonce succeeded")
	}
}

func TestDecrypt_WrongNonceLength(t *testing.T) {
	k := mustKey(t)
	ct, _, err := k.Encrypt([]byte("plaintext"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k.Decrypt(ct, []byte{0x00}); err == nil {
		t.Fatal("decrypt with 1-byte nonce succeeded")
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
