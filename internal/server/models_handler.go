package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
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
		items = append(items, modelCatalogRow{
			Key:         e.Key,
			DisplayName: e.DisplayName,
			Provider:    e.Provider,
			Enabled:     enabled.Has(e.Key),
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
		})
	}
	writeJSON(w, http.StatusOK, modelCatalogResponse{Items: items})
}
