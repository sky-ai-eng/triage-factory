package server

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// What PUT /api/teams/{team_id}/jira-projects checks against Jira, and what it
// deliberately does not.
//
// The picker offers only projects the org credential can see, and the status
// board only statuses the project's workflow has, so the handler enforces both
// sets: a rule the UI applies and the handler does not is a convention, and the
// first headless caller breaks it. Both are asked live — the same project read
// GET /api/orgs/{org_id}/jira/projects/{jira_id} serves from, by the Jira id
// the caller sent, and the same ProjectStatuses read its statuses list serves
// from — because a catalog of dozens behind a single org credential has
// nothing for a mirror to earn. The rules rows are the only persistence: they
// snapshot the statuses a team actually acts on, at the moment it arms them.
//
// Both checks are scoped to what CHANGED, and that scoping is the load-bearing
// part. This route is a replace-set: every write resends the whole set. Gating
// what was already stored would mean a project deleted upstream, a status
// retired from a workflow, or a Jira outage locks the team out of its own
// configuration door — including the write that would remove the offending row.
// So a stored project is grandfathered, an unchanged rule is kept
// byte-for-byte, and a write that only removes things asks Jira nothing at all.
//
// Two failure modes, kept distinct because they ask different things of the
// caller:
//
//   - An identifier Jira does not know is the caller's mistake: 400, one
//     INVALID_FIELD per bad value, each naming the element it arrived in.
//   - Jira not answering is nobody's mistake and establishes nothing: an
//     upstream error, never a silent skip and never a stored success. Storing
//     an unverified rule "because Jira was down" is the one outcome a caller
//     can neither see nor undo by retrying.
//
// Status NAMES are never accepted and never gated. They are a drifting external
// vocabulary — the id is the identity — so the server resolves each id to the
// name Jira spells it with today, in the same fetch that validates it. That is
// also why a rename needs no handling anywhere: nothing matches on the name.
// --------------------------------------------------------------------

// ruleWish is one rule as the caller expressed it, after the comparison
// against what is stored. changed=false means the stored rule is carried
// verbatim and Jira is never asked about it.
type ruleWish struct {
	changed     bool
	memberIDs   []string
	canonicalID string
}

// ids is every status id this wish needs resolved.
func (rw ruleWish) ids() []string {
	if !rw.changed {
		return nil
	}
	out := slices.Clone(rw.memberIDs)
	if rw.canonicalID != "" {
		out = append(out, rw.canonicalID)
	}
	return out
}

// projectPlan is one project's settled work: the row as far as the stored
// rules carry it, plus what still has to be resolved against Jira.
type projectPlan struct {
	wish jiraProjectWish
	row  domain.JiraProjectStatusRules
	// adding is a project the team does not track yet, which Jira has to
	// confirm before its row can be registered.
	adding bool

	pickup, inProgress, inReview, done ruleWish
}

// label is how a fault names the project: its key once one is known, else
// the Jira id the caller sent.
func (p projectPlan) label() string {
	if p.row.ProjectKey != "" {
		return p.row.ProjectKey
	}
	return p.wish.jiraID
}

// statusRef names the project to Jira's status read: by its project id when
// one is known, which a key rename does not move, else by its key.
func (p projectPlan) statusRef() string {
	if p.row.ProjectExternalID != "" {
		return p.row.ProjectExternalID
	}
	return p.row.ProjectKey
}

// changedIDs is every status id this plan still has to resolve, across all four
// rules — the one place the rule set is enumerated for the "does this project
// need Jira at all?" question.
func (p projectPlan) changedIDs() int {
	return len(p.pickup.ids()) + len(p.inProgress.ids()) + len(p.inReview.ids()) + len(p.done.ids())
}

func (p projectPlan) needsJira() bool {
	return p.adding || p.changedIDs() > 0
}

// gateJiraProjects turns the request's wishes into the rules to store.
//
// Each rule is resolved one of two ways. Carried: the caller left it out, or
// sent exactly what is stored — the stored refs are kept verbatim, ids, names
// and all, and Jira is not asked. Resolved: the caller sent a different set of
// status ids — each is looked up in the project's workflow, which both refuses
// the unknown ones and supplies the display names.
//
// A stored project keeps its registry row (JiraProjectID). A project being
// added has none yet: the gate reads it from Jira by the id the caller sent,
// which refuses one the credential cannot see and supplies its key as Jira
// spells it and its name, into ProjectExternalID, ProjectKey and ProjectName,
// and the write registers the row.
//
// site is the org's Jira site, which every row added here is registered on.
//
// Beside the rows it returns, by the index of each row, the statuses of every
// project whose workflow it read: every project with a rule that changed.
//
// It writes the response and returns false when the write must not proceed.
func (s *Server) gateJiraProjects(
	w http.ResponseWriter, r *http.Request,
	orgID, site string,
	wishes []jiraProjectWish,
) ([]domain.JiraProjectStatusRules, map[int][]domain.JiraStatusRef, bool) {
	// First pass: settle what each project needs from Jira, without asking yet.
	// A set that needs nothing must not resolve a credential, let alone make a
	// call — that is what keeps the door open while Jira is down.
	plans := make([]projectPlan, 0, len(wishes))
	needsJira := false
	for _, wish := range wishes {
		var prior domain.JiraProjectStatusRules
		row := domain.JiraProjectStatusRules{ProjectExternalID: wish.jiraID}
		if wish.stored != nil {
			prior = *wish.stored
			row = domain.JiraProjectStatusRules{
				JiraProjectID:     prior.JiraProjectID,
				ProjectKey:        prior.ProjectKey,
				ProjectName:       prior.ProjectName,
				ProjectExternalID: prior.ProjectExternalID,
			}
		}
		p := projectPlan{
			wish:       wish,
			row:        row,
			adding:     wish.stored == nil,
			pickup:     requestedPickup(wish.write.Pickup, prior.PickupMembers),
			inProgress: requestedRule(wish.write.InProgress, prior.InProgressMembers, prior.InProgressCanonical),
			inReview:   requestedRule(wish.write.InReview, prior.InReviewMembers, prior.InReviewCanonical),
			done:       requestedRule(wish.write.Done, prior.DoneMembers, prior.DoneCanonical),
		}
		if !p.pickup.changed {
			p.row.PickupMembers = slices.Clone(prior.PickupMembers)
		}
		if !p.inProgress.changed {
			p.row.InProgressMembers = slices.Clone(prior.InProgressMembers)
			p.row.InProgressCanonical = prior.InProgressCanonical
		}
		if !p.inReview.changed {
			p.row.InReviewMembers = slices.Clone(prior.InReviewMembers)
			p.row.InReviewCanonical = prior.InReviewCanonical
		}
		if !p.done.changed {
			p.row.DoneMembers = slices.Clone(prior.DoneMembers)
			p.row.DoneCanonical = prior.DoneCanonical
		}
		needsJira = needsJira || p.needsJira()
		plans = append(plans, p)
	}

	if !needsJira {
		out := make([]domain.JiraProjectStatusRules, 0, len(plans))
		for _, p := range plans {
			out = append(out, p.row)
		}
		return out, nil, true
	}

	// The client is resolved only now that something needs verifying, so an
	// unconnected or turned-off org can still remove a project it can no
	// longer map.
	client, ok := s.jiraSystemClient(w, r, orgID, "settings/team/jira-projects",
		"Jira is not connected for this organization, so a project or a status mapping cannot be added")
	if !ok {
		return nil, nil, false
	}
	// A connected credential implies a usable site, so this holds only if the
	// two ever disagree on what a usable base URL is.
	if site == "" {
		writeNotConfigured(w, "Jira has no site configured for this organization, so a project or a status mapping cannot be added")
		return nil, nil, false
	}

	var v httpx.Validation
	out := make([]domain.JiraProjectStatusRules, 0, len(plans))
	workflows := map[int][]domain.JiraStatusRef{}
	for i := range plans {
		p := &plans[i]
		base := fmt.Sprintf("jira_projects[%d]", p.wish.index)
		if p.adding {
			project, err := client.GetProject(r.Context(), p.wish.jiraID)
			if jira.IsNotFound(err) {
				v.Invalid(base+".jira_id", "no Jira project "+p.wish.jiraID+" is visible to this organization's Jira credential")
				continue
			}
			if err == nil && project.ID != p.wish.jiraID {
				err = fmt.Errorf("jira answered project %s with id %q", p.wish.jiraID, project.ID)
			}
			if err != nil {
				writeJiraGateStopped(w, &v, orgID, "Jira project "+p.wish.jiraID, err)
				return nil, nil, false
			}
			p.row.ProjectKey = normalizeJiraProjectKey(project.Key)
			p.row.ProjectName = project.Name
		}
		if p.changedIDs() == 0 {
			out = append(out, p.row)
			continue
		}
		// One fetch per project with a changed rule, shared by all four rules
		// and doing both jobs at once: it is the gate, and it is where every
		// display name comes from.
		statuses, err := client.ProjectStatuses(r.Context(), p.statusRef())
		if err != nil {
			writeJiraGateStopped(w, &v, orgID, "the statuses of project "+p.label(), err)
			return nil, nil, false
		}
		byID := make(map[string]domain.JiraStatusRef, len(statuses))
		workflow := make([]domain.JiraStatusRef, 0, len(statuses))
		for _, st := range statuses {
			byID[st.ID] = domain.JiraStatusRef{ID: st.ID, Name: st.Name}
			workflow = append(workflow, byID[st.ID])
		}
		workflows[len(out)] = workflow
		resolve := func(ids []string, field string) []domain.JiraStatusRef {
			refs := make([]domain.JiraStatusRef, 0, len(ids))
			for _, id := range ids {
				ref := resolveOne(id, byID, &v, field, p.label())
				if !ref.IsZero() {
					refs = append(refs, ref)
				}
			}
			return refs
		}
		row := p.row
		if p.pickup.changed {
			row.PickupMembers = resolve(p.pickup.memberIDs, base+".pickup.member_ids")
		}
		if p.inProgress.changed {
			row.InProgressMembers = resolve(p.inProgress.memberIDs, base+".in_progress.member_ids")
			row.InProgressCanonical = resolveOne(p.inProgress.canonicalID, byID, &v, base+".in_progress.canonical_id", p.label())
		}
		if p.inReview.changed {
			row.InReviewMembers = resolve(p.inReview.memberIDs, base+".in_review.member_ids")
			row.InReviewCanonical = resolveOne(p.inReview.canonicalID, byID, &v, base+".in_review.canonical_id", p.label())
		}
		if p.done.changed {
			row.DoneMembers = resolve(p.done.memberIDs, base+".done.member_ids")
			row.DoneCanonical = resolveOne(p.done.canonicalID, byID, &v, base+".done.canonical_id", p.label())
		}
		out = append(out, row)
	}
	if v.Flush(w, http.StatusBadRequest) {
		return nil, nil, false
	}
	return out, workflows, true
}

// resolveOne resolves a canonical id. An empty id is an unset canonical — an
// unmapped write-target rule — not a fault.
func resolveOne(id string, byID map[string]domain.JiraStatusRef, v *httpx.Validation, field, projectKey string) domain.JiraStatusRef {
	if id == "" {
		return domain.JiraStatusRef{}
	}
	ref, known := byID[id]
	if !known {
		v.Invalid(field, "no status "+id+" in project "+projectKey+"'s workflow")
		return domain.JiraStatusRef{}
	}
	return ref
}

// requestedPickup reports the pickup member ids the caller asked for, and
// whether that differs from what is stored. An absent rule is not a request:
// the stored one is kept, which is how a client resends a set containing a rule
// it cannot express in ids — a row written before statuses were identified.
func requestedPickup(write *jiraPickupRuleWrite, stored []domain.JiraStatusRef) ruleWish {
	if write == nil || sameStatusSet(write.MemberIDs, stored) {
		return ruleWish{}
	}
	return ruleWish{changed: true, memberIDs: write.MemberIDs}
}

// requestedRule is requestedPickup for a write-target rule, whose canonical
// moves with its members.
func requestedRule(write *jiraStatusRuleWrite, storedMembers []domain.JiraStatusRef, storedCanonical domain.JiraStatusRef) ruleWish {
	if write == nil {
		return ruleWish{}
	}
	if sameStatusSet(write.MemberIDs, storedMembers) && write.CanonicalID == storedCanonical.ID {
		return ruleWish{}
	}
	return ruleWish{changed: true, memberIDs: write.MemberIDs, canonicalID: write.CanonicalID}
}

// sameStatusSet reports whether a set of requested ids names exactly the stored
// refs, order-insensitively. A stored ref with no id can never match a
// requested id, which is what makes a name-only rule always look changed — and
// therefore always get resolved, which is how it gains its ids.
func sameStatusSet(ids []string, stored []domain.JiraStatusRef) bool {
	if len(ids) != len(stored) {
		return false
	}
	want := make([]string, 0, len(stored))
	for _, ref := range stored {
		if ref.ID == "" {
			return false
		}
		want = append(want, ref.ID)
	}
	got := slices.Clone(ids)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

// writeJiraGateStopped ends the gate when Jira stops answering partway through
// a set. Faults already found in earlier elements win: they are the caller's
// own, they are true whether or not Jira is reachable, and they have to be
// fixed before any retry can store anything — whereas the upstream failure
// says only "try again". Reporting the 502 over them would throw away the half
// of the answer that is certain. Nothing is stored either way.
func writeJiraGateStopped(w http.ResponseWriter, v *httpx.Validation, orgID, what string, err error) {
	if v.Flush(w, http.StatusBadRequest) {
		serverLog.Warn("jira write gate could not reach jira; reporting the field faults found first",
			"org", orgID, "checking", what, "error", err)
		return
	}
	writeJiraGateUpstream(w, orgID, what, err)
}

func writeJiraGateUpstream(w http.ResponseWriter, orgID, what string, err error) {
	serverLog.Warn("jira write gate could not reach jira", "org", orgID, "checking", what, "error", err)
	httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
		Reason:  httpx.ReasonUpstreamUnavailable,
		Message: "could not confirm " + what + " with Jira, so nothing was saved" + httpx.LocalDetail(err),
	})
}
