// Package license verifies ECDSA P-256 / SHA-256 signed Triage Factory
// Enterprise Edition license tokens.
//
// A token is two base64url segments joined by a dot:
//
//	base64url(payloadJSON) "." base64url(ASN.1/DER ECDSA signature)
//
// The signature is an ASN.1/DER-encoded ECDSA P-256 signature over the
// SHA-256 digest of the payload bytes — the conventional, interoperable
// encoding, so a signature produced by a standard signing service drops in
// without re-encoding. The payload is a Claims JSON object. Verification
// checks the signature against the build's baked-in public key and rejects
// expired tokens. The format is deliberately minimal — not a JWT — because a
// license is a single self-issued credential with one signer, so the JOSE
// machinery (alg negotiation, JWKS, kid) would be ceremony without benefit.
//
// The binary only ever verifies; it never signs. The private signing key
// never ships in (or touches) the binary — production tokens are minted
// out-of-band by the licensor's issuing service. The verify path is
// exercised end to end by a test-only signer in license_test.go.
package license

import (
	"crypto/ecdsa"
	"crypto/sha256"
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
	// MaxSeats is the committed per-seat cap for per-seat billing (Professional
	// tier). A top-level int in the signed payload, so it is tamper-evident like
	// every other claim. Absent / zero means UNCAPPED — additive and backward-
	// compatible: a token minted before the claim existed, or by an issuer that
	// omits it, verifies exactly as before and enforces no cap; an older build
	// that doesn't know the field ignores it. Surfaced to enforcement through the
	// entitlements seam (see ee.Install → entitlements.LimitSeats), never read
	// directly by core.
	MaxSeats int `json:"maxSeats,omitempty"`
	// Limits carries deployment-wide quota values (e.g. seats), keyed by the
	// same string values as internal/entitlements.Limit. Absent from the
	// signed payload → nil map → no limits. Not surfaced as a per-org
	// entitlement (see ee.licenseProvider) — a deployment-wide cap is a
	// license property, read straight off the license when self-host seat
	// enforcement lands.
	Limits map[string]int `json:"limits,omitempty"`
}

// Has reports whether the license lists the given feature key. Matching is
// exact and case-sensitive: feature ids are stable lower-case wire identifiers
// (see entitlements.Feature), so the issuing tool must emit them lower-case —
// "SSO" would silently not match "sso".
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
	// ErrSignature means the ECDSA signature did not verify against the key.
	ErrSignature = errors.New("license: signature verification failed")
	// ErrExpired means the signature was valid but the token has expired.
	ErrExpired = errors.New("license: token expired")
)

var b64 = base64.RawURLEncoding

// Verifier checks tokens against a single ECDSA P-256 public key.
type Verifier struct {
	pub *ecdsa.PublicKey
	// now is overridable in tests; nil means time.Now.
	now func() time.Time
}

// NewVerifier returns a Verifier bound to pub.
func NewVerifier(pub *ecdsa.PublicKey) *Verifier {
	return &Verifier{pub: pub}
}

// Verify checks the signature and expiry and returns the claims on
// success. Every failure path returns a typed error and zero claims —
// callers must treat any error as "unlicensed".
func (v *Verifier) Verify(token string) (Claims, error) {
	if v == nil || v.pub == nil {
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
	// ECDSA P-256 verify over the SHA-256 digest of the payload, against an
	// ASN.1/DER signature. VerifyASN1 is agnostic to how the signature was
	// produced, so any standard signer interoperates.
	digest := sha256.Sum256(payload)
	if !ecdsa.VerifyASN1(v.pub, digest[:], sig) {
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
