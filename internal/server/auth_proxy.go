package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// newGotrueProxy builds the /auth/v1/* reverse proxy. Path is stripped
// of the /auth/v1 prefix before being forwarded — gotrue sees the
// request at its own root (e.g. /authorize, /.well-known/jwks.json).
//
// Why in-binary instead of Caddy/Nginx: one less moving part for the
// self-host operator. D13 (SKY-256) can refactor to a fronting Caddy
// once the container packaging lands. For v1, the TF binary fronts
// the only HTTPS-bearing endpoint anyway.
//
// We pin the Host header to the gotrue upstream's host rather than
// passing the client's Host through. GoTrue's config validates
// API_EXTERNAL_URL against incoming Host on some flows; rewriting
// here is the safe default and matches the GoTrue self-host docs.
func newGotrueProxy(rawURL string) (http.Handler, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", rawURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("gotrue url missing scheme or host: %q", rawURL)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL points the outgoing request at the target's
			// scheme + host (preserving query string). We strip the
			// /auth/v1 prefix from the outgoing path so GoTrue sees
			// requests at its own root (/authorize, /token,
			// /.well-known/jwks.json).
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.Out.URL.Path, "/auth/v1")
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			if pr.Out.URL.RawPath != "" {
				pr.Out.URL.RawPath = strings.TrimPrefix(pr.Out.URL.RawPath, "/auth/v1")
			}
			// Pin Host to the upstream — GoTrue validates some flows
			// against the API_EXTERNAL_URL host, but our compose wires
			// it to the public host, so passing the gotrue:9999 host
			// matches the internal API expectations.
			pr.Out.Host = target.Host
		},
	}
	return denyGotrueAdmin(proxy), nil
}

// denyGotrueAdmin wraps the GoTrue proxy to refuse /auth/v1/admin/* — GoTrue's
// service-role user-management API (create/delete/list users, etc.). The
// browser SPA never calls it; only a server-side holder of the service-role
// JWT legitimately would, and that path doesn't traverse this public proxy.
// Denying the subpath here means a future GoTrue misconfig or a leaked
// service-role key isn't directly reachable from the internet — defense in
// depth on top of GoTrue's own service-role gate (TFAC-409 item 3).
//
// Responds 404 (not 403) so the deny is indistinguishable from "no such route"
// — we don't confirm to a prober that an admin surface exists behind the proxy.
// The match is on the raw request path (before the proxy strips the /auth/v1
// prefix): an exact "/auth/v1/admin" and anything under "/auth/v1/admin/".
func denyGotrueAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/v1/admin" || strings.HasPrefix(r.URL.Path, "/auth/v1/admin/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
