package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// modelsHandler serves GET /api/orgs/{org_id}/models — the models this
// deployment offers, as one org sees them.
//
// The subject is the org, not the caller, because the answer depends on the
// org: `enabled` is org state, and an org admin belonging to several orgs must
// be able to read each one's catalog without first moving a session cursor.
//
// Read is any member, not admin. The widest audience for this read is a team
// admin who cannot read org settings at all but has to know what the org
// enabled, because a team's own model choices are drawn from that set — gate it
// on admin and the team page has nothing to draw.
type modelsHandler struct {
	az *authz.Checker
	tx db.TxRunner
}

// modelPricesPerMTok is a model's headline rates in dollars per million tokens.
// They are base-tier list prices for display and comparison; what a run
// actually cost is recorded per message in the ledger and never derived here.
type modelPricesPerMTok struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// modelCatalogRow is one offered model.
//
// display_order is presentation only — the catalog file's order — and asserts
// nothing about capability. There is no rank or tier field, and adding one
// would require a defensible ordering over models that does not exist.
type modelCatalogRow struct {
	Key                   string             `json:"key"`
	DisplayName           string             `json:"display_name"`
	Provider              string             `json:"provider"`
	Enabled               bool               `json:"enabled"`
	PricesPerMTok         modelPricesPerMTok `json:"prices_per_mtok"`
	ContextWindow         int                `json:"context_window"`
	SupportsPromptCaching bool               `json:"supports_prompt_caching"`
	Availability          string             `json:"availability"`
	DisplayOrder          int                `json:"display_order"`
}

type modelCatalogResponse struct {
	Items []modelCatalogRow `json:"items"`
}

// modelAvailabilityAssumed means TF has not confirmed this credential can
// invoke this model — it is offering it on the strength of the catalog alone.
// Every entry reports it today, in both modes: nothing probes yet, and a
// deployment whose runs authenticate through a Claude Code subscription has no
// API key to probe with. It is a field rather than an omission so that
// confirming availability later changes what this read reports, not what it
// reports it in.
const modelAvailabilityAssumed = "assumed"

// handleModelsList returns the org's model catalog.
//
// Unpaginated by design: the catalog is the build's own vocabulary, fixed at
// compile time and four entries long, and a page token would address a set that
// changes only when the binary does. Same call as GET /api/event-types.
//
// Local mode answers the identical contract from the identical catalog. Its
// universe is what the SDK subprocess can actually drive — the Claude family
// via Anthropic, Bedrock, or Vertex — which every entry in the catalog
// currently is, so the two sets coincide and no filter is applied. Should the
// catalog ever name a model the SDK cannot invoke, local's universe is the
// narrower one: offering a row that nothing local can execute would be a lie
// the picker tells, and the mode difference belongs in this data, never in a
// mode branch in the client.
//
// GET /api/orgs/{org_id}/models
func (h *modelsHandler) handleModelsList(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.az.RequireOrgMember(w, r); !ok {
		return
	}

	// The org's stored enable-set has no column yet, so every org resolves to
	// the default set — every catalog entry. The seam is this call, not a
	// branch here: filling in the stored value changes the argument and
	// nothing else.
	// TODO(TFAC-703): pass the org's org_settings.enabled_models here.
	enabled := modelcatalog.Enabled(nil)

	catalog := modelcatalog.Entries()
	items := make([]modelCatalogRow, 0, len(catalog))
	for _, e := range catalog {
		items = append(items, catalogRow(e, enabled.Has(e.Key)))
	}
	writeJSON(w, http.StatusOK, modelCatalogResponse{Items: items})
}

// catalogRow projects one catalog entry onto the wire shape both scopes serve,
// so the org read and the team read cannot describe the same model differently.
func catalogRow(e modelcatalog.Entry, enabled bool) modelCatalogRow {
	return modelCatalogRow{
		Key:         e.Key,
		DisplayName: e.DisplayName,
		Provider:    e.Provider,
		Enabled:     enabled,
		PricesPerMTok: modelPricesPerMTok{
			Input:      e.Prices.Input,
			Output:     e.Prices.Output,
			CacheRead:  e.Prices.CacheRead,
			CacheWrite: e.Prices.CacheWrite,
		},
		ContextWindow:         e.ContextWindow,
		SupportsPromptCaching: e.SupportsPromptCaching,
		Availability:          modelAvailabilityAssumed,
		DisplayOrder:          e.DisplayOrder,
	}
}

// handleTeamModelsList returns the models one TEAM may run on: the org's
// catalog minus the providers an org admin restricted this team from spending
// against.
//
// The same node name at a second scope, the way /usage is mounted at
// /api/me, /api/teams/{id} and /api/orgs/{id}: a caller who found the org's
// catalog can predict where a team's lives. It is a separate path rather than a
// filter parameter on the org read because the subject genuinely differs — the
// answer depends on the {team_id}, so a session cursor could not address it and
// a token caller could not reach it at all.
//
// Restricted entries are omitted rather than flagged. The list's job is to be
// the picker's options, and a model this team cannot spend against is not an
// option; the restriction itself is readable on the team's settings, which is
// where an admin goes to change it.
//
// GET /api/teams/{team_id}/models
func (h *modelsHandler) handleTeamModelsList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeam(w, r)
	if !ok {
		return
	}

	var allowed modelcatalog.ProviderSet
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		set, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("read team settings: %w", err)
		}
		allowed = modelcatalog.AllowedProviders(set.AllowedProviders)
		return nil
	}); err != nil {
		internalError(w, "models", err)
		return
	}

	// TODO(TFAC-703): pass the org's org_settings.enabled_models here, as the
	// org-scoped read will.
	enabled := modelcatalog.Enabled(nil)

	catalog := modelcatalog.Entries()
	items := make([]modelCatalogRow, 0, len(catalog))
	for _, e := range catalog {
		if !allowed.Has(e.Provider) {
			continue
		}
		items = append(items, catalogRow(e, enabled.Has(e.Key)))
	}
	writeJSON(w, http.StatusOK, modelCatalogResponse{Items: items})
}

// teamProvidersUpdate is the PUT …/models/providers body. The list IS the
// resource: an empty list (or null) is the unrestricted state, because a
// restriction naming nothing and no restriction are the same fact.
type teamProvidersUpdate struct {
	AllowedProviders []string `json:"allowed_providers"`
}

// handleTeamProvidersPut restricts which providers a team may spend against.
//
// Org admin, and only org admin: a team that could widen its own restriction
// would not be restricted. That is the same reason the per-team spend cap is
// org-admin-only, and it is why the write goes through the admin-pool
// SetAllowedProvidersSystem — the org admin need not be a member of the team
// they are restricting. The org comes from the session (as the other
// /api/teams/{team_id} writes do), the team from the path.
//
// The restriction is forward-acting. It does not rewrite a model already stored
// as this team's default or pinned on its prompts; those are caught at the next
// dispatch, which refuses by name rather than substituting a model nobody chose.
//
// PUT /api/teams/{team_id}/models/providers
func (h *modelsHandler) handleTeamProvidersPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeam(w, r)
	if !ok {
		return
	}
	if !h.az.RequireOrgAdminRole(w, r, orgID, userID) {
		return
	}
	// An archived team runs nothing, so a restriction on one could never take
	// effect — the rest of the team-settings write family refuses these too.
	if !h.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}

	var req teamProvidersUpdate
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	providers, ok := validateProviderRestriction(w, req.AllowedProviders)
	if !ok {
		return
	}

	// The write returns the settings row it persisted, so the echo reflects what
	// actually landed rather than this caller's request body — what keeps two
	// admins racing on the same team convergent.
	var stored []string
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		set, e := tx.Teams.SetAllowedProvidersSystem(r.Context(), teamID, providers)
		if e != nil {
			return e
		}
		stored = set.AllowedProviders
		return nil
	}); err != nil {
		internalError(w, "models", err)
		return
	}
	writeJSON(w, http.StatusOK, teamProvidersUpdate{AllowedProviders: nonNil(stored)})
}

// validateProviderRestriction normalizes and checks the requested restriction.
// A provider this build cannot drive is rejected by name rather than stored: a
// restriction nothing matches is a team that can run nothing, and a typo would
// produce exactly that silently.
func validateProviderRestriction(w http.ResponseWriter, requested []string) ([]string, bool) {
	var v httpx.Validation
	out := make([]string, 0, len(requested))
	seen := map[string]bool{}
	for _, raw := range requested {
		p := strings.TrimSpace(raw)
		switch {
		case p == "":
			v.Invalid("allowed_providers", "allowed_providers must not contain an empty provider; send [] to remove the restriction")
		case !modelcatalog.KnownProvider(p):
			v.Invalid("allowed_providers", fmt.Sprintf("%q is not a provider this deployment drives: %s", p, strings.Join(modelcatalog.SupportedProviders(), ", ")))
		case seen[p]:
			// A duplicate changes nothing, but accepting it would store a list
			// that does not read back as it was sent.
			v.Invalid("allowed_providers", fmt.Sprintf("%q appears twice", p))
		default:
			seen[p] = true
			out = append(out, p)
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	return out, true
}

// resolveTeam is the shared prefix of the two team-scoped model routes: the
// session's org, the path's team, and the check that the team is in that org so
// a cross-org id 404s rather than reaching a store.
func (h *modelsHandler) resolveTeam(w http.ResponseWriter, r *http.Request) (orgID, userID, teamID string, ok bool) {
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", "", false
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return "", "", "", false
	}
	userID = claims.Subject
	teamID, ok = h.az.TeamIDFromPath(w, r, "models", orgID, userID)
	if !ok {
		return "", "", "", false
	}
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return "", "", "", false
	}
	return orgID, userID, teamID, true
}

// writeModelAccessError renders a modelaccess refusal as the fault it is,
// returning false for anything else so the caller falls through to its own
// error handling.
//
// Both are 400 INVALID_FIELD on the named field: the value is well-formed and
// the write is well-addressed, but it names a model this team cannot run. They
// stay distinguishable in the message, which carries the remedy — connect the
// provider, or ask an org admin to widen the team's restriction.
func writeModelAccessError(w http.ResponseWriter, err error, field string) bool {
	if !errors.Is(err, modelaccess.ErrProviderUnconfigured) && !errors.Is(err, modelaccess.ErrProviderRestricted) {
		return false
	}
	httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
		Reason: httpx.ReasonInvalidField, Message: err.Error(), Field: field,
	})
	return true
}
