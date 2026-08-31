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

// withSecurityHeaders wraps next with the standard HTTP security headers.
// Applied at the outermost layer in ListenAndServe so every response —
// static asset, /api/*, /auth/v1/* proxy — carries them.
//
// HSTS is emitted only under HTTPS (buildHSTS). The CSP carries a hash
// directive per inline <script> in index.html, so changing the inline
// bootstrap re-tightens the policy on the next boot with no frontend work.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	// The CSP is composed per request, not once here: SetStatic may run after
	// New (and so after this wrap), and the inline-script hashes exist only
	// from that point on.
	deployHTTPS := s.deployCfg != nil && s.deployCfg.secureCookies

	// HSTS resolves once — its inputs are fixed for the process lifetime.
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
// Under HTTPS the base is a 1-year max-age with includeSubDomains. The
// "; preload" suffix is gated behind TF_HSTS_PRELOAD because preload is
// heavyweight and slow to reverse: submitting a domain to the browser preload
// list hardcodes "HTTPS-only, including every subdomain" into shipped browser
// binaries for that whole registrable domain. That is right for a SaaS
// deployment on a domain its operator controls, and a footgun to default-on in
// the self-host image, where TF often runs on a subdomain of a larger domain
// the operator may not be entitled to commit org-wide. So it is strictly
// opt-in: the SaaS deploy sets TF_HSTS_PRELOAD=true and submits its domain;
// self-hosters leave it unset.
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
// hardening nicety, not load-bearing, so a typo shouldn't keep the server
// down.
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

// buildCSP composes the Content-Security-Policy. Tight by default —
// `default-src 'none'` denies everything, then the specific source kinds the
// SPA loads are allowed back.
func (s *Server) buildCSP() string {
	// Each hash joins AFTER `'self'`, so external bundles stay gated by origin
	// and only the known-good inlines are allowed.
	scriptSources := []string{"'self'"}
	for _, h := range s.inlineScriptHashes {
		scriptSources = append(scriptSources, fmt.Sprintf("'sha256-%s'", h))
	}

	// img-src: 'self' covers the favicon, the app's own assets, AND
	// user/member avatars — those render through the same-origin
	// /api/avatars/{id} proxy (avatars_handler.go), which fetches + caches the
	// OAuth-CDN image server-side. `data:` covers base64-embedded SVGs that
	// some libs emit.
	//
	// Do NOT widen img-src to provider CDNs. Holding it to `'self'` is what
	// keeps the per-deployment Jira avatar host out of the policy — otherwise
	// the CSP would have to be composed from org settings. Route a new avatar
	// surface through the proxy instead.
	imgSources := []string{"'self'", "data:"}

	directives := []string{
		// `default-src 'none'` denies everything; the rest opt back in.
		"default-src 'none'",
		"script-src " + strings.Join(scriptSources, " "),
		// 'unsafe-inline' covers React's inline `style=` attribute pattern,
		// which many component libs rely on. It also admits inline `<style>`
		// blocks, which don't carry the XSS risk script-src 'unsafe-inline'
		// would.
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

// computeInlineScriptHashes extracts each inline <script> block from the
// served index.html and returns the base64 SHA-256 that CSP's `'sha256-...'`
// directive takes. Called from SetStatic, once the embedded FS exists.
//
// Errors are non-fatal (empty slice): the CSP then enforces a bare script-src
// 'self' and the inline bootstrap won't run — visible in the browser console
// immediately, which beats a silent permissive fallback.
func computeInlineScriptHashes(static fs.FS) ([]string, error) {
	if static == nil {
		return nil, nil
	}
	data, err := fs.ReadFile(static, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}

	// (?is): `.` matches newlines, and the tag matches case-insensitively.
	re := regexp.MustCompile(`(?is)<script(?:\s+[^>]*)?>([\s\S]*?)</script>`)
	matches := re.FindAllSubmatch(data, -1)

	var hashes []string
	for _, m := range matches {
		full, body := string(m[0]), m[1]
		// A src= attribute means an external file, not an inline block.
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
