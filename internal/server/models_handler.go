package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
//   - POST /api/orgs/{org_id}/models/{model_key}/test — verify one model.
//   - POST /api/orgs/{org_id}/models/tests — verify one provider's candidates.
//
// The subject is the org or the team, never the caller, because the answer
// depends on it: `enabled` is the org's enable-set and the team read narrows to
// the team's, and an admin belonging to several of either must be able to read
// each without first moving a session cursor.
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

	var orgSet domain.OrgSettings
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		orgSet, err = tx.Orgs.GetSettings(r.Context(), orgID)
		return err
	}); err != nil {
		internalError(w, "models", err)
		return
	}
	universe := deploymentUniverse()
	enabled := domain.OrgModelSet(orgSet.EnabledModels, universe.DefaultEnabled())

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
// identical at both scopes. A team's enable-set removes an entry from the team
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

// handleTeamModelsList returns the models one TEAM may run on: the org's
// enable-set narrowed to the team's own.
//
// The same node name at a second scope, the way /usage is mounted at
// /api/me, /api/teams/{id} and /api/orgs/{id}: a caller who found the org's
// catalog can predict where a team's lives. It is a separate path rather than a
// filter parameter on the org read because the subject genuinely differs — the
// answer depends on the {team_id}, so a session cursor could not address it and
// a token caller could not reach it at all.
//
// Models outside the team's set are omitted rather than flagged. The list's job
// is to be the picker's options, and a model this team may not pick is not an
// option; the set itself is readable on the team's settings, which is where an
// admin goes to change it.
//
// GET /api/teams/{team_id}/models
func (h *modelsHandler) handleTeamModelsList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeam(w, r)
	if !ok {
		return
	}

	// The team's set is read on the admin pool, after the gate above has
	// established the team is in the caller's org. Its first reader is an org
	// admin comparing what a team may run against what the org enabled — who
	// need not be a member of the team, and whom the membership-gated
	// team_settings RLS would silently answer with the defaults, showing them an
	// unnarrowed catalog. A wrong answer here is worse than a wider one: what it
	// discloses to another org member is which models a sibling team may spend
	// on.
	var enabled domain.ModelSet
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("read org settings: %w", err)
		}
		teamSet, err := tx.Teams.GetSettingsSystem(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("read team settings: %w", err)
		}
		enabled = domain.TeamModelSet(teamSet.EnabledModels,
			domain.OrgModelSet(orgSet.EnabledModels, deploymentUniverse().DefaultEnabled()))
		return nil
	}); err != nil {
		internalError(w, "models", err)
		return
	}

	avail, ok := h.availability(w, r, orgID, userID)
	if !ok {
		return
	}

	models := deploymentUniverse().Models()
	items := make([]modelCatalogRow, 0, len(models))
	for _, m := range models {
		if !enabled.Has(m.Key) {
			continue
		}
		items = append(items, catalogRow(m, true, avail))
	}
	writeJSON(w, http.StatusOK, modelCatalogResponse{Items: items})
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
// 400 INVALID_FIELD on the named field: the value is well-formed and the write
// is well-addressed, but it names a model this organization cannot
// authenticate. The message carries the remedy — connect the provider.
func writeModelAccessError(w http.ResponseWriter, err error, field string) bool {
	if !errors.Is(err, modelaccess.ErrProviderUnconfigured) {
		return false
	}
	httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
		Reason: httpx.ReasonInvalidField, Message: err.Error(), Field: field,
	})
	return true
}
