// Package sso is the Enterprise Edition SSO surface: the per-org SAML/OIDC
// connection, domain-verification, and break-glass HTTP handlers, mounted
// into the core server through the route-extension seam and gated on the
// `sso` entitlement. Core holds zero SSO symbols: store access goes through
// ee/sso/store's typed view of core's opaque extension slot, and HTTP helpers
// come from the shared httpx package.
//
// Licensed under the Enterprise Edition License (see ee/LICENSE).
package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	ssostore "github.com/sky-ai-eng/triage-factory/ee/sso/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// envServiceRoleToken names the env var holding the pre-minted RS256
// service-role JWT TF presents to GoTrue's admin SSO API. `triagefactory
// jwk-init` mints it (signed with the same RSA key as GOTRUE_JWT_KEYS).
// Optional-but-required-for-SSO: a deployment that never registers an SSO
// connection need not set it. Treat it as a secret.
const envServiceRoleToken = "TF_GOTRUE_SERVICE_ROLE_TOKEN"

// ssoHTTPClient bounds the GoTrue admin call so a hung GoTrue can't wedge a
// request. GoTrue is in-network (compose service), so this is the upper
// bound for the abnormal case.
var ssoHTTPClient = &http.Client{Timeout: 30 * time.Second}

// ssoConnectionHandler serves /api/sso/connection — the org-admin SSO
// connection lifecycle. requireOrg for the active org, ClaimsFrom for the
// user, az.UserIsOrgAdmin for the gate (404 not 403 on non-admin —
// non-disclosure), tx.WithTx for app-pool writes under RLS.
type ssoConnectionHandler struct {
	tx     db.TxRunner
	az     *authz.Checker
	client *http.Client

	// gotrueURL returns the in-network GoTrue admin base URL. Read lazily
	// (a func) because the deployment's auth config lands after routes().
	gotrueURL func() string
	// publicURL returns the externally-visible deployment base, used to
	// build the SP Entity-ID / ACS values the operator pastes into Entra.
	publicURL func() string

	// regMu serializes provider registration per org so two concurrent
	// POSTs can't both pass the existing-connection check and mint
	// duplicate GoTrue providers. Keyed by orgID; entries are never removed.
	regMu sync.Map // map[orgID]*sync.Mutex
}

func (h *ssoConnectionHandler) regMutex(orgID string) *sync.Mutex {
	if v, ok := h.regMu.Load(orgID); ok {
		return v.(*sync.Mutex)
	}
	v, _ := h.regMu.LoadOrStore(orgID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type ssoConnectionView struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	ProviderID  string    `json:"provider_id"`
	DefaultRole string    `json:"default_role"`
	Enabled     bool      `json:"enabled"`
	Enforced    bool      `json:"enforced"`
	Tested      bool      `json:"tested"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ssoConnectionResponse struct {
	Connection *ssoConnectionView `json:"connection"`
	EntityID   string             `json:"entity_id"`
	ACSURL     string             `json:"acs_url"`
}

func toSSOConnectionView(c *ssostore.SSOConnection) *ssoConnectionView {
	if c == nil {
		return nil
	}
	return &ssoConnectionView{
		ID:          c.ID,
		Kind:        c.Kind,
		ProviderID:  c.ProviderID,
		DefaultRole: c.DefaultRole,
		Enabled:     c.Enabled,
		Enforced:    c.Enforced,
		Tested:      c.LastTestedAt != nil,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

// spValues returns the SAML SP Entity-ID and ACS URL for the deployment —
// GoTrue's fixed SP endpoints under the public URL.
func spValues(publicURL string) (entityID, acsURL string) {
	base := strings.TrimRight(publicURL, "/")
	return base + "/auth/v1/sso/saml/metadata", base + "/auth/v1/sso/saml/acs"
}

func (h *ssoConnectionHandler) spValuesOr500(w http.ResponseWriter) (entityID, acsURL string, ok bool) {
	pub := h.publicURL()
	if pub == "" {
		httpx.InternalError(w, "sso", fmt.Errorf("deployment public URL not configured (TF_PUBLIC_URL); cannot build SAML SP values"))
		return "", "", false
	}
	entityID, acsURL = spValues(pub)
	return entityID, acsURL, true
}

// currentConnection returns the org's connection, or (nil, nil) when none
// is registered. App pool under the caller's claims (RLS gates on
// org-admin in the current org).
func (h *ssoConnectionHandler) currentConnection(ctx context.Context, orgID, userID string) (*ssostore.SSOConnection, error) {
	var conns []ssostore.SSOConnection
	if err := h.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		conns, e = ssostore.FromTx(tx).Connections.ListByOrg(ctx, orgID)
		return e
	}); err != nil {
		return nil, err
	}
	if len(conns) == 0 {
		return nil, nil
	}
	return &conns[0], nil
}

// adminGate resolves the active org + caller and confirms org-admin. On any
// failure it has already written the response (404 for local/non-admin,
// etc.). The shared preamble for all three handlers.
func (h *ssoConnectionHandler) adminGate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return "", "", false
	}
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = httpx.ClaimsFrom(r.Context()).Subject
	isAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return "", "", false
	}
	if !isAdmin {
		http.NotFound(w, r)
		return "", "", false
	}
	return orgID, userID, true
}

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
		httpx.InternalError(w, "sso", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ssoConnectionResponse{
		Connection: toSSOConnectionView(conn),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// POST /api/sso/connection  body: { "metadata_url": "<Entra metadata URL>" }
func (h *ssoConnectionHandler) handleSSOConnectionCreate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	var body struct {
		MetadataURL string `json:"metadata_url"`
	}
	if !httpx.DecodeJSON(w, r, &body, "") {
		return
	}
	metadataURL := strings.TrimSpace(body.MetadataURL)
	if metadataURL == "" {
		httpx.BadRequest(w, "metadata_url is required")
		return
	}
	// Defense-in-depth against SSRF: GoTrue fetches this URL server-side, so
	// constrain it to a well-formed https URL.
	if u, err := url.Parse(metadataURL); err != nil || u.Scheme != "https" || u.Host == "" {
		httpx.BadRequest(w, "metadata_url must be a valid https URL")
		return
	}

	entityID, acsURL, ok := h.spValuesOr500(w)
	if !ok {
		return
	}

	mu := h.regMutex(orgID)
	mu.Lock()
	defer mu.Unlock()

	existing, err := h.currentConnection(r.Context(), orgID, userID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if existing != nil {
		httpx.WriteJSON(w, http.StatusOK, ssoConnectionResponse{
			Connection: toSSOConnectionView(existing),
			EntityID:   entityID,
			ACSURL:     acsURL,
		})
		return
	}

	token := strings.TrimSpace(os.Getenv(envServiceRoleToken))
	if token == "" {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "SSO admin token not configured — run `triagefactory jwk-init` and set " + envServiceRoleToken,
		})
		return
	}
	gotrueURL := h.gotrueURL()
	if gotrueURL == "" {
		httpx.InternalError(w, "sso", fmt.Errorf("GoTrue URL unknown; auth not wired"))
		return
	}
	providerID, err := createSAMLProvider(r.Context(), h.client, gotrueURL, token, metadataURL, []string{})
	if err != nil {
		httpx.InternalError(w, "sso", fmt.Errorf("register SAML provider with GoTrue: %w", err))
		return
	}

	var created *ssostore.SSOConnection
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		id, e := ssostore.FromTx(tx).Connections.Create(r.Context(), ssostore.CreateSSOConnectionParams{
			OrgID:      orgID,
			ProviderID: providerID,
		})
		if e != nil {
			return e
		}
		created, e = ssostore.FromTx(tx).Connections.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		if httpx.IsUniqueViolation(err) {
			httpx.WriteJSON(w, http.StatusConflict, map[string]string{
				"error": "this identity provider is already registered",
			})
			return
		}
		ssoLog.Warn("GoTrue SAML provider created but sso_connections write failed; provider is orphaned",
			"org", orgID, "provider_id", providerID, "error", err)
		httpx.InternalError(w, "sso", err)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, ssoConnectionResponse{
		Connection: toSSOConnectionView(created),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// PATCH /api/sso/connection  body: { "enabled": <bool> }
func (h *ssoConnectionHandler) handleSSOConnectionUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !httpx.DecodeJSON(w, r, &body, "") {
		return
	}
	if body.Enabled == nil {
		httpx.BadRequest(w, "enabled is required")
		return
	}

	entityID, acsURL, ok := h.spValuesOr500(w)
	if !ok {
		return
	}

	existing, err := h.currentConnection(r.Context(), orgID, userID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if existing == nil {
		http.NotFound(w, r)
		return
	}

	var updated *ssostore.SSOConnection
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := ssostore.FromTx(tx).Connections.Update(r.Context(), orgID, existing.ID, ssostore.UpdateSSOConnectionParams{
			Enabled: *body.Enabled,
		}); e != nil {
			return e
		}
		var e error
		updated, e = ssostore.FromTx(tx).Connections.GetByID(r.Context(), orgID, existing.ID)
		return e
	}); err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, ssoConnectionResponse{
		Connection: toSSOConnectionView(updated),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// PATCH /api/sso/connection/enforcement  body: { "enforced": <bool> }
func (h *ssoConnectionHandler) handleSSOEnforcementUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	var body struct {
		Enforced *bool `json:"enforced"`
	}
	if !httpx.DecodeJSON(w, r, &body, "") {
		return
	}
	if body.Enforced == nil {
		httpx.BadRequest(w, "enforced is required")
		return
	}

	entityID, acsURL, ok := h.spValuesOr500(w)
	if !ok {
		return
	}

	existing, err := h.currentConnection(r.Context(), orgID, userID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if existing == nil {
		http.NotFound(w, r)
		return
	}

	var updated *ssostore.SSOConnection
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		sso := ssostore.FromTx(tx)
		if *body.Enforced {
			// Gate ON only behind a proven-working connection. Re-read state
			// inside the tx so the checks can't race a concurrent disable/un-verify.
			conn, e := sso.Connections.GetByID(r.Context(), orgID, existing.ID)
			if e != nil {
				return e
			}
			if conn == nil {
				return errEnforceNoConnection
			}
			if !conn.Enabled {
				return errEnforceNotEnabled
			}
			if conn.LastTestedAt == nil {
				return errEnforceNotTested
			}
			doms, e := sso.Domains.ListByOrg(r.Context(), orgID)
			if e != nil {
				return e
			}
			if !anyVerified(doms) {
				return errEnforceNoVerifiedDomain
			}
			if e := sso.BreakGlass.SeedOwnerIfEmpty(r.Context(), orgID); e != nil {
				return e
			}
		}
		if e := sso.Connections.SetEnforced(r.Context(), orgID, existing.ID, *body.Enforced); e != nil {
			return e
		}
		var e error
		updated, e = sso.Connections.GetByID(r.Context(), orgID, existing.ID)
		return e
	}); err != nil {
		switch {
		case errors.Is(err, errEnforceNotEnabled):
			httpx.WriteJSON(w, http.StatusConflict, map[string]string{
				"error": "enable the connection before requiring SSO",
			})
		case errors.Is(err, errEnforceNotTested):
			httpx.WriteJSON(w, http.StatusConflict, map[string]string{
				"error": "run a successful Test before requiring SSO",
			})
		case errors.Is(err, errEnforceNoVerifiedDomain):
			httpx.WriteJSON(w, http.StatusConflict, map[string]string{
				"error": "verify a domain before requiring SSO",
			})
		case errors.Is(err, errEnforceNoConnection):
			http.NotFound(w, r)
		default:
			httpx.InternalError(w, "sso", err)
		}
		return
	}

	httpx.WriteJSON(w, http.StatusOK, ssoConnectionResponse{
		Connection: toSSOConnectionView(updated),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

var (
	errEnforceNoConnection     = errors.New("sso: no connection to enforce")
	errEnforceNotEnabled       = errors.New("sso: connection not enabled")
	errEnforceNotTested        = errors.New("sso: connection not tested")
	errEnforceNoVerifiedDomain = errors.New("sso: no verified domain")
)

func anyVerified(doms []ssostore.SSODomain) bool {
	for _, d := range doms {
		if d.VerifiedAt != nil {
			return true
		}
	}
	return false
}

type gotrueSSOProvider struct {
	ID string `json:"id"`
}

// createSAMLProvider POSTs the provider registration to GoTrue's admin SSO
// API. GoTrue fetches the Entra metadata server-side from metadataURL.
// Returns the new provider id on 201 (200 tolerated for forward-compat).
func createSAMLProvider(ctx context.Context, client *http.Client, baseURL, token, metadataURL string, domains []string) (string, error) {
	if domains == nil {
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
