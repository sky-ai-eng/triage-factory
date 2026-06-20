package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// Per-org Configure-SSO backend (TFAC-424, epic TFAC-422). An org admin
// registers their IdP connection at runtime via these endpoints — there is no
// env-driven / startup-bootstrap SSO path (the prior single-tenant approach was
// scrapped). TF is one multi-org build: self-host and SaaS run the identical
// image, so the org↔provider binding is per-org config, not deployment env.
//
// Division of ownership (epic decision): GoTrue owns authN + the provider
// registry; TF owns authorization (the org + the role a JIT-provisioned user
// gets, in sso_connections). The GoTrue provider_id is the single bridge, and
// it is globally unique (TFAC-425 index), so binding it to the caller's org
// here is what makes "provider_id → org" the isolation boundary TFAC-426's JIT
// trusts — a connection can never be bound to an org the caller doesn't admin.
//
// Multi-mode only: GoTrue (and therefore SAML) exists only in the multi-mode
// stack. Local mode is N=1 with no IdP, so every route 404s there — the same
// "feature absent" posture as teams/invites management.
//
// The createSAMLProvider / mintServiceRoleJWT GoTrue admin-API helpers below
// are re-homed from the scrapped startup bootstrap (claude/cool-davinci-jahpyk);
// they were the good part. What changed is the trigger (an org-admin request,
// not boot) and the idempotency key (the caller org's existing connection row,
// not a domain match — domains are out of scope here, TFAC-428).

// envGoTrueJWTSecret is the HS256 secret GoTrue validates admin bearer tokens
// against. The TFAC-423 spike (#4) confirmed GoTrue accepts a service_role
// token signed with this even under GOTRUE_JWT_ALGORITHM=RS256 — the secret is
// the shared admin credential, distinct from the RS256 keypair that signs user
// sessions. The same var the gotrue service reads; the compose stack passes it
// to the TF service too so this handler can mint the token.
const envGoTrueJWTSecret = "GOTRUE_JWT_SECRET"

// ssoHTTPClient bounds the GoTrue admin call so a hung GoTrue can't wedge a
// request. GoTrue is in-network (compose service), so this is the upper bound
// for the abnormal case. Mirrors samlHTTPClient's posture in the scrapped
// bootstrap.
var ssoHTTPClient = &http.Client{Timeout: 30 * time.Second}

// ssoConnectionHandler serves /api/sso/connection — the org-admin SSO
// connection lifecycle. Follows teams_handler / invites_handler: requireOrg for
// the active org, ClaimsFrom for the user, az.UserIsOrgAdmin for the gate (404
// not 403 on non-admin — non-disclosure), tx.WithTx for app-pool writes under
// RLS.
type ssoConnectionHandler struct {
	tx     db.TxRunner
	az     *authz.Checker
	client *http.Client

	// gotrueURL returns the in-network GoTrue admin base URL. Read lazily
	// (a func, not a value) because authCfg lands after routes() runs — wired
	// in SetAuthDeps — and tests point it at a fake GoTrue.
	gotrueURL func() string
	// publicURL returns the externally-visible deployment base, used to build
	// the SP Entity-ID / ACS values the operator pastes into Entra. Read
	// lazily: deployCfg is populated after routes() runs.
	publicURL func() string

	// regMu serializes provider registration per org so two concurrent POSTs
	// can't both pass the existing-connection check and mint duplicate GoTrue
	// providers. Mirrors Server.githubAppRegMu / projectMutexes. Keyed by
	// orgID; entries are never removed (a stale mutex on a deleted org is just
	// unused memory).
	regMu sync.Map // map[orgID]*sync.Mutex
}

// regMutex returns the per-org registration mutex, created on demand.
func (h *ssoConnectionHandler) regMutex(orgID string) *sync.Mutex {
	if v, ok := h.regMu.Load(orgID); ok {
		return v.(*sync.Mutex)
	}
	v, _ := h.regMu.LoadOrStore(orgID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// ssoConnectionView is the wire shape of one connection. provider_id is the
// GoTrue handle the operator/admin can cross-reference; the rest are TF-owned
// management fields. No secrets here — the connection carries none (the IdP
// signing material lives in GoTrue).
type ssoConnectionView struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	ProviderID  string    `json:"provider_id"`
	DefaultRole string    `json:"default_role"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ssoConnectionResponse is the shape all three endpoints return: the org's
// connection (null until one is registered) plus the two SP values the operator
// must paste into Entra. The SP values are always present — even with no
// connection — because the operator needs them to configure the IdP side, and
// they're a pure function of the deployment's public URL (fixed by GoTrue's
// SAML SP endpoints, confirmed by the TFAC-423 spike #3).
type ssoConnectionResponse struct {
	Connection *ssoConnectionView `json:"connection"`
	EntityID   string             `json:"entity_id"`
	ACSURL     string             `json:"acs_url"`
}

// toSSOConnectionView projects a domain row to its wire shape, or nil.
func toSSOConnectionView(c *domain.SSOConnection) *ssoConnectionView {
	if c == nil {
		return nil
	}
	return &ssoConnectionView{
		ID:          c.ID,
		Kind:        c.Kind,
		ProviderID:  c.ProviderID,
		DefaultRole: c.DefaultRole,
		Enabled:     c.Enabled,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

// spValues returns the SAML SP Entity-ID and ACS URL for the deployment. These
// are GoTrue's fixed SP endpoints under the public URL (TFAC-423 spike #3):
// Entity ID = {public}/auth/v1/sso/saml/metadata, ACS =
// {public}/auth/v1/sso/saml/acs. The operator enters these into Entra as the
// Identifier and Reply URL respectively (see docs/sso-entra.md).
func spValues(publicURL string) (entityID, acsURL string) {
	base := strings.TrimRight(publicURL, "/")
	return base + "/auth/v1/sso/saml/metadata", base + "/auth/v1/sso/saml/acs"
}

// spValuesOr500 resolves the SP Entity-ID/ACS values for the response, or writes
// a 500 and returns ok=false when the deployment's public URL isn't configured.
// A missing public URL (TF_PUBLIC_URL unset, or deployCfg unwired) is a server
// misconfiguration — not a client error — and emitting the relative paths
// spValues("") would produce ("/auth/v1/sso/saml/metadata", …) into the
// response would mislead an operator pasting them into Entra. TF_PUBLIC_URL is
// `:?`-required in compose, so this is a defensive guard for a state a correct
// multi-mode deploy never reaches; we fail fast rather than emit a bad value.
func (h *ssoConnectionHandler) spValuesOr500(w http.ResponseWriter) (entityID, acsURL string, ok bool) {
	pub := h.publicURL()
	if pub == "" {
		internalError(w, "sso", fmt.Errorf("deployment public URL not configured (TF_PUBLIC_URL); cannot build SAML SP values"))
		return "", "", false
	}
	entityID, acsURL = spValues(pub)
	return entityID, acsURL, true
}

// currentConnection returns the org's connection, or (nil, nil) when none is
// registered. The endpoint treats SSO as singular per org; ListByOrg returns
// newest-first, so the head is "the" connection. App pool under the caller's
// claims (RLS gates on org-admin in the current org).
func (h *ssoConnectionHandler) currentConnection(ctx context.Context, orgID, userID string) (*domain.SSOConnection, error) {
	var conns []domain.SSOConnection
	if err := h.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		conns, e = tx.SSOConnections.ListByOrg(ctx, orgID)
		return e
	}); err != nil {
		return nil, err
	}
	if len(conns) == 0 {
		return nil, nil
	}
	return &conns[0], nil
}

// adminGate resolves the active org + caller and confirms org-admin, returning
// (orgID, userID, true) on success. On any failure it has already written the
// response: 404 for local mode / non-admin (non-disclosure), 409 for no active
// org, 500 for a probe error. The shared preamble for all three handlers.
func (h *ssoConnectionHandler) adminGate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		// Multi-mode only: local is N=1 with no IdP. 404 = "feature absent".
		http.NotFound(w, r)
		return "", "", false
	}
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = ClaimsFrom(r.Context()).Subject
	isAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "sso", err)
		return "", "", false
	}
	if !isAdmin {
		// 404 not 403 — same non-disclosure posture as teams/invites: don't
		// reveal the org's SSO surface to a non-admin.
		http.NotFound(w, r)
		return "", "", false
	}
	return orgID, userID, true
}

// handleSSOConnectionGet returns the org's connection (if any) plus the SP
// values the operator pastes into Entra.
//
// GET /api/sso/connection
func (h *ssoConnectionHandler) handleSSOConnectionGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}
	entityID, acsURL, ok := h.spValuesOr500(w)
	if !ok {
		return
	}
	conn, err := h.currentConnection(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	writeJSON(w, http.StatusOK, ssoConnectionResponse{
		Connection: toSSOConnectionView(conn),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// handleSSOConnectionCreate registers the org's IdP connection. It mints a
// service_role JWT, asks GoTrue to create the SAML provider (GoTrue fetches the
// metadata server-side), then writes the TF-owned sso_connections binding.
//
// Idempotent: an org that already has a connection reuses it (200, no second
// GoTrue provider) — rotating the metadata is a deferred follow-up, so a
// re-register is a no-op that returns the existing provider/row. The per-org
// regMutex makes that hold under concurrent POSTs.
//
// POST /api/sso/connection  body: { "metadata_url": "<Entra App Federation Metadata URL>" }
func (h *ssoConnectionHandler) handleSSOConnectionCreate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	var body struct {
		MetadataURL string `json:"metadata_url"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	metadataURL := strings.TrimSpace(body.MetadataURL)
	if metadataURL == "" {
		badRequest(w, "metadata_url is required")
		return
	}
	// Defense-in-depth against SSRF: GoTrue fetches this URL server-side from
	// inside the compose network, so constrain it to a well-formed https URL
	// (Entra's federation metadata is always https). Blocks an org admin from
	// pointing GoTrue at an internal plaintext service — http://169.254.169.254/…
	// or http://postgres:5432/ — for port-scanning. Admins are already trusted,
	// so this is hardening, not the trust boundary.
	if u, err := url.Parse(metadataURL); err != nil || u.Scheme != "https" || u.Host == "" {
		badRequest(w, "metadata_url must be a valid https URL")
		return
	}

	entityID, acsURL, ok := h.spValuesOr500(w)
	if !ok {
		return
	}

	// Serialize registration per org: two concurrent POSTs must not both pass
	// the existing-connection check below and mint duplicate GoTrue providers
	// (the schema allows >1 connection per org for a future multi-IdP world, so
	// nothing downstream would dedup them).
	mu := h.regMutex(orgID)
	mu.Lock()
	defer mu.Unlock()

	// Idempotent reuse: if a connection already exists, return it untouched.
	existing, err := h.currentConnection(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusOK, ssoConnectionResponse{
			Connection: toSSOConnectionView(existing),
			EntityID:   entityID,
			ACSURL:     acsURL,
		})
		return
	}

	// Register the provider with GoTrue's admin SSO API.
	secret := strings.TrimSpace(os.Getenv(envGoTrueJWTSecret))
	if secret == "" {
		internalError(w, "sso", fmt.Errorf("%s is unset; cannot authenticate to GoTrue admin API", envGoTrueJWTSecret))
		return
	}
	gotrueURL := h.gotrueURL()
	if gotrueURL == "" {
		internalError(w, "sso", fmt.Errorf("GoTrue URL unknown; auth not wired"))
		return
	}
	token, err := mintServiceRoleJWT(secret)
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	// Domains stay empty: TF routes login by its own verified sso_domains
	// (TFAC-428), not GoTrue's email-domain HRD, so the provider claims none.
	providerID, err := createSAMLProvider(r.Context(), h.client, gotrueURL, token, metadataURL, []string{})
	if err != nil {
		internalError(w, "sso", fmt.Errorf("register SAML provider with GoTrue: %w", err))
		return
	}

	// Persist the TF-owned binding (app pool, org-scoped under RLS). The
	// provider now exists in GoTrue; if this write fails we log the orphaned
	// provider_id so an operator can reconcile it (connection delete/rotate is
	// a deferred follow-up).
	var created *domain.SSOConnection
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		id, e := tx.SSOConnections.Create(r.Context(), domain.CreateSSOConnectionParams{
			OrgID:      orgID,
			ProviderID: providerID,
		})
		if e != nil {
			return e
		}
		created, e = tx.SSOConnections.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		if isUniqueViolation(err) {
			// provider_id already bound to some org (global-unique index). The
			// holder is opaque cross-org; surface a generic conflict. Effectively
			// unreachable — provider_id is a freshly minted GoTrue UUID — so this
			// is the expected-conflict path, not an orphan: don't log a WARN that
			// would make a routine 409 look like a silent failure.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this identity provider is already registered",
			})
			return
		}
		// A genuine write failure (transient/RLS/etc.): the GoTrue provider was
		// created but couldn't be bound, so it's orphaned. Log the provider_id so
		// an operator can reconcile it (connection delete/rotate is deferred).
		ssoLog.Warn("GoTrue SAML provider created but sso_connections write failed; provider is orphaned",
			"org", orgID, "provider_id", providerID, "error", err)
		internalError(w, "sso", err)
		return
	}

	writeJSON(w, http.StatusCreated, ssoConnectionResponse{
		Connection: toSSOConnectionView(created),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// handleSSOConnectionUpdate enables/disables the org's connection. A disabled
// connection stops routing logins (TFAC-426/427 honor enabled) without
// destroying the binding or the GoTrue provider.
//
// PATCH /api/sso/connection  body: { "enabled": <bool> }
func (h *ssoConnectionHandler) handleSSOConnectionUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	if body.Enabled == nil {
		// *bool so an omitted field is distinguishable from an explicit false —
		// a PATCH that names nothing must not silently disable the connection.
		badRequest(w, "enabled is required")
		return
	}

	entityID, acsURL, ok := h.spValuesOr500(w)
	if !ok {
		return
	}

	existing, err := h.currentConnection(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	if existing == nil {
		http.NotFound(w, r)
		return
	}

	var updated *domain.SSOConnection
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// DefaultRole left empty → Update preserves the stored role; this PATCH
		// only flips enabled.
		if e := tx.SSOConnections.Update(r.Context(), orgID, existing.ID, domain.UpdateSSOConnectionParams{
			Enabled: *body.Enabled,
		}); e != nil {
			return e
		}
		var e error
		updated, e = tx.SSOConnections.GetByID(r.Context(), orgID, existing.ID)
		return e
	}); err != nil {
		internalError(w, "sso", err)
		return
	}

	writeJSON(w, http.StatusOK, ssoConnectionResponse{
		Connection: toSSOConnectionView(updated),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// gotrueAdminBaseURL returns the in-network GoTrue base URL for admin calls,
// preferring the value wired into the auth config (SetAuthDeps) and falling
// back to TF_GOTRUE_URL for paths that run before/without the full auth stack
// (e.g. tests). Trailing slash trimmed so path joins are clean. Re-homed from
// the scrapped bootstrap.
func (s *Server) gotrueAdminBaseURL() string {
	if s.authCfg != nil && s.authCfg.gotrueURL != "" {
		return s.authCfg.gotrueURL
	}
	return strings.TrimRight(strings.TrimSpace(os.Getenv("TF_GOTRUE_URL")), "/")
}

// mintServiceRoleJWT signs a short-lived HS256 token carrying role=service_role,
// the credential GoTrue's admin API requires. The TFAC-423 spike (#4) confirmed
// GoTrue accepts this symmetric token for /admin even under RS256 session JWTs.
// Short TTL because it's used immediately and never persisted; iat is nudged
// back a touch for clock-skew tolerance (mirrors the githubapp mint).
//
// The token deliberately carries only role/iat/exp — the minimal set the pinned
// GoTrue (supabase/gotrue:v2.189.0) admin middleware checks; it does not require
// iss/aud on the service_role bearer. Adding those speculatively could regress
// the spike-validated path, so we don't. If the pinned GoTrue version is bumped,
// re-validate admin auth still accepts this shape (a newer version could begin
// enforcing GOTRUE_JWT_ISSUER / GOTRUE_JWT_AUD here).
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

// gotrueSSOProvider is the subset of GoTrue's SSOProvider create-response TF
// reads: just the id (GoTrue returns saml metadata + timestamps too). The id is
// the provider_id we bind to the org.
type gotrueSSOProvider struct {
	ID string `json:"id"`
}

// createSAMLProvider POSTs the provider registration to GoTrue's admin SSO API.
// GoTrue fetches the Entra metadata server-side from metadataURL; TF passes only
// the URL + the (empty) domain set. Returns the new provider id on 201 (200
// tolerated for forward-compat). Re-homed from the scrapped bootstrap, with the
// domain-idempotency logic dropped — idempotency now lives at the TF row level.
func createSAMLProvider(ctx context.Context, client *http.Client, baseURL, token, metadataURL string, domains []string) (string, error) {
	if domains == nil {
		// Marshal to "domains":[] rather than null — GoTrue's admin API expects
		// an array, and the spike-confirmed body shape is domains:[].
		domains = []string{}
	}
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
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create sso provider: http %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	var out gotrueSSOProvider
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("create sso provider: %d with no provider id in response", resp.StatusCode)
	}
	return out.ID, nil
}
