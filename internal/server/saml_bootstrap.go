package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SAML SSO bootstrap (TFAC-424). The self-host single-tenant slice of SAML SSO
// (epic TFAC-422) registers exactly one statically-configured Entra SAML
// provider with GoTrue at startup, so the login-flow ticket (TFAC-426) has a
// working provider to drive. There is no in-app "Configure SSO" UI — the one
// provider comes from operator env, mirroring the never-overwrite,
// warn-don't-crash spirit of the local-mode headless bootstrap (headless.go).
// This file only makes GoTrue SAML-ready and registers the provider; it does
// not touch TF's own login handlers (that's TFAC-426).
//
// Invariants:
//   - Multi mode only. GoTrue (and therefore SAML) exists only in multi mode;
//     local mode is N=1 with no IdP. The caller gates on the multi-mode boot
//     path, and a hard guard here makes a stray call a no-op regardless.
//   - Opt-in. With no TF_SAML_ENTRA_METADATA_URL the bootstrap is a silent
//     no-op — a GitHub-OAuth-only multi deploy is entirely unaffected.
//   - Idempotent + never-overwrite. The provider is created once; a re-run
//     (restart) finds the existing provider by domain and leaves it untouched.
//     TF never updates or deletes a provider — operator changes to the Entra
//     metadata are out of band.
//   - Warn, don't crash. A missing JWT secret, an unreachable GoTrue, or a
//     rejected registration logs a WARN and skips; SSO is simply unavailable
//     until the operator resolves it. Boot is never aborted.

const (
	// envSAMLMetadataURL is Entra's App Federation Metadata URL. GoTrue fetches
	// it server-side (the GoTrue container needs outbound) to learn the IdP's
	// entityID, SSO URL, and signing cert — so TF passes only the URL, never the
	// parsed metadata. Presence of this var is the bootstrap's on-switch.
	envSAMLMetadataURL = "TF_SAML_ENTRA_METADATA_URL"

	// envSAMLDomains is the comma-separated org email-domain set the provider
	// claims (e.g. "acme.com"). Single-tenant, so usually one domain. Used both
	// as the POST body's domains and as the idempotency key on re-run.
	envSAMLDomains = "TF_SAML_ENTRA_DOMAINS"

	// envGoTrueJWTSecret is the HS256 secret GoTrue validates admin bearer
	// tokens against (the legacy-fallback secret jwk-init also writes). The
	// spike (TFAC-423 #4) confirmed GoTrue accepts a service_role token signed
	// with this even under GOTRUE_JWT_ALGORITHM=RS256 — the secret is the shared
	// admin credential, distinct from the RS256 keypair that signs user
	// sessions. Same var the gotrue service reads; the compose stack passes it
	// to the TF service too.
	envGoTrueJWTSecret = "GOTRUE_JWT_SECRET"
)

// samlHTTPClient bounds the GoTrue admin calls so a hung GoTrue can't wedge
// boot. The registration runs after the JWKS verifier is already up (boot
// order), so GoTrue is reachable in the normal case; this is the upper bound
// for the abnormal one. Mirrors gotrueHTTPClient's posture in auth_handlers.go.
var samlHTTPClient = &http.Client{Timeout: 30 * time.Second}

// samlBootstrapConfig is the resolved input for one registration attempt,
// gathered from env + the wired GoTrue URL then handed to the pure registration
// logic so that logic is unit-testable against an httptest GoTrue.
type samlBootstrapConfig struct {
	gotrueURL   string
	jwtSecret   string
	metadataURL string
	domains     []string
}

// RunSAMLProviderBootstrap registers the single Entra SAML provider with GoTrue
// via its admin SSO API. Multi-mode only; the caller (app.runStartupTasks)
// invokes it on the multi-mode boot path after auth is wired. Idempotent and
// never-overwriting (see the file doc). Returns an error only for an unexpected
// failure the caller should log; every "couldn't register" case is handled
// in-band with a WARN.
func (s *Server) RunSAMLProviderBootstrap(ctx context.Context) error {
	// Defense in depth: GoTrue/SAML are multi-mode only. The caller already
	// branches on mode, but a hard guard here makes a stray call a no-op.
	if runmode.Current() != runmode.ModeMulti {
		return nil
	}

	metadataURL := strings.TrimSpace(os.Getenv(envSAMLMetadataURL))
	domains := parseCSV(os.Getenv(envSAMLDomains))
	if metadataURL == "" {
		// Opt-in: no Entra metadata URL means SSO isn't configured. If the
		// operator set domains but forgot the URL, say why nothing happened
		// rather than skipping in silence.
		if len(domains) > 0 {
			samlLog.Warn("TF_SAML_ENTRA_DOMAINS is set but TF_SAML_ENTRA_METADATA_URL is empty; no SAML provider registered (set the Entra App Federation Metadata URL to enable SSO)")
		}
		return nil
	}
	if len(domains) == 0 {
		samlLog.Warn("TF_SAML_ENTRA_METADATA_URL is set but TF_SAML_ENTRA_DOMAINS is empty; skipping SAML provider registration (set the org email domain, e.g. acme.com)")
		return nil
	}

	jwtSecret := strings.TrimSpace(os.Getenv(envGoTrueJWTSecret))
	if jwtSecret == "" {
		samlLog.Warn("SAML SSO is configured but GOTRUE_JWT_SECRET is unset; cannot authenticate to GoTrue's admin API — skipping provider registration")
		return nil
	}

	gotrueURL := s.gotrueAdminBaseURL()
	if gotrueURL == "" {
		samlLog.Warn("SAML SSO is configured but the GoTrue URL is unknown (auth not wired?); skipping provider registration")
		return nil
	}

	cfg := samlBootstrapConfig{
		gotrueURL:   gotrueURL,
		jwtSecret:   jwtSecret,
		metadataURL: metadataURL,
		domains:     domains,
	}
	providerID, created, err := registerSAMLProvider(ctx, samlHTTPClient, cfg)
	if err != nil {
		samlLog.Warn("Entra SAML provider registration failed; SSO will be unavailable until resolved", "domains", domains, "error", err)
		return nil
	}
	if created {
		// provider_id is logged so the operator can pin it / the login-flow
		// ticket (TFAC-425) can reference it in sso_connections. It is also
		// re-derivable any time via GoTrue's admin API (lookup by domain).
		samlLog.Info("registered Entra SAML provider with GoTrue", "provider_id", providerID, "domains", domains)
	} else {
		samlLog.Info("Entra SAML provider already registered; left untouched (idempotent)", "provider_id", providerID, "domains", domains)
	}
	return nil
}

// gotrueAdminBaseURL returns the in-network GoTrue base URL for admin calls,
// preferring the value wired into the auth config (SetAuthDeps) and falling
// back to TF_GOTRUE_URL for the path where this runs before/without the full
// auth stack (e.g. tests). Trailing slash trimmed so path joins are clean.
func (s *Server) gotrueAdminBaseURL() string {
	if s.authCfg != nil && s.authCfg.gotrueURL != "" {
		return s.authCfg.gotrueURL
	}
	return strings.TrimRight(strings.TrimSpace(os.Getenv("TF_GOTRUE_URL")), "/")
}

// registerSAMLProvider ensures exactly one SAML provider exists for cfg.domains,
// idempotently. It first lists existing providers and returns the match if one
// already claims any configured domain (never-overwrite); otherwise it creates
// the provider and returns the new id. Pure of global state — the http client
// and config are passed in so it unit-tests against an httptest GoTrue.
//
// Returns (providerID, created, err): created is true only when this call made
// the POST that minted the provider, false when an existing one was reused.
func registerSAMLProvider(ctx context.Context, client *http.Client, cfg samlBootstrapConfig) (string, bool, error) {
	token, err := mintServiceRoleJWT(cfg.jwtSecret)
	if err != nil {
		return "", false, fmt.Errorf("mint service_role jwt: %w", err)
	}

	// Never-overwrite: if a provider already claims one of our domains, keep it.
	if existing, lookErr := findSAMLProviderByDomain(ctx, client, cfg.gotrueURL, token, cfg.domains); lookErr != nil {
		return "", false, lookErr
	} else if existing != "" {
		return existing, false, nil
	}

	id, createErr := createSAMLProvider(ctx, client, cfg.gotrueURL, token, cfg.metadataURL, cfg.domains)
	if createErr != nil {
		// We lost a race (a concurrent boot registered it between our GET and
		// POST) or GoTrue rejected an overlap our pre-GET didn't reflect.
		// Re-resolve by domain: if it now exists, treat this as the idempotent
		// already-registered case. If it still doesn't, the failure is real
		// (e.g. GoTrue couldn't fetch the Entra metadata) — surface it.
		if existing, lookErr := findSAMLProviderByDomain(ctx, client, cfg.gotrueURL, token, cfg.domains); lookErr == nil && existing != "" {
			return existing, false, nil
		}
		return "", false, createErr
	}
	return id, true, nil
}

// mintServiceRoleJWT signs a short-lived HS256 token carrying role=service_role,
// the credential GoTrue's admin API requires. The spike (TFAC-423 #4) confirmed
// GoTrue accepts this symmetric token for /admin even under RS256 session JWTs.
// Short TTL because it's used immediately at boot and never persisted; iat is
// nudged back a touch for clock-skew tolerance (mirrors githubapp mint).
func mintServiceRoleJWT(secret string) (string, error) {
	now := timeNow()
	claims := jwt.MapClaims{
		"role": "service_role",
		"iat":  now.Add(-30 * time.Second).Unix(),
		"exp":  now.Add(5 * time.Minute).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("sign service_role jwt: %w", err)
	}
	return signed, nil
}

// gotrueSSODomain is one domain entry inside a provider. GoTrue's response has
// historically used the field name "domains"; "sso_domains" is accepted too so
// a minor field-name drift across GoTrue versions doesn't silently break the
// idempotency match (and re-register a duplicate).
type gotrueSSODomain struct {
	Domain string `json:"domain"`
}

// gotrueSSOProvider is the subset of GoTrue's SSOProvider response we read — the
// id plus the claimed domains. GoTrue returns more (saml metadata, timestamps);
// we only need these two to match on and to surface the provider_id.
type gotrueSSOProvider struct {
	ID         string            `json:"id"`
	Domains    []gotrueSSODomain `json:"domains"`
	SSODomains []gotrueSSODomain `json:"sso_domains"`
}

// claimedDomains returns the provider's domain list, tolerating either response
// field name (see gotrueSSODomain).
func (p gotrueSSOProvider) claimedDomains() []string {
	src := p.Domains
	if len(src) == 0 {
		src = p.SSODomains
	}
	out := make([]string, 0, len(src))
	for _, d := range src {
		out = append(out, d.Domain)
	}
	return out
}

// findSAMLProviderByDomain lists providers and returns the id of the first one
// that claims any of the wanted domains (case-insensitive), or "" if none.
func findSAMLProviderByDomain(ctx context.Context, client *http.Client, baseURL, token string, wantDomains []string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/admin/sso/providers", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("list sso providers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("list sso providers: http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var list struct {
		Items []gotrueSSOProvider `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", fmt.Errorf("decode sso providers: %w", err)
	}

	want := make(map[string]struct{}, len(wantDomains))
	for _, d := range wantDomains {
		if v := strings.ToLower(strings.TrimSpace(d)); v != "" {
			want[v] = struct{}{}
		}
	}
	for _, p := range list.Items {
		for _, d := range p.claimedDomains() {
			if _, ok := want[strings.ToLower(strings.TrimSpace(d))]; ok {
				return p.ID, nil
			}
		}
	}
	return "", nil
}

// createSAMLProvider POSTs the provider registration. GoTrue fetches the Entra
// metadata server-side from metadataURL; TF passes only the URL + the claimed
// domains. Returns the new provider id on 201 (200 tolerated for forward-compat).
func createSAMLProvider(ctx context.Context, client *http.Client, baseURL, token, metadataURL string, domains []string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"type":         "saml",
		"metadata_url": metadataURL,
		"domains":      domains,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/admin/sso/providers", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("create sso provider: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create sso provider: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var out gotrueSSOProvider
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("create sso provider: %d with no provider id in response", resp.StatusCode)
	}
	return out.ID, nil
}
