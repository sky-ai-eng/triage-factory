// Package models serves the models this deployment offers — the catalog read
// at org and team scope and the paid availability probes — and is the one
// door the rest of the server takes onto which model universe this deployment
// may offer or store (DeploymentUniverse).
package models

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Handler serves the models this deployment offers, at the two scopes
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
type Handler struct {
	az *authz.Checker
	tx db.TxRunner
	// prober runs one paid request against the org's credentials to establish
	// whether a model is invocable. A getter (not a captured value) because
	// routes register before the app injects it, so the closure over the
	// server's field is what lets a route registered first read a value set
	// later.
	prober func() Prober
}

// New builds the models handler. prober is read on every probe request, not
// once here; see Handler.prober.
func New(az *authz.Checker, tx db.TxRunner, prober func() Prober) *Handler {
	return &Handler{az: az, tx: tx, prober: prober}
}

// modelPricesPerMTok is a model's headline rates in dollars per million tokens.
// They are base-tier list prices for display and comparison; what a run
// actually cost is recorded per message in the ledger and never derived here.
type modelPricesPerMTok struct {
	Input      float64 `json:"input" doc:"The list price of input tokens, in USD per million tokens."`
	Output     float64 `json:"output" doc:"The list price of output tokens, in USD per million tokens."`
	CacheRead  float64 `json:"cache_read" doc:"The list price of input tokens read from the prompt cache, in USD per million tokens."`
	CacheWrite float64 `json:"cache_write" doc:"The list price of input tokens written to the prompt cache, in USD per million tokens."`
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
	Key         string `json:"key" doc:"The model's id, the value a model setting stores."`
	DisplayName string `json:"display_name" doc:"The model's name for display."`
	// Provider is absent where the id does not name one. See the type doc.
	Provider string `json:"provider,omitempty" doc:"The provider that serves the model, anthropic or bedrock, absent in local mode, where the credential the agent runs under decides it."`
	// ProviderDisplayName is how a person reads Provider, and travels beside it
	// so a client renders the name TF uses rather than keeping its own map of
	// the provider vocabulary — a second copy that can only ever fall behind
	// this one. Absent exactly when Provider is.
	ProviderDisplayName string `json:"provider_display_name,omitempty" doc:"The provider's name for display, such as Amazon Bedrock, absent exactly when provider is."`
	// Enabled is the enable-set's answer and says nothing about credentials:
	// whether the organization, or on the team read the team, may pick this
	// model. It is true on every row of the team read, which omits rows
	// outside the team's set.
	Enabled bool `json:"enabled" doc:"Whether the organization, or on a team's list the team, may choose the model; true on every row of a team's list, which leaves out the models the team may not choose."`
	// PricesPerMTok is absent where cost is harness-settled. See the type doc.
	PricesPerMTok *modelPricesPerMTok `json:"prices_per_mtok,omitempty" doc:"The model's list prices, absent in local mode, where the agent runtime settles what a run costs."`
	ContextWindow int                 `json:"context_window,omitempty" doc:"The most tokens the model reads in one request, absent in local mode."`
	// SupportsPromptCaching is a POINTER because false is an answer here and
	// absent is not one: a model that cannot cache its prompt costs several
	// times more to run, which a client has to say out loud, and a bare bool
	// with omitempty would publish that fact and "TF makes no claim" as the
	// same missing field.
	SupportsPromptCaching *bool `json:"supports_prompt_caching,omitempty" doc:"Whether the model can cache its prompt, absent in local mode, where Triage Factory makes no claim either way."`
	// Availability is the whole triple's presence gate: it is absent — with the
	// two fields below — when this org's credentials are not TF-owned, because
	// a verdict about the machine an agent happens to run on has no stable
	// subject. See modelaccess.Credentials.BringsOwn. It is present on every
	// row for an organization that brings its own credentials, which is every
	// organization in multi mode.
	//
	// It is the one field a caller reads to learn whether work can start on
	// this model:
	//
	//   - "unconfigured" is refused with 409 NOT_CONFIGURED by the message
	//     route and by delegate, and at the claim, which parks the
	//     conversation.
	//   - Every other row is accepted by those doors: "verified",
	//     "unverified", "red", and a row with no availability. "red" and
	//     "unverified" are advisory, because a stored probe verdict can be
	//     stale in either direction, and the server never refuses on one.
	//   - The team-default and background-jobs saves refuse an
	//     "unconfigured" model with the same status, except for an
	//     organization that brings its own credentials and has connected
	//     none: setup picks a model before it connects a provider, and
	//     nothing runs for that organization until it connects one.
	//   - The enabled-set writes and a prompt's model save do not consult
	//     it. They store a choice for later, and the doors above decide when
	//     the work starts.
	Availability string `json:"availability,omitempty" doc:"Whether the organization's credentials can run the model: unconfigured when no credential for its provider is connected, unverified before a test has concluded, verified after a test ran it, or red after the provider refused a test; absent while the organization runs on the credentials of the machine hosting it. Work on an unconfigured model is refused, and every other value is advisory."`
	// AvailabilityDetail is the provider's own refusal, present only on "red".
	// It is what turns an unavailable badge into something an admin can act
	// on: "not entitled in this account" and "this id does not exist" are the
	// same badge and completely different fixes.
	AvailabilityDetail string `json:"availability_detail,omitempty" doc:"The provider's message refusing the model, present only when availability is red."`
	// AvailabilityCheckedAt is when the probe behind this state ran. Absent
	// for "unconfigured" and "unverified", which are the states no probe
	// produced.
	AvailabilityCheckedAt *time.Time `json:"availability_checked_at,omitempty" doc:"When the test behind availability ran, as an RFC 3339 timestamp, present only when availability is verified or red."`
	// EffortLevels are the reasoning efforts a message on this model may ask
	// for, lowest first, as their wire values. Always present: an empty list
	// is a model that takes no effort, which a composer answers by hiding the
	// control, and an absent field would say nothing.
	EffortLevels []string `json:"effort_levels" doc:"The reasoning efforts a message on the model may ask for, lowest first, from low, medium, high, xhigh, and max; an empty array when the model takes none."`
	DisplayOrder int      `json:"display_order" doc:"The model's position when the models are listed for display, which says nothing about capability."`
}

type modelCatalogResponse struct {
	Items []modelCatalogRow `json:"items" doc:"The models, in display order."`
}

var opModelsList = httpx.NewJSONOp[httpx.NoBody, modelCatalogResponse](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/models",
	ID:          "list_org_models",
	Summary:     "List the models an organization may use",
	Description: "Lists every model this deployment offers, with whether the organization has enabled it, its prices and limits, and whether the organization's credentials can run it. The list is not paginated: it holds the deployment's fixed set of models.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	// checked. Least-fixable-first, the same ordering internal/eventing/eventsource
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
func (h *Handler) handleModelsList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	avail, ok := h.availability(w, r, orgID, userID)
	if !ok {
		return
	}

	var orgSet domain.OrgSettings
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		orgSet, err = tx.Orgs.GetSettings(r.Context(), orgID)
		return err
	}); err != nil {
		httpx.InternalError(w, "models", err)
		return
	}
	universe := DeploymentUniverse()
	enabled := domain.OrgModelSet(orgSet.EnabledModels, universe.DefaultEnabled())

	models := universe.Models()
	items := make([]modelCatalogRow, 0, len(models))
	for _, m := range models {
		items = append(items, catalogRow(m, enabled.Has(m.Key), avail))
	}
	opModelsList.Write(w, modelCatalogResponse{Items: items})
}

// DeploymentUniverse is the handler surface's one door onto what this
// deployment may offer or store.
//
// One helper rather than a modelcatalog call per site, so the models read, every
// model validator elsewhere in the server and the probe routes cannot end up
// asking about different universes. modelcatalog takes the mode as a parameter
// and reads no ambient mode of its own; this is the single place in the server
// that supplies it.
func DeploymentUniverse() modelcatalog.Universe {
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
		EffortLevels:          append([]string{}, m.EffortLevels...),
		DisplayOrder:          m.DisplayOrder,
	}
	if m.Provider != "" {
		row.ProviderDisplayName = modelcatalog.ProviderDisplayName(m.Provider)
	}
	if f := m.Facts; f != nil {
		row.PricesPerMTok = &modelPricesPerMTok{
			Input:      f.Prices.Input,
			Output:     f.Prices.Output,
			CacheRead:  f.Prices.CacheRead,
			CacheWrite: f.Prices.CacheWrite,
		}
		row.ContextWindow = f.ContextWindow
		caching := f.SupportsPromptCaching
		row.SupportsPromptCaching = &caching
	}
	return row
}

var opTeamModelsList = httpx.NewJSONOp[httpx.NoBody, modelCatalogResponse](httpx.OpSpec{
	Pattern:     "GET /api/teams/{team_id}/models",
	ID:          "list_team_models",
	Summary:     "List the models a team may use",
	Description: "Lists the models the team may choose, which are those the organization enabled narrowed to the team's own set, with their prices and limits and whether the organization's credentials can run them. The list is not paginated.",
	PathParams:  map[string]string{"team_id": "The team's id, or default for the one team in local mode."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
func (h *Handler) handleTeamModelsList(w http.ResponseWriter, r *http.Request) {
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
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("read org settings: %w", err)
		}
		teamSet, err := tx.Teams.GetSettingsSystem(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("read team settings: %w", err)
		}
		enabled = domain.TeamModelSet(teamSet.EnabledModels,
			domain.OrgModelSet(orgSet.EnabledModels, DeploymentUniverse().DefaultEnabled()))
		return nil
	}); err != nil {
		httpx.InternalError(w, "models", err)
		return
	}

	avail, ok := h.availability(w, r, orgID, userID)
	if !ok {
		return
	}

	models := DeploymentUniverse().Models()
	items := make([]modelCatalogRow, 0, len(models))
	for _, m := range models {
		if !enabled.Has(m.Key) {
			continue
		}
		items = append(items, catalogRow(m, true, avail))
	}
	opTeamModelsList.Write(w, modelCatalogResponse{Items: items})
}

// resolveTeam is the shared prefix of the two team-scoped model routes: the
// session's org, the path's team, and the check that the team is in that org so
// a cross-org id 404s rather than reaching a store.
func (h *Handler) resolveTeam(w http.ResponseWriter, r *http.Request) (orgID, userID, teamID string, ok bool) {
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", "", "", false
	}
	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil {
		httpx.WriteUnauth(w)
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

// WriteAccessError renders a modelaccess refusal as the fault it is,
// returning false for anything else so the caller falls through to its own
// error handling.
//
// 409 NOT_CONFIGURED on the named field, the status the message route and
// delegate give the same fault: the value is well-formed and the write is
// well-addressed, and the organization's credentials as they stand refuse it.
// The message carries the remedy — connect the provider. The item is written
// directly because httpx.WriteNotConfigured carries no field.
func WriteAccessError(w http.ResponseWriter, err error, field string) bool {
	if !errors.Is(err, modelaccess.ErrProviderUnconfigured) {
		return false
	}
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason: httpx.ReasonNotConfigured, Message: err.Error(), Field: field,
	})
	return true
}

// Register mounts the models routes.
func (h *Handler) Register(m httpx.Mounts) {
	// The models this deployment offers, as one org sees them: the shipped
	// catalog joined to the org's own enable-set. Org-scoped because that
	// enable-set is org state, and member-level because its widest reader is a
	// team admin who cannot see org settings but must know what the org
	// enabled. Unpaginated for the same reason /api/event-types is — a
	// compile-time vocabulary, not a collection that grows with use.
	//
	// The team-scoped read is the same node one scope down (as /usage is
	// mounted at both): the org's enable-set narrowed to that team's own. Both
	// sets are WRITTEN on their scope's settings resource — they are ordinary
	// columns, not effects a field write cannot express — so there is no write
	// verb here.
	m.Op(opModelsList, h.handleModelsList)
	m.Op(opTeamModelsList, h.handleTeamModelsList)
	// The availability probes: the same verb on the item and on the
	// collection, because "test this model" and "test this provider's
	// candidates" are two nameable things with two response shapes. Both are
	// POSTs for an effect no field write can express — each one calls the
	// provider and is billed for it.
	m.Op(opModelTest, h.handleModelTest)
	m.Op(opModelTestSweep, h.handleModelTestSweep)
}
