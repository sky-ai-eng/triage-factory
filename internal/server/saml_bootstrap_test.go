package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeGoTrue is a minimal stand-in for GoTrue's admin SSO API. It validates the
// service_role bearer token, records POSTs, enforces a one-provider-per-domain
// conflict, and serves GET/POST /admin/sso/providers in the shape TF reads.
type fakeGoTrue struct {
	srv    *httptest.Server
	secret string

	mu        sync.Mutex
	providers []fakeProvider
	postCount int
	lastBody  map[string]any
}

type fakeProvider struct {
	id      string
	domains []string
}

func newFakeGoTrue(t *testing.T, secret string) *fakeGoTrue {
	t.Helper()
	f := &fakeGoTrue{secret: secret}
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/sso/providers", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkAuth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			items := make([]map[string]any, 0, len(f.providers))
			for _, p := range f.providers {
				items = append(items, map[string]any{"id": p.id, "domains": domainObjs(p.domains)})
			}
			f.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"items": items})
		case http.MethodPost:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.postCount++
			f.lastBody = body
			f.mu.Unlock()

			domains := toStringSlice(body["domains"])
			if body["type"] != "saml" || strings.TrimSpace(asString(body["metadata_url"])) == "" || len(domains) == 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing type/metadata_url/domains"})
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, p := range f.providers {
				for _, ed := range p.domains {
					for _, nd := range domains {
						if strings.EqualFold(ed, nd) {
							writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "SAML provider for this domain already exists"})
							return
						}
					}
				}
			}
			id := uuid.NewString()
			f.providers = append(f.providers, fakeProvider{id: id, domains: domains})
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "domains": domainObjs(domains)})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoTrue) checkAuth(r *http.Request) bool {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	tok, err := jwt.Parse(raw, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", tok.Header["alg"])
		}
		return []byte(f.secret), nil
	})
	if err != nil || !tok.Valid {
		return false
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	return ok && claims["role"] == "service_role"
}

func (f *fakeGoTrue) posts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCount
}

func (f *fakeGoTrue) providerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.providers)
}

// --- helpers ---------------------------------------------------------------

func domainObjs(domains []string) []map[string]string {
	out := make([]map[string]string, 0, len(domains))
	for _, d := range domains {
		out = append(out, map[string]string{"domain": d})
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func toStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

const testJWTSecret = "test-gotrue-jwt-secret-deadbeefdeadbeefdeadbeefdeadbeef"

// --- registerSAMLProvider (pure logic) -------------------------------------

func TestRegisterSAMLProvider_FreshCreate(t *testing.T) {
	fake := newFakeGoTrue(t, testJWTSecret)
	cfg := samlBootstrapConfig{
		gotrueURL:   fake.srv.URL,
		jwtSecret:   testJWTSecret,
		metadataURL: "https://login.microsoftonline.com/tenant/federationmetadata.xml",
		domains:     []string{"acme.com"},
	}

	id, created, err := registerSAMLProvider(context.Background(), fake.srv.Client(), cfg)
	if err != nil {
		t.Fatalf("registerSAMLProvider: %v", err)
	}
	if !created {
		t.Errorf("created=false, want true on fresh registration")
	}
	if _, perr := uuid.Parse(id); perr != nil {
		t.Errorf("provider_id %q is not a UUID: %v", id, perr)
	}
	if fake.providerCount() != 1 {
		t.Errorf("GoTrue has %d providers, want 1", fake.providerCount())
	}

	// The POST body must match the spike-confirmed shape exactly.
	fake.mu.Lock()
	body := fake.lastBody
	fake.mu.Unlock()
	if body["type"] != "saml" {
		t.Errorf("POST body type=%v, want saml", body["type"])
	}
	if body["metadata_url"] != cfg.metadataURL {
		t.Errorf("POST body metadata_url=%v, want %q", body["metadata_url"], cfg.metadataURL)
	}
	if got := toStringSlice(body["domains"]); len(got) != 1 || got[0] != "acme.com" {
		t.Errorf("POST body domains=%v, want [acme.com]", body["domains"])
	}
}

func TestRegisterSAMLProvider_IdempotentReRun(t *testing.T) {
	fake := newFakeGoTrue(t, testJWTSecret)
	cfg := samlBootstrapConfig{
		gotrueURL:   fake.srv.URL,
		jwtSecret:   testJWTSecret,
		metadataURL: "https://login.microsoftonline.com/tenant/federationmetadata.xml",
		domains:     []string{"acme.com"},
	}

	id1, created1, err := registerSAMLProvider(context.Background(), fake.srv.Client(), cfg)
	if err != nil || !created1 {
		t.Fatalf("first register: id=%q created=%v err=%v", id1, created1, err)
	}

	id2, created2, err := registerSAMLProvider(context.Background(), fake.srv.Client(), cfg)
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if created2 {
		t.Errorf("created=true on re-run, want false (never-overwrite)")
	}
	if id2 != id1 {
		t.Errorf("provider_id changed across runs: %q -> %q", id1, id2)
	}
	if fake.providerCount() != 1 {
		t.Errorf("GoTrue has %d providers after re-run, want 1 (no duplicate)", fake.providerCount())
	}
	if fake.posts() != 1 {
		t.Errorf("POST hit %d times, want 1 (re-run must GET-then-skip)", fake.posts())
	}
}

// TestRegisterSAMLProvider_ConflictRace simulates a provider that the initial
// list doesn't surface but the POST 422-rejects (a concurrent boot created it):
// the re-resolve-by-domain path must recover the existing id idempotently.
func TestRegisterSAMLProvider_ConflictRace(t *testing.T) {
	const existingID = "11111111-2222-3333-4444-555555555555"
	var listCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/sso/providers", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listCalls++
			// First GET (pre-POST) hides the provider; later GETs reveal it.
			if listCalls == 1 {
				writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{
				{"id": existingID, "domains": domainObjs([]string{"acme.com"})},
			}})
		case http.MethodPost:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "SAML provider for this domain already exists"})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := samlBootstrapConfig{
		gotrueURL:   srv.URL,
		jwtSecret:   testJWTSecret,
		metadataURL: "https://idp/metadata.xml",
		domains:     []string{"acme.com"},
	}
	id, created, err := registerSAMLProvider(context.Background(), srv.Client(), cfg)
	if err != nil {
		t.Fatalf("registerSAMLProvider on conflict race: %v", err)
	}
	if created {
		t.Errorf("created=true, want false (lost the race, must reuse)")
	}
	if id != existingID {
		t.Errorf("provider_id=%q, want %q (re-resolved by domain)", id, existingID)
	}
}

// TestRegisterSAMLProvider_RealCreateErrorSurfaces verifies that a genuine
// create failure (e.g. GoTrue can't fetch the Entra metadata) with no existing
// provider for the domain surfaces the error rather than being swallowed.
func TestRegisterSAMLProvider_RealCreateErrorSurfaces(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/sso/providers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
			return
		}
		// 422 with no matching provider on the re-GET -> a real failure.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "could not fetch metadata"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := samlBootstrapConfig{
		gotrueURL:   srv.URL,
		jwtSecret:   testJWTSecret,
		metadataURL: "https://bad/metadata.xml",
		domains:     []string{"acme.com"},
	}
	if _, _, err := registerSAMLProvider(context.Background(), srv.Client(), cfg); err == nil {
		t.Fatal("expected a real create error to surface, got nil")
	}
}

func TestMintServiceRoleJWT(t *testing.T) {
	signed, err := mintServiceRoleJWT(testJWTSecret)
	if err != nil {
		t.Fatalf("mintServiceRoleJWT: %v", err)
	}
	tok, err := jwt.Parse(signed, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("alg=%v, want HMAC", tok.Header["alg"])
		}
		return []byte(testJWTSecret), nil
	})
	if err != nil {
		t.Fatalf("parse minted token: %v", err)
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("claims are not MapClaims")
	}
	if claims["role"] != "service_role" {
		t.Errorf("role=%v, want service_role", claims["role"])
	}
	if _, err := claims.GetExpirationTime(); err != nil {
		t.Errorf("token has no usable exp: %v", err)
	}
}

// --- RunSAMLProviderBootstrap (env + gating) -------------------------------

func TestRunSAMLProviderBootstrap_LocalModeNoOp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	// A GoTrue URL is wired, but local mode must short-circuit before any call.
	fake := newFakeGoTrue(t, testJWTSecret)
	t.Setenv(envSAMLMetadataURL, "https://idp/metadata.xml")
	t.Setenv(envSAMLDomains, "acme.com")
	t.Setenv(envGoTrueJWTSecret, testJWTSecret)

	s := &Server{authCfg: &authConfig{gotrueURL: fake.srv.URL}}
	if err := s.RunSAMLProviderBootstrap(context.Background()); err != nil {
		t.Fatalf("RunSAMLProviderBootstrap: %v", err)
	}
	if fake.posts() != 0 {
		t.Errorf("local mode hit GoTrue %d times, want 0", fake.posts())
	}
}

func TestRunSAMLProviderBootstrap_OptInNoOp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	fake := newFakeGoTrue(t, testJWTSecret)
	// No TF_SAML_ENTRA_METADATA_URL -> SSO not configured -> silent no-op.
	t.Setenv(envSAMLMetadataURL, "")
	t.Setenv(envSAMLDomains, "")
	t.Setenv(envGoTrueJWTSecret, testJWTSecret)

	s := &Server{authCfg: &authConfig{gotrueURL: fake.srv.URL}}
	if err := s.RunSAMLProviderBootstrap(context.Background()); err != nil {
		t.Fatalf("RunSAMLProviderBootstrap: %v", err)
	}
	if fake.posts() != 0 {
		t.Errorf("opt-in no-op hit GoTrue %d times, want 0", fake.posts())
	}
}

func TestRunSAMLProviderBootstrap_MissingJWTSecretNoOp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	fake := newFakeGoTrue(t, testJWTSecret)
	t.Setenv(envSAMLMetadataURL, "https://idp/metadata.xml")
	t.Setenv(envSAMLDomains, "acme.com")
	t.Setenv(envGoTrueJWTSecret, "") // can't authenticate -> warn + skip

	s := &Server{authCfg: &authConfig{gotrueURL: fake.srv.URL}}
	if err := s.RunSAMLProviderBootstrap(context.Background()); err != nil {
		t.Fatalf("RunSAMLProviderBootstrap: %v", err)
	}
	if fake.posts() != 0 {
		t.Errorf("missing-secret path hit GoTrue %d times, want 0", fake.posts())
	}
}

func TestRunSAMLProviderBootstrap_HappyPath(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	fake := newFakeGoTrue(t, testJWTSecret)
	t.Setenv(envSAMLMetadataURL, "https://login.microsoftonline.com/tenant/federationmetadata.xml")
	t.Setenv(envSAMLDomains, "acme.com, sub.acme.com")
	t.Setenv(envGoTrueJWTSecret, testJWTSecret)

	s := &Server{authCfg: &authConfig{gotrueURL: fake.srv.URL}}
	if err := s.RunSAMLProviderBootstrap(context.Background()); err != nil {
		t.Fatalf("RunSAMLProviderBootstrap: %v", err)
	}
	if fake.providerCount() != 1 {
		t.Fatalf("GoTrue has %d providers, want 1", fake.providerCount())
	}
	if fake.posts() != 1 {
		t.Errorf("POST hit %d times, want 1", fake.posts())
	}

	// Re-run is idempotent end-to-end (restart): still one provider, no new POST.
	if err := s.RunSAMLProviderBootstrap(context.Background()); err != nil {
		t.Fatalf("re-run RunSAMLProviderBootstrap: %v", err)
	}
	if fake.providerCount() != 1 {
		t.Errorf("after re-run GoTrue has %d providers, want 1", fake.providerCount())
	}
	if fake.posts() != 1 {
		t.Errorf("after re-run POST hit %d times, want 1 (idempotent)", fake.posts())
	}
}

func TestGotrueAdminBaseURL(t *testing.T) {
	// Prefers the wired auth config.
	s := &Server{authCfg: &authConfig{gotrueURL: "http://gotrue:9999"}}
	if got := s.gotrueAdminBaseURL(); got != "http://gotrue:9999" {
		t.Errorf("gotrueAdminBaseURL=%q, want wired value", got)
	}
	// Falls back to env, trimming the trailing slash.
	s2 := &Server{}
	t.Setenv("TF_GOTRUE_URL", "http://gotrue:9999/")
	if got := s2.gotrueAdminBaseURL(); got != "http://gotrue:9999" {
		t.Errorf("gotrueAdminBaseURL fallback=%q, want trailing-slash-trimmed env", got)
	}
}
