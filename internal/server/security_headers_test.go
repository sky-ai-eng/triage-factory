package server

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// mustReadDistIndex reads frontend/dist/index.html relative to the
// repo root. Used by the real-index hash test to catch divergence
// between the hasher and the built SPA. Skips (not fails) if the
// dist hasn't been built — fresh checkouts don't have it.
func mustReadDistIndex(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	// internal/server/security_headers_test.go → repo root is ../..
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	indexPath := filepath.Join(repoRoot, "frontend", "dist", "index.html")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Skipf("frontend/dist/index.html not present (run `npm run build`): %v", err)
	}
	return string(data)
}

// TestComputeInlineScriptHashes_BootstrapHTML pins the parser against a
// minimal index.html shape that mirrors the real one — inline theme
// bootstrap in <head>, external bundle reference, no other inline blocks.
func TestComputeInlineScriptHashes_BootstrapHTML(t *testing.T) {
	inline := `(function(){console.log("theme");})()`
	html := `<!doctype html>
<html>
<head>
  <script>` + inline + `</script>
  <script type="module" src="/assets/index.js"></script>
</head>
<body><div id="root"></div></body>
</html>`
	fs := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(html)}}

	got, err := computeInlineScriptHashes(fs)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d hashes, want 1 (inline-only; src=... blocks must be skipped)", len(got))
	}

	sum := sha256.Sum256([]byte(inline))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got[0] != want {
		t.Errorf("hash mismatch:\n got %s\nwant %s", got[0], want)
	}
}

// TestComputeInlineScriptHashes_RealIndex confirms the hasher works
// against the actual built SPA — catches frontend build changes that
// would invalidate the CSP allowlist.
func TestComputeInlineScriptHashes_RealIndex(t *testing.T) {
	// Read from disk rather than embed: the test package can't easily
	// import the parent package's embed.FS, and we just need to prove
	// the hasher finds the bootstrap.
	html := mustReadDistIndex(t)
	if !strings.Contains(html, "<script>") {
		t.Skip("no inline scripts in built index.html — nothing to hash")
	}
	fs := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(html)}}

	hashes, err := computeInlineScriptHashes(fs)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(hashes) == 0 {
		t.Fatal("found zero hashes, but the HTML contains <script> blocks")
	}
	for i, h := range hashes {
		// Plausibility: SHA-256 base64 std-encoded is 44 chars.
		if len(h) != 44 {
			t.Errorf("hash[%d] = %q (len=%d), want 44 chars base64-std", i, h, len(h))
		}
	}
}

// TestComputeInlineScriptHashes_NilFS — defensive: callers may pass nil
// before SetStatic. Should return (nil, nil) without panic.
func TestComputeInlineScriptHashes_NilFS(t *testing.T) {
	got, err := computeInlineScriptHashes(nil)
	if err != nil {
		t.Errorf("nil fs returned err: %v", err)
	}
	if got != nil {
		t.Errorf("nil fs returned %v, want nil", got)
	}
}

// TestSecurityHeaders_StandardSet asserts each header lands on a 200
// response. The CSP value is checked for structure (contains the
// expected directives) rather than exact string match — that allows
// CSP tuning without rewriting the test.
func TestSecurityHeaders_StandardSet(t *testing.T) {
	s := &Server{
		// deployCfg with secureCookies=false → no HSTS, exercised below
		deployCfg:          &deployConfig{secureCookies: false},
		inlineScriptHashes: []string{"AAAA"}, // fake hash to confirm it's interpolated
	}
	handler := s.withSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	want := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if rec.Header().Get("Permissions-Policy") == "" {
		t.Error("Permissions-Policy missing")
	}

	// HSTS off in HTTP mode.
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS leaked in HTTP mode: %q", got)
	}

	csp := rec.Header().Get("Content-Security-Policy")
	for _, needle := range []string{
		"default-src 'none'",
		"script-src 'self' 'sha256-AAAA'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	} {
		if !strings.Contains(csp, needle) {
			t.Errorf("CSP missing %q\n  full: %s", needle, csp)
		}
	}
}

// TestSecurityHeaders_HandlerCanOverrideCSP pins the override-hook
// contract the GitHub App launch page relies on: the wrapper sets the
// global CSP, THEN calls next, so a handler that re-Sets
// Content-Security-Policy on its own response wins. Regression guard for
// the bounce-page per-response CSP.
func TestSecurityHeaders_HandlerCanOverrideCSP(t *testing.T) {
	s := &Server{deployCfg: &deployConfig{}}
	const override = "default-src 'none'; form-action https://github.com; base-uri 'none'"
	handler := s.withSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", override)
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/orgs/x/github-app/register/launch", nil))

	if got := rec.Header().Get("Content-Security-Policy"); got != override {
		t.Errorf("handler CSP override didn't win:\n got %q\nwant %q", got, override)
	}
}

// TestSecurityHeaders_HSTSOnlyInHTTPS — HSTS must not be emitted in
// local/test deployments (publicURL is http://), otherwise the browser
// caches "always-HTTPS this host" and breaks neighbor projects on the
// same port.
func TestSecurityHeaders_HSTSOnlyInHTTPS(t *testing.T) {
	cases := []struct {
		name    string
		secure  bool
		wantSet bool
	}{
		{"http deployment", false, false},
		{"https deployment", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{deployCfg: &deployConfig{secureCookies: tc.secure}}
			handler := s.withSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
			got := rec.Header().Get("Strict-Transport-Security")
			if tc.wantSet && got == "" {
				t.Error("HTTPS deployment: HSTS missing")
			}
			if !tc.wantSet && got != "" {
				t.Errorf("HTTP deployment: HSTS leaked = %q", got)
			}
		})
	}
}

// TestSecurityHeaders_NilDeployCfg — edge case where deployCfg is nil.
// The middleware must still emit the universal headers (X-Frame-Options
// etc.) and just skip HSTS.
func TestSecurityHeaders_NilDeployCfg(t *testing.T) {
	s := &Server{} // no deployCfg
	handler := s.withSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("X-Frame-Options missing in local mode")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("CSP missing in local mode")
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS leaked in local mode")
	}
}

// TestBuildHSTS pins the TFAC-409 item-4 contract: no header without HTTPS,
// base directive under HTTPS, and "; preload" appended ONLY when the explicit
// opt-in env is truthy. preload is never default-on (it's a near-irreversible
// domain-wide commitment) and never emitted on plain HTTP.
func TestBuildHSTS(t *testing.T) {
	const base = "max-age=31536000; includeSubDomains"
	cases := []struct {
		name        string
		deployHTTPS bool
		preloadEnv  string
		want        string
	}{
		{"no https no header", false, "", ""},
		{"no https ignores preload", false, "true", ""},
		{"https base no preload", true, "", base},
		{"https preload off", true, "false", base},
		{"https preload on", true, "true", base + "; preload"},
		{"https preload on alt spelling", true, "1", base + "; preload"},
		{"https preload typo falls back off", true, "yesss", base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TF_HSTS_PRELOAD", tc.preloadEnv)
			if got := buildHSTS(tc.deployHTTPS); got != tc.want {
				t.Errorf("buildHSTS(%v) with TF_HSTS_PRELOAD=%q = %q, want %q",
					tc.deployHTTPS, tc.preloadEnv, got, tc.want)
			}
		})
	}
}
