package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
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
// first headless caller breaks it. Both are asked live — the same catalog read
// POST /api/jira/projects/list serves from, and the same ProjectStatuses read
// GET /api/jira/statuses serves from — because a catalog of dozens behind a
// single org credential has nothing for a mirror to earn. The rules rows are
// the only persistence: they snapshot the statuses a team actually acts on, at
// the moment it arms them.
//
// Both checks are scoped to what CHANGED, and that scoping is the load-bearing
// part. This route is a replace-set: every write resends the whole set. Gating
// what was already stored would mean a project deleted upstream, a status
// retired from a workflow, or a Jira outage locks the team out of its own
// configuration door — including the write that would remove the offending row.
// So a stored key is grandfathered, an unchanged rule is kept byte-for-byte,
// and a write that only removes things asks Jira nothing at all.
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

const (
	// jiraGateWindow is the page size the gate reads the filtered catalog in.
	// Cloud caps its own page below this and serves fewer; Data Center honors
	// it against the catalog it holds in memory.
	jiraGateWindow = 100
	// jiraGateMaxPages bounds the catalog walk. Jira matches the filter against
	// key AND name, so a key that is a common word can be preceded by pages of
	// name-matches; this is what stops that from becoming an unbounded read.
	jiraGateMaxPages = 20
)

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
	wish   jiraProjectWish
	row    domain.JiraProjectStatusRules
	newKey bool

	pickup, inProgress, inReview, done ruleWish
}

// changedIDs is every status id this plan still has to resolve, across all four
// rules — the one place the rule set is enumerated for the "does this project
// need Jira at all?" question.
func (p projectPlan) changedIDs() int {
	return len(p.pickup.ids()) + len(p.inProgress.ids()) + len(p.inReview.ids()) + len(p.done.ids())
}

func (p projectPlan) needsJira() bool {
	return p.newKey || p.changedIDs() > 0
}

// gateJiraProjects turns the request's wishes into the rules to store.
//
// Each rule is resolved one of two ways. Carried: the caller left it out, or
// sent exactly what is stored — the stored refs are kept verbatim, ids, names
// and all, and Jira is not asked. Resolved: the caller sent a different set of
// status ids — each is looked up in the project's workflow, which both refuses
// the unknown ones and supplies the display names.
//
// It writes the response and returns false when the write must not proceed.
func (s *Server) gateJiraProjects(
	w http.ResponseWriter, r *http.Request,
	orgID, userID string,
	wishes []jiraProjectWish, stored []domain.JiraProjectStatusRules,
) ([]domain.JiraProjectStatusRules, bool) {
	storedByKey := make(map[string]domain.JiraProjectStatusRules, len(stored))
	for _, p := range stored {
		storedByKey[p.ProjectKey] = p
	}

	// First pass: settle what each project needs from Jira, without asking yet.
	// A set that needs nothing must not resolve a credential, let alone make a
	// call — that is what keeps the door open while Jira is down.
	plans := make([]projectPlan, 0, len(wishes))
	needsJira := false
	for _, wish := range wishes {
		prior, isStored := storedByKey[wish.key]
		p := projectPlan{
			wish:       wish,
			row:        domain.JiraProjectStatusRules{ProjectKey: wish.key},
			newKey:     !isStored,
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
		return out, true
	}

	client, ok := s.jiraGateClient(w, r, orgID, userID)
	if !ok {
		return nil, false
	}

	var v httpx.Validation
	out := make([]domain.JiraProjectStatusRules, 0, len(plans))
	for _, p := range plans {
		base := fmt.Sprintf("jira_projects[%d]", p.wish.index)
		if p.newKey {
			visible, err := jiraProjectVisible(r.Context(), client, p.wish.key)
			if err != nil {
				writeJiraGateStopped(w, &v, orgID, "project "+p.wish.key, err)
				return nil, false
			}
			if !visible {
				v.Invalid(base+".key", "no Jira project "+p.wish.key+" is visible to this workspace's Jira credential")
				continue
			}
		}
		if p.changedIDs() == 0 {
			out = append(out, p.row)
			continue
		}
		// One fetch per project with a changed rule, shared by all four rules
		// and doing both jobs at once: it is the gate, and it is where every
		// display name comes from.
		statuses, err := client.ProjectStatuses(r.Context(), p.wish.key)
		if err != nil {
			writeJiraGateStopped(w, &v, orgID, "the statuses of project "+p.wish.key, err)
			return nil, false
		}
		byID := make(map[string]domain.JiraStatusRef, len(statuses))
		for _, st := range statuses {
			byID[st.ID] = domain.JiraStatusRef{ID: st.ID, Name: st.Name}
		}
		resolve := func(ids []string, field string) []domain.JiraStatusRef {
			refs := make([]domain.JiraStatusRef, 0, len(ids))
			for _, id := range ids {
				ref := resolveOne(id, byID, &v, field, p.wish.key)
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
			row.InProgressCanonical = resolveOne(p.inProgress.canonicalID, byID, &v, base+".in_progress.canonical_id", p.wish.key)
		}
		if p.inReview.changed {
			row.InReviewMembers = resolve(p.inReview.memberIDs, base+".in_review.member_ids")
			row.InReviewCanonical = resolveOne(p.inReview.canonicalID, byID, &v, base+".in_review.canonical_id", p.wish.key)
		}
		if p.done.changed {
			row.DoneMembers = resolve(p.done.memberIDs, base+".done.member_ids")
			row.DoneCanonical = resolveOne(p.done.canonicalID, byID, &v, base+".done.canonical_id", p.wish.key)
		}
		out = append(out, row)
	}
	if v.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	return out, true
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

// jiraGateClient resolves the org's Jira service credential into a client. It
// is called only once something actually needs verifying, so an unconnected
// workspace can still remove a project it can no longer map.
func (s *Server) jiraGateClient(w http.ResponseWriter, r *http.Request, orgID, userID string) (*jira.Client, bool) {
	// Read through the app pool inside WithTx so the org_secrets read runs
	// under the caller's claims — the same door GET /api/jira/statuses uses for
	// the same credential.
	var creds auth.Credentials
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		creds, lerr = integrations.Load(r.Context(), tx.Secrets, orgID)
		if lerr != nil {
			return fmt.Errorf("load integration credentials: %w", lerr)
		}
		return nil
	}); err != nil {
		internalError(w, "settings/team/jira-projects", err)
		return nil, false
	}
	cfg, ok := integrations.JiraSystemConfig(creds)
	if !ok {
		writeNotConfigured(w, "Jira is not connected for this workspace, so a project or a status mapping cannot be added")
		return nil, false
	}
	return jira.NewClient(cfg), true
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

// jiraProjectVisible reports whether key names a project this credential can
// see, reading the same live catalog the picker's list route serves from.
//
// The filter is matched against key AND name upstream, so the exact key is
// looked for across the pages that filter returns rather than assumed to be the
// first row. Exhausting the page budget is an ERROR, not an absence: the walk
// stopped without establishing anything, and reporting "no such project" there
// would refuse a project that exists.
func jiraProjectVisible(ctx context.Context, client *jira.Client, key string) (bool, error) {
	startAt := 0
	for page := 0; page < jiraGateMaxPages; page++ {
		got, err := client.ListProjects(ctx, key, startAt, jiraGateWindow)
		if err != nil {
			return false, err
		}
		for _, p := range got.Projects {
			if normalizeJiraProjectKey(p.Key) == key {
				return true, nil
			}
		}
		if got.NextStartAt == 0 {
			return false, nil
		}
		startAt = got.NextStartAt
	}
	return false, fmt.Errorf("jira project catalog for %s still had pages after %d", key, jiraGateMaxPages)
}
