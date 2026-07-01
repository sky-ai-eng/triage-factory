package license

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
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
