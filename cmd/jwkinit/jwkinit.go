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
// Default emits to stdout. With --write-env <path>, upserts these vars into
// the given .env file (replacing a var's line in place, collapsing any stale
// duplicate lines, rather than blindly appending):
//   - GOTRUE_JWT_KEYS=<json>               the RS256 signing material
//   - GOTRUE_JWT_SECRET=<hex>              a fresh random value (GoTrue config
//     validation requires this even under
//     RS256; it's the legacy HS256 fallback
//     and isn't used for signing in practice)
//   - TF_GOTRUE_SERVICE_ROLE_TOKEN=<jwt>   a long-lived RS256 token signed with
//     the key above, carrying role:service_role — TF presents it as a static
//     bearer to GoTrue's admin SSO API (TFAC-424). TF holds only this token,
//     never the signing key.
//
// so the operator can pipe install setup into one step.
//
// Idempotent by default (TFAC-435): if a GOTRUE_JWT_KEYS already exists — in
// the --write-env file, or in the process env — this reuses that keypair to
// mint the service-role token and writes ONLY the token line, leaving the
// signing key (and every in-flight session signed by it) untouched. This is
// the enable-SSO-on-a-live-deployment path: acquire the token without a
// keypair rotation. To deliberately rotate the keypair (forced re-auth for all
// users), pass --rotate, which always generates a fresh key + secret + token.
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
	"path/filepath"
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
	writeEnv := fs.String("write-env", "", "upsert GOTRUE_JWT_KEYS + GOTRUE_JWT_SECRET + TF_GOTRUE_SERVICE_ROLE_TOKEN into this .env file instead of printing to stdout")
	rotate := fs.Bool("rotate", false, "force a fresh JWT signing keypair (forced re-auth for all users); default reuses any existing GOTRUE_JWT_KEYS and mints only the service-role token")
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

	if err := runGenerate(*writeEnv, *rotate); err != nil {
		fmt.Fprintf(os.Stderr, "jwk-init: %v\n", err)
		os.Exit(1)
	}
}

func runGenerate(writeEnvPath string, rotate bool) error {
	// Idempotent path (TFAC-435): unless --rotate is set, reuse an existing
	// GOTRUE_JWT_KEYS so enabling SSO on a live deployment mints only the
	// service-role token — no keypair rotation, no forced re-auth. We look in
	// the --write-env file first, then the process env.
	var (
		priv   *rsa.PrivateKey
		kid    string
		reused bool
	)
	if !rotate {
		if existing := readExistingJWTKeys(writeEnvPath); existing != "" {
			p, k, err := parseSigningKey(existing)
			if err != nil {
				return fmt.Errorf("reuse existing GOTRUE_JWT_KEYS: %w (pass --rotate to replace the keypair instead)", err)
			}
			priv, kid, reused = p, k, true
		}
	}

	// Fresh keypair material — only generated when there's nothing to reuse (or
	// the operator asked to rotate). jwtSecret/keys stay zero-valued on the
	// reuse path, signalling "don't touch GOTRUE_JWT_KEYS / GOTRUE_JWT_SECRET".
	var (
		keys      []map[string]any
		jwtSecret string
	)
	if !reused {
		var err error
		keys, priv, kid, err = generateGoTrueKeys()
		if err != nil {
			return err
		}
		// GoTrue's config validation still requires GOTRUE_JWT_SECRET to be
		// non-empty even when ALGORITHM=RS256 (it's a legacy HS256 fallback that
		// the asymmetric path doesn't actually exercise). Generate a random value
		// alongside the JWKS so the operator install flow stays one command — and
		// so the unused secret is at least cryptographically random rather than a
		// sentinel string.
		jwtSecret, err = randomHexSecret(32)
		if err != nil {
			return fmt.Errorf("generate jwt secret: %w", err)
		}
	}

	// The long-lived RS256 service-role token TF presents to GoTrue's admin SSO
	// API (TFAC-424). Signed with the (reused or fresh) private key — the key
	// stays in this process and never reaches the TF server, which holds only
	// the resulting token. The header kid must match the signing JWK's kid.
	serviceToken, err := mintServiceRoleToken(priv, kid, serviceRoleIssuer())
	if err != nil {
		return fmt.Errorf("mint service-role token: %w", err)
	}

	if writeEnvPath == "" {
		if reused {
			// Reusing an existing key from the env — print only the token; the
			// keypair and secret already live wherever the operator keeps them.
			fmt.Println("# service-role token signed with your existing GOTRUE_JWT_KEYS (TFAC-424); treat as a secret:")
			fmt.Printf("TF_GOTRUE_SERVICE_ROLE_TOKEN=%s\n", serviceToken)
			return nil
		}
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

	vars := []envVar{}
	if !reused {
		encoded, err := json.Marshal(keys)
		if err != nil {
			return fmt.Errorf("marshal keys: %w", err)
		}
		vars = append(vars,
			envVar{"GOTRUE_JWT_KEYS", string(encoded)},
			envVar{"GOTRUE_JWT_SECRET", jwtSecret},
		)
	}
	vars = append(vars, envVar{"TF_GOTRUE_SERVICE_ROLE_TOKEN", serviceToken})

	if err := upsertEnvFile(writeEnvPath, vars); err != nil {
		return err
	}
	if reused {
		fmt.Fprintf(os.Stderr, "reused existing GOTRUE_JWT_KEYS; wrote TF_GOTRUE_SERVICE_ROLE_TOKEN to %s (no keypair rotation)\n", writeEnvPath)
	} else {
		fmt.Fprintf(os.Stderr, "wrote GOTRUE_JWT_KEYS + GOTRUE_JWT_SECRET + TF_GOTRUE_SERVICE_ROLE_TOKEN to %s\n", writeEnvPath)
	}
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

// envVar is one KEY=value pair destined for the .env file.
type envVar struct {
	key   string
	value string
}

// readExistingJWTKeys resolves the current GOTRUE_JWT_KEYS value, preferring the
// --write-env file (so the documented one-file workflow needs no extra `export`)
// and falling back to the process env. Returns "" when neither has it — the
// fresh-install signal. When the file has stale duplicate lines (a relic of the
// old append-only --write-env), the last one wins, matching env-file semantics.
func readExistingJWTKeys(writeEnvPath string) string {
	if writeEnvPath != "" {
		if b, err := os.ReadFile(writeEnvPath); err == nil {
			val := ""
			for _, line := range strings.Split(string(b), "\n") {
				if envLineKey(line) == "GOTRUE_JWT_KEYS" {
					_, v, _ := strings.Cut(line, "=")
					val = unquoteEnvValue(strings.TrimSpace(v))
				}
			}
			if val != "" {
				return val
			}
		}
	}
	return strings.TrimSpace(os.Getenv("GOTRUE_JWT_KEYS"))
}

// parseSigningKey reconstructs the RSA private key (and its kid) from a
// GOTRUE_JWT_KEYS JSON array. GoTrue's set has exactly one private signing key —
// the entry with key_ops containing "sign" — which is the one we must sign the
// service-role token with so the token's kid resolves against the same set.
func parseSigningKey(jwtKeysJSON string) (*rsa.PrivateKey, string, error) {
	var set []map[string]any
	if err := json.Unmarshal([]byte(jwtKeysJSON), &set); err != nil {
		return nil, "", fmt.Errorf("parse GOTRUE_JWT_KEYS JSON: %w", err)
	}
	for _, jwk := range set {
		if !jwkHasSignOp(jwk) {
			continue
		}
		priv, err := jwkToPrivateKey(jwk)
		if err != nil {
			return nil, "", err
		}
		kid, _ := jwk["kid"].(string)
		if kid == "" {
			return nil, "", fmt.Errorf("signing JWK has no kid")
		}
		return priv, kid, nil
	}
	return nil, "", fmt.Errorf("no signing key (key_ops contains \"sign\") in GOTRUE_JWT_KEYS")
}

// jwkHasSignOp reports whether a JWK's key_ops array contains "sign" — the
// marker generateGoTrueKeys stamps on the private member of the set.
func jwkHasSignOp(jwk map[string]any) bool {
	ops, ok := jwk["key_ops"].([]any)
	if !ok {
		return false
	}
	for _, op := range ops {
		if s, _ := op.(string); s == "sign" {
			return true
		}
	}
	return false
}

// jwkToPrivateKey rebuilds an *rsa.PrivateKey from the base64url-encoded RSA
// members of a JWK. Only n, e, d, p, q are needed; the precomputed CRT values
// are re-derived locally so we don't depend on dp/dq/qi being present.
func jwkToPrivateKey(jwk map[string]any) (*rsa.PrivateKey, error) {
	n, err := jwkBigInt(jwk, "n")
	if err != nil {
		return nil, err
	}
	e, err := jwkBigInt(jwk, "e")
	if err != nil {
		return nil, err
	}
	d, err := jwkBigInt(jwk, "d")
	if err != nil {
		return nil, err
	}
	p, err := jwkBigInt(jwk, "p")
	if err != nil {
		return nil, err
	}
	q, err := jwkBigInt(jwk, "q")
	if err != nil {
		return nil, err
	}
	priv := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: n, E: int(e.Int64())},
		D:         d,
		Primes:    []*big.Int{p, q},
	}
	if err := priv.Validate(); err != nil {
		return nil, fmt.Errorf("invalid RSA private key in GOTRUE_JWT_KEYS: %w", err)
	}
	priv.Precompute()
	return priv, nil
}

// jwkBigInt decodes one base64url RSA member into a big.Int.
func jwkBigInt(jwk map[string]any, field string) (*big.Int, error) {
	s, ok := jwk[field].(string)
	if !ok || s == "" {
		return nil, fmt.Errorf("JWK missing %q member", field)
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode JWK %q: %w", field, err)
	}
	return new(big.Int).SetBytes(b), nil
}

// upsertEnvFile writes vars into path, replacing each key's existing line in
// place (and dropping any stale duplicate lines for that key) rather than
// appending — so re-running jwk-init never leaves the duplicate GOTRUE_JWT_KEYS
// the old append path produced. Keys not already present are appended in order.
// The write is atomic (temp file + rename) and the file stays 0600.
func upsertEnvFile(path string, vars []envVar) error {
	var existing []byte
	if b, err := os.ReadFile(path); err == nil {
		existing = b
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read env file: %w", err)
	}

	targets := make(map[string]string, len(vars))
	order := make([]string, 0, len(vars))
	for _, v := range vars {
		targets[v.key] = v.value
		order = append(order, v.key)
	}

	written := make(map[string]bool, len(vars))
	var out []string
	if len(existing) > 0 {
		body := strings.TrimSuffix(string(existing), "\n")
		for _, line := range strings.Split(body, "\n") {
			key := envLineKey(line)
			if _, isTarget := targets[key]; isTarget {
				if written[key] {
					continue // collapse a stale duplicate line
				}
				written[key] = true
				out = append(out, key+"="+targets[key])
				continue
			}
			out = append(out, line)
		}
	}
	for _, k := range order {
		if !written[k] {
			written[k] = true
			out = append(out, k+"="+targets[k])
		}
	}

	content := strings.Join(out, "\n") + "\n"
	return writeFileAtomic(path, []byte(content), 0o600)
}

// envLineKey returns the variable name a .env line assigns, or "" for blank
// lines, comments, and continuations. Tolerates a leading `export ` and
// surrounding whitespace so hand-edited files round-trip cleanly.
func envLineKey(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")
	key, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(key)
}

// unquoteEnvValue strips a single pair of matching surrounding quotes from a
// .env value. jwk-init writes values raw, but an operator may have quoted the
// GOTRUE_JWT_KEYS JSON by hand.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// writeFileAtomic writes data to a temp file in the destination's directory and
// renames it over path, so a crash mid-write can't leave a half-written .env.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".jwk-init-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp env file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp env file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp env file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp env file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp env file into place: %w", err)
	}
	return nil
}

const usage = `triagefactory jwk-init — generate the GoTrue RS256 signing key.

USAGE
  triagefactory jwk-init                       print JWKS + GOTRUE_JWT_SECRET +
                                               TF_GOTRUE_SERVICE_ROLE_TOKEN to
                                               stdout (or, if GOTRUE_JWT_KEYS is
                                               already in the env, just the
                                               service-role token)
  triagefactory jwk-init --write-env .env      upsert GOTRUE_JWT_KEYS=<jwks>,
                                               GOTRUE_JWT_SECRET=<hex>, and
                                               TF_GOTRUE_SERVICE_ROLE_TOKEN=<jwt>
                                               into .env. If .env already has a
                                               GOTRUE_JWT_KEYS, REUSE it and
                                               write only the token (no rotation)
  triagefactory jwk-init --rotate              force a fresh keypair even when
                                               one exists (FORCED RE-AUTH for all
                                               users); combine with --write-env
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

  Acquiring the token on a live deployment (TFAC-435): the default --write-env
  is IDEMPOTENT. If a GOTRUE_JWT_KEYS already exists (in the .env file, or the
  process env), jwk-init reuses that keypair to mint the service-role token and
  writes ONLY the token line — existing sessions keep verifying, no re-auth.
  --write-env also upserts in place (replacing a var's line, collapsing stale
  duplicates) instead of blindly appending, so re-running stays clean.

  Pass --rotate to deliberately replace the signing keypair. Every access token
  signed by the old key then fails verification against the new JWKS — a forced
  re-auth for all active sessions. Recreate the GoTrue container afterwards
  (docker compose up -d gotrue — NOT docker compose start) to pick up the new env.
`
