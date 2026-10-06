package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// What PUT /api/teams/{team_id}/linear-teams checks, and against what. The
// Linear sibling of jira_project_gate.go, and scoped the same way.
//
// The picker offers only teams the org credential can see, and the rules board
// only states the team's workflow has, so the handler enforces both: a rule the
// UI applies and the handler does not is a convention. Both are asked live —
// the same reads the picker routes serve from.
//
// Every check is scoped to what CHANGED. The route is a replace-set, so every
// write resends the whole set; gating what is already stored would let a team
// deleted in Linear, a retired state, or a Linear outage lock the team out of
// the write that removes the stale row. So a stored team is carried as stored,
// an unchanged rule is kept byte for byte, and a write that only removes or
// clears things asks Linear nothing at all.
//
// What a check finds wrong with the body is a 422 naming the element and rule:
// the body is well formed, and what it names does not fit the workspace — a
// team the credential cannot see, a state from another team's workflow, a
// mapping left half done. Linear not answering is a 502 and stores nothing.
// --------------------------------------------------------------------

// linearRuleWish is one rule as the caller asked for it, settled against the
// stored rule. changed=false means the stored rule is carried and Linear is not
// asked about it.
type linearRuleWish struct {
	changed     bool
	memberIDs   []string
	canonicalID string
}

// ids is every state id this wish needs resolved.
func (rw linearRuleWish) ids() []string {
	if !rw.changed {
		return nil
	}
	out := slices.Clone(rw.memberIDs)
	if rw.canonicalID != "" {
		out = append(out, rw.canonicalID)
	}
	return out
}

// mapped reports whether the rule will hold anything once written: the wish if
// it changed, the stored rule otherwise.
func (rw linearRuleWish) mapped(storedMembers []domain.LinearStateRef) bool {
	if !rw.changed {
		return len(storedMembers) > 0
	}
	return len(rw.memberIDs) > 0 || rw.canonicalID != ""
}

// linearTeamPlan is one element's settled work: the row as far as the stored
// row carries it, and what still has to be resolved against Linear.
type linearTeamPlan struct {
	wish  linearTeamWish
	prior domain.LinearTeamRules
	row   domain.LinearTeamRules
	// newTeam is a team this team does not track yet: its visibility, key and
	// name come from Linear.
	newTeam bool

	pickup, inProgress, done linearRuleWish
}

func (p linearTeamPlan) changedIDs() int {
	return len(p.pickup.ids()) + len(p.inProgress.ids()) + len(p.done.ids())
}

// needsLinear reports whether this element asks Linear anything. A changed
// rule re-reads the team itself as well as its states, which is what keeps a
// stored key and name current with Linear.
func (p linearTeamPlan) needsLinear() bool {
	return p.newTeam || p.changedIDs() > 0
}

// gateLinearTeams turns the shape-checked wishes into the rows to store, in
// request order. It writes the response and returns false when the write must
// not proceed.
func (s *Server) gateLinearTeams(
	w http.ResponseWriter, r *http.Request, orgID string,
	wishes []linearTeamWish, stored []domain.LinearTeamRules,
) ([]domain.LinearTeamRules, bool) {
	storedByID := make(map[string]domain.LinearTeamRules, len(stored))
	for _, row := range stored {
		storedByID[row.LinearTeamID] = row
	}

	plans := make([]linearTeamPlan, 0, len(wishes))
	for _, wish := range wishes {
		prior, isStored := storedByID[wish.write.ID]
		p := linearTeamPlan{
			wish:       wish,
			prior:      prior,
			newTeam:    !isStored,
			pickup:     requestedLinearPickup(wish.write.Pickup, prior.PickupMembers),
			inProgress: requestedLinearRule(wish.write.InProgress, prior.InProgressMembers, prior.InProgressCanonical),
			done:       requestedLinearRule(wish.write.Done, prior.DoneMembers, prior.DoneCanonical),
			row: domain.LinearTeamRules{
				LinearTeamID:   wish.write.ID,
				LinearTeamKey:  prior.LinearTeamKey,
				LinearTeamName: prior.LinearTeamName,
			},
		}
		if !p.pickup.changed {
			p.row.PickupMembers = slices.Clone(prior.PickupMembers)
		}
		if !p.inProgress.changed {
			p.row.InProgressMembers = slices.Clone(prior.InProgressMembers)
			p.row.InProgressCanonical = prior.InProgressCanonical
		}
		if !p.done.changed {
			p.row.DoneMembers = slices.Clone(prior.DoneMembers)
			p.row.DoneCanonical = prior.DoneCanonical
		}
		plans = append(plans, p)
	}

	// Everything decidable from the ids alone, before Linear is asked: a write
	// that cannot be stored should not cost a request to find that out.
	var v httpx.Validation
	for _, p := range plans {
		checkLinearPlanShape(&v, p)
	}
	if v.Flush(w, http.StatusUnprocessableEntity) {
		return nil, false
	}

	if !slices.ContainsFunc(plans, linearTeamPlan.needsLinear) {
		out := make([]domain.LinearTeamRules, 0, len(plans))
		for _, p := range plans {
			out = append(out, p.row)
		}
		return out, true
	}

	client, ok := s.linearSystemClient(w, r, orgID, "settings/team/linear-teams",
		"Linear is not connected for this workspace, so a Linear team or a state mapping cannot be added")
	if !ok {
		return nil, false
	}

	out := make([]domain.LinearTeamRules, 0, len(plans))
	for _, p := range plans {
		if !p.needsLinear() {
			out = append(out, p.row)
			continue
		}
		base := fmt.Sprintf("linear_teams[%d]", p.wish.index)
		team, err := client.GetTeam(r.Context(), p.wish.write.ID)
		if errors.Is(err, linear.ErrNotFound) {
			v.Invalid(base+".id", "no Linear team "+p.wish.write.ID+" is visible to this workspace's Linear credential")
			continue
		}
		if err != nil {
			writeLinearGateStopped(w, &v, orgID, "Linear team "+p.wish.write.ID, err)
			return nil, false
		}
		row := p.row
		row.LinearTeamKey, row.LinearTeamName = team.Key, team.Name
		if p.changedIDs() == 0 {
			out = append(out, row)
			continue
		}

		// One read per team with a changed rule, shared by all three rules:
		// it is the gate, and it is where each state's name and type come
		// from.
		states, err := client.ListWorkflowStates(r.Context(), p.wish.write.ID)
		if err != nil {
			writeLinearGateStopped(w, &v, orgID, "the workflow states of Linear team "+team.Key, err)
			return nil, false
		}
		byID := make(map[string]domain.LinearStateRef, len(states))
		for _, st := range states {
			byID[st.ID] = domain.LinearStateRef{ID: st.ID, Name: st.Name, Type: st.Type}
		}
		resolve := func(id, field string) domain.LinearStateRef {
			ref, known := byID[id]
			if !known {
				v.Invalid(field, "no workflow state "+id+" in Linear team "+team.Key)
			}
			return ref
		}
		resolveAll := func(ids []string, field string) []domain.LinearStateRef {
			refs := make([]domain.LinearStateRef, 0, len(ids))
			for _, id := range ids {
				if ref := resolve(id, field); !ref.IsZero() {
					refs = append(refs, ref)
				}
			}
			return refs
		}
		if p.pickup.changed {
			row.PickupMembers = resolveAll(p.pickup.memberIDs, base+".pickup.member_ids")
		}
		for _, rule := range []struct {
			name      string
			wish      linearRuleWish
			members   *[]domain.LinearStateRef
			canonical *domain.LinearStateRef
		}{
			{"in_progress", p.inProgress, &row.InProgressMembers, &row.InProgressCanonical},
			{"done", p.done, &row.DoneMembers, &row.DoneCanonical},
		} {
			if !rule.wish.changed {
				continue
			}
			*rule.members = resolveAll(rule.wish.memberIDs, base+"."+rule.name+".member_ids")
			// The shape check established the canonical is one of the
			// members, so an unknown canonical is already reported as an
			// unknown member.
			*rule.canonical = byID[rule.wish.canonicalID]
		}
		out = append(out, row)
	}
	if v.Flush(w, http.StatusUnprocessableEntity) {
		return nil, false
	}
	return out, true
}

// checkLinearPlanShape records every fault in one element that the ids alone
// decide: a write-target rule with members and no canonical (or the reverse),
// a canonical outside its rule's members, and a mapping left half done.
func checkLinearPlanShape(v *httpx.Validation, p linearTeamPlan) {
	base := fmt.Sprintf("linear_teams[%d]", p.wish.index)
	ruleFault := false
	for _, rule := range []struct {
		name string
		wish linearRuleWish
	}{{"in_progress", p.inProgress}, {"done", p.done}} {
		if !rule.wish.changed {
			continue
		}
		hasMembers, hasCanonical := len(rule.wish.memberIDs) > 0, rule.wish.canonicalID != ""
		switch {
		case hasMembers && !hasCanonical:
			v.Invalid(base+"."+rule.name+".canonical_id",
				rule.name+" needs a canonical_id: the state TF moves an issue into, one of its member_ids")
			ruleFault = true
		case !hasMembers && hasCanonical:
			v.Invalid(base+"."+rule.name+".member_ids",
				rule.name+" names a canonical_id but no member_ids; the canonical must be one of them")
			ruleFault = true
		case hasMembers && !slices.Contains(rule.wish.memberIDs, rule.wish.canonicalID):
			v.Invalid(base+"."+rule.name+".canonical_id",
				"canonical_id "+rule.wish.canonicalID+" is not one of "+rule.name+".member_ids")
			ruleFault = true
		}
	}
	if ruleFault {
		return
	}
	pickup := p.pickup.mapped(p.prior.PickupMembers)
	inProgress := p.inProgress.mapped(p.prior.InProgressMembers)
	done := p.done.mapped(p.prior.DoneMembers)
	mapped := 0
	for _, m := range []bool{pickup, inProgress, done} {
		if m {
			mapped++
		}
	}
	if mapped > 0 && mapped < 3 {
		v.Invalid(base, "a Linear team is either watched with no rules or mapped with all three — "+
			"pickup, in_progress and done; this would leave "+missingLinearRules(pickup, inProgress, done)+" unmapped")
	}
}

// missingLinearRules names the rules a half mapping leaves empty.
func missingLinearRules(pickup, inProgress, done bool) string {
	var missing []string
	if !pickup {
		missing = append(missing, "pickup")
	}
	if !inProgress {
		missing = append(missing, "in_progress")
	}
	if !done {
		missing = append(missing, "done")
	}
	switch len(missing) {
	case 1:
		return missing[0]
	default:
		return missing[0] + " and " + missing[1]
	}
}

// requestedLinearPickup reports the pickup member ids the caller asked for and
// whether they differ from what is stored. An absent rule is not a request.
func requestedLinearPickup(write *linearPickupRuleWrite, stored []domain.LinearStateRef) linearRuleWish {
	if write == nil || sameLinearStateSet(write.MemberIDs, stored) {
		return linearRuleWish{}
	}
	return linearRuleWish{changed: true, memberIDs: write.MemberIDs}
}

// requestedLinearRule is requestedLinearPickup for a write-target rule, whose
// canonical moves with its members.
func requestedLinearRule(write *linearStateRuleWrite, storedMembers []domain.LinearStateRef, storedCanonical domain.LinearStateRef) linearRuleWish {
	if write == nil {
		return linearRuleWish{}
	}
	if sameLinearStateSet(write.MemberIDs, storedMembers) && write.CanonicalID == storedCanonical.ID {
		return linearRuleWish{}
	}
	return linearRuleWish{changed: true, memberIDs: write.MemberIDs, canonicalID: write.CanonicalID}
}

// sameLinearStateSet reports whether ids name exactly the stored refs,
// order-insensitively.
func sameLinearStateSet(ids []string, stored []domain.LinearStateRef) bool {
	if len(ids) != len(stored) {
		return false
	}
	return slices.Equal(sortedStrings(ids), linearStateIDSet(stored))
}

func sortedStrings(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// writeLinearGateStopped ends the gate when Linear stops answering partway
// through a set. Faults already found in earlier elements win: they are true
// whether or not Linear is reachable, and they must be fixed before any retry
// can store anything. Nothing is stored either way.
func writeLinearGateStopped(w http.ResponseWriter, v *httpx.Validation, orgID, what string, err error) {
	if v.Flush(w, http.StatusUnprocessableEntity) {
		serverLog.Warn("linear write gate could not reach linear; reporting the field faults found first",
			"org", orgID, "checking", what, "error", err)
		return
	}
	serverLog.Warn("linear write gate could not reach linear", "org", orgID, "checking", what, "error", err)
	httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
		Reason:  httpx.ReasonUpstreamUnavailable,
		Message: "could not confirm " + what + " with Linear, so nothing was saved" + httpx.LocalDetail(err),
	})
}
