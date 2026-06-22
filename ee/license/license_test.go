package license

import (
	"crypto/ed25519"
	"testing"
	"time"
)

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return pub, priv
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	tok, err := Sign(priv, Claims{Org: "acme", Features: []string{"sso", "scim"}, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := NewVerifier(pub).Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.Has("sso") || !got.Has("scim") || got.Has("sandbox_fleet") {
		t.Fatalf("features wrong: %v", got.Features)
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	pub, priv := mustKey(t)
	tok, _ := Sign(priv, Claims{Org: "acme", Features: []string{"sso"}, Expires: time.Now().Add(time.Hour).Unix()})
	if _, err := NewVerifier(pub).Verify("A" + tok[1:]); err == nil {
		t.Fatal("expected error on tampered token")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	pub, priv := mustKey(t)
	tok, _ := Sign(priv, Claims{Org: "acme", Features: []string{"sso"}, Expires: time.Now().Add(-time.Hour).Unix()})
	if _, err := NewVerifier(pub).Verify(tok); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestVerifyWrongKey(t *testing.T) {
	otherPub, _ := mustKey(t)
	_, priv := mustKey(t)
	tok, _ := Sign(priv, Claims{Org: "x", Features: []string{"sso"}, Expires: time.Now().Add(time.Hour).Unix()})
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
