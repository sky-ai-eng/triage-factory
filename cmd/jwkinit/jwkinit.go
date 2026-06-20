// Package jwkinit is the CLI entrypoint for the `triagefactory jwk-init`
// subcommand. It generates the RS256 keypair that GoTrue uses to sign
// access tokens.
//
// GoTrue's GOTRUE_JWT_KEYS expects a JSON ARRAY of JWK objects (each
// carrying both public and private material), NOT the RFC-7517-wrapped
// {"keys": [...]} JWKS form. The Verifier consumes the standard JWKS
// from GoTrue's /.well-known/jwks.json endpoint; GoTrue translates
// between the two shapes internally.
//
// Default emits to stdout. With --write-env <path>, appends three lines to
// the given .env file:
//   - GOTRUE_JWT_KEYS=<json>               the RS256 signing material
//   - GOTRUE_JWT_SECRET=<hex>              a fresh random value (GoTrue config
//     validation requires this even under
//     RS256; it's the legacy HS256 fallback
//     and isn't used for signing in practice)
//   - TF_GOTRUE_SERVICE_ROLE_TOKEN=<jwt>   a long-lived RS256 token signed with
//     the key above, carrying role:service_role — TF presents it as a static
//     bearer to GoTrue's admin SSO API (TFAC-424). TF holds only this token,
//     never the signing key. Rotating the keypair (re-running this command)
//     regenerates this token with it.
//
// so the operator can pipe install setup into one step.
//
// Also exposes --verify, which reads one JWT from stdin and verifies it
// against the JWKS at TF_GOTRUE_JWKS_URL — the operator-facing version of
// the unit-test rotation smoke check.
package jwkinit

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
)

// serviceRoleSubject is the fixed `sub` on the long-lived service-role admin
// token. It's a non-user system subject — GoTrue doesn't enforce sub on
// service_role admin calls — so the nil UUID is a clear sentinel.
const serviceRoleSubject = "00000000-0000-0000-0000-000000000000"

// serviceRoleTokenTTL is how long the minted admin token stays valid. It's a
// static deployment credential (rotated by re-running jwk-init), so a long
// horizon avoids surprise expiry; ~10 years is effectively "until rotated".
const serviceRoleTokenTTL = 10 * 365 * 24 * time.Hour

// Handle dispatches from main.go on `triagefactory jwk-init ...`.
func Handle(args []string) {
	fs := flag.NewFlagSet("jwk-init", flag.ExitOnError)
	writeEnv := fs.String("write-env", "", "append GOTRUE_JWT_KEYS + GOTRUE_JWT_SECRET to this .env file instead of printing to stdout")
	verifyMode := fs.Bool("verify", false, "read a JWT from stdin and verify it against TF_GOTRUE_JWKS_URL / TF_GOTRUE_ISSUER")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *verifyMode {
		if err := runVerify(); err != nil {
			fmt.Fprintf(os.Stderr, "verify: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := runGenerate(*writeEnv); err != nil {
		fmt.Fprintf(os.Stderr, "jwk-init: %v\n", err)
		os.Exit(1)
	}
}

func runGenerate(writeEnvPath string) error {
	keys, priv, kid, err := generateGoTrueKeys()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("marshal keys: %w", err)
	}

	// GoTrue's config validation still requires GOTRUE_JWT_SECRET to be non-empty
	// even when ALGORITHM=RS256 (it's a legacy HS256 fallback that the asymmetric
	// path doesn't actually exercise). Generate a random value alongside the
	// JWKS so the operator install flow stays one command — and so the unused
	// secret is at least cryptographically random rather than a sentinel string.
	jwtSecret, err := randomHexSecret(32)
	if err != nil {
		return fmt.Errorf("generate jwt secret: %w", err)
	}

	// The long-lived RS256 service-role token TF presents to GoTrue's admin SSO
	// API (TFAC-424). Signed here with the freshly generated private key — the
	// key stays in this process and never reaches the TF server, which holds only
	// the resulting token. Minted alongside the keypair so rotation is one step.
	serviceToken, err := mintServiceRoleToken(priv, kid, serviceRoleIssuer())
	if err != nil {
		return fmt.Errorf("mint service-role token: %w", err)
	}

	if writeEnvPath == "" {
		// Pretty-print stdout for human inspection; the .env form is compact.
		pretty, _ := json.MarshalIndent(keys, "", "  ")
		fmt.Println(string(pretty))
		fmt.Println()
		fmt.Println("# also set GOTRUE_JWT_SECRET (required by gotrue config; unused under RS256):")
		fmt.Printf("GOTRUE_JWT_SECRET=%s\n", jwtSecret)
		fmt.Println()
		fmt.Println("# service-role token TF presents to GoTrue's admin SSO API (TFAC-424); treat as a secret:")
		fmt.Printf("TF_GOTRUE_SERVICE_ROLE_TOKEN=%s\n", serviceToken)
		return nil
	}

	// O_RDWR (not O_WRONLY) so we can ReadAt the last byte before appending —
	// otherwise an existing .env that doesn't end in \n could produce a
	// `TF_SESSION_KEY=...GOTRUE_JWT_KEYS=...` smush on one line.
	f, err := os.OpenFile(writeEnvPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer f.Close()
	// OpenFile's mode arg only applies on CREATE; explicit chmod handles the
	// common case where .env already exists with looser perms.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod env file: %w", err)
	}
	if err := ensureTrailingNewline(f); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "GOTRUE_JWT_KEYS=%s\nGOTRUE_JWT_SECRET=%s\nTF_GOTRUE_SERVICE_ROLE_TOKEN=%s\n", string(encoded), jwtSecret, serviceToken); err != nil {
		return fmt.Errorf("write env lines: %w", err)
	}
	fmt.Fprintf(os.Stderr, "appended GOTRUE_JWT_KEYS + GOTRUE_JWT_SECRET + TF_GOTRUE_SERVICE_ROLE_TOKEN to %s\n", writeEnvPath)
	return nil
}

func runVerify() error {
	jwksURL := os.Getenv("TF_GOTRUE_JWKS_URL")
	issuer := os.Getenv("TF_GOTRUE_ISSUER")
	audience := os.Getenv("TF_GOTRUE_AUD")
	if audience == "" {
		audience = "authenticated"
	}

	v, err := verify.NewVerifier(context.Background(), jwksURL, issuer, audience)
	if err != nil {
		return fmt.Errorf("init verifier: %w", err)
	}

	tokenBytes, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return fmt.Errorf("stdin: empty")
	}

	claims, err := v.Verify(token)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(claims, "", "  ")
	fmt.Println(string(out))
	return nil
}

// generateGoTrueKeys produces GoTrue's GOTRUE_JWT_KEYS shape — a JSON
// array of JWK objects, each with both public and private RSA material.
// GoTrue exposes only the public side at /.well-known/jwks.json. It also
// returns the private key + its kid so the caller can mint the service-role
// admin token against the same key (the kid must match for GoTrue to resolve
// the verifying key from its set).
func generateGoTrueKeys() (keys []map[string]any, priv *rsa.PrivateKey, kid string, err error) {
	priv, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, "", fmt.Errorf("rsa generate: %w", err)
	}
	priv.Precompute()

	kid = uuid.NewString()
	jwk := map[string]any{
		"kty": "RSA",
		"use": "sig",
		// GoTrue's JwtKeysDecoder.Validate requires exactly one private key
		// in the set with key_ops containing "sign". Setting it here marks
		// this entry as the active signing key; the derived public-side JWK
		// gets key_ops=["verify"] applied automatically by GoTrue.
		"key_ops": []string{"sign"},
		"alg":     "RS256",
		"kid":     kid,
		"n":       b64(priv.N.Bytes()),
		"e":       b64(big.NewInt(int64(priv.E)).Bytes()),
		"d":       b64(priv.D.Bytes()),
		"p":       b64(priv.Primes[0].Bytes()),
		"q":       b64(priv.Primes[1].Bytes()),
		"dp":      b64(priv.Precomputed.Dp.Bytes()),
		"dq":      b64(priv.Precomputed.Dq.Bytes()),
		"qi":      b64(priv.Precomputed.Qinv.Bytes()),
	}
	return []map[string]any{jwk}, priv, kid, nil
}

// mintServiceRoleToken signs the long-lived RS256 admin token TF presents to
// GoTrue's admin SSO API. The JWT header `kid` MUST equal the signing JWK's kid
// so GoTrue resolves the matching public key from GOTRUE_JWT_KEYS. role is the
// only claim GoTrue enforces for admin auth (plus a valid signature + exp);
// aud/iss aren't enforced but are stamped for hygiene. The empirical spike
// against supabase/gotrue:v2.189.0 confirmed an HS256 token (GOTRUE_JWT_SECRET)
// is rejected under GOTRUE_JWT_ALGORITHM=RS256 — this RS256 token is the one
// GoTrue accepts.
func mintServiceRoleToken(priv *rsa.PrivateKey, kid, issuer string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"role": "service_role",
		"aud":  "authenticated",
		"sub":  serviceRoleSubject,
		"iat":  now.Unix(),
		"exp":  now.Add(serviceRoleTokenTTL).Unix(),
	}
	if issuer != "" {
		claims["iss"] = issuer
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		return "", fmt.Errorf("sign service-role token: %w", err)
	}
	return signed, nil
}

// serviceRoleIssuer derives the GoTrue issuer (API_EXTERNAL_URL =
// <TF_PUBLIC_URL>/auth/v1) for the token's iss claim, best-effort from the env.
// Returns "" when TF_PUBLIC_URL isn't set — iss isn't enforced by GoTrue, so the
// token still works; this is hygiene only.
func serviceRoleIssuer() string {
	pub := strings.TrimRight(strings.TrimSpace(os.Getenv("TF_PUBLIC_URL")), "/")
	if pub == "" {
		return ""
	}
	return pub + "/auth/v1"
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// randomHexSecret returns 2*nBytes hex chars from a crypto/rand source.
// Hex avoids any shell-quoting concerns when the value gets exported into
// a docker-compose env block.
func randomHexSecret(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensureTrailingNewline checks the last byte of an O_APPEND-opened file and
// writes a \n if it's missing. Required because an existing .env that the
// operator hand-edited without a trailing newline would otherwise concatenate
// our new line onto the previous one.
func ensureTrailingNewline(f *os.File) error {
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat env file: %w", err)
	}
	if stat.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, stat.Size()-1); err != nil {
		return fmt.Errorf("read last byte of env file: %w", err)
	}
	if last[0] == '\n' {
		return nil
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("write leading newline: %w", err)
	}
	return nil
}

const usage = `triagefactory jwk-init — generate the GoTrue RS256 signing key.

USAGE
  triagefactory jwk-init                       print JWKS + GOTRUE_JWT_SECRET +
                                               TF_GOTRUE_SERVICE_ROLE_TOKEN to
                                               stdout
  triagefactory jwk-init --write-env .env      append GOTRUE_JWT_KEYS=<jwks>,
                                               GOTRUE_JWT_SECRET=<hex>, and
                                               TF_GOTRUE_SERVICE_ROLE_TOKEN=<jwt>
                                               to .env
  triagefactory jwk-init --verify              read JWT from stdin; verify
                                               against TF_GOTRUE_JWKS_URL +
                                               TF_GOTRUE_ISSUER; print claims

NOTES
  The JWKS contains private material — treat the output like a secret. Only
  the public side is published by GoTrue at /.well-known/jwks.json. The
  GOTRUE_JWT_SECRET is the legacy HS256 fallback that GoTrue config
  validation still requires even under RS256; the value isn't used for
  signing but is required to be non-empty.

  TF_GOTRUE_SERVICE_ROLE_TOKEN is a long-lived RS256 token (role:service_role)
  signed with the generated key; the TF server presents it as a static bearer
  to GoTrue's admin SSO API (TFAC-424) and never holds the signing key itself.
  Treat it as a secret. Set TF_PUBLIC_URL before running so the token's iss
  claim matches the deployment (optional — iss isn't enforced by GoTrue).

  Re-running rotates the key (and the service-role token with it). --write-env
  APPENDS new values rather than editing in place, so delete the prior
  GOTRUE_JWT_KEYS / GOTRUE_JWT_SECRET / TF_GOTRUE_SERVICE_ROLE_TOKEN lines before
  re-running to avoid stale duplicates (env files take the last value, so the
  stack still works — the old lines are just noise). Recreate the GoTrue
  container (docker compose up -d gotrue — NOT docker compose start) to pick up
  the new env.
`
