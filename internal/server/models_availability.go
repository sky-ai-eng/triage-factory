package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/modelprobe"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// modelProber is the slice of internal/modelprobe the test endpoints call: one
// paid request establishing whether this org's credentials can invoke one
// model. A narrow interface so a handler test can answer with a canned verdict
// instead of standing up a provider.
type modelProber interface {
	Probe(ctx context.Context, orgID string, m modelcatalog.Model) (modelprobe.Result, error)
}

// availabilityIndex answers what the models read should publish for one model's
// availability, and is the single resolution every route on this handler reads
// — the catalog reads to render a badge, the test routes to refuse before
// spending anything.
//
// Two inputs. creds is a LOCAL, CERTAIN fact resolved off the settings row, so
// it needs no probe; it is modelaccess's own value rather than a second copy of
// the same derivation, which is what keeps a badge and a dispatch refusal from
// disagreeing about one org. rows are probe RESULTS, and their ABSENCE is the
// third state: an org that has probed nothing has no rows, and "we did not
// look" must not render the same as "we looked and found nothing".
type availabilityIndex struct {
	creds modelaccess.Credentials
	rows  map[string]domain.ModelAvailability
}

// availabilityRowKey addresses a stored row from a model. Both halves, because
// the stored identity is (credential family, model) — the same key reached
// through two credential paths is two independent facts. On an SDK universe the
// id names no family, so the family is the one the probe resolved against and
// the row records what THAT credential can invoke through the harness.
func availabilityRowKey(provider, modelKey string) string { return provider + "\x00" + modelKey }

// forModel renders one model's availability triple for the wire.
//
// An empty state is the fourth answer and it is not a gap: an org whose
// credentials are not TF-owned has no subject for a verdict to be about, so
// nothing is stored, nothing is derived, and the three fields are absent from
// the row entirely.
//
// Otherwise the order is the precedence, least-fixable first, and it is the same
// predicate the test routes gate on — so a green that a later disconnect
// invalidated cannot outlive the credential that earned it, and nobody is
// badged "unverified" and pointed at a test button that the routes beside this
// one refuse.
func (a availabilityIndex) forModel(m modelcatalog.Model) (state, detail string, checkedAt *time.Time) {
	if !a.creds.BringsOwn() {
		return "", "", nil
	}
	// A model whose id names no provider is reached through whichever family
	// the org bound, so "has the org connected this model's provider" has no
	// per-model answer; the org-wide one is what stands in.
	if !a.hasCredentialFor(m) {
		return modelAvailabilityUnconfigured, "", nil
	}
	row, ok := a.rows[availabilityRowKey(a.familyFor(m), m.Key)]
	if !ok {
		return modelAvailabilityUnverified, "", nil
	}
	at := row.CheckedAt
	switch row.State {
	case domain.ModelAvailabilityGreen:
		return modelAvailabilityVerified, "", &at
	case domain.ModelAvailabilityRed:
		return modelAvailabilityRed, row.Detail, &at
	}
	// A stored state this build has no word for. The schema's CHECK makes it
	// unreachable through this application; reporting it as unverified rather
	// than passing it through keeps a hand-written row from becoming a badge
	// no client knows how to draw.
	return modelAvailabilityUnverified, "", nil
}

// hasCredentialFor reports whether the org holds a credential that could invoke
// m. A model that names its provider asks about that one; an alias asks whether
// the org can authenticate at all, since the harness picks the path.
func (a availabilityIndex) hasCredentialFor(m modelcatalog.Model) bool {
	if m.Provider != "" {
		return a.creds.Has(m.Provider)
	}
	return a.creds.Ready() == nil
}

// familyFor is the credential family half of m's stored row key. An alias names
// none, so the org's bound family stands in.
//
// That is unambiguous exactly while an org binds one family. An org holding two
// resolves by this order, which is a guess rather than an answer: a verdict
// swept under the second family would be keyed where the read below never looks.
//
// TODO(TFAC-888): key on the org's chosen active path once the SDK credential
// wiring makes that a real selection rather than a precedence order.
func (a availabilityIndex) familyFor(m modelcatalog.Model) string {
	if m.Provider != "" {
		return m.Provider
	}
	for _, family := range modelcatalog.SupportedProviders() {
		if a.creds.Has(family) {
			return family
		}
	}
	return ""
}

// availability resolves everything this handler knows about what the org can
// run, in one pass, so no two routes can answer differently and no request
// reads the settings row twice.
//
// One resolution for both modes, off the same two reads: the settings row for
// what the org brings and what it has bound, and model_availability for what a
// probe has established. Only the credential source is mode-sensitive — a multi
// org always brings its own — and modelaccess.For settles that, so the mode is
// read once here rather than by anything downstream.
//
// The settings read runs on the app pool as the caller. org_settings SELECT is
// member-level, the same gate the catalog read itself carries, so a plain
// member resolves the same providers an admin does — which matters, because a
// member silently answered with the defaults would see a picker full of models
// their org cannot run.
func (h *modelsHandler) availability(w http.ResponseWriter, r *http.Request, orgID, userID string) (availabilityIndex, bool) {
	multi := runmode.Current() == runmode.ModeMulti
	var index availabilityIndex
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		set, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		index.creds = modelaccess.For(set, multi)
		stored, err := tx.ModelAvailability.List(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("read model availability: %w", err)
		}
		index.rows = make(map[string]domain.ModelAvailability, len(stored))
		for _, row := range stored {
			index.rows[availabilityRowKey(row.Provider, row.ModelKey)] = row
		}
		return nil
	}); err != nil {
		internalError(w, "models", err)
		return availabilityIndex{}, false
	}
	return index, true
}

// The per-model outcomes a test reports. The first two are also availability
// states, spelled the same way on purpose so a client can compare them
// directly; the last two name things a TEST can do that a stored state cannot.
const (
	// modelTestVerified — the probe succeeded and the row is now verified.
	modelTestVerified = modelAvailabilityVerified
	// modelTestRed — the provider refused; the row is now red and carries its
	// message.
	modelTestRed = modelAvailabilityRed
	// modelTestInconclusive — nobody answered. NOTHING was written: the row
	// stands exactly as it did, whatever it was. This is the outcome that
	// keeps one bad provider minute from turning a working model red or
	// blocking a save.
	modelTestInconclusive = "inconclusive"
	// modelTestSkipped — a sweep passed over a model already verified. Green
	// is terminal for automatic transitions, and re-testing it would spend
	// money to re-learn a permanent fact.
	modelTestSkipped = "skipped"
)

// modelTestResult is one model's test outcome — the shape both routes speak,
// so the per-row button and the post-connect sweep report a model the same way.
type modelTestResult struct {
	ModelKey string `json:"model_key"`
	Outcome  string `json:"outcome"`
	// Detail is the provider's own message on red or inconclusive. On
	// inconclusive it is the only trace of what went wrong, since nothing was
	// stored.
	Detail string `json:"detail,omitempty"`
	// CheckedAt is the stored row's stamp — this test's, or on a skip the
	// earlier one that made it verified. Absent when nothing is stored.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// modelTestSweepResponse is the sweep's answer: one result per candidate, in
// catalog order. There are no summary counts on it — every count the confirm
// dialog's aftermath wants is a filter over these items, and a second
// representation of the same numbers is one more thing that can disagree.
type modelTestSweepResponse struct {
	Provider string            `json:"provider"`
	Items    []modelTestResult `json:"items"`
}

// modelTestSweepRequest names which provider's candidates to test. Required:
// the sweep exists because connecting ONE provider's credential is what
// prompted it, and a body that meant "all of them" when the field was absent
// would make an empty object spend money on the other provider too.
type modelTestSweepRequest struct {
	Provider string `json:"provider"`
}

// handleModelTest tests one model against the org's credentials and records
// what it learns.
//
// This is the manual re-verification affordance, and it ALWAYS spends a
// request — including on a model already verified. That is the one thing that
// may overwrite a green row: green is terminal for automatic transitions
// because nothing should be re-probing on a timer, but a person pressing this
// asked for the truth, and a re-test that is refused moves the row to red.
//
// Org admin, because the request is billed to the org. A 200 is the TEST
// having run, not the model being available: a refusal is a successful test
// with a negative answer, and an inconclusive one is reported as such rather
// than as a TF failure, because the resource is fine and the caller's next
// move is to try again.
//
// POST /api/orgs/{org_id}/models/{model_key}/test
func (h *modelsHandler) handleModelTest(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	prober, ok := h.requireProbing(w)
	if !ok {
		return
	}

	model, ok := offeredModel(w, r.PathValue("model_key"))
	if !ok {
		return
	}
	avail, ok := h.availability(w, r, orgID, userID)
	if !ok {
		return
	}
	if !requireOwnCredentials(w, avail) {
		return
	}
	family := avail.familyFor(model)
	if !requireProviderConnected(w, avail, family) {
		return
	}

	result, ok := h.runProbe(w, r, prober, orgID, userID, family, model)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleModelTestSweep tests every candidate of one provider — the eager pass
// that runs when an admin connects or rotates that provider's credential.
//
// Candidates follow the universe, minus the ones already verified: on the native
// one they are the ids the named provider serves, and on an SDK one they are
// every alias, because one credential family reaches all of them there. Team
// provider restrictions do NOT narrow the set:
// availability is org truth about what the org's credentials can do, and a
// restriction is a decision about which team may spend against it — filtering
// here would make one team's restriction silently leave another team's picker
// unverified. The org's enable-set does not narrow it either, for the same
// reason: what is offered is a separate question from what works.
//
// The probes run one after another and each commits on its own, with no
// sweep-level transaction. A process that dies halfway leaves the rows it
// already wrote standing and the rest absent, which is exactly the state the
// save gate re-offers a test for — so there is nothing for a rollback to
// protect.
//
// The caller is expected to have said, before calling this, how many paid
// requests it is about to make. It can count them without a second endpoint:
// the same candidate rule reads off GET /api/orgs/{org_id}/models as the rows
// whose provider matches and whose availability is not "verified". A dedicated
// count route would be a second answer to a question that read already
// answers, and one more thing that can disagree with what the sweep does.
//
// POST /api/orgs/{org_id}/models/tests
func (h *modelsHandler) handleModelTestSweep(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	prober, ok := h.requireProbing(w)
	if !ok {
		return
	}

	var req modelTestSweepRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	provider := strings.TrimSpace(req.Provider)
	switch {
	case provider == "":
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "provider is required", Field: "provider",
		})
		return
	case !modelcatalog.KnownProvider(provider):
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField,
			Message: fmt.Sprintf("%q is not a provider this deployment drives: %s",
				provider, strings.Join(modelcatalog.SupportedProviders(), ", ")),
			Field: "provider",
		})
		return
	}
	// One resolution up front, for both the connected-provider gate and what
	// is already verified. Read before any probe so the skip decision is made
	// against the state the admin saw, not against rows this same sweep just
	// wrote.
	avail, ok := h.availability(w, r, orgID, userID)
	if !ok {
		return
	}
	if !requireOwnCredentials(w, avail) {
		return
	}
	if !requireProviderConnected(w, avail, provider) {
		return
	}

	items := []modelTestResult{}
	for _, model := range deploymentUniverse().CandidatesFor(provider) {
		if state, _, checkedAt := avail.forModel(model); state == modelAvailabilityVerified {
			items = append(items, modelTestResult{
				ModelKey: model.Key, Outcome: modelTestSkipped, CheckedAt: checkedAt,
			})
			continue
		}
		result, ok := h.runProbe(w, r, prober, orgID, userID, provider, model)
		if !ok {
			// runProbe has written the fault. Rows from earlier candidates
			// stand; they are each their own committed fact.
			return
		}
		items = append(items, result)
	}
	writeJSON(w, http.StatusOK, modelTestSweepResponse{Provider: provider, Items: items})
}

// runProbe spends one request and persists whatever it concluded, returning
// the per-model result both routes report. ok=false means it has already
// written an error response.
//
// family is the credential family the row is keyed under. It is a parameter
// rather than read off the model because an SDK alias names none — the harness
// resolves the path from the credential — so the caller, which knows which
// credential this probe was about, is the only thing that can answer.
//
// The write happens only for a conclusive verdict. An inconclusive probe
// writes nothing at all — not a row, not a timestamp — because the alternative
// is a state that means "we tried and could not tell", which every consumer
// would then have to decide how to render and which one bad provider minute
// could put on a model that works.
func (h *modelsHandler) runProbe(w http.ResponseWriter, r *http.Request, prober modelProber, orgID, userID, family string, model modelcatalog.Model) (modelTestResult, bool) {
	res, err := prober.Probe(r.Context(), orgID, model)
	if err != nil {
		if modelprobe.IsSetupGap(err) {
			// The org cannot reach this provider at all. Not a verdict about
			// the model, and nothing is recorded — the fix is a credential,
			// which is the same page the caller is already on.
			writeNotConfigured(w, err.Error())
			return modelTestResult{}, false
		}
		internalError(w, "models", fmt.Errorf("probe %s: %w", model.Key, err))
		return modelTestResult{}, false
	}

	state := ""
	switch res.Verdict {
	case modelprobe.VerdictGreen:
		state = domain.ModelAvailabilityGreen
	case modelprobe.VerdictRed:
		state = domain.ModelAvailabilityRed
	default:
		return modelTestResult{ModelKey: model.Key, Outcome: modelTestInconclusive, Detail: res.Detail}, true
	}

	var stored domain.ModelAvailability
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		row, e := tx.ModelAvailability.Record(r.Context(), orgID, family, model.Key, state, res.Detail)
		if e != nil {
			return e
		}
		stored = row
		return nil
	}); err != nil {
		// The request was made and billed; only the record of it failed. Say
		// so rather than reporting an outcome the next read will contradict.
		internalError(w, "models", fmt.Errorf("record availability for %s: %w", model.Key, err))
		return modelTestResult{}, false
	}

	// Reported from the STORED row, not from the verdict: the detail a green
	// row carries is the empty string the write imposed, and the timestamp is
	// the one a later read will show.
	checkedAt := stored.CheckedAt
	outcome := modelTestVerified
	if stored.State == domain.ModelAvailabilityRed {
		outcome = modelTestRed
	}
	return modelTestResult{
		ModelKey:  model.Key,
		Outcome:   outcome,
		Detail:    stored.Detail,
		CheckedAt: &checkedAt,
	}, true
}

// requireProbing gates both test routes on the one thing that makes a probe
// possible at all: a prober wired into this process.
//
// There is no mode gate. Both modes can spend a probe — multi from this
// process, local through the agent runtime — so the only deployment that
// cannot answer is one where the seam was never wired.
func (h *modelsHandler) requireProbing(w http.ResponseWriter) (modelProber, bool) {
	// Both nils mean the same thing and both must be caught here: the getter
	// itself is nil in a unit test that never set it, and the getter RETURNS
	// nil on a pod where SetModelProber has not run — routes register before
	// the app injects the seam, so the second case is the live one. Checking
	// only the getter would hand runProbe a nil interface to call.
	if h.prober == nil {
		writeNotConfigured(w, noProberConfigured)
		return nil, false
	}
	prober := h.prober()
	if prober == nil {
		// A serving pod that never had the credential seam wired. Deployment
		// shape, not a transient outage.
		writeNotConfigured(w, noProberConfigured)
		return nil, false
	}
	return prober, true
}

// noProberConfigured is the one message both nil arms answer with. They are the
// same fact to a caller — this deployment cannot probe — and two spellings
// would only invite a client to tell them apart.
const noProberConfigured = "this deployment has no model prober configured"

// offeredModel resolves a {model_key} path segment against this deployment's
// universe. A key it does not offer is a 404: the addressed resource does not
// exist, and probing an arbitrary caller-supplied string would let anyone spend
// the org's money on requests to models TF never named.
func offeredModel(w http.ResponseWriter, rawKey string) (modelcatalog.Model, bool) {
	model, ok := deploymentUniverse().Lookup(strings.TrimSpace(rawKey))
	if !ok {
		notFound(w, "model")
		return modelcatalog.Model{}, false
	}
	return model, true
}

// requireOwnCredentials refuses both test routes while the org's credentials are
// not TF-owned — a local install running on the host's environment.
//
// It is a setup gap, not a verdict, and it is refused rather than answered
// because there is nothing to record the answer ABOUT: a probe there would
// establish something true of one machine at one moment, keyed to no credential
// TF can name, watch, or invalidate. The read beside this publishes no
// availability at all under the same condition, so the badge a reader sees and
// the refusal a tester hits come from one decision.
//
// The surface appears the moment the org brings its own credential — always in
// multi mode, and in local once its credential wiring makes BYOK real there — so
// this is not a mode branch.
func requireOwnCredentials(w http.ResponseWriter, avail availabilityIndex) bool {
	if avail.creds.BringsOwn() {
		return true
	}
	writeNotConfigured(w, "this organization runs on the credentials of the machine hosting it, which no stored availability result can describe — connect a provider in Settings → Claude credentials to test models against it")
	return false
}

// requireProviderConnected refuses a probe against a provider the org holds no
// credential for, before anything is attempted.
//
// It reads the already-resolved index rather than the store, so the refusal
// and the badge the catalog read publishes for the same model are one
// decision: a caller told "unconfigured" by the read and "not connected" by
// this cannot be told two different things about one credential. Deciding it
// up front is also what makes a sweep of an unconnected provider write no rows
// at all, rather than a few before giving up.
func requireProviderConnected(w http.ResponseWriter, avail availabilityIndex, provider string) bool {
	if avail.creds.Has(provider) {
		return true
	}
	writeNotConfigured(w, fmt.Sprintf(
		"%s is not connected for this organization — connect it in Settings → Claude credentials before testing its models",
		modelcatalog.ProviderDisplayName(provider)))
	return false
}
