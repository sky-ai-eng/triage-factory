package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// PUT /api/teams/{team_id}/linear-teams — the Linear teams this team tracks,
// and the pickup / in-progress / done workflow-state rules for each. The
// Linear sibling of PUT /api/teams/{team_id}/jira-projects, with a Linear team
// (by UUID) as the tracked unit.
//
// A replace-set: the body is the desired set, and a Linear team absent from it
// is untracked. Team admin, non-archived team — the same gate as its Jira
// sibling, for the same reason: the rules decide which issues become tasks.
//
// **Ids only.** A Linear team arrives as its UUID and a workflow state as its
// UUID; names are never accepted. The server resolves the team's key and name
// and each state's name and type from Linear, in the fetch that validates the
// ids (linear_team_gate.go), so a stored name is always one Linear gave us.
//
// **A team is watched, or armed, never half of either.** An element naming
// only an id watches the team without mapping anything. Mapping is all three
// rules at once: pickup members, and members plus a canonical for in_progress
// and done. The client pre-fills a mapping from the states' types, so the
// half-mapped state a Jira project can sit in is not one a Linear team needs.
//
// An absent rule keeps the stored one, as on the Jira route, so an id-only
// element for a team that is already armed leaves it armed. Disarming is three
// explicitly empty rules.
// --------------------------------------------------------------------

// linearPickupRuleWrite is the pickup rule's write shape: members and nothing
// else. Its own type, so a canonical_id sent here fails the strict decode with
// the field named — TF never moves an issue back into pickup.
type linearPickupRuleWrite struct {
	MemberIDs []string `json:"member_ids"`
}

// linearStateRuleWrite is a write-target rule: the states that count as this
// one, and the state TF moves an issue into. Both are state ids.
type linearStateRuleWrite struct {
	MemberIDs   []string `json:"member_ids"`
	CanonicalID string   `json:"canonical_id"`
}

// linearTeamWrite is one Linear team in the desired set. The rules are
// pointers because absent and empty are different requests: absent keeps the
// stored rule, an explicit empty clears it.
type linearTeamWrite struct {
	ID         string                 `json:"id"`
	Pickup     *linearPickupRuleWrite `json:"pickup"`
	InProgress *linearStateRuleWrite  `json:"in_progress"`
	Done       *linearStateRuleWrite  `json:"done"`
}

// teamLinearTeamsRequest is the desired set, in display order. The field is
// required: a body that names no set is not a request to untrack everything.
type teamLinearTeamsRequest struct {
	LinearTeams *[]linearTeamWrite `json:"linear_teams"`
}

// teamLinearTeamsResponse echoes the set as stored, in display order.
type teamLinearTeamsResponse struct {
	LinearTeams []linearTeamSettings `json:"linear_teams"`
}

// linearStateRefJSON is one workflow state as the API renders it: the id the
// rules are keyed on, and the name and type Linear gave for it at the last
// save.
type linearStateRefJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// linearPickupRuleJSON is the pickup rule's read shape: members only.
type linearPickupRuleJSON struct {
	Members []linearStateRefJSON `json:"members"`
}

// linearStateRuleJSON is a write-target rule's read shape. canonical is null
// on an unmapped rule rather than absent, so a client reads one shape in both
// states.
type linearStateRuleJSON struct {
	Members   []linearStateRefJSON `json:"members"`
	Canonical *linearStateRefJSON  `json:"canonical"`
}

// linearTeamSettings is one tracked Linear team as the API renders it, on this
// route and on the team-settings read.
type linearTeamSettings struct {
	ID         string               `json:"id"`
	Key        string               `json:"key"`
	Name       string               `json:"name"`
	Armed      bool                 `json:"armed"`
	Pickup     linearPickupRuleJSON `json:"pickup"`
	InProgress linearStateRuleJSON  `json:"in_progress"`
	Done       linearStateRuleJSON  `json:"done"`
}

// linearTeamWish is one shape-checked body element.
type linearTeamWish struct {
	// index is the element's position in the request body, which is what an
	// error field names.
	index int
	write linearTeamWrite
}

func (s *Server) handleTeamLinearTeamsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, ok := s.az.TeamIDFromPath(w, r, "settings/team/linear-teams", orgID, userID)
	if !ok {
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}
	if !s.az.RequireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req teamLinearTeamsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.LinearTeams == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "linear_teams is required: the whole set of Linear teams this team tracks, [] for none",
			Field:   "linear_teams",
		})
		return
	}
	wishes, ok := shapeLinearTeamWishes(w, *req.LinearTeams)
	if !ok {
		return
	}

	// The pre-image: what an absent rule is carried from, what needs no Linear
	// call, and what the poll re-due decision compares against. It is read in
	// the org's current Linear workspace, which is also the one the write
	// stamps: rows saved under another workspace are neither shown, carried
	// nor pruned.
	prev, workspaceID, err := s.storedLinearTeams(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team/linear-teams", err)
		return
	}

	// A rule names ids of one workspace, so a set can only be saved under the
	// one the org's credential belongs to. With none recorded there is nowhere
	// to save it. An empty set still answers, since it asks for nothing, and
	// writes nothing: rows and the display order saved under a previous
	// workspace stay stored for its rebind.
	if workspaceID == "" {
		if len(wishes) > 0 {
			writeNotConfigured(w, "Linear is not connected for this workspace, so a Linear team or a state mapping cannot be added")
			return
		}
		writeJSON(w, http.StatusOK, teamLinearTeamsResponse{LinearTeams: toLinearTeamSettings(nil)})
		return
	}

	next, ok := s.gateLinearTeams(w, r, orgID, wishes, prev)
	if !ok {
		return
	}

	// team_settings.linear_teams holds the display order; linear_team_rules
	// holds the rules. One transaction, so the order never names a Linear team
	// whose row did not land.
	order := make([]string, 0, len(next))
	for _, row := range next {
		order = append(order, row.LinearTeamID)
	}
	var stored []domain.LinearTeamRules
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamSet, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		teamSet.LinearTeams = order
		if _, err := tx.Teams.UpdateSettings(r.Context(), teamID, teamSet); err != nil {
			return fmt.Errorf("save team settings: %w", err)
		}
		if stored, err = tx.LinearTeamRules.ReplaceForTeam(r.Context(), teamID, workspaceID, next); err != nil {
			return fmt.Errorf("save linear rules: %w", err)
		}
		return nil
	}); err != nil {
		internalError(w, "settings/team/linear-teams", err)
		return
	}

	// Re-due the org's Linear poll only when what the poller would ask moved:
	// watching a team without mapping it contributes nothing to a poll, and
	// resending an identical set changes nothing at all.
	if !sameArmedLinearTeams(prev, next) && s.onLinearChanged != nil {
		s.MarkLinearRestarted(r.Context(), orgID)
		go s.onLinearChanged(orgID)
	}

	writeJSON(w, http.StatusOK, teamLinearTeamsResponse{
		LinearTeams: toLinearTeamSettings(orderLinearTeamRules(stored, order)),
	})
}

// shapeLinearTeamWishes checks the body's shape — every id present, a Linear
// id, and named once — and answers 400 naming every bad field, before any read
// or Linear call. Whether an id names something Linear can see is the gate's.
func shapeLinearTeamWishes(w http.ResponseWriter, in []linearTeamWrite) ([]linearTeamWish, bool) {
	var v httpx.Validation
	wishes := make([]linearTeamWish, 0, len(in))
	seen := map[string]bool{}
	for i, t := range in {
		base := fmt.Sprintf("linear_teams[%d]", i)
		switch {
		case t.ID == "":
			v.Missing(base + ".id")
			continue
		case !isLinearID(t.ID):
			v.Invalid(base+".id", "not a Linear team id (a lowercase UUID): "+t.ID)
			continue
		case seen[t.ID]:
			v.Invalid(base+".id", "Linear team "+t.ID+" appears more than once")
			continue
		}
		seen[t.ID] = true
		if t.Pickup != nil {
			shapeLinearStateIDs(&v, t.Pickup.MemberIDs, base+".pickup.member_ids")
		}
		for _, rule := range []struct {
			name  string
			write *linearStateRuleWrite
		}{{"in_progress", t.InProgress}, {"done", t.Done}} {
			if rule.write == nil {
				continue
			}
			shapeLinearStateIDs(&v, rule.write.MemberIDs, base+"."+rule.name+".member_ids")
			if c := rule.write.CanonicalID; c != "" && !isLinearID(c) {
				v.Invalid(base+"."+rule.name+".canonical_id", "not a Linear workflow state id (a lowercase UUID): "+c)
			}
		}
		wishes = append(wishes, linearTeamWish{index: i, write: t})
	}
	if v.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	return wishes, true
}

// shapeLinearStateIDs checks a rule's member ids: each a Linear id, none
// repeated.
func shapeLinearStateIDs(v *httpx.Validation, ids []string, field string) {
	seen := map[string]bool{}
	for _, id := range ids {
		switch {
		case !isLinearID(id):
			v.Invalid(field, "not a Linear workflow state id (a lowercase UUID): "+id)
		case seen[id]:
			v.Invalid(field, "workflow state "+id+" appears more than once")
		}
		seen[id] = true
	}
}

// storedLinearTeams reads the team's rows in the org's current Linear
// workspace, in display order — the order the team-settings read renders, so
// "already stored" means what a client that just read the resource would
// resend — and returns that workspace id ("" when none is bound).
func (s *Server) storedLinearTeams(ctx context.Context, orgID, userID, teamID string) ([]domain.LinearTeamRules, string, error) {
	var (
		out         []domain.LinearTeamRules
		workspaceID string
	)
	err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		workspaceID = domain.EntityScope("linear", orgSet)
		teamSet, err := tx.Teams.GetSettings(ctx, teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		rules, err := tx.LinearTeamRules.ListForTeam(ctx, teamID, workspaceID)
		if err != nil {
			return fmt.Errorf("load linear rules: %w", err)
		}
		out = orderLinearTeamRules(rules, teamSet.LinearTeams)
		return nil
	})
	return out, workspaceID, err
}

// orderLinearTeamRules puts rules in the display order. An id in order with no
// row is skipped — the rules table is the truth for membership — and a row
// whose id order does not name is appended in the store's order, so a row
// written without its order entry still surfaces.
func orderLinearTeamRules(rules []domain.LinearTeamRules, order []string) []domain.LinearTeamRules {
	byID := make(map[string]domain.LinearTeamRules, len(rules))
	for _, r := range rules {
		byID[r.LinearTeamID] = r
	}
	out := make([]domain.LinearTeamRules, 0, len(rules))
	placed := make(map[string]bool, len(rules))
	for _, id := range order {
		if r, ok := byID[id]; ok && !placed[id] {
			out = append(out, r)
			placed[id] = true
		}
	}
	for _, r := range rules {
		if !placed[r.LinearTeamID] {
			out = append(out, r)
		}
	}
	return out
}

// toLinearTeamSettings renders stored rows in the read shape, with empty
// members as [] rather than null.
func toLinearTeamSettings(in []domain.LinearTeamRules) []linearTeamSettings {
	out := make([]linearTeamSettings, 0, len(in))
	for _, r := range in {
		out = append(out, linearTeamSettings{
			ID:         r.LinearTeamID,
			Key:        r.LinearTeamKey,
			Name:       r.LinearTeamName,
			Armed:      r.Armed(),
			Pickup:     linearPickupRuleJSON{Members: toLinearStateRefsJSON(r.PickupMembers)},
			InProgress: toLinearStateRuleJSON(r.InProgressMembers, r.InProgressCanonical),
			Done:       toLinearStateRuleJSON(r.DoneMembers, r.DoneCanonical),
		})
	}
	return out
}

func toLinearStateRuleJSON(members []domain.LinearStateRef, canonical domain.LinearStateRef) linearStateRuleJSON {
	out := linearStateRuleJSON{Members: toLinearStateRefsJSON(members)}
	if !canonical.IsZero() {
		out.Canonical = &linearStateRefJSON{ID: canonical.ID, Name: canonical.Name, Type: canonical.Type}
	}
	return out
}

func toLinearStateRefsJSON(refs []domain.LinearStateRef) []linearStateRefJSON {
	out := make([]linearStateRefJSON, 0, len(refs))
	for _, m := range refs {
		out = append(out, linearStateRefJSON{ID: m.ID, Name: m.Name, Type: m.Type})
	}
	return out
}

// sameArmedLinearTeams reports whether two sets arm the same Linear teams with
// the same rules, by id. Order and display names are not compared: neither
// changes anything a poll asks Linear.
func sameArmedLinearTeams(a, b []domain.LinearTeamRules) bool {
	key := func(in []domain.LinearTeamRules) []string {
		var out []string
		for _, r := range in {
			if !r.Armed() {
				continue
			}
			out = append(out, fmt.Sprint(r.LinearTeamID,
				linearStateIDSet(r.PickupMembers),
				linearStateIDSet(r.InProgressMembers), r.InProgressCanonical.ID,
				linearStateIDSet(r.DoneMembers), r.DoneCanonical.ID))
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

// linearStateIDSet is refs' ids, sorted, for set comparison.
func linearStateIDSet(refs []domain.LinearStateRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	slices.Sort(out)
	return out
}
