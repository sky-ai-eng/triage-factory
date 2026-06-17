package app

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// buildServer constructs the HTTP server, wires mode-specific deployment
// identity (local HMAC key / multi-mode GoTrue auth stack), serves the
// embedded frontend, and exposes the websocket hub the rest of the graph
// broadcasts through. Must run after openStores (it needs the stores +
// the boot-time port).
func (a *App) buildServer(ctx context.Context, static fs.FS) error {
	a.srv = server.New(a.database, a.stores, a.storedPort)
	a.wsHub = a.srv.WSHub()

	if a.local() {
		// Local deploy config: publicURL is the local address, HMAC key is
		// ephemeral (only needs to survive the registration window, not
		// across restarts).
		var hmacKey [32]byte
		if _, err := cryptorand.Read(hmacKey[:]); err != nil {
			return fmt.Errorf("generate local HMAC key: %w", err)
		}
		a.srv.SetDeployConfig(a.cfg.BrowserURL, hmacKey)
	} else {
		if err := a.wireAuth(ctx); err != nil {
			return err
		}
	}

	a.srv.SetStatic(static)
	return nil
}

// wireAuth wires the multi-mode-only auth stack: it validates the public
// URL, reads the org-creation toggle, builds the JWKS verifier (which
// blocks on the initial fetch, so GoTrue must be reachable), loads the
// session/cookie keys, and hands them all to the server. ctx is the boot
// context; the session reaper goroutine spawned inside SetAuthDeps
// inherits it and stops on shutdown.
func (a *App) wireAuth(ctx context.Context) error {
	// Validate TF_PUBLIC_URL up front. SetAuthDeps derives the
	// secureCookies flag from `HasPrefix(publicURL, "https://")` — an empty
	// or typo'd URL would silently land in the non-secure branch and emit
	// OAuth-state cookies without the Secure flag. Failing fast here is
	// much louder than the runtime cookie-flag drift.
	publicURL := os.Getenv("TF_PUBLIC_URL")
	if err := validateHTTPURL("TF_PUBLIC_URL", publicURL); err != nil {
		return err
	}

	// Org-creation toggle. Unset → creation allowed (right default for
	// hosted SaaS + unconfigured self-hosts); a locked-down self-host sets
	// TF_PREVENT_ORG_CREATION=true. A non-boolean value fails here so a
	// typo in .env surfaces loudly at boot.
	if err := runmode.InitOrgCreationFromEnv(); err != nil {
		return err
	}

	verifier, err := newVerifierWithRetry(
		ctx,
		os.Getenv("TF_GOTRUE_JWKS_URL"),
		os.Getenv("TF_GOTRUE_ISSUER"),
		"authenticated", // GoTrue's standard audience claim
	)
	if err != nil {
		return fmt.Errorf("build verifier: %w", err)
	}

	sessionKey, err := aead.LoadKeyFromEnv(sessions.EnvSessionEncryptionKey)
	if err != nil {
		return fmt.Errorf("load session encryption key: %w", err)
	}
	cookieKey, err := aead.LoadKeyFromEnv(sessions.EnvCookieSecret)
	if err != nil {
		return fmt.Errorf("load cookie secret: %w", err)
	}

	sessionStore := sessions.NewStore(a.database, sessionKey)
	if err := a.srv.SetAuthDeps(
		ctx,
		verifier,
		sessionStore,
		os.Getenv("TF_GOTRUE_URL"),
		publicURL,
		cookieKey,
	); err != nil {
		return fmt.Errorf("wire auth deps: %w", err)
	}
	return nil
}

// newVerifierWithRetry builds the JWKS verifier, tolerating a GoTrue that
// is still coming up. verify.NewVerifier blocks on the initial JWKS fetch
// and fails fast on any error — including the "connection refused" of a
// GoTrue that hasn't bound its port yet. Behind docker-compose that race
// can't happen (depends_on: gotrue: service_healthy gates this boot), but
// Fly and a bare `docker run` have no such ordering, so a cold-start race
// would crash-loop the container. This mirrors entrypoint.sh's bounded
// retry around `migrate up` against a not-yet-ready Postgres: ride out the
// transient window, surface the real error if it never resolves. A
// cancelled ctx (SIGTERM during boot) aborts immediately.
func newVerifierWithRetry(ctx context.Context, jwksURL, issuer, audience string) (*verify.Verifier, error) {
	// Preserve fail-fast behavior for clear configuration errors.
	if jwksURL == "" || issuer == "" || audience == "" {
		return verify.NewVerifier(ctx, jwksURL, issuer, audience)
	}

	const (
		attempts = 30
		backoff  = 2 * time.Second
	)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		v, err := verify.NewVerifier(ctx, jwksURL, issuer, audience)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if attempt == attempts {
			break
		}
		log.Printf("[auth] JWKS verifier not ready (attempt %d/%d): %v; retrying in %s",
			attempt, attempts, err, backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

// validateHTTPURL parses raw and rejects anything that isn't an absolute
// http(s) URL with a host. Used for TF_PUBLIC_URL, which drives the OAuth
// redirect base and the Secure-cookie flag — an empty or scheme-less value
// would either crash on the redirect or silently disable Secure.
func validateHTTPURL(name, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: parse %q: %w", name, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: scheme must be http or https, got %q", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: missing host in %q", name, raw)
	}
	return nil
}
