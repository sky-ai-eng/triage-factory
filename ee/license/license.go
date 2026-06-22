// Package license verifies Ed25519-signed Triage Factory Enterprise
// Edition license tokens.
//
// A token is two base64url segments joined by a dot:
//
//	base64url(payloadJSON) "." base64url(ed25519-signature-over-payload-bytes)
//
// The payload is a Claims JSON object. Verification checks the signature
// against the build's baked-in public key and rejects expired tokens. The
// format is deliberately minimal — not a JWT — because a license is a
// single self-issued credential with one signer, so the JOSE machinery
// (alg negotiation, JWKS, kid) would be ceremony without benefit. The
// private signing key never ships in the binary; it lives with the
// licensor's issuing tool (which calls Sign).
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claims is the verified payload of a license token.
type Claims struct {
	// Org is the licensed organization (human label for diagnostics; the
	// signature, not this field, is the authority).
	Org string `json:"org"`
	// Features lists the entitlement keys this license unlocks — the same
	// string values as internal/entitlements.Feature.
	Features []string `json:"features"`
	// IssuedAt / Expires are Unix seconds. Expires is required; a token
	// with a zero or past Expires is never valid.
	IssuedAt int64 `json:"iat,omitempty"`
	Expires  int64 `json:"exp"`
	// Issuer optionally names who minted the token.
	Issuer string `json:"iss,omitempty"`
}

// Has reports whether the license lists the given feature key.
func (c Claims) Has(feature string) bool {
	for _, f := range c.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// ExpiresAt returns the expiry as a time.Time.
func (c Claims) ExpiresAt() time.Time { return time.Unix(c.Expires, 0) }

// Valid reports whether the token is unexpired as of now. Fails closed: a
// missing/zero Expires is invalid.
func (c Claims) Valid(now time.Time) bool {
	return c.Expires > 0 && now.Before(c.ExpiresAt())
}

var (
	// ErrMalformed means the token isn't two base64url segments / valid JSON.
	ErrMalformed = errors.New("license: malformed token")
	// ErrSignature means the Ed25519 signature did not verify against the key.
	ErrSignature = errors.New("license: signature verification failed")
	// ErrExpired means the signature was valid but the token has expired.
	ErrExpired = errors.New("license: token expired")
)

var b64 = base64.RawURLEncoding

// Sign produces a token for the given claims. Used by the licensor's
// issuing tool — never by the server, which only ever verifies.
func Sign(priv ed25519.PrivateKey, c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, payload)
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(sig), nil
}

// Verifier checks tokens against a single Ed25519 public key.
type Verifier struct {
	pub ed25519.PublicKey
	// now is overridable in tests; nil means time.Now.
	now func() time.Time
}

// NewVerifier returns a Verifier bound to pub.
func NewVerifier(pub ed25519.PublicKey) *Verifier {
	return &Verifier{pub: pub}
}

// Verify checks the signature and expiry and returns the claims on
// success. Every failure path returns a typed error and zero claims —
// callers must treat any error as "unlicensed".
func (v *Verifier) Verify(token string) (Claims, error) {
	if v == nil || len(v.pub) != ed25519.PublicKeySize {
		return Claims{}, ErrSignature
	}
	payloadB64, sigB64, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, ErrMalformed
	}
	payload, err := b64.DecodeString(payloadB64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: payload: %v", ErrMalformed, err)
	}
	sig, err := b64.DecodeString(sigB64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: signature: %v", ErrMalformed, err)
	}
	if !ed25519.Verify(v.pub, payload, sig) {
		return Claims{}, ErrSignature
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, fmt.Errorf("%w: claims: %v", ErrMalformed, err)
	}
	now := time.Now
	if v.now != nil {
		now = v.now
	}
	if !c.Valid(now()) {
		return Claims{}, ErrExpired
	}
	return c, nil
}
