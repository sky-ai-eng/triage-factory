package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// modelsHandler serves the models this deployment offers, at the two scopes
// that shape the answer, plus the writes that shape them:
//
//   - GET /api/orgs/{org_id}/models — as one org sees them.
//   - GET /api/teams/{team_id}/models — as one team may run them.
//   - PUT /api/teams/{team_id}/models/providers — the org admin's restriction.
//   - POST /api/orgs/{org_id}/models/{model_key}/test — verify one model.
//   - POST /api/orgs/{org_id}/models/tests — verify one provider's candidates.
//
// The subject is the org or the team, never the caller, because the answer
// depends on it: `enabled` is org state and the provider restriction is team
// state, and an admin belonging to several of either must be able to read each
// without first moving a session cursor.
//
// Read is any member, not admin. The widest audience for this read is a team
// admin who cannot read org settings at all but has to know what the org
// enabled, because a team's own model choices are drawn from that set — gate it
// on admin and the team page has nothing to draw. The two test routes are org
// admin, because each one spends the org's money.
type modelsHandler struct {
	az *authz.Checker
	tx db.TxRunner
	// prober runs one paid request against the org's credentials to establish
	// whether a model is invocable. A getter (not a captured value) because
	// routes register before the app injects it, so the closure over the
	// server's field is what lets a route registered first read a value set
	// later.
	prober func() modelProber
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
// display_order is presentation only — the registry file's order — and asserts
// nothing about capability. There is no rank or tier field, and adding one
// would require a defensible ordering over models that does not exist.
//
// One contract, two universes, and the difference travels as ABSENT FIELDS
// rather than as a mode a client reads. A native row names the provider that
// serves it and joins the pricing datasheet, so it carries provider, prices and
// the two window/caching facts. An SDK row carries none of them: the harness
// resolves the alias against whichever access path its environment selects, so
// the provider is a property of the credential rather than of the id, and the
// cost is settled by the harness rather than interpolated from a price table —
// publishing a zero, or the provider TF guessed, would be a claim nothing backs.
// A client renders what is present and says nothing about what is not.
type modelCatalogRow struct {
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	// Provider is absent where the id does not name one. See the type doc.
	Provider string `json:"provider,omitempty"`
	Enabled  bool   `json:"enabled"`
	// PricesPerMTok is absent where cost is harness-settled. See the type doc.
	PricesPerMTok         *modelPricesPerMTok `json:"prices_per_mtok,omitempty"`
	ContextWindow         int                 `json:"context_window,omitempty"`
	SupportsPromptCaching bool                `json:"supports_prompt_caching,omitempty"`
	// Availability is the whole triple's presence gate: it is absent — with the
	// two fields below — when this org's credentials are not TF-owned, because
	// a verdict about the machine an agent happens to run on has no stable
	// subject. See modelaccess.Credentials.BringsOwn.
	Availability string `json:"availability,omitempty"`
	// AvailabilityDetail is the provider's own refusal, present only on "red".
	// It is what turns an unavailable badge into something an admin can act
	// on: "not entitled in this account" and "this id does not exist" are the
	// same badge and completely different fixes.
	AvailabilityDetail string `json:"availability_detail,omitempty"`
	// AvailabilityCheckedAt is when the probe behind this state ran. Absent
	// for "unconfigured" and "unverified", which are the states no probe
	// produced.
	AvailabilityCheckedAt *time.Time `json:"availability_checked_at,omitempty"`
	DisplayOrder          int        `json:"display_order"`
}

type modelCatalogResponse struct {
	Items []modelCatalogRow `json:"items"`
}

// The availability vocabulary this read publishes. Four values, published
// whenever the org's credentials are TF-owned — always in multi mode, and in
// local once the org binds its own — and no value at all when they are not: a
// stored verdict is a fact about a credential, and the host environment an
// agent authenticates from is not one TF can name, watch, or invalidate.
const (
	// modelAvailabilityUnconfigured — this org brings its own credentials and
	// holds none for this model's provider, so nothing can invoke it and no
	// probe is worth spending. Not a probe result: a local, certain fact,
	// derived from what is bound and from the org's recorded credential source
	// — an org running on the host's has no provider to be missing.
	//
	// An org that brings its own and has bound NOTHING is this for every model,
	// in either mode, and the dispatch gates agree: a run there would otherwise
	// authenticate from whatever the operator's environment holds, spending
	// against a credential nobody configured.
	//
	// It outranks every other value, INCLUDING a stored green — a credential
	// unbound after a successful probe leaves a row that was true when written
	// and is not true now, and the derived fact is the one that can still be
	// checked. Least-fixable-first, the same ordering internal/eventsource
	// uses, and the same argument modelaccess.Check already makes internally:
	// sending someone to test a provider they never connected is sending them
	// to do useless work.
	modelAvailabilityUnconfigured = "unconfigured"
	// modelAvailabilityVerified — a probe invoked this model with this org's
	// credentials and it answered. Permanent: nothing re-probes on a timer.
	modelAvailabilityVerified = "verified"
	// modelAvailabilityRed — a probe was refused. Carries the provider's own
	// message in availability_detail.
	modelAvailabilityRed = "red"
	// modelAvailabilityUnverified — the org has connected a credential that
	// could invoke this model and no probe has concluded anything yet. It is
	// deliberately not distinguished from "every attempt timed out": both mean
	// nobody has established anything, and both are fixed by pressing test
	// again — which is exactly why an unconnected provider must not land here,
	// since testing one is refused rather than inconclusive.
	modelAvailabilityUnverified = "unverified"
)

// handleModelsList returns the models the org may pick from.
//
// Unpaginated by design: the universe is the build's own vocabulary, fixed at
// compile time and a handful of rows long, and a page token would address a set
// that changes only when the binary does. Same call as GET /api/event-types.
//
// Local mode answers the identical contract from its own universe: the Claude
// Code SDK's alias list rather than the native registry, because that harness is
// what executes a local conversation and its vocabulary is what a local row
// stores and sends. Offering a concrete wire id there would be a lie the picker
// tells, and offering it under the provider spellings that id implies would ask
// the user a question their environment has already answered. The difference
// belongs in this data — absent fields — never in a mode branch in the client.
//
// GET /api/orgs/{org_id}/models
func (h *modelsHandler) handleModelsList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	avail, ok := h.availability(w, r, orgID, userID)
	if !ok {
		return
	}

	// The org's stored enable-set has no column yet, so every org resolves to
	// the default set — every model in the universe. The seam is this call, not
	// a branch here: filling in the stored value changes the argument and
	// nothing else.
	// TODO(TFAC-703): pass the org's org_settings.enabled_models here.
	universe := deploymentUniverse()
	enabled := universe.Enabled(nil)

	models := universe.Models()
	items := make([]modelCatalogRow, 0, len(models))
	for _, m := range models {
		items = append(items, catalogRow(m, enabled.Has(m.Key), avail))
	}
	writeJSON(w, http.StatusOK, modelCatalogResponse{Items: items})
}

// deploymentUniverse is the handler surface's one door onto what this
// deployment may offer or store.
//
// One helper rather than a modelcatalog call per site, so the models read, every
// model validator beside it and the probe routes cannot end up asking about
// different universes. modelcatalog takes the mode as a parameter and reads no
// ambient mode of its own; this is the single place in this package that
// supplies it.
func deploymentUniverse() modelcatalog.Universe {
	return modelcatalog.UniverseFor(runmode.Current() == runmode.ModeMulti)
}

// catalogRow projects one catalog entry onto the wire shape both scopes serve,
// so the org read and the team read cannot describe the same model
// differently — availability included, which is org truth and therefore
// identical at both scopes. A team restriction removes an entry from the team
// read; it never changes what the remaining entries say.
func catalogRow(m modelcatalog.Model, enabled bool, avail availabilityIndex) modelCatalogRow {
	state, detail, checkedAt := avail.forModel(m)
	row := modelCatalogRow{
		Key:                   m.Key,
		DisplayName:           m.DisplayName,
		Provider:              m.Provider,
		Enabled:               enabled,
		Availability:          state,
		AvailabilityDetail:    detail,
		AvailabilityCheckedAt: checkedAt,
		DisplayOrder:          m.DisplayOrder,
	}
	if f := m.Facts; f != nil {
		row.PricesPerMTok = &modelPricesPerMTok{
			Input:      f.Prices.Input,
			Output:     f.Prices.Output,
			CacheRead:  f.Prices.CacheRead,
			CacheWrite: f.Prices.CacheWrite,
		}
		row.ContextWindow = f.ContextWindow
		row.SupportsPromptCaching = f.SupportsPromptCaching
	}
	return row
}

// handleTeamModelsList returns the models one TEAM may run on: the org's read
// minus the providers an org admin restricted this team from spending against.
//
// A restriction narrows the native universe only. On the SDK path no id names an
// access path — the harness resolves one from the credential — so there is
// nothing for a restriction over access paths to match, and the team read is the
// org read.
//
// The same node name at a second scope, the way /usage is mounted at
// /api/me, /api/teams/{id} and /api/orgs/{id}: a caller who found the org's
// catalog can predict where a team's lives. It is a separate path rather than a
// filter parameter on the org read because the subject genuinely differs — the
// answer depends on the {team_id}, so a session cursor could not address it and
// a token caller could not reach it at all.
//
// Restricted models are omitted rather than flagged. The list's job is to be
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

	// The restriction is read on the admin pool, after the gate above has
	// established the team is in the caller's org. It is org policy ABOUT a
	// team rather than the team's own configuration, and its first reader is
	// the org admin who set it — who need not be a member of the team, and
	// whom the membership-gated team_settings RLS would silently answer with
	// the defaults, showing them an unrestricted catalog seconds after they
	// restricted it. A wrong answer here is worse than a wider one: what it
	// discloses to another org member is which providers a sibling team may
	// spend against.
	var allowed modelcatalog.ProviderSet
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		set, err := tx.Teams.GetSettingsSystem(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("read team settings: %w", err)
		}
		allowed = modelcatalog.AllowedProviders(set.AllowedProviders)
		return nil
	}); err != nil {
		internalError(w, "models", err)
		return
	}

	avail, ok := h.availability(w, r, orgID, userID)
	if !ok {
		return
	}

	// TODO(TFAC-703): pass the org's org_settings.enabled_models here, as the
	// org-scoped read will.
	universe := deploymentUniverse()
	enabled := universe.Enabled(nil)

	models := universe.Models()
	items := make([]modelCatalogRow, 0, len(models))
	for _, m := range models {
		// A model whose id names no provider is outside what a provider
		// restriction can narrow: the restriction ranks credential paths, and
		// an alias resolves its path from the credential the deployment
		// supplies rather than from the id. Removing one here would filter on a
		// provider nobody asserted it was served by.
		if m.Provider != "" && !allowed.Has(m.Provider) {
			continue
		}
		items = append(items, catalogRow(m, enabled.Has(m.Key), avail))
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
