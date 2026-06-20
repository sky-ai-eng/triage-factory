package jwkinit

import (
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestMintServiceRoleToken verifies the long-lived admin token is an RS256 JWT
// signed by the generated key, carries the claims GoTrue's admin path expects
// (role:service_role) plus the hygiene claims, and — critically — stamps a
// header kid matching the signing JWK so GoTrue can resolve the verifying key
// from GOTRUE_JWT_KEYS.
func TestMintServiceRoleToken(t *testing.T) {
	_, priv, kid, err := generateGoTrueKeys()
	if err != nil {
		t.Fatalf("generateGoTrueKeys: %v", err)
	}

	const issuer = "https://tf.example/auth/v1"
	signed, err := mintServiceRoleToken(priv, kid, issuer)
	if err != nil {
		t.Fatalf("mintServiceRoleToken: %v", err)
	}

	tok, err := jwt.Parse(signed, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("alg = %v; want an RSA method", tok.Header["alg"])
		}
		return &priv.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("parse/verify minted token: %v", err)
	}
	if !tok.Valid {
		t.Fatal("minted token did not validate")
	}
	if tok.Method.Alg() != "RS256" {
		t.Errorf("alg = %q; want RS256", tok.Method.Alg())
	}
	// The header kid must match the signing key's kid — GoTrue selects the
	// verifying key from its set by kid, so a mismatch 403s every admin call.
	if got := tok.Header["kid"]; got != kid {
		t.Errorf("header kid = %v; want %q", got, kid)
	}

	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("claims are not MapClaims")
	}
	if claims["role"] != "service_role" {
		t.Errorf("role = %v; want service_role", claims["role"])
	}
	if claims["aud"] != "authenticated" {
		t.Errorf("aud = %v; want authenticated", claims["aud"])
	}
	if claims["sub"] != serviceRoleSubject {
		t.Errorf("sub = %v; want %q", claims["sub"], serviceRoleSubject)
	}
	if claims["iss"] != issuer {
		t.Errorf("iss = %v; want %q", claims["iss"], issuer)
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		t.Fatalf("exp claim: %v", err)
	}
	// Far-future expiry (a static, rotation-on-rerun credential): at least 9
	// years out, guarding against an accidental short TTL.
	if time.Until(exp.Time) < 9*365*24*time.Hour {
		t.Errorf("exp = %v; want ~10y out", exp.Time)
	}
}

// TestMintServiceRoleToken_NoIssuerOmitsClaim: an empty issuer (TF_PUBLIC_URL
// unset) omits iss entirely rather than stamping an empty string — iss isn't
// enforced by GoTrue, so the token still works.
func TestMintServiceRoleToken_NoIssuerOmitsClaim(t *testing.T) {
	_, priv, kid, err := generateGoTrueKeys()
	if err != nil {
		t.Fatalf("generateGoTrueKeys: %v", err)
	}
	signed, err := mintServiceRoleToken(priv, kid, "")
	if err != nil {
		t.Fatalf("mintServiceRoleToken: %v", err)
	}
	tok, err := jwt.Parse(signed, func(*jwt.Token) (any, error) { return &priv.PublicKey, nil })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claims := tok.Claims.(jwt.MapClaims)
	if _, present := claims["iss"]; present {
		t.Errorf("iss present with empty issuer; want omitted")
	}
}
