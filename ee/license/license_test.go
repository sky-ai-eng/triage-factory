package license

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustKey(t *testing.T) (*ecdsa.PublicKey, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return &priv.PublicKey, priv
}

// signTest mints a token the way the out-of-band issuer does: ECDSA P-256
// over the SHA-256 digest of the payload, ASN.1/DER signature. Test-only —
// the shipping package never signs (production signing is key-custody-only),
// so this lives here rather than in license.go.
func signTest(t *testing.T, priv *ecdsa.PrivateKey, c Claims) string {
	t.Helper()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(sig)
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	tok := signTest(t, priv, Claims{
		Org: "acme", Features: []string{"sso", "scim"}, Expires: time.Now().Add(time.Hour).Unix(),
		Limits: map[string]int{"seats": 25},
	})
	got, err := NewVerifier(pub).Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.Has("sso") || !got.Has("scim") || got.Has("sandbox_fleet") {
		t.Fatalf("features wrong: %v", got.Features)
	}
	if got.Limits["seats"] != 25 {
		t.Fatalf("limits wrong: %v, want seats=25", got.Limits)
	}
}

func TestSignVerifyRoundTrip_NoLimits(t *testing.T) {
	pub, priv := mustKey(t)
	tok := signTest(t, priv, Claims{Org: "acme", Features: []string{"sso"}, Expires: time.Now().Add(time.Hour).Unix()})
	got, err := NewVerifier(pub).Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Limits != nil {
		t.Fatalf("limits = %v, want nil when absent from the signed payload", got.Limits)
	}
}

func TestSignVerifyRoundTrip_MaxSeats(t *testing.T) {
	pub, priv := mustKey(t)
	tok := signTest(t, priv, Claims{
		Org: "acme", Features: []string{"sso"}, MaxSeats: 25,
		Expires: time.Now().Add(time.Hour).Unix(),
	})
	got, err := NewVerifier(pub).Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.MaxSeats != 25 {
		t.Fatalf("MaxSeats = %d, want 25", got.MaxSeats)
	}
}

func TestSignVerifyRoundTrip_NoMaxSeats(t *testing.T) {
	pub, priv := mustKey(t)
	// A token minted without the claim (older issuer / uncapped tier) must parse
	// as MaxSeats == 0, the uncapped sentinel — additive + backward-compatible.
	tok := signTest(t, priv, Claims{Org: "acme", Features: []string{"sso"}, Expires: time.Now().Add(time.Hour).Unix()})
	got, err := NewVerifier(pub).Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.MaxSeats != 0 {
		t.Fatalf("MaxSeats = %d, want 0 (uncapped) when absent from the payload", got.MaxSeats)
	}
}

// TestVerifyRejectsTamperedMaxSeats proves maxSeats rides inside the signed
// payload: forging a bigger cap onto a validly-signed token (reusing its
// signature) fails verification. A customer cannot self-raise their seat cap.
func TestVerifyRejectsTamperedMaxSeats(t *testing.T) {
	pub, priv := mustKey(t)
	exp := time.Now().Add(time.Hour).Unix()
	// A real token committed to maxSeats=5 — keep its signature.
	realTok := signTest(t, priv, Claims{Org: "acme", Features: []string{"sso"}, MaxSeats: 5, Expires: exp})
	_, sigB64, _ := strings.Cut(realTok, ".")
	// Forge a payload that claims maxSeats=500, and staple the real signature on.
	forgedPayload, err := json.Marshal(Claims{Org: "acme", Features: []string{"sso"}, MaxSeats: 500, Expires: exp})
	if err != nil {
		t.Fatalf("marshal forged payload: %v", err)
	}
	forgedTok := b64.EncodeToString(forgedPayload) + "." + sigB64
	if _, err := NewVerifier(pub).Verify(forgedTok); err != ErrSignature {
		t.Fatalf("want ErrSignature on a forged maxSeats, got %v", err)
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	pub, priv := mustKey(t)
	tok := signTest(t, priv, Claims{Org: "acme", Features: []string{"sso"}, Expires: time.Now().Add(time.Hour).Unix()})
	if _, err := NewVerifier(pub).Verify("A" + tok[1:]); err == nil {
		t.Fatal("expected error on tampered token")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	pub, priv := mustKey(t)
	tok := signTest(t, priv, Claims{Org: "acme", Features: []string{"sso"}, Expires: time.Now().Add(-time.Hour).Unix()})
	if _, err := NewVerifier(pub).Verify(tok); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestVerifyWrongKey(t *testing.T) {
	otherPub, _ := mustKey(t)
	_, priv := mustKey(t)
	tok := signTest(t, priv, Claims{Org: "x", Features: []string{"sso"}, Expires: time.Now().Add(time.Hour).Unix()})
	if _, err := NewVerifier(otherPub).Verify(tok); err != ErrSignature {
		t.Fatalf("want ErrSignature, got %v", err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	pub, _ := mustKey(t)
	if _, err := NewVerifier(pub).Verify("not-a-token"); err != ErrMalformed {
		t.Fatalf("want ErrMalformed, got %v", err)
	}
}
