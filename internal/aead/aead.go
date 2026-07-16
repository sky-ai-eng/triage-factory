// Package aead is the shared AES-256-GCM primitive: a 32-byte Key plus
// Encrypt/Decrypt and an env-var key loader. It is a leaf package
// (stdlib only) so any layer can depend on it without pulling in a
// heavier package's transitive deps.
//
// Two callers share it:
//   - internal/sessions — the at-rest envelope for GoTrue access +
//     refresh tokens in public.sessions (multi mode only).
//   - internal/db/postgres — the at-rest envelope for org/user
//     integration secrets in public.org_secrets. Factoring
//     the primitive out here is what lets the Postgres secret store
//     encrypt app-side without internal/db/postgres importing
//     internal/sessions.
//
// The env-var names themselves stay with their consumers (sessions owns
// TF_SESSION_ENCRYPTION_KEY / TF_COOKIE_SECRET; the secret store owns
// TF_SECRET_ENCRYPTION_KEY) — this package only knows how to load and
// decode a 32-byte key from a named variable, not which variable.
package aead

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
)

// Key is a 32-byte fixed-size key. AES-256 takes a 32-byte key; the
// same type also serves callers that want a 32-byte HMAC-SHA256 secret
// (the cookie-signing secret in internal/sessions) — same loader/decoder
// code, semantically distinct callers.
type Key [32]byte

// LoadKeyFromEnv reads the named deployment secret (through secretenv, which
// has captured it out of the environment at boot; falls back to os.Getenv for
// names it didn't capture) and decodes it as either hex (64 chars) or standard
// base64. Returns a clear error rather than panicking — multi-mode boot fails
// fast on this, surfacing a readable startup error. The caller names the
// variable.
func LoadKeyFromEnv(envVar string) (Key, error) {
	raw := secretenv.Get(envVar)
	if raw == "" {
		return Key{}, fmt.Errorf("%s is empty (generate with `openssl rand -hex 32`)", envVar)
	}
	b, err := decodeKey(raw)
	if err != nil {
		return Key{}, fmt.Errorf("%s: %w", envVar, err)
	}
	if len(b) != 32 {
		return Key{}, fmt.Errorf("%s must decode to 32 bytes, got %d", envVar, len(b))
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// decodeKey accepts hex first (the documented format), falling back to
// standard base64. We don't try URL-safe base64 because `openssl rand
// -base64 32` (the alternative we mention in docs) emits standard.
func decodeKey(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, errors.New("not a valid hex or base64 string")
}

// Encrypt returns (ciphertext, nonce) for a fresh AES-GCM encryption.
// Nonce is 12 bytes from crypto/rand — never reuse across (key, plaintext)
// pairs. We hand back the nonce as a separate value rather than prefixing
// the ciphertext so callers can store it in its own column (the schema
// stays self-describing).
//
// aad is additional authenticated data: folded into the GCM auth tag but
// neither encrypted nor returned (the caller does not store it). Pass it
// to bind a ciphertext to its context — e.g. the (org_id, user_id, key)
// row identity of a stored secret — so a blob copied into a different
// context fails Decrypt's auth-tag check instead of yielding its
// plaintext. Decrypt must be called with byte-identical aad. Callers that
// need no binding pass nil; nil and an empty slice are equivalent to GCM,
// so nil reproduces the plain "no associated data" envelope.
func (k Key) Encrypt(plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("gcm: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, aad)
	return ciphertext, nonce, nil
}

// Decrypt inverts Encrypt. Returns an error on auth-tag failure — a
// tampered ciphertext, the wrong key, OR aad that doesn't byte-match the
// value passed to Encrypt. AEAD doesn't distinguish those, which is the
// point: a relocated ciphertext (right key, wrong context) fails exactly
// like a forged one. Callers render a generic failure ("session invalid",
// "secret undecryptable") rather than surfacing the underlying error to
// the user. Pass the same aad that was given to Encrypt (nil if none).
func (k Key) Decrypt(ciphertext, nonce, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("nonce length %d, want %d", len(nonce), gcm.NonceSize())
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}
