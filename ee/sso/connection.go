// Package sso is the Enterprise Edition SSO surface: the per-org SAML/OIDC
// connection, domain-verification, and break-glass HTTP handlers, mounted
// into the core server through the route-extension seam and gated on the
// `sso` entitlement. Core holds zero SSO symbols: store access goes through
// ee/sso/store's typed view of core's opaque extension slot, and HTTP helpers
// come from the shared httpx package.
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
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
	"strings"
	"sync"
	"time"

	ssostore "github.com/sky-ai-eng/triage-factory/ee/sso/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
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
var ssoHTTPClient = telemetry.TracedHTTPClient(30*time.Second, "gotrue")

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

// ssoAccess is the sentence every org-admin SSO route's description ends
// with.
const ssoAccess = " It acts on the caller's active organization, needs an Enterprise licence that includes SSO, and is available only in multi mode."

type ssoConnectionView struct {
	ID          string `json:"id" doc:"The connection's id."`
	Kind        string `json:"kind" doc:"The connection's protocol: saml or oidc."`
	ProviderID  string `json:"provider_id" doc:"The identity provider's id in the deployment's sign-in service, which an SSO sign-in URL names."`
	DefaultRole string `json:"default_role" doc:"The organization role, admin or member, given to a person who joins the organization by signing in through the connection."`
	Enabled     bool   `json:"enabled" doc:"Whether people can sign in through the connection."`
	Enforced    bool   `json:"enforced" doc:"Whether the organization requires SSO, so that only sign-ins through the connection and break-glass principals reach it."`
	Tested      bool   `json:"tested" doc:"Whether a test sign-in through the connection has succeeded."`
	// IdP is the identity-provider product behind the connection, one of
	// ssostore.IdPValues; omitted when not known. Derived at registration,
	// correctable through the PATCH.
	IdP       string    `json:"idp,omitempty" doc:"The identity provider's product (entra, okta, google, onelogin, ping, or other), absent when it is not known."`
	CreatedAt time.Time `json:"created_at" doc:"When the connection was registered, as an RFC 3339 timestamp."`
	UpdatedAt time.Time `json:"updated_at" doc:"When the connection was last changed, as an RFC 3339 timestamp."`
}

type ssoConnectionResponse struct {
	Connection *ssoConnectionView `json:"connection" doc:"The organization's SSO connection, or null when none is registered."`
	EntityID   string             `json:"entity_id" doc:"The deployment's SAML service provider entity ID, to enter in the identity provider's settings."`
	ACSURL     string             `json:"acs_url" doc:"The deployment's SAML assertion consumer service URL, to enter in the identity provider's settings."`
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
		IdP:         c.IdP,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

// idpFromMetadataURL guesses which identity-provider product a SAML metadata
// URL belongs to, from its host alone. TF never fetches the metadata document
// (GoTrue does), so the host is the only signal available at registration;
// the guess is a default the admin can correct, never a gate. Anything the
// table does not name is "other", which is a real answer — "" is reserved for
// a URL that cannot be parsed at all.
func idpFromMetadataURL(metadataURL string) string {
	u, err := url.Parse(metadataURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	suffix := func(dom string) bool { return host == dom || strings.HasSuffix(host, "."+dom) }
	switch {
	case suffix("microsoftonline.com"), suffix("microsoftonline.us"), suffix("windows.net"):
		return "entra"
	case suffix("okta.com"), suffix("oktapreview.com"), suffix("okta-emea.com"):
		return "okta"
	case suffix("google.com"):
		return "google"
	case suffix("onelogin.com"):
		return "onelogin"
	case suffix("pingone.com"), suffix("pingidentity.com"):
		return "ping"
	default:
		return "other"
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
	if err := h.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
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
// failure it has already written the response (404 for local/unlicensed-org/
// non-admin, etc.). The shared preamble for all three handlers.
func (h *ssoConnectionHandler) adminGate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		httpx.NotFound(w, "route")
		return "", "", false
	}
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", "", false
	}
	if !entitlements.For(orgID).Has(entitlements.FeatureSSO) {
		httpx.NotFound(w, "route")
		return "", "", false
	}
	userID = httpx.ClaimsFrom(r.Context()).Subject
	isAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return "", "", false
	}
	if !isAdmin {
		// Visible org, missing role: 403, matching the core admin gates.
		httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
			Reason: httpx.ReasonForbidden, Message: "org admin role required",
		})
		return "", "", false
	}
	return orgID, userID, true
}

var opConnectionGet = httpx.NewJSONOp[httpx.NoBody, ssoConnectionResponse](httpx.OpSpec{
	Pattern:     "GET /api/sso/connection",
	ID:          "get_sso_connection",
	Summary:     "Get the SSO connection",
	Description: "Returns the organization's SSO connection, or null when none is registered, with the service provider values to enter in the identity provider's settings." + ssoAccess,
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	opConnectionGet.Write(w, ssoConnectionResponse{
		Connection: toSSOConnectionView(conn),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// ssoConnectionCreateRequest is the body of POST /api/sso/connection.
type ssoConnectionCreateRequest struct {
	MetadataURL string `json:"metadata_url" doc:"The https URL of the identity provider's SAML metadata, which the deployment fetches to register the connection. Required."`
}

var opConnectionCreate = httpx.NewJSONOp[ssoConnectionCreateRequest, ssoConnectionResponse](httpx.OpSpec{
	Pattern:       "POST /api/sso/connection",
	ID:            "create_sso_connection",
	Summary:       "Register an SSO connection",
	Description:   "Registers a SAML connection, enabled but not required, from the identity provider's metadata URL, which the deployment fetches, and returns it with the service provider values. An organization that already has a connection gets that one back unchanged, with status 200." + ssoAccess,
	Status:        http.StatusCreated,
	OtherStatuses: []int{http.StatusOK},
	Hints:         httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// POST /api/sso/connection  body: { "metadata_url": "<Entra metadata URL>" }
func (h *ssoConnectionHandler) handleSSOConnectionCreate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	body, ok := opConnectionCreate.Decode(w, r)
	if !ok {
		return
	}
	metadataURL := strings.TrimSpace(body.MetadataURL)
	if metadataURL == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "metadata_url is required", Field: "metadata_url"})
		return
	}
	// Defense-in-depth against SSRF: GoTrue fetches this URL server-side, so
	// constrain it to a well-formed https URL.
	if u, err := url.Parse(metadataURL); err != nil || u.Scheme != "https" || u.Host == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "metadata_url must be a valid https URL", Field: "metadata_url"})
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
		opConnectionCreate.WriteStatus(w, http.StatusOK, ssoConnectionResponse{
			Connection: toSSOConnectionView(existing),
			EntityID:   entityID,
			ACSURL:     acsURL,
		})
		return
	}

	token := strings.TrimSpace(secretenv.Get(envServiceRoleToken))
	if token == "" {
		httpx.WriteErrors(w, http.StatusServiceUnavailable, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "SSO admin token not configured — run `triagefactory jwk-init` and set " + envServiceRoleToken})
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
			IdP:        idpFromMetadataURL(metadataURL),
		})
		if e != nil {
			return e
		}
		if e := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionSSOConnectionCreated,
			DetailJSON:  domain.AccessDetailSSOConnection(providerID),
		}); e != nil {
			return fmt.Errorf("audit sso connection create: %w", e)
		}
		created, e = ssostore.FromTx(tx).Connections.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		if httpx.IsUniqueViolation(err) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this identity provider is already registered"})
			return
		}
		ssoLog.Warn("GoTrue SAML provider created but sso_connections write failed; provider is orphaned",
			"org", orgID, "provider_id", providerID, "error", err)
		httpx.InternalError(w, "sso", err)
		return
	}

	opConnectionCreate.Write(w, ssoConnectionResponse{
		Connection: toSSOConnectionView(created),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// ssoConnectionUpdateRequest is the body of PATCH /api/sso/connection.
// Enabled keeps its raw bytes so a null reads apart from an absent field: the
// flag has no unset state, so a null is refused rather than kept or cleared.
type ssoConnectionUpdateRequest struct {
	Enabled httpx.Patch[bool]   `json:"enabled" doc:"Whether people can sign in through the connection; absent keeps it, and null is refused, since the flag has no unset state. It cannot be turned off while the organization requires SSO."`
	IdP     httpx.Patch[string] `json:"idp" doc:"The identity provider's product (entra, okta, google, onelogin, ping, or other), or null to clear it to not known; absent keeps it."`
}

var opConnectionUpdate = httpx.NewJSONOp[ssoConnectionUpdateRequest, ssoConnectionResponse](httpx.OpSpec{
	Pattern:     "PATCH /api/sso/connection",
	ID:          "update_sso_connection",
	Summary:     "Update the SSO connection",
	Description: "Turns the organization's SSO connection on or off, or sets or clears which identity provider product it is, and returns the connection as changed. A body that names neither field is refused." + ssoAccess,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// PATCH /api/sso/connection  body: { "enabled": <bool>, "idp": <string|null> }
//
// Field-by-field: absent keeps, and idp additionally takes an explicit null
// to clear (back to "not known"). A body naming neither field is refused —
// a PATCH that wrote nothing must not answer as if it had.
func (h *ssoConnectionHandler) handleSSOConnectionUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	body, ok := opConnectionUpdate.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	enabledValue, enabledState := body.Enabled.ReadNonNull(&v, "enabled")
	idpValue, idpState := body.IdP.Read(&v, "idp")
	if enabledState == httpx.PatchAbsent && idpState == httpx.PatchAbsent {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: provide enabled and/or idp (null clears idp)",
		})
		return
	}
	if enabledState == httpx.PatchClear {
		v.Invalid("enabled", "enabled must be true or false; it has no unset state for null to clear it to")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	// A null reads as the empty product, which clears it to not known.
	var idp *string
	if idpState != httpx.PatchAbsent {
		if idpState == httpx.PatchSet && !ssostore.ValidIdP(idpValue) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidField, Field: "idp",
				Message: "idp must be one of " + strings.Join(ssostore.IdPValues, ", "),
			})
			return
		}
		idp = &idpValue
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
		httpx.NotFound(w, "sso connection")
		return
	}

	var updated *ssostore.SSOConnection
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		sso := ssostore.FromTx(tx)
		// Locked first, so a concurrent Require SSO toggle commits before or
		// after this read rather than between it and the Update below: the
		// disable refusal turns on the enforced flag this reads.
		if e := sso.Connections.LockForOrg(r.Context(), orgID); e != nil {
			return e
		}
		// Re-read inside the tx: the audit row turns on whether this call
		// actually flipped the switch. The PATCH is idempotent, so an admin
		// re-saving an already-enabled connection must not read back as a
		// second enablement.
		before, e := sso.Connections.GetByID(r.Context(), orgID, existing.ID)
		if e != nil {
			return e
		}
		if before == nil {
			return errNoConnection
		}
		// Enabled keeps its stored value when the body omits it: the store's
		// Update writes the whole mutable set, so "absent" is spelled by
		// passing what is already there.
		enabled := before.Enabled
		if enabledState == httpx.PatchSet {
			enabled = enabledValue
		}
		// A disabled connection is one nobody can sign in through, so while it
		// is required that would leave only break-glass accounts able to act in
		// the org. Requirement first, then the connection.
		if enabledState == httpx.PatchSet && !enabled && before.Enforced {
			return errTurnOffEnforcementFirst
		}
		if e := sso.Connections.Update(r.Context(), orgID, existing.ID, ssostore.UpdateSSOConnectionParams{
			Enabled: enabled,
			IdP:     idp,
		}); e != nil {
			return e
		}
		// Only the enabled flip is audited: which product the IdP is names
		// nothing about who can get in, so correcting it is not an access
		// change.
		if before.Enabled != enabled {
			action := domain.AccessActionSSOConnectionDisabled
			if enabled {
				action = domain.AccessActionSSOConnectionEnabled
			}
			if e := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
				ActorUserID: userID,
				Action:      action,
				DetailJSON:  domain.AccessDetailSSOConnection(before.ProviderID),
			}); e != nil {
				return fmt.Errorf("audit sso connection update: %w", e)
			}
		}
		updated, e = sso.Connections.GetByID(r.Context(), orgID, existing.ID)
		return e
	}); err != nil {
		if errors.Is(err, errNoConnection) {
			httpx.NotFound(w, "sso connection")
			return
		}
		if errors.Is(err, errTurnOffEnforcementFirst) {
			turnOffEnforcementFirst(w)
			return
		}
		httpx.InternalError(w, "sso", err)
		return
	}

	opConnectionUpdate.Write(w, ssoConnectionResponse{
		Connection: toSSOConnectionView(updated),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

// ssoEnforcementUpdateRequest is the body of PATCH
// /api/sso/connection/enforcement.
type ssoEnforcementUpdateRequest struct {
	Enforced *bool `json:"enforced" doc:"Whether the organization requires SSO. Required. Turning it on needs an enabled connection, a successful test sign-in, a verified domain, and a caller who signed in through the connection or is a break-glass principal."`
}

var opEnforcementUpdate = httpx.NewJSONOp[ssoEnforcementUpdateRequest, ssoConnectionResponse](httpx.OpSpec{
	Pattern:     "PATCH /api/sso/connection/enforcement",
	ID:          "update_sso_connection_enforcement",
	Summary:     "Require or stop requiring SSO",
	Description: "Sets whether the organization requires SSO, which while on lets members other than break-glass principals reach the organization only by signing in through its connection, and returns the connection as changed." + ssoAccess,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// PATCH /api/sso/connection/enforcement  body: { "enforced": <bool> }
func (h *ssoConnectionHandler) handleSSOEnforcementUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	body, ok := opEnforcementUpdate.Decode(w, r)
	if !ok {
		return
	}
	if body.Enforced == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "enforced is required", Field: "enforced"})
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
		httpx.NotFound(w, "sso connection")
		return
	}

	var updated *ssostore.SSOConnection
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		sso := ssostore.FromTx(tx)
		// The gate checks below read the connection, the org's domains and its
		// break-glass list, and a concurrent disable, domain removal or
		// break-glass removal writes them. The lock makes each of those commit
		// before these reads or wait until this transaction ends, so the checks
		// never pass against state another change is replacing.
		if e := sso.Connections.LockForOrg(r.Context(), orgID); e != nil {
			return e
		}
		// Hoisted out of the enforce-ON branch because the audit row below
		// needs the prior enforced value on BOTH directions.
		conn, e := sso.Connections.GetByID(r.Context(), orgID, existing.ID)
		if e != nil {
			return e
		}
		if conn == nil {
			return errNoConnection
		}
		if *body.Enforced {
			// Gate ON only behind a proven-working connection.
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
			// Requiring SSO bars every request that did not come through this
			// connection, the caller's own included. An admin who is neither
			// signed in through it nor break-glass would lock themselves out
			// with their next request, so the switch refuses them. Checked
			// after the seed, which can make the owner break-glass right here.
			if httpx.SignInProviderFrom(r.Context()) != conn.ProviderID {
				isBG, e := sso.BreakGlass.Includes(r.Context(), orgID, userID)
				if e != nil {
					return e
				}
				if !isBG {
					return errEnforceLocksOutCaller
				}
			}
		}
		if e := sso.Connections.SetEnforced(r.Context(), orgID, existing.ID, *body.Enforced); e != nil {
			return e
		}
		// Requiring SSO is the single widest-reaching access change this
		// surface makes — it decides whether a password/GitHub login still
		// works for the whole org — so it records in both directions, but only
		// when the value actually moved.
		if conn.Enforced != *body.Enforced {
			action := domain.AccessActionSSOEnforcementDisabled
			if *body.Enforced {
				action = domain.AccessActionSSOEnforcementEnabled
			}
			if e := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
				ActorUserID: userID,
				Action:      action,
				DetailJSON:  domain.AccessDetailSSOConnection(conn.ProviderID),
			}); e != nil {
				return fmt.Errorf("audit sso enforcement: %w", e)
			}
		}
		updated, e = sso.Connections.GetByID(r.Context(), orgID, existing.ID)
		return e
	}); err != nil {
		switch {
		case errors.Is(err, errEnforceNotEnabled):
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "enable the connection before requiring SSO"})
		case errors.Is(err, errEnforceNotTested):
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "run a successful Test before requiring SSO"})
		case errors.Is(err, errEnforceNoVerifiedDomain):
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "verify a domain before requiring SSO"})
		case errors.Is(err, errEnforceLocksOutCaller):
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "Sign in through single sign-on, or be designated break-glass, before requiring it."})
		case errors.Is(err, errNoConnection):
			httpx.NotFound(w, "sso connection")
		default:
			httpx.InternalError(w, "sso", err)
		}
		return
	}

	opEnforcementUpdate.Write(w, ssoConnectionResponse{
		Connection: toSSOConnectionView(updated),
		EntityID:   entityID,
		ACSURL:     acsURL,
	})
}

var (
	// errNoConnection: the connection vanished between a handler's pre-read and
	// its transaction. Shared by the enable/disable and enforce paths, which
	// both re-read inside the tx and both answer 404.
	errNoConnection            = errors.New("sso: no connection")
	errEnforceNotEnabled       = errors.New("sso: connection not enabled")
	errEnforceNotTested        = errors.New("sso: connection not tested")
	errEnforceNoVerifiedDomain = errors.New("sso: no verified domain")
	errEnforceLocksOutCaller   = errors.New("sso: caller neither signed in through sso nor break-glass")
	// errTurnOffEnforcementFirst: a change that would leave Require SSO on
	// with nothing to sign in through — disabling the connection, or removing
	// its last verified domain. Shared by both, which answer the same 409.
	errTurnOffEnforcementFirst = errors.New("sso: require sso is on")
)

// turnOffEnforcementFirst is the 409 for errTurnOffEnforcementFirst.
func turnOffEnforcementFirst(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "Turn off Require SSO first."})
}

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
		return "", fmt.Errorf("create sso provider: http %d: %s", resp.StatusCode, upstream.Excerpt(respBody))
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
