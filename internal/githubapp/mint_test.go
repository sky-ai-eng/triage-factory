package githubapp_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// newTestKey generates a fresh RSA-2048 keypair for each test. 2048 is
// the GitHub-App-supported minimum; using 1024 would generate faster
// but doesn't exercise the production-equivalent code path through
// jwt-go's RS256 implementation.
func newTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return key
}

// pkcs1PEM encodes a key in the PKCS#1 PEM format that GitHub Apps
// download by default ("BEGIN RSA PRIVATE KEY").
func pkcs1PEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// pkcs8PEM encodes a key in PKCS#8 PEM format ("BEGIN PRIVATE KEY").
// Some operators convert their App's PKCS#1 key to PKCS#8 via openssl
// before storing; ParsePrivateKey must accept both.
func pkcs8PEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

// TestParsePrivateKey_AcceptsPKCS1AndPKCS8 pins that both PEM formats
// GitHub App operators encounter parse successfully.
func TestParsePrivateKey_AcceptsPKCS1AndPKCS8(t *testing.T) {
	key := newTestKey(t)

	for _, c := range []struct {
		name string
		pem  []byte
	}{
		{"pkcs1", pkcs1PEM(t, key)},
		{"pkcs8", pkcs8PEM(t, key)},
	} {
		t.Run(c.name, func(t *testing.T) {
			parsed, err := githubapp.ParsePrivateKey(c.pem)
			if err != nil {
				t.Fatalf("ParsePrivateKey: %v", err)
			}
			if parsed.N.Cmp(key.N) != 0 {
				t.Error("parsed key modulus does not match input")
			}
		})
	}
}

// TestParsePrivateKey_RejectsInvalidInput pins that empty / malformed
// PEM bytes fail loudly rather than returning a nil key + nil error.
func TestParsePrivateKey_RejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		pem  []byte
	}{
		{"empty", []byte{}},
		{"not_pem", []byte("not a PEM block")},
		{"truncated", []byte("-----BEGIN RSA PRIVATE KEY-----\nincomplete")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := githubapp.ParsePrivateKey(c.pem); err == nil {
				t.Error("ParsePrivateKey returned nil err; want validation error")
			}
		})
	}
}

// TestNewMinter_RejectsInvalidConfig pins eager construction-time
// validation so a missing key, zero AppID, or malformed APIBase fails
// at boot rather than on the first mint attempt.
//
// The APIBase cases mirror llmproxy / gitproxy validation: a base
// without scheme or host, with query or fragment, or http to a non-
// loopback host are all rejected. The JWT we send to APIBase is a
// Bearer-class secret and must not cross cleartext on a real network.
func TestNewMinter_RejectsInvalidConfig(t *testing.T) {
	validKey := newTestKey(t)
	cases := []struct {
		name string
		cfg  githubapp.Config
	}{
		{"nil_key", githubapp.Config{PrivateKey: nil, AppID: 1}},
		{"zero_app_id", githubapp.Config{PrivateKey: validKey, AppID: 0}},
		{"negative_app_id", githubapp.Config{PrivateKey: validKey, AppID: -1}},
		{"apibase_no_scheme", githubapp.Config{PrivateKey: validKey, AppID: 1, APIBase: "api.github.com"}},
		{"apibase_with_query", githubapp.Config{PrivateKey: validKey, AppID: 1, APIBase: "https://api.github.com?x=1"}},
		{"apibase_with_fragment", githubapp.Config{PrivateKey: validKey, AppID: 1, APIBase: "https://api.github.com#frag"}},
		{"apibase_http_non_loopback", githubapp.Config{PrivateKey: validKey, AppID: 1, APIBase: "http://api.github.com"}},
		{"apibase_unparseable", githubapp.Config{PrivateKey: validKey, AppID: 1, APIBase: "https://%zz"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := githubapp.NewMinter(c.cfg); err == nil {
				t.Errorf("NewMinter(%+v) = nil err; want validation error", c.cfg)
			}
		})
	}
}

// TestNewMinter_AcceptsValidAPIBase pins the shapes a real caller
// would pass. The GHE path-on-base case ("https://<ghe>/api/v3") is
// load-bearing: rejecting it would block self-hosted GitHub Enterprise
// integration.
func TestNewMinter_AcceptsValidAPIBase(t *testing.T) {
	validKey := newTestKey(t)
	cases := []string{
		"https://api.github.com",
		"https://api.github.com/",
		"https://ghe.example.com/api/v3", // GHE — non-empty path is allowed
		"http://127.0.0.1:8080",          // loopback http is permitted for httptest
		"http://127.0.0.1",               // loopback http without explicit port
		"http://[::1]:8080",              // IPv6 loopback for httptest
		"http://[::1]",                   // IPv6 loopback without explicit port (bracket-handling regression)
	}
	for _, base := range cases {
		t.Run(base, func(t *testing.T) {
			if _, err := githubapp.NewMinter(githubapp.Config{
				PrivateKey: validKey,
				AppID:      1,
				APIBase:    base,
			}); err != nil {
				t.Errorf("NewMinter with valid APIBase %q rejected: %v", base, err)
			}
		})
	}
}

// TestNewMinter_TrimsTrailingSlashOnAPIBase pins ergonomics: callers
// pass "https://api.github.com/" or "https://api.github.com" and either
// works without producing "//app/installations/..." in the request URL.
func TestNewMinter_TrimsTrailingSlashOnAPIBase(t *testing.T) {
	called := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if strings.Contains(r.URL.Path, "//") {
			t.Errorf("request path contains //: %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_x",
			"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: newTestKey(t),
		AppID:      42,
		APIBase:    srv.URL + "/",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	if _, err := m.MintInstallationToken(context.Background(), 999); err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}
	if called.Load() != 1 {
		t.Errorf("upstream hit count = %d, want 1", called.Load())
	}
}

// TestAppJWT_ShapeAndSignature pins the claim set + RS256 signature
// against the public key. The shape (iss = appID, iat backdated, exp
// ≤ 10min, alg = RS256) is exactly what GitHub validates, so this is
// the asserts-the-contract-the-server-checks test.
func TestAppJWT_ShapeAndSignature(t *testing.T) {
	key := newTestKey(t)
	const appID int64 = 12345
	m, err := githubapp.NewMinter(githubapp.Config{PrivateKey: key, AppID: appID})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	before := time.Now()
	signed, err := m.AppJWT()
	if err != nil {
		t.Fatalf("AppJWT: %v", err)
	}
	after := time.Now()

	tok, err := jwt.Parse(signed, func(t *jwt.Token) (interface{}, error) {
		if t.Method.Alg() != "RS256" {
			return nil, fmt.Errorf("unexpected alg %q", t.Method.Alg())
		}
		return &key.PublicKey, nil
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		t.Fatalf("parse jwt: %v", err)
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims wrong type: %T", tok.Claims)
	}

	// iss must be appID (as a number).
	iss, _ := claims["iss"].(float64)
	if int64(iss) != appID {
		t.Errorf("iss = %v, want %d", iss, appID)
	}

	// iat must be backdated (≤ before) — proves the skew compensation
	// is happening rather than just using time.Now() raw.
	iat, _ := claims["iat"].(float64)
	if int64(iat) > before.Unix() {
		t.Errorf("iat = %v not backdated (before = %d); skew compensation missing", iat, before.Unix())
	}

	// exp must be within the 10-minute window GitHub enforces.
	exp, _ := claims["exp"].(float64)
	maxExp := after.Add(10 * time.Minute).Unix()
	if int64(exp) > maxExp {
		t.Errorf("exp = %v exceeds GitHub's 10-minute cap (max %d)", exp, maxExp)
	}
	if int64(exp) <= after.Unix() {
		t.Errorf("exp = %v is in the past at signing time", exp)
	}
}

// TestMintInstallationToken_HappyPath pins the wire shape of the mint
// request and that the response is parsed into the Token return value.
// Validates exactly what the real GitHub endpoint expects: POST,
// Authorization: Bearer <jwt>, the right path with the right ID.
func TestMintInstallationToken_HappyPath(t *testing.T) {
	const (
		installationID int64 = 7654321
		appID          int64 = 12345
		mintedTok            = "ghs_TESTTOKENABCDEF1234567890"
	)
	key := newTestKey(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	var (
		seenMethod string
		seenPath   string
		seenAccept string
		seenAuth   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenAccept = r.Header.Get("Accept")
		seenAuth = r.Header.Get("Authorization")

		// Verify the Bearer JWT signature with the App's public key.
		// This is the GitHub-equivalent validation — if our JWT shape
		// is wrong, this fails like the real API would.
		bearer := strings.TrimPrefix(seenAuth, "Bearer ")
		_, err := jwt.Parse(bearer, func(t *jwt.Token) (interface{}, error) {
			return &key.PublicKey, nil
		}, jwt.WithValidMethods([]string{"RS256"}))
		if err != nil {
			http.Error(w, fmt.Sprintf("bad jwt: %v", err), http.StatusUnauthorized)
			return
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      mintedTok,
			"expires_at": expiresAt.Format(time.RFC3339),
			"permissions": map[string]string{
				"contents": "write",
			},
		})
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	tok, err := m.MintInstallationToken(context.Background(), installationID)
	if err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}

	if seenMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", seenMethod)
	}
	wantPath := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if seenPath != wantPath {
		t.Errorf("path = %q, want %q", seenPath, wantPath)
	}
	if seenAccept != "application/vnd.github+json" {
		t.Errorf("accept = %q, want application/vnd.github+json", seenAccept)
	}
	if !strings.HasPrefix(seenAuth, "Bearer ") {
		t.Errorf("authorization = %q; want \"Bearer <jwt>\"", seenAuth)
	}
	if tok.Value != mintedTok {
		t.Errorf("Token.Value = %q, want %q", tok.Value, mintedTok)
	}
	if !tok.ExpiresAt.Equal(expiresAt) {
		t.Errorf("Token.ExpiresAt = %v, want %v", tok.ExpiresAt, expiresAt)
	}
}

// TestMintScopedInstallationToken pins that the scoped mint narrows the token:
// the request body carries the requested repositories, and permissions only
// when non-empty (an empty/nil map omits the field, yielding the installation's
// full permissions on the single repo). Same response handling as the unscoped
// path, so a 201 still parses token + expiry.
func TestMintScopedInstallationToken(t *testing.T) {
	const (
		installationID int64 = 7654321
		appID          int64 = 12345
		mintedTok            = "ghs_SCOPED_TOKEN"
	)
	key := newTestKey(t)
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	cases := []struct {
		name        string
		repos       []string
		permissions map[string]string
		wantRepos   []any
		wantPerms   map[string]any // nil → the permissions field must be absent
	}{
		{
			name:        "repo + contents:write (the git proxy's Layer 1)",
			repos:       []string{"core"},
			permissions: map[string]string{"contents": "write"},
			wantRepos:   []any{"core"},
			wantPerms:   map[string]any{"contents": "write"},
		},
		{
			name:      "repo only, nil permissions → full-install perms on the repo",
			repos:     []string{"core"},
			wantRepos: []any{"core"},
			wantPerms: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"token":      mintedTok,
					"expires_at": expiresAt.Format(time.RFC3339),
				})
			}))
			defer srv.Close()

			m, err := githubapp.NewMinter(githubapp.Config{
				PrivateKey: key, AppID: appID, APIBase: srv.URL, HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatalf("NewMinter: %v", err)
			}

			tok, err := m.MintScopedInstallationToken(context.Background(), installationID, c.repos, c.permissions)
			if err != nil {
				t.Fatalf("MintScopedInstallationToken: %v", err)
			}
			if tok.Value != mintedTok {
				t.Errorf("Token.Value = %q, want %q", tok.Value, mintedTok)
			}

			if repos, _ := gotBody["repositories"].([]any); !reflect.DeepEqual(repos, c.wantRepos) {
				t.Errorf("repositories = %v, want %v", repos, c.wantRepos)
			}
			perms, hasPerms := gotBody["permissions"]
			if c.wantPerms == nil {
				if hasPerms {
					t.Errorf("permissions present = %v, want the field omitted", perms)
				}
			} else if !reflect.DeepEqual(perms, any(c.wantPerms)) {
				t.Errorf("permissions = %v, want %v", perms, c.wantPerms)
			}
		})
	}
}

// TestMintInstallationToken_HTTPError pins that a non-201 response
// surfaces as an error containing both the status and (truncated)
// body, so debugging a misconfigured App / installation ID doesn't
// require packet captures.
func TestMintInstallationToken_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials","documentation_url":"https://docs.github.com/"}`))
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: newTestKey(t),
		AppID:      1,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	_, err = m.MintInstallationToken(context.Background(), 1)
	if err == nil {
		t.Fatal("MintInstallationToken returned nil err on 401; want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error missing status code: %q", msg)
	}
	if !strings.Contains(msg, "Bad credentials") {
		t.Errorf("error missing body text: %q", msg)
	}
}

// TestMintInstallationToken_RejectsBadInstallationID pins eager
// argument validation rather than waiting for GitHub to 404.
func TestMintInstallationToken_RejectsBadInstallationID(t *testing.T) {
	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: newTestKey(t),
		AppID:      1,
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	for _, id := range []int64{0, -1} {
		if _, err := m.MintInstallationToken(context.Background(), id); err == nil {
			t.Errorf("MintInstallationToken(%d) = nil err; want validation", id)
		}
	}
}

// TestMintInstallationToken_RejectsMalformedResponse pins that a 201
// with no token field is treated as a hard error rather than silently
// returning an empty Token (which would then surface as a confusing
// 401 from the next git call against the proxy).
func TestMintInstallationToken_RejectsMalformedResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing_token", `{"expires_at":"2030-01-01T00:00:00Z"}`},
		{"missing_expires", `{"token":"ghs_x"}`},
		{"not_json", `not json at all`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			m, err := githubapp.NewMinter(githubapp.Config{
				PrivateKey: newTestKey(t),
				AppID:      1,
				APIBase:    srv.URL,
				HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatalf("NewMinter: %v", err)
			}
			if _, err := m.MintInstallationToken(context.Background(), 1); err == nil {
				t.Errorf("MintInstallationToken accepted body %q; want validation error", c.body)
			}
		})
	}
}

// TestMintInstallationToken_RespectsContextCancellation pins that a
// cancelled context aborts the mint call rather than blocking on the
// HTTP roundtrip. Important so a run cancellation actually propagates
// to in-flight credential operations.
func TestMintInstallationToken_RespectsContextCancellation(t *testing.T) {
	// Upstream that blocks until the client's context is cancelled —
	// the natural stop signal here is r.Context().Done(), which fires
	// when the test cancels the outer context below. No separate
	// channel is needed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: newTestKey(t),
		AppID:      1,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := m.MintInstallationToken(ctx, 1); err == nil {
		t.Error("MintInstallationToken returned nil err on cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Mint took %v after cancel; want quick abort", elapsed)
	}
}
