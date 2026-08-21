package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
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
