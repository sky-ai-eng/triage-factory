package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// orgSourcesResponse is the org's event-source availability — a singleton
// sub-resource of the org, not a collection: its cardinality is the source
// vocabulary this deployment carries, so there is nothing to filter and
// nothing to page. The envelope is an object rather than a bare array for the
// same reason every other read here is: a list that is one day joined by a
// sibling field must not have to change shape to get one.
type orgSourcesResponse struct {
	Sources eventsource.Availability `json:"sources"`
}

// handleOrgSources serves which event sources can reach this org, and when one
// cannot, why not.
//
// Member-readable, deliberately. "Events from this source can reach you" is
// what a plain member has to render — the whole event-authoring surface and
// every source card on the team page are meaningless without it — while "a
// credential is bound" is org posture that is tightening toward admin-only.
// Two different facts wanting two different scopes, which is why this route
// answers the first one and says nothing at all about the second: no
// credential, no host, no workspace name, not even a count.
//
// One read rather than three, also deliberately. Composing this from the org
// settings read, the entitlements probe and the workspace list would make the
// answer three async probes wide, and a card that renders on the first of them
// to land flashes through states that were never true.
//
// GET /api/orgs/{org_id}/sources
func (s *Server) handleOrgSources(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var sources eventsource.Availability
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		sources, e = eventsource.Resolve(r.Context(), tx, orgID)
		return e
	}); err != nil {
		// A fault is a 500, never a source reported off. The two are
		// indistinguishable to the reader, and answering 200 with everything
		// dark is how a status read becomes one nothing downstream trusts.
		internalError(w, "sources", err)
		return
	}
	writeJSON(w, http.StatusOK, orgSourcesResponse{Sources: sources})
}

// handleOrgSource serves one source's answer — the same element the list
// answers with, at its own address. It exists because the PATCH below does: a
// PATCHable address that cannot be read is a hole, and `sources` is a
// collection with fixed membership rather than a singleton.
//
// A kind outside this deployment's vocabulary is a 404. There is no such
// resource here — not a source reported off, which would be a claim about an
// org's setup that nobody made.
//
// GET /api/orgs/{org_id}/sources/{kind}
func (s *Server) handleOrgSource(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")

	var (
		state eventsource.State
		known bool
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		state, known, e = eventsource.StateFor(r.Context(), tx, orgID, kind)
		return e
	}); err != nil {
		internalError(w, "sources", err)
		return
	}
	if !known {
		notFound(w, "source")
		return
	}
	writeJSON(w, http.StatusOK, eventsource.NewSource(kind, state))
}

// orgSourcePatch is the PATCH body. json.RawMessage rather than *bool so an
// absent field and an explicit null are distinguishable: absent means the
// caller named no field to write, which must not answer "updated".
type orgSourcePatch struct {
	Disabled json.RawMessage `json:"disabled,omitempty"`
}

// handleOrgSourcePatch turns one source off, or back on, for the whole org.
//
// Off means off. From this write the source is not polled, its events mint no
// tasks, and no credential for it resolves for an agent — the three gates read
// the row this route writes (internal/poller, internal/routing, and
// eventsource.Disabled at the agenthost and credential-provisioning funnels).
// A switch that only stopped one of those would be the worst kind of control:
// an admin who turns Jira off to take load off their instance, or to cut a
// tenant's reach into it, has no way to tell from the UI that the agents kept
// their access.
//
// What it does NOT do is destroy anything, and that is the whole reason it
// exists next to DELETE …/jira/access/credential. Unbinding the credential
// achieves the same silence by throwing the credential away: it prices an undo
// at "go find the API token again" and audits as a credential removal. Here the
// credential stays bound, the tracked repos and authored handlers stay as they
// are, and re-enabling is one more PATCH.
//
// Org admin, not team admin. CLAUDE.md's org-wide-write rule admits a team
// admin of a team whose tracked set contains the subject — but a source is in
// nobody's tracked set, and turning one off lands on every team in the org. The
// binding argument is symmetry: unbinding the credential is RequireOrgAdmin, so
// a team-admin gate here would be a way for a team admin to reach an org-admin
// effect.
//
// Not every source is addressable here. GitHub has no off switch at all
// (eventsource.Disableable) — an org with GitHub off is an org where nothing
// works — so a PATCH naming it is a 422 rather than a stored row.
//
// Forward-only, like every other tracking change: existing tasks, in-flight
// runs and authored handlers are untouched. A handler whose source went dark
// renders inert and explained rather than being hidden or deleted, because a
// task is durable work that may have an open PR and agent memory behind it. A
// run already in flight keeps the task it is working; what it loses is the
// turned-off source's verbs, from its next call.
//
// PATCH /api/orgs/{org_id}/sources/{kind}
func (s *Server) handleOrgSourcePatch(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	// The vocabulary check runs before the body decode and reads nothing: a
	// kind this build does not carry is not an address, so there is no row to
	// store against it and no state to report for it.
	if !eventsource.Declared(kind) {
		notFound(w, "source")
		return
	}
	// Declared but not disableable: a 422, not a 404. The source is real and
	// the GET beside this answers for it — pretending it does not exist would
	// send a caller looking for a spelling mistake. 422 is the same fault class
	// the event-handler create gate uses for a source reference the caller
	// could in principle fix, and it is deliberately not 403: no role reaches
	// this, so it is not a permission the caller is missing.
	if !eventsource.Disableable(kind) {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: eventsource.Label(kind) + " cannot be turned off: every core surface is built on it",
			Field:   "disabled",
		})
		return
	}

	var req orgSourcePatch
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.Disabled == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: provide disabled",
			Field:   "disabled",
		})
		return
	}
	var disabled bool
	if err := json.Unmarshal(req.Disabled, &disabled); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "disabled must be true or false", Field: "disabled",
		})
		return
	}

	var state eventsource.State
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if _, err := tx.OrgEventSources.SetDisabled(r.Context(), orgID, kind, disabled, userID); err != nil {
			return fmt.Errorf("set event source policy: %w", err)
		}
		// Clearing the snapshots is what makes re-enabling safe, and it belongs
		// in this transaction rather than after it: a pause that commits
		// without its clear leaves the org armed to emit a burst of stale
		// transitions the moment it is turned back on, and nothing later
		// notices. See EntityStore.ClearSnapshotsForSourceSystem.
		if disabled {
			cleared, err := tx.Entities.ClearSnapshotsForSourceSystem(r.Context(), orgID, kind)
			if err != nil {
				return fmt.Errorf("clear %s snapshots: %w", kind, err)
			}
			serverLog.Info("event source disabled; cleared tracker snapshots",
				"org", orgID, "source", kind, "entities", cleared)
		}
		action := domain.AccessActionEventSourceEnabled
		if disabled {
			action = domain.AccessActionEventSourceDisabled
		}
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      action,
			DetailJSON:  domain.AccessDetailEventSource(kind),
		}); err != nil {
			return fmt.Errorf("record access change: %w", err)
		}
		// Read the answer back through the same derivation every other reader
		// uses, in the same transaction as the write. The stored flag is one
		// input of five — a paused source that is also unlicensed still reports
		// unlicensed — so echoing the request's own boolean back as a state
		// would be a different answer than the next GET gives.
		var e error
		state, _, e = eventsource.StateFor(r.Context(), tx, orgID, kind)
		return e
	}); err != nil {
		internalError(w, "sources", err)
		return
	}

	if s.onSourcesChanged != nil {
		s.onSourcesChanged(orgID, kind)
	}
	// Payload-free invalidation ping, the cheap default: the client refetches
	// through the REST read, which carries the scoping. Org-scoped because
	// availability is — every member of the org renders it.
	if s.ws != nil {
		s.ws.Broadcast(websocket.Event{Type: "sources_updated", OrgID: orgID, Data: map[string]any{}})
	}
	writeJSON(w, http.StatusOK, eventsource.NewSource(kind, state))
}
