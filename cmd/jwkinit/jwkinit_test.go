package jwkinit

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
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

// TestParseSigningKey_RoundTrips: the key reconstructed from a GOTRUE_JWT_KEYS
// JSON array (the reuse path) is the same key generateGoTrueKeys produced —
// it verifies a token the original key signed, and recovers the same kid.
func TestParseSigningKey_RoundTrips(t *testing.T) {
	keys, origPriv, origKid, err := generateGoTrueKeys()
	if err != nil {
		t.Fatalf("generateGoTrueKeys: %v", err)
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}

	priv, kid, err := parseSigningKey(string(encoded))
	if err != nil {
		t.Fatalf("parseSigningKey: %v", err)
	}
	if kid != origKid {
		t.Errorf("kid = %q; want %q", kid, origKid)
	}
	if priv.N.Cmp(origPriv.N) != 0 || priv.D.Cmp(origPriv.D) != 0 {
		t.Fatal("reconstructed key differs from the original")
	}

	// A token signed by the original key must verify against the reconstructed
	// public key — proves the reuse path can mint a token GoTrue will accept.
	signed, err := mintServiceRoleToken(origPriv, origKid, "")
	if err != nil {
		t.Fatalf("mintServiceRoleToken: %v", err)
	}
	if _, err := jwt.Parse(signed, func(*jwt.Token) (any, error) { return &priv.PublicKey, nil }); err != nil {
		t.Fatalf("verify token with reconstructed key: %v", err)
	}
}

// TestParseSigningKey_NoSigningKey: a JWKS with no key_ops:["sign"] member is
// rejected rather than silently picking an arbitrary key.
func TestParseSigningKey_NoSigningKey(t *testing.T) {
	if _, _, err := parseSigningKey(`[{"kty":"RSA","kid":"x","use":"sig"}]`); err == nil {
		t.Fatal("want error for a set with no signing key")
	}
}

// TestParseSigningKey_OutOfRangeExponent: a hand-edited "e" too large for an int
// is rejected before the narrowing conversion can truncate it into a bogus
// (or negative) exponent that might slip past Validate.
func TestParseSigningKey_OutOfRangeExponent(t *testing.T) {
	keys, _, _, err := generateGoTrueKeys()
	if err != nil {
		t.Fatalf("generateGoTrueKeys: %v", err)
	}
	// Replace e with 2^96 — well past math.MaxInt on any platform.
	huge := new(big.Int).Lsh(big.NewInt(1), 96)
	keys[0]["e"] = base64.RawURLEncoding.EncodeToString(huge.Bytes())
	encoded, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, _, err := parseSigningKey(string(encoded)); err == nil {
		t.Fatal("want error for an out-of-range RSA exponent")
	}
}

// TestRunGenerate_ReusesExistingKeypair is the TFAC-435 core: a second
// --write-env run against a .env that already has GOTRUE_JWT_KEYS must NOT
// rotate the key — it reuses it and writes only the service-role token.
func TestRunGenerate_ReusesExistingKeypair(t *testing.T) {
	t.Setenv("GOTRUE_JWT_KEYS", "")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("EXISTING=1\n"), 0o600); err != nil {
		t.Fatalf("seed env: %v", err)
	}

	// First run: fresh install, mints the keypair.
	if err := runGenerate(path, false); err != nil {
		t.Fatalf("first runGenerate: %v", err)
	}
	keys1 := envValue(t, path, "GOTRUE_JWT_KEYS")
	secret1 := envValue(t, path, "GOTRUE_JWT_SECRET")
	token1 := envValue(t, path, "TF_GOTRUE_SERVICE_ROLE_TOKEN")
	if keys1 == "" || secret1 == "" || token1 == "" {
		t.Fatal("first run did not write all three vars")
	}

	// Second run, no --rotate: must reuse the keypair (keys + secret unchanged),
	// only refresh the token.
	if err := runGenerate(path, false); err != nil {
		t.Fatalf("second runGenerate: %v", err)
	}
	if got := envValue(t, path, "GOTRUE_JWT_KEYS"); got != keys1 {
		t.Error("GOTRUE_JWT_KEYS changed on idempotent re-run; want reuse")
	}
	if got := envValue(t, path, "GOTRUE_JWT_SECRET"); got != secret1 {
		t.Error("GOTRUE_JWT_SECRET changed on idempotent re-run; want reuse")
	}
	// The reused-key token must verify against the original keypair.
	priv, _, err := parseSigningKey(keys1)
	if err != nil {
		t.Fatalf("parseSigningKey: %v", err)
	}
	token2 := envValue(t, path, "TF_GOTRUE_SERVICE_ROLE_TOKEN")
	if _, err := jwt.Parse(token2, func(*jwt.Token) (any, error) { return &priv.PublicKey, nil }); err != nil {
		t.Fatalf("reused-key token does not verify against original key: %v", err)
	}

	// Unrelated lines survive, and no var is duplicated.
	if envValue(t, path, "EXISTING") != "1" {
		t.Error("pre-existing unrelated line was dropped")
	}
	assertNoDuplicateKeys(t, path)
}

// TestRunGenerate_RotateReplacesKeypair: --rotate replaces the keypair in place
// (no duplicate lines) and the new token verifies only against the new key.
func TestRunGenerate_RotateReplacesKeypair(t *testing.T) {
	t.Setenv("GOTRUE_JWT_KEYS", "")
	path := filepath.Join(t.TempDir(), ".env")
	if err := runGenerate(path, false); err != nil {
		t.Fatalf("first runGenerate: %v", err)
	}
	keys1 := envValue(t, path, "GOTRUE_JWT_KEYS")

	if err := runGenerate(path, true); err != nil {
		t.Fatalf("rotate runGenerate: %v", err)
	}
	keys2 := envValue(t, path, "GOTRUE_JWT_KEYS")
	if keys2 == keys1 {
		t.Fatal("--rotate did not change GOTRUE_JWT_KEYS")
	}
	assertNoDuplicateKeys(t, path)

	// New token must NOT verify against the old key (it's a real rotation).
	oldPriv, _, err := parseSigningKey(keys1)
	if err != nil {
		t.Fatalf("parseSigningKey(old): %v", err)
	}
	token2 := envValue(t, path, "TF_GOTRUE_SERVICE_ROLE_TOKEN")
	if _, err := jwt.Parse(token2, func(*jwt.Token) (any, error) { return &oldPriv.PublicKey, nil }); err == nil {
		t.Error("rotated token still verifies against the old key; want rotation")
	}
}

// TestUpsertEnvFile_CollapsesDuplicates: an .env with stale duplicate lines (the
// old append-path relic) collapses to a single in-place line on upsert.
func TestUpsertEnvFile_CollapsesDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	seed := "A=1\nGOTRUE_JWT_KEYS=old1\nB=2\nGOTRUE_JWT_KEYS=old2\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := upsertEnvFile(path, []envVar{{"GOTRUE_JWT_KEYS", "new"}}); err != nil {
		t.Fatalf("upsertEnvFile: %v", err)
	}
	if got := envValue(t, path, "GOTRUE_JWT_KEYS"); got != "new" {
		t.Errorf("GOTRUE_JWT_KEYS = %q; want new", got)
	}
	assertNoDuplicateKeys(t, path)
	if envValue(t, path, "A") != "1" || envValue(t, path, "B") != "2" {
		t.Error("unrelated lines not preserved")
	}
}

// envValue returns the last value assigned to key in the .env at path, or "".
func envValue(t *testing.T, path, key string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	val := ""
	for _, line := range strings.Split(string(b), "\n") {
		if envLineKey(line) == key {
			_, v, _ := strings.Cut(line, "=")
			val = strings.TrimSpace(v)
		}
	}
	return val
}

// assertNoDuplicateKeys fails if any variable appears on more than one line.
func assertNoDuplicateKeys(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	seen := map[string]int{}
	for _, line := range strings.Split(string(b), "\n") {
		if k := envLineKey(line); k != "" {
			seen[k]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("%s appears %d times; want 1", k, n)
		}
	}
}
