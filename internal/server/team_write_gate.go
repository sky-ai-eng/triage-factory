package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// gateActingTeamWrite rejects a viewer before a team-picker write — the
// create/duplicate paths that target an acting team rather than an existing row
// (TFAC-447). It resolves the acting team read-only (ResolveActingNoStamp — no
// last-acting stamp, since a viewer is about to 403) and gates the caller as a
// writer of that team.
//
// Returns false when a response was already written: 400 for a bad team pick
// (selection error), 403 for a viewer, or 500 on a resolve failure. scope tags
// the internalError log line. Shared by the prompt / blueprint / event-handler
// create paths so the two-phase "gate read-only, then stamp in the main tx"
// shape lives in one place.
func gateActingTeamWrite(w http.ResponseWriter, r *http.Request, tx db.TxRunner, az *authz.Checker, orgID, userID, picked, scope string) bool {
	_, ok := actingTeamForWrite(w, r, tx, az, orgID, userID, picked, scope)
	return ok
}

// actingTeamForWrite is gateActingTeamWrite for a write that needs the team it
// gated before its transaction, such as an event handler create, whose scope
// predicate is checked against the team's tracked sources first. Passing the
// team it returns to ResolveActing as the pick inside the write's transaction
// re-checks the caller's membership and resolves the same team.
func actingTeamForWrite(w http.ResponseWriter, r *http.Request, tx db.TxRunner, az *authz.Checker, orgID, userID, picked, scope string) (string, bool) {
	var actingTeam string
	if err := tx.WithReadTx(r.Context(), orgID, userID, func(s db.TxStores) error {
		var e error
		actingTeam, e = teamscope.ResolveActingNoStamp(r.Context(), s.Teams, s.Users, orgID, userID, picked)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return "", false
		}
		internalError(w, scope, err)
		return "", false
	}
	return actingTeam, az.RequireTeamWrite(w, r, orgID, userID, actingTeam)
}
