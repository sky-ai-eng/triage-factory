// Package sessions owns multi-mode user sessions: the AES-GCM envelope
// that encrypts GoTrue access + refresh tokens at rest, and the
// public.sessions CRUD that wraps them.
//
// Local mode never imports this package — sessions only exist in
// multi-mode (cookie-bearer auth backed by GoTrue). See
// docs/multi-tenant-architecture.html §4 + §13 D7.
package sessions

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
)

// Key is the AES-256-GCM / HMAC-SHA256 key type. It aliases aead.Key so
// the encryption primitive lives in the leaf internal/aead package (the
// Postgres secret store shares it without importing internal/sessions);
// the alias keeps every sessions.Key / NewStore caller compiling
// unchanged. Encrypt/Decrypt come from aead.Key, and callers load keys
// via aead.LoadKeyFromEnv(sessions.EnvSessionEncryptionKey) etc.
type Key = aead.Key

// Operator-facing env var names. Kept as exported constants so the
// runtime loader, docs, and any future helper tooling can't drift on
// the spelling.
//
// The two are kept separate (rather than one master key with HKDF
// subkeys) because:
//   - Rotating the cookie secret should not invalidate every active
//     session's encrypted blobs, and vice versa
//   - It matches the convention in skynet/authkit
//     (SESSION_ENCRYPTION_KEY + a separate signing pepper)
const (
	EnvSessionEncryptionKey = "TF_SESSION_ENCRYPTION_KEY" // AES-GCM at-rest key for jwt_enc / refresh_token_enc
	EnvCookieSecret         = "TF_COOKIE_SECRET"          // HMAC key for the short-lived OAuth state cookie
)

// LogID returns a short, non-reversible identifier for a session UUID
// suitable for log lines and error messages. Implementation: first 8
// hex chars of SHA-256(sid). Properties:
//   - One-way: an attacker reading logs can't recover the sid
//   - Stable: the same sid always maps to the same prefix, so a
//     support engineer can correlate "this user's session keeps
//     failing refresh" by spotting repeats
//   - Collision-resistant at our scale: 8 hex chars = 32 bits ≈ 4B
//     possibilities; for 10K active sessions, the birthday-collision
//     probability is negligible
//
// Production log lines should always wrap sid arguments through this
// helper. Logging the raw UUID gives an attacker who exfiltrates logs
// a roster of recently-valid session ids that could be paired with
// stolen cookies for replay.
func LogID(sid uuid.UUID) string {
	sum := sha256.Sum256(sid[:])
	return hex.EncodeToString(sum[:4])
}
