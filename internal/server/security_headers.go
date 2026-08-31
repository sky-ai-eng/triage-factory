package server

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// withSecurityHeaders wraps next with the standard HTTP security
// headers. Applied at the outermost layer in ListenAndServe so every
// response — static asset, /api/*, /auth/v1/* proxy — gets them.
//
// Per-deployment behavior:
//   - HSTS is only emitted when the deployment is HTTPS
//     (publicURL starts with https://). Setting it on a plain-HTTP
//     localhost deployment would teach the browser to always-HTTPS
//     localhost, breaking neighboring projects on the same port.
//   - CSP includes hash directives for each inline <script> block
//     embedded in index.html, computed at SetStatic time. A change
//     to the inline bootstrap regenerates the hash on next boot;
//     no frontend code change is needed for CSP to stay tight.
//
// Operator override hook: none. If a single endpoint legitimately
// needs different headers, register it as `http.Handler` that
// overrides w.Header() before the wrapping middleware passes through —
// but most endpoints should accept the defaults.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	// Resolved once at wrap time. The CSP must be re-resolved on each
	// request via a closure call: SetStatic may run after New() (and
	// therefore after this wrap), so the inline-script hashes only
	// exist later. We compose the CSP string fresh per request to
	// pick up that late-bound state.
	deployHTTPS := s.deployCfg != nil && s.deployCfg.secureCookies

	// HSTS is resolved once here too — its inputs (deployHTTPS, the
	// TF_HSTS_PRELOAD env) are fixed for the process lifetime.
	hstsValue := buildHSTS(deployHTTPS)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		if hstsValue != "" {
			h.Set("Strict-Transport-Security", hstsValue)
		}
		h.Set("Content-Security-Policy", s.buildCSP())
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Block browser APIs we never use. interest-cohort opts out of
		// FLoC tracking; the rest disable physical-world sensors that
		// have no role in a triage tool.
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")

		next.ServeHTTP(w, r)
	})
}

// buildHSTS returns the Strict-Transport-Security header value, or "" when no
// HSTS should be emitted. HSTS is meaningful only under HTTPS: setting it on a
// plain-HTTP localhost deployment would teach the browser to always-HTTPS
// localhost and break neighboring projects on the same port. So a non-HTTPS
// deployment gets "" (no header) regardless of the preload toggle.
//
// Under HTTPS the base is a 1-year max-age with includeSubDomains. The optional
// "; preload" suffix is gated behind TF_HSTS_PRELOAD because preload is a
// heavyweight, slow-to-reverse commitment: submitting a domain to the browser
// preload list hardcodes "HTTPS-only, including every subdomain" into shipped
// browser binaries for that whole registrable domain. That's the right call for
// a single SaaS deployment on a domain its operator controls, but a footgun to
// default-on in the self-host image, where operators run TF on a subdomain of a
// larger domain they may not be entitled to commit org-wide. So it's strictly
// opt-in: the SaaS deploy sets TF_HSTS_PRELOAD=true (and submits its domain),
// self-hosters leave it unset. See TFAC-409 item 4.
func buildHSTS(deployHTTPS bool) string {
	if !deployHTTPS {
		return ""
	}
	v := "max-age=31536000; includeSubDomains"
	if hstsPreloadEnabled() {
		v += "; preload"
	}
	return v
}

// hstsPreloadEnabled parses TF_HSTS_PRELOAD. Unset/empty → false (the safe
// default). Accepts the usual boolean spellings; an unparseable value logs a
// warning and falls back to false rather than failing boot — preload is a
// hardening nicety, not load-bearing, so a typo shouldn't keep the server down.
func hstsPreloadEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("TF_HSTS_PRELOAD"))
	switch strings.ToLower(raw) {
	case "", "0", "false", "f", "no", "n", "off":
		return false
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		serverLog.Warn("unrecognized TF_HSTS_PRELOAD value; treating as off", "value", raw)
		return false
	}
}

// buildCSP composes the Content-Security-Policy string. Tight by
// default — `default-src 'none'` denies everything, then we
// allow-list the specific source kinds the SPA actually loads.
//
// img-src stays `'self' data:` even with avatar rendering live: OAuth-CDN
// avatars (avatars.githubusercontent.com, the org's Jira host, ...) are served
// same-origin through the /api/avatars/{id} proxy (TFAC-480), so no external
// host is punched into the policy. See the img-src note below.
func (s *Server) buildCSP() string {
	// Each script-src hash is a `'sha256-<base64>'` entry. Joined
	// after `'self'` so external bundles are still gated by origin
	// and only the known-good inlines are allowed.
	scriptSources := []string{"'self'"}
	for _, h := range s.inlineScriptHashes {
		scriptSources = append(scriptSources, fmt.Sprintf("'sha256-%s'", h))
	}

	// img-src: 'self' covers the favicon, the app's own assets, AND user/member
	// avatars — those render through the same-origin /api/avatars/{id} proxy
	// (avatars_handler.go), which fetches + caches the OAuth-CDN image server-
	// side. `data:` covers base64-embedded SVGs that some libs emit.
	//
	// This is option (a) of the original TFAC-480 choice (proxy vs. allowlist
	// the provider CDNs vs. an admin-configurable host): it keeps img-src to
	// `'self'` with no external hosts, and avoids composing the per-deployment
	// Jira avatar host into the CSP from org settings. Don't widen img-src to
	// provider CDNs — route new avatar surfaces through the proxy instead.
	imgSources := []string{"'self'", "data:"}

	directives := []string{
		// `default-src 'none'` denies everything; the rest opt back in.
		"default-src 'none'",
		"script-src " + strings.Join(scriptSources, " "),
		// 'unsafe-inline' for style-src covers React's inline `style=`
		// attribute pattern (used by many component libs). Inline
		// `<style>` blocks would also be allowed; those don't carry
		// the XSS risk that script-src 'unsafe-inline' would.
		"style-src 'self' 'unsafe-inline'",
		"img-src " + strings.Join(imgSources, " "),
		"font-src 'self'",
		// connect-src governs fetch() / XHR / WebSocket. 'self'
		// covers our REST API and /api/ws.
		"connect-src 'self'",
		// frame-ancestors does what X-Frame-Options does (DENY) but
		// is the standards-track replacement. Keep both for older
		// browsers that don't honor frame-ancestors.
		"frame-ancestors 'none'",
		// Prevent <base href="evil"> injection from re-pointing
		// relative URLs.
		"base-uri 'self'",
		// Prevent <form action="evil"> from redirecting POSTs.
		"form-action 'self'",
	}
	return strings.Join(directives, "; ")
}

// computeInlineScriptHashes parses the served index.html, extracts each
// inline <script> block, and computes the SHA-256 hash + base64-encodes
// it as required by CSP's `'sha256-...'` directive.
//
// Called from SetStatic — by then we have the embedded FS to read from.
// Returns the hashes; the caller stores them on the Server for reuse.
// Errors are non-fatal (returns empty slice); CSP then enforces a
// strict script-src 'self' and the inline bootstrap won't run — visible
// in the browser console immediately, which is preferable to silent
// permissive fallback.
func computeInlineScriptHashes(static fs.FS) ([]string, error) {
	if static == nil {
		return nil, nil
	}
	data, err := fs.ReadFile(static, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}

	// Match <script ...>BODY</script> where ... doesn't contain
	// `src=` (those reference external files, not inline).
	// (?s) = . matches newlines. (?i) = case-insensitive.
	re := regexp.MustCompile(`(?is)<script(?:\s+[^>]*)?>([\s\S]*?)</script>`)
	matches := re.FindAllSubmatch(data, -1)

	var hashes []string
	for _, m := range matches {
		full, body := string(m[0]), m[1]
		// Skip scripts with src=... — those are external file refs.
		if strings.Contains(strings.ToLower(full[:strings.Index(full, ">")+1]), "src=") {
			continue
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			continue
		}
		sum := sha256.Sum256(body)
		hashes = append(hashes, base64.StdEncoding.EncodeToString(sum[:]))
	}
	return hashes, nil
}
