package app

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/apitokens"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// buildExecutorRuntime is the executor role's stand-in for buildServer: an
// executor serves no user HTTP, so there is no server.Server, no auth
// stack, and no embedded SPA. It still needs two things the server would
// otherwise have provided — a websocket hub for the spawner's run-status
// broadcasts (a standalone, client-less Hub, so those broadcasts are safe
// no-ops until TFAC-584 relays them cross-pod), and the deployment's
// public base URL for the {{RUN_URL}} prompt placeholder (executors take no
// inbound traffic, so this points at the control pod's URL; best-effort —
// an unset value just renders the placeholder empty).
func (a *App) buildExecutorRuntime() {
	a.wsHub = websocket.NewHub()
	a.deployPublicURL = os.Getenv("TF_PUBLIC_URL")
}

// buildServer constructs the HTTP server, wires mode-specific deployment
// identity (local HMAC key / multi-mode GoTrue auth stack), serves the
// embedded frontend, and exposes the websocket hub the rest of the graph
// broadcasts through. Must run after openStores (it needs the stores).
// Control/all only — an executor uses buildExecutorRuntime instead.
func (a *App) buildServer(ctx context.Context, static fs.FS) error {
	a.srv = server.New(a.database, a.stores)
	a.wsHub = a.srv.WSHub()
	// TFAC-573: main.Version isn't otherwise threaded into this package;
	// GET /readyz surfaces it so an operator can confirm a deploy landed.
	a.srv.SetVersion(a.cfg.Version)

	if a.local() {
		// Local deploy config: publicURL is the local address, HMAC key is
		// ephemeral (only needs to survive the registration window, not
		// across restarts).
		var hmacKey [32]byte
		if _, err := cryptorand.Read(hmacKey[:]); err != nil {
			return fmt.Errorf("generate local HMAC key: %w", err)
		}
		a.srv.SetDeployConfig(a.cfg.BrowserURL, hmacKey)
		a.deployPublicURL = a.cfg.BrowserURL
		// Trust the Vite dev server's origin too, so `cd frontend && pnpm
		// run dev` (port 5173, proxying /api to this backend) can make
		// mutating requests — see AllowDevFrontendOrigin's doc comment.
		a.srv.AllowDevFrontendOrigin()
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
	a.deployPublicURL = publicURL

	// Org-creation toggle. Unset → creation allowed (right default for
	// a SaaS deployment + unconfigured self-hosts); a locked-down self-host sets
	// TF_PREVENT_ORG_CREATION=true. A non-boolean value fails here so a
	// typo in .env surfaces loudly at boot.
	if err := runmode.InitOrgCreationFromEnv(); err != nil {
		return err
	}

	// Trusted-proxy / client-IP capture policy (TFAC-488). Parsed once here
	// so clientIP — which feeds sessions.ip_addr, the SOC2 auth audit log,
	// and the pre-auth per-IP rate limiter — extracts a non-spoofable client
	// IP. A malformed CIDR / non-boolean toggle fails boot loudly rather than
	// silently degrading a security-relevant default.
	if err := runmode.InitClientIPPolicyFromEnv(); err != nil {
		return err
	}
	// Loud warning when we capture IPs but have no trusted-proxy allowlist:
	// X-Forwarded-For is then ignored and every request is attributed to its
	// direct peer. Correct for a directly-exposed self-host (ignore the
	// line); a misconfiguration behind a load balancer / CDN, where it
	// collapses the per-IP rate limiter to a single global bucket and records
	// the LB's IP in the audit log instead of the client's.
	if runmode.CaptureClientIP() && !runmode.TrustedProxyConfigured() {
		authLog.Warn("TF_TRUSTED_PROXY_CIDR is unset: X-Forwarded-For is ignored and client IP is taken from the direct peer (RemoteAddr). Correct if TF is directly exposed; if it runs behind a load balancer or CDN, set TF_TRUSTED_PROXY_CIDR to the proxy egress CIDR(s) so the per-IP rate limiter and audit logs reflect the real client — and configure the edge to strip inbound X-Forwarded-For")
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
	// Same admin handle as the session store, for the same reason: a token
	// lookup runs before the request has any claims to install, so it cannot
	// ride RLS.
	tokenStore := apitokens.NewStore(a.database)
	if err := a.srv.SetAuthDeps(
		ctx,
		verifier,
		sessionStore,
		tokenStore,
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
		authLog.Warn("jwks verifier not ready; retrying",
			"attempt", attempt, "attempts", attempts, "error", err, "backoff", backoff)
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
