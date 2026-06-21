// Package verify wraps a GoTrue JWKS endpoint and verifies access tokens
// produced by it.
//
// The server-side auth path is multi-mode only — local-mode (TF_MODE=local)
// boots without a GoTrue dependency and the request-handler middleware never
// constructs a Verifier. The `triagefactory jwk-init --verify` CLI smoke
// helper does construct one regardless of mode (against an explicitly
// configured TF_GOTRUE_JWKS_URL); that path exists purely for operator
// debugging and doesn't touch the local server.
//
// The package deliberately does not import anything from internal/auth
// (keychain) since the concerns are orthogonal: keychain stores credentials
// the operator owns; this verifies tokens GoTrue mints for end-users.
package verify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Claims is the projection of a verified GoTrue access token we care about.
// Raw maps are kept addressable so downstream callers (D7 /api/me, public.users
// sync) can pull provider-specific fields without re-parsing.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Provider      string
	UserMetadata  map[string]any
	AppMetadata   map[string]any
	ExpiresAt     time.Time
}

// Verifier validates signed JWTs against a remote JWKS. Safe for concurrent use.
type Verifier struct {
	kf       keyfunc.Keyfunc
	issuer   string
	audience string
}

// NewVerifier blocks on the initial JWKS fetch — misconfig fails fast at boot.
// Subsequent refreshes happen in the background; unknown-kid lookups trigger
// an on-demand refresh inside keyfunc.
func NewVerifier(ctx context.Context, jwksURL, issuer, audience string) (*Verifier, error) {
	if jwksURL == "" {
		return nil, errors.New("jwks url is empty")
	}
	if issuer == "" {
		return nil, errors.New("issuer is empty")
	}
	if audience == "" {
		return nil, errors.New("audience is empty")
	}
	kf, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("init jwks fetch from %q: %w", jwksURL, err)
	}
	return &Verifier{kf: kf, issuer: issuer, audience: audience}, nil
}

// Verify parses, validates, and projects a JWT.
//
// Enforces RS256 only (rejects alg=none + HS-family downgrade attempts),
// requires a non-empty exp, and matches iss + aud against the configured
// values. The keyfunc auto-refreshes on unknown kid so rotated keys
// propagate without a restart.
func (v *Verifier) Verify(tokenStr string) (*Claims, error) {
	parsed, err := jwt.Parse(tokenStr, v.kf.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("verify jwt: %w", err)
	}
	if !parsed.Valid {
		return nil, errors.New("jwt invalid")
	}
	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("jwt claims wrong type: %T", parsed.Claims)
	}
	return extractClaims(mc)
}

func extractClaims(mc jwt.MapClaims) (*Claims, error) {
	out := &Claims{}

	sub, _ := mc["sub"].(string)
	if sub == "" {
		return nil, errors.New("jwt missing sub")
	}
	out.Subject = sub

	if email, ok := mc["email"].(string); ok {
		out.Email = email
	}

	if appMeta, ok := mc["app_metadata"].(map[string]any); ok {
		out.AppMetadata = appMeta
		if prov, ok := appMeta["provider"].(string); ok {
			out.Provider = prov
		}
	}
	if userMeta, ok := mc["user_metadata"].(map[string]any); ok {
		out.UserMetadata = userMeta
	}

	expAt, err := mc.GetExpirationTime()
	if err != nil || expAt == nil {
		return nil, fmt.Errorf("jwt missing valid exp: %w", err)
	}
	out.ExpiresAt = expAt.Time

	return out, nil
}
