package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The /usage activity feeds are the EE governance surface, behind
// FeatureGovernance. There are TWO resources, and they are two routes:
//
//   - .../artifacts/list → the objects the scope's runs changed, read from the
//     ledger: one item per object however many runs and actions changed it,
//     with the action that last did, how many actions and runs there were, and
//     the deliverable that is the same object, if any.
//   - .../actions/list → the append-only external-action audit log of record:
//     one row per external WRITE TF performed under an org credential (the
//     bot's GitHub/Jira mutations, the human-authorized GitHub approval
//     lifecycle, the Jira board mirror), who/what/when/from→to. The source of
//     truth for "what did TF do." Includes the rows no artifact is made of,
//     such as TF's own, refusals and failures.
//
// The two have different item shapes and different filter vocabularies, which
// is why they are separate routes rather than one route with a lens selector.
//
// Each resource has the same two scopes + gates (mirroring the spend
// endpoints):
//
//   - POST /api/teams/{team_id}/usage/{artifacts,actions}/list — one team's
//     history (team admin OR org admin).
//   - POST /api/orgs/{org_id}/usage/{artifacts,actions}/list — every team's
//     history (org admin), the System cross-team read; items carry their teams
//     (+ actor_name for actions), and team_id narrows to one team's rows.
//
// Beside each list, at both scopes and behind the same gates, a facets node
// (…/{artifacts,actions}/facets) answers the teams and people its filters
// offer and how many rows carry each.
//
// Unlicensed builds (local mode included) 404, agreeing with the FE
// useEntitlements gate that hides the surface entirely.

// objectActivityJSON is one item of the artifacts feeds: an object the
// scope's runs changed (domain.ObjectActivity). object_id is the spelling the
// actions lists' object_id filter takes. deliverable is the most recently
// created deliverable in the scope that is the same object
// (domain.ArtifactDeliverable), or null. name is the object's name within its
// repository (domain.Artifact.Name), "" for a kind target already names.
type objectActivityJSON struct {
	ObjectID          domain.ObjectRef         `json:"object_id"`
	ObjectKind        string                   `json:"object_kind"`
	Provider          string                   `json:"provider"`
	Target            string                   `json:"target"`
	Name              string                   `json:"name"`
	URL               string                   `json:"url"`
	LastAction        string                   `json:"last_action"`
	ActionCount       int                      `json:"action_count"`
	ConversationCount int                      `json:"run_count"`
	LastAt            time.Time                `json:"last_at"`
	Deliverable       *ArtifactDeliverableJSON `json:"deliverable"`
}

// ArtifactDeliverableJSON names the deliverable an artifact matched: enough
// to address it and read where it stands, with the deliverable's own read
// for the rest. The artifacts feeds here and a conversation's artifacts list
// both carry it.
type ArtifactDeliverableJSON struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
}

// orgObjectActivityJSON is the org feed's item: the teams whose runs changed
// the object, sorted by id, ride along so a cross-team item says whose bots
// acted. The team feed is one team and carries none.
type orgObjectActivityJSON struct {
	objectActivityJSON
	Teams []activityTeamJSON `json:"teams"`
}

// activityTeamJSON names a team on an org feed item. Name is empty for a team
// whose row is gone.
type activityTeamJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func toObjectActivityJSON(a domain.ObjectActivity, deliverables []domain.Deliverable) objectActivityJSON {
	out := objectActivityJSON{
		ObjectID:          a.ObjectID,
		ObjectKind:        a.ObjectKind,
		Provider:          a.Provider,
		Target:            a.Target,
		Name:              a.Name(),
		URL:               a.URL,
		LastAction:        a.LastAction,
		ActionCount:       a.ActionCount,
		ConversationCount: a.ConversationCount,
		LastAt:            a.LastAt,
	}
	if d, ok := domain.ArtifactDeliverable(a.Artifact, deliverables); ok {
		out.Deliverable = &ArtifactDeliverableJSON{ID: d.ID, Kind: d.Kind, State: d.State}
	}
	return out
}

// The artifacts feeds' bodies. The filters select ledger rows
// (db.ArtifactListOpts): Since / Until bound occurred_at to the half-open
// window [since, until), RFC3339 or YYYY-MM-DD, and both default to
// UNBOUNDED, since the feed is a full history, not a calendar month. The org
// feed adds team_id, the rows one team's runs wrote; the team feed names its
// team in the path, so a team_id in its body is an unknown field.
//
// Each body spells its fields out rather than embedding one shared struct:
// encoding/json names a type fault inside an embedded struct by the struct's
// Go name, which would reach the error's field. They meet in
// artifactActivityFilters.
// TODO(TFAC-1255): embed one shared filter struct once the decode error
// names the JSON field.

// artifactActivityListRequest is the body of POST
// /api/teams/{team_id}/usage/artifacts/list.
type artifactActivityListRequest struct {
	Provider   string `json:"provider"`
	ObjectKind string `json:"object_kind"`
	Since      string `json:"since"`
	Until      string `json:"until"`

	httpx.PageRequest
}

// orgArtifactActivityListRequest is the body of POST
// /api/orgs/{org_id}/usage/artifacts/list.
type orgArtifactActivityListRequest struct {
	Provider   string `json:"provider"`
	ObjectKind string `json:"object_kind"`
	TeamID     string `json:"team_id"`
	Since      string `json:"since"`
	Until      string `json:"until"`

	httpx.PageRequest
}

// artifactActivityFacetsRequest is the body of POST
// /api/teams/{team_id}/usage/artifacts/facets: the team list's filters, and
// the paging pair declared only to be refused (refuseFacetPaging).
type artifactActivityFacetsRequest struct {
	Provider   string `json:"provider"`
	ObjectKind string `json:"object_kind"`
	Since      string `json:"since"`
	Until      string `json:"until"`

	PageSize  json.RawMessage `json:"page_size"`
	PageToken json.RawMessage `json:"page_token"`
}

// orgArtifactActivityFacetsRequest is the body of POST
// /api/orgs/{org_id}/usage/artifacts/facets: the org list's filters.
type orgArtifactActivityFacetsRequest struct {
	Provider   string `json:"provider"`
	ObjectKind string `json:"object_kind"`
	TeamID     string `json:"team_id"`
	Since      string `json:"since"`
	Until      string `json:"until"`

	PageSize  json.RawMessage `json:"page_size"`
	PageToken json.RawMessage `json:"page_token"`
}

// artifactActivityFilters is the artifacts filters as any of the four bodies
// sent them, before validation. TeamID is empty on a team-scoped body.
type artifactActivityFilters struct {
	Provider, ObjectKind, TeamID, Since, Until string
}

// resolveArtifactActivityFilters validates the body's filters into the store's
// opts. Both vocabularies are closed: an unknown value is a 400 naming the
// field, never a filter that matches nothing and reads as "the bot did
// nothing".
func resolveArtifactActivityFilters(v *httpx.Validation, f artifactActivityFilters) db.ArtifactListOpts {
	opts := db.ArtifactListOpts{
		Provider:   resolveActivityProvider(v, f.Provider),
		ObjectKind: f.ObjectKind,
		TeamID:     f.TeamID,
	}
	if opts.ObjectKind != "" && !slices.Contains(domain.ObjectKinds(), opts.ObjectKind) {
		v.Invalid("object_kind", "must be one of: "+strings.Join(domain.ObjectKinds(), ", "))
	}
	httpx.CanonicalIDFilterField(v, "team_id", opts.TeamID, "team")
	opts.Since, opts.Until = resolveActivityWindow(v, f.Since, f.Until)
	return opts
}

// refuseFacetPaging refuses the paging pair on a facets body. The fields are
// declared rather than left to strict decoding's unknown-field fault because
// each is a real field of the sibling list: a caller sending one has misread
// what the node answers rather than mistyped a name. json.RawMessage, so
// presence is what is refused, whatever the value.
func refuseFacetPaging(v *httpx.Validation, pageSize, pageToken json.RawMessage) {
	const pages = "%s is not accepted: the facets are one bounded answer over the scope's teams and people, not a page"
	if pageSize != nil {
		v.Invalid("page_size", fmt.Sprintf(pages, "page_size"))
	}
	if pageToken != nil {
		v.Invalid("page_token", fmt.Sprintf(pages, "page_token"))
	}
}

// resolveActivityProvider validates both feeds' provider filter against
// domain.Providers. Like every filter here, a value is taken as sent: one with
// surrounding whitespace is no provider and is refused.
func resolveActivityProvider(v *httpx.Validation, raw string) string {
	if raw != "" && !slices.Contains(domain.Providers(), raw) {
		v.Invalid("provider", "must be one of: "+strings.Join(domain.Providers(), ", "))
	}
	return raw
}

// artifactKeys returns what a deliverable carries when it is the same object
// as one of items (domain.ArtifactDeliverableKeys). ok is false when there is
// nothing to match, so the reader skips the deliverable read.
func artifactKeys(items []domain.ObjectActivity) (entityIDs, reviewIDs []string, ok bool) {
	artifacts := make([]domain.Artifact, len(items))
	for i, a := range items {
		artifacts[i] = a.Artifact
	}
	entityIDs, reviewIDs = domain.ArtifactDeliverableKeys(artifacts)
	return entityIDs, reviewIDs, len(entityIDs)+len(reviewIDs) > 0
}

// handleUsageTeamArtifacts returns the objects one team's runs changed, by the
// latest action on each first. Gate: FeatureGovernance AND (team admin OR org
// admin). The ledger read runs under the caller's claims; the deliverables it
// matches are read on the admin pool, bound to the team, so an org admin who
// is on no team sees the same deliverables here that the org feed shows them.
// The team feed carries no teams (it is one team).
//
// POST /api/teams/{team_id}/usage/artifacts/list
func (h *Handler) handleUsageTeamArtifacts(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeamActivityCaller(w, r)
	if !ok {
		return
	}
	var req artifactActivityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	opts := resolveArtifactActivityFilters(&v, artifactActivityFilters{
		Provider: req.Provider, ObjectKind: req.ObjectKind, Since: req.Since, Until: req.Until,
	})
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(opts), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	opts.Limit, opts.Offset, opts.CountOnly = page.Limit, page.Offset, page.CountOnly

	var (
		items        []domain.ObjectActivity
		deliverables []domain.Deliverable
		total        int
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		items, total, e = tx.Artifacts.ListByTeam(r.Context(), orgID, teamID, opts)
		if e != nil {
			return e
		}
		entityIDs, reviewIDs, matchable := artifactKeys(items)
		if !matchable {
			return nil
		}
		deliverables, e = tx.Deliverables.ListArtifactMatchesByTeamSystem(r.Context(), orgID, teamID, entityIDs, reviewIDs)
		return e
	}); err != nil {
		httpx.InternalError(w, "usage", err)
		return
	}

	out := make([]objectActivityJSON, len(items))
	for i, a := range items {
		out[i] = toObjectActivityJSON(a, deliverables)
	}
	httpx.WriteList(w, page, out, total)
}

// handleUsageOrgArtifacts returns the objects every team's runs changed, by
// the latest action on each first. Gate: FeatureGovernance AND org admin — a
// cross-team read, so the ledger and the deliverables are both the System
// reads on the admin pool. Each item names its teams, resolved through
// Teams.GetSystem. team_id keeps the rows one team's runs wrote, so an item
// then names that team alone and counts its runs and actions.
//
// POST /api/orgs/{org_id}/usage/artifacts/list
func (h *Handler) handleUsageOrgArtifacts(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveGovernedOrgAdmin(w, r)
	if !ok {
		return
	}
	var req orgArtifactActivityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	opts := resolveArtifactActivityFilters(&v, artifactActivityFilters{
		Provider: req.Provider, ObjectKind: req.ObjectKind, TeamID: req.TeamID, Since: req.Since, Until: req.Until,
	})
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(opts), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	opts.Limit, opts.Offset, opts.CountOnly = page.Limit, page.Offset, page.CountOnly

	var (
		items        []domain.ObjectActivity
		deliverables []domain.Deliverable
		total        int
		teamNames    map[string]string
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		items, total, e = tx.Artifacts.ListByOrgSystem(r.Context(), orgID, opts)
		if e != nil {
			return e
		}
		var teamIDs []string
		for _, a := range items {
			teamIDs = append(teamIDs, a.TeamIDs...)
		}
		if teamNames, e = resolveTeamNames(r.Context(), tx, orgID, teamIDs); e != nil {
			return e
		}
		entityIDs, reviewIDs, matchable := artifactKeys(items)
		if !matchable {
			return nil
		}
		deliverables, e = tx.Deliverables.ListArtifactMatchesByOrgSystem(r.Context(), orgID, entityIDs, reviewIDs)
		return e
	}); err != nil {
		httpx.InternalError(w, "usage", err)
		return
	}

	out := make([]orgObjectActivityJSON, len(items))
	for i, a := range items {
		teams := make([]activityTeamJSON, len(a.TeamIDs))
		for j, id := range a.TeamIDs {
			teams[j] = activityTeamJSON{ID: id, Name: teamNames[id]}
		}
		out[i] = orgObjectActivityJSON{objectActivityJSON: toObjectActivityJSON(a, deliverables), Teams: teams}
	}
	httpx.WriteList(w, page, out, total)
}

// --- The activity feeds' facets nodes ---
//
// Each list has a facets node beside it at the same scope, answering the
// values its select filters offer and how many rows carry each. A cut is
// computed under the list's filters except its own, so by_team still offers
// every team while one is selected, and a client cannot derive the set from
// the list either: it holds one page of a filtered query. No paging and no
// total: each cut is bounded by the scope's teams or people. The gates are the
// list's, and the same node name answers the same cuts at both scopes (the
// team scope's by_team is its one team).

// activityFacetJSON is one value of an activity facets cut: the id the list's
// filter takes, how many rows carry it, and the name the list's rows already
// show for it (team_name, actor_name), so a client can label the option with
// nothing the list does not already disclose. Name is empty for a team or a
// user whose row is gone.
type activityFacetJSON struct {
	httpx.FacetJSON
	Name string `json:"name"`
}

// artifactActivityFacetsJSON is the artifacts facets node's fixed schema:
// by_team counts the objects each team's runs changed.
type artifactActivityFacetsJSON struct {
	ByTeam []activityFacetJSON `json:"by_team"`
}

// actionActivityFacetsJSON is the actions facets node's fixed schema: by_actor
// counts the rows each person performed or authorized, by_team the rows each
// team holds.
type actionActivityFacetsJSON struct {
	ByActor []activityFacetJSON `json:"by_actor"`
	ByTeam  []activityFacetJSON `json:"by_team"`
}

// namedFacets renders a cut with each value's name, ordered by name the way a
// select lists its options (case-folded, then by id), with the values that
// have no name last. Never nil: a cut nothing matched is `[]`.
func namedFacets(in []db.Facet, names map[string]string) []activityFacetJSON {
	out := make([]activityFacetJSON, len(in))
	for i, f := range in {
		out[i] = activityFacetJSON{FacetJSON: httpx.FacetJSON{Value: f.Value, Count: f.Count}, Name: names[f.Value]}
	}
	slices.SortStableFunc(out, func(a, b activityFacetJSON) int {
		if (a.Name == "") != (b.Name == "") {
			if a.Name == "" {
				return 1
			}
			return -1
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
	return out
}

// facetValues is the values of a cut, for the name resolvers.
func facetValues(in []db.Facet) []string {
	out := make([]string, len(in))
	for i, f := range in {
		out[i] = f.Value
	}
	return out
}

// handleUsageTeamArtifactFacets answers the team artifacts feed's facets.
// Same gates and pools as the list.
//
// POST /api/teams/{team_id}/usage/artifacts/facets
func (h *Handler) handleUsageTeamArtifactFacets(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeamActivityCaller(w, r)
	if !ok {
		return
	}
	var req artifactActivityFacetsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	refuseFacetPaging(&v, req.PageSize, req.PageToken)
	opts := resolveArtifactActivityFilters(&v, artifactActivityFilters{
		Provider: req.Provider, ObjectKind: req.ObjectKind, Since: req.Since, Until: req.Until,
	})
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	h.writeArtifactFacets(w, r, orgID, userID, func(tx db.TxStores) (db.ArtifactFacets, error) {
		return tx.Artifacts.FacetsByTeam(r.Context(), orgID, teamID, opts)
	})
}

// handleUsageOrgArtifactFacets answers the org artifacts feed's facets. Same
// gates and pools as the list.
//
// POST /api/orgs/{org_id}/usage/artifacts/facets
func (h *Handler) handleUsageOrgArtifactFacets(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveGovernedOrgAdmin(w, r)
	if !ok {
		return
	}
	var req orgArtifactActivityFacetsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	refuseFacetPaging(&v, req.PageSize, req.PageToken)
	opts := resolveArtifactActivityFilters(&v, artifactActivityFilters{
		Provider: req.Provider, ObjectKind: req.ObjectKind, TeamID: req.TeamID, Since: req.Since, Until: req.Until,
	})
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	h.writeArtifactFacets(w, r, orgID, userID, func(tx db.TxStores) (db.ArtifactFacets, error) {
		return tx.Artifacts.FacetsByOrgSystem(r.Context(), orgID, opts)
	})
}

// writeArtifactFacets runs read under the caller's claims, names the teams
// the way the org list names them (Teams.GetSystem), and writes the cuts.
func (h *Handler) writeArtifactFacets(w http.ResponseWriter, r *http.Request, orgID, userID string, read func(db.TxStores) (db.ArtifactFacets, error)) {
	var (
		facets    db.ArtifactFacets
		teamNames map[string]string
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if facets, e = read(tx); e != nil {
			return e
		}
		teamNames, e = resolveTeamNames(r.Context(), tx, orgID, facetValues(facets.ByTeam))
		return e
	}); err != nil {
		httpx.InternalError(w, "usage", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, artifactActivityFacetsJSON{ByTeam: namedFacets(facets.ByTeam, teamNames)})
}

// handleUsageTeamActionFacets answers the team actions feed's facets. Same
// gates and pools as the list.
//
// POST /api/teams/{team_id}/usage/actions/facets
func (h *Handler) handleUsageTeamActionFacets(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeamActivityCaller(w, r)
	if !ok {
		return
	}
	var req actionActivityFacetsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	refuseFacetPaging(&v, req.PageSize, req.PageToken)
	opts := resolveActionActivityFilters(&v, actionActivityFilters{
		Provider: req.Provider, Action: req.Action, Actor: req.Actor, Since: req.Since, Until: req.Until,
	})
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	h.writeActionFacets(w, r, orgID, userID, func(tx db.TxStores) (db.ExternalActionFacets, error) {
		return tx.ExternalActions.FacetsByTeam(r.Context(), orgID, teamID, opts)
	})
}

// handleUsageOrgActionFacets answers the org actions feed's facets. Same
// gates and pools as the list.
//
// POST /api/orgs/{org_id}/usage/actions/facets
func (h *Handler) handleUsageOrgActionFacets(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveGovernedOrgAdmin(w, r)
	if !ok {
		return
	}
	var req orgActionActivityFacetsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	refuseFacetPaging(&v, req.PageSize, req.PageToken)
	opts := resolveActionActivityFilters(&v, actionActivityFilters{
		Provider: req.Provider, Action: req.Action, Actor: req.Actor, TeamID: req.TeamID, Since: req.Since, Until: req.Until,
	})
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	h.writeActionFacets(w, r, orgID, userID, func(tx db.TxStores) (db.ExternalActionFacets, error) {
		return tx.ExternalActions.FacetsByOrgSystem(r.Context(), orgID, opts)
	})
}

// writeActionFacets runs read under the caller's claims, names the teams and
// people the way the lists name them (Teams.GetSystem, Users.GetProfile), and
// writes the cuts.
func (h *Handler) writeActionFacets(w http.ResponseWriter, r *http.Request, orgID, userID string, read func(db.TxStores) (db.ExternalActionFacets, error)) {
	var (
		facets     db.ExternalActionFacets
		teamNames  map[string]string
		actorNames map[string]string
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if facets, e = read(tx); e != nil {
			return e
		}
		if teamNames, e = resolveTeamNames(r.Context(), tx, orgID, facetValues(facets.ByTeam)); e != nil {
			return e
		}
		actorNames, e = resolveActorNames(r.Context(), tx, facetValues(facets.ByActor))
		return e
	}); err != nil {
		httpx.InternalError(w, "usage", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, actionActivityFacetsJSON{
		ByActor: namedFacets(facets.ByActor, actorNames),
		ByTeam:  namedFacets(facets.ByTeam, teamNames),
	})
}

// resolveActivityWindow parses the shared half-open [since, until) filter. Each
// side applies only when supplied, so the zero value is unbounded; a
// non-positive window is rejected only when BOTH bounds are present (a single
// open side is fine), mirroring httpx.ParseWindow.
func resolveActivityWindow(v *httpx.Validation, since, until string) (time.Time, time.Time) {
	var sinceT, untilT time.Time
	if since != "" {
		t, err := httpx.ParseWindowTime(since)
		if err != nil {
			v.Invalid("since", "must be RFC3339 or YYYY-MM-DD")
		} else {
			sinceT = t
		}
	}
	if until != "" {
		t, err := httpx.ParseWindowTime(until)
		if err != nil {
			v.Invalid("until", "must be RFC3339 or YYYY-MM-DD")
		} else {
			untilT = t
		}
	}
	if !sinceT.IsZero() && !untilT.IsZero() && !sinceT.Before(untilT) {
		v.Invalid("since", "since must be before until")
	}
	return sinceT, untilT
}

// resolveTeamActivityCaller runs the shared front gate for both team-scoped
// activity feeds: the one {team_id} grammar, the cross-org 404, the
// team-admin-or-org-admin role, and THEN the governance entitlement.
//
// Role before licence, for the reason resolveGovernedOrgAdmin gives: a caller
// who fails the role check gets the same 403 licensed or not, so no non-admin
// can read the org's plan tier off a status code.
func (h *Handler) resolveTeamActivityCaller(w http.ResponseWriter, r *http.Request) (orgID, userID, teamID string, ok bool) {
	orgID, userID, ok = h.resolveCaller(w, r)
	if !ok {
		return "", "", "", false
	}
	// The one {team_id} grammar; a segment that resolves to nothing is "not
	// found" (parity with handleUsageTeam), not a role failure or a 500 from a
	// uuid cast downstream.
	teamID, ok = h.az.TeamIDFromPath(w, r, "usage", orgID, userID)
	if !ok {
		return "", "", "", false
	}
	// Confirm the team is in the caller's org first so a cross-org id 404s cleanly
	// (non-disclosure) rather than falling through to the 403.
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return "", "", "", false
	}
	if !h.requireTeamOrOrgAdmin(w, r, orgID, userID, teamID) {
		return "", "", "", false
	}
	if !requireGovernance(w, r, orgID) {
		return "", "", "", false
	}
	return orgID, userID, teamID, true
}

// requireGovernance gates a handler on the FeatureGovernance entitlement for
// orgID, writing a 404 (not 403) and returning false when it's unlicensed.
// The 404 matches the FE useEntitlements gate, which hides the surface
// entirely in an unlicensed build — so the route reads as "not here", not
// "forbidden". The check is mode-agnostic (entitlements never carve out by
// runmode): a community / unlicensed build — local mode included — answers
// false and 404s.
//
// It runs AFTER the caller's role check on every route that uses it, so the
// 404 is only ever shown to someone who would otherwise have been allowed in.
// A caller who fails the role check never reaches here, and therefore cannot
// tell a licensed deployment from an unlicensed one by the status they get.
func requireGovernance(w http.ResponseWriter, r *http.Request, orgID string) bool {
	if !entitlements.For(orgID).Has(entitlements.FeatureGovernance) {
		httpx.NotFound(w, "route")
		return false
	}
	return true
}

// requireTeamOrOrgAdmin authorizes the team-scoped bot-activity feed: the caller
// must be an admin of teamID OR an org admin. It writes a 403 and returns false
// otherwise. Local mode short-circuits to allowed (N=1, the single user owns
// the org) — the governance gate that follows still 404s an unlicensed build,
// local included, so the route stays hidden there. VerifyTeamInOrg has already
// confirmed teamID is in the caller's org, so a non-admin here is a clean 403,
// never a cross-org probe. The raw probes don't short-circuit local, hence the
// guard.
func (h *Handler) requireTeamOrOrgAdmin(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	isTeamAdmin, err := h.az.UserIsTeamAdmin(r.Context(), userID, orgID, teamID)
	if err != nil {
		httpx.InternalError(w, "usage", err)
		return false
	}
	if isTeamAdmin {
		return true
	}
	isOrgAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		httpx.InternalError(w, "usage", err)
		return false
	}
	if isOrgAdmin {
		return true
	}
	httpx.Forbidden(w, "team admin or org admin role required")
	return false
}

// --- The actions feed (the external-action audit log) ---

// ActionJSON is the action-log feed's wire shape — one external_actions row
// projected for the FE. The team_id/team_name/actor_name fields populate ONLY the
// org-wide feed (so an org-admin row shows which team's bot acted and who
// authorized it) and are omitted (omitempty) on the team-scoped feed. A
// conversation's actions list (POST
// /api/agent/conversations/{conversationID}/actions/list) serves the same shape
// and sets none of the three.
type ActionJSON struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	Scope          string `json:"scope"`
	Action         string `json:"action"`
	Target         string `json:"target"`
	ExternalID     string `json:"external_id,omitempty"`
	URL            string `json:"url,omitempty"`
	FromState      string `json:"from_state,omitempty"`
	ToState        string `json:"to_state,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	ActorUserID    string `json:"actor_user_id,omitempty"`
	PerformedBy    string `json:"performed_by"`
	// ObjectKind and ObjectID name the object the action changed; ObjectID
	// is the value an artifact's object_id carries. A row that names no
	// object has neither, and one whose kind's identifying columns are empty
	// has no ObjectID.
	ObjectKind string           `json:"object_kind,omitempty"`
	ObjectID   domain.ObjectRef `json:"object_id,omitempty"`
	Credential string           `json:"credential"`
	Details    json.RawMessage  `json:"details,omitempty"`
	OccurredAt time.Time        `json:"occurred_at"`
	// org feed only:
	TeamID    string `json:"team_id,omitempty"`
	TeamName  string `json:"team_name,omitempty"`
	ActorName string `json:"actor_name,omitempty"`
}

// ToActionJSON projects a stored action onto the wire shape, emitting detail_json
// as embedded JSON (omitted when absent/corrupt — the row's who/what/when/from→to
// stand on their own).
func ToActionJSON(a domain.ExternalAction) ActionJSON {
	var details json.RawMessage
	if a.DetailJSON != "" && json.Valid([]byte(a.DetailJSON)) {
		details = json.RawMessage(a.DetailJSON)
	}
	objectID, _ := domain.ObjectID(a)
	return ActionJSON{
		ID:             a.ID,
		Provider:       a.Provider,
		Scope:          a.Scope,
		Action:         a.Action,
		Target:         a.Target,
		ExternalID:     a.ExternalID,
		URL:            a.URL,
		FromState:      a.FromState,
		ToState:        a.ToState,
		ConversationID: a.ConversationID,
		ActorUserID:    a.ActorUserID,
		PerformedBy:    a.PerformedBy,
		ObjectKind:     a.ObjectKind,
		ObjectID:       domain.ObjectRef(objectID),
		Credential:     a.Credential,
		Details:        details,
		OccurredAt:     a.OccurredAt,
	}
}

// The action-log feeds' bodies. They share provider and the time window with
// the artifacts bodies but filter on the action discriminator and the actor
// user id instead of the object kind — the two resources' filter vocabularies
// are genuinely different, which is why they are two routes rather than one
// with a lens selector. Since / Until bound occurred_at (not created_at) to
// [since, until). actor is the actor_user_id the rows carry. The org feed adds
// team_id; the team feed names its team in the path. The four bodies spell
// their fields out for the reason the artifacts bodies do, and meet in
// actionActivityFilters.

// actionActivityListRequest is the body of POST
// /api/teams/{team_id}/usage/actions/list.
type actionActivityListRequest struct {
	Provider string `json:"provider"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	Since    string `json:"since"`
	Until    string `json:"until"`

	httpx.PageRequest
}

// orgActionActivityListRequest is the body of POST
// /api/orgs/{org_id}/usage/actions/list.
type orgActionActivityListRequest struct {
	Provider string `json:"provider"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	TeamID   string `json:"team_id"`
	Since    string `json:"since"`
	Until    string `json:"until"`

	httpx.PageRequest
}

// actionActivityFacetsRequest is the body of POST
// /api/teams/{team_id}/usage/actions/facets: the team list's filters, and the
// paging pair declared only to be refused (refuseFacetPaging).
type actionActivityFacetsRequest struct {
	Provider string `json:"provider"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	Since    string `json:"since"`
	Until    string `json:"until"`

	PageSize  json.RawMessage `json:"page_size"`
	PageToken json.RawMessage `json:"page_token"`
}

// orgActionActivityFacetsRequest is the body of POST
// /api/orgs/{org_id}/usage/actions/facets: the org list's filters.
type orgActionActivityFacetsRequest struct {
	Provider string `json:"provider"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	TeamID   string `json:"team_id"`
	Since    string `json:"since"`
	Until    string `json:"until"`

	PageSize  json.RawMessage `json:"page_size"`
	PageToken json.RawMessage `json:"page_token"`
}

// actionActivityFilters is the actions filters as any of the four bodies sent
// them, before validation. TeamID is empty on a team-scoped body.
type actionActivityFilters struct {
	Provider, Action, Actor, TeamID, Since, Until string
}

func resolveActionActivityFilters(v *httpx.Validation, f actionActivityFilters) domain.ExternalActionListOpts {
	opts := domain.ExternalActionListOpts{
		Provider:    resolveActivityProvider(v, f.Provider),
		Action:      f.Action,
		ActorUserID: f.Actor,
		TeamID:      f.TeamID,
	}
	if opts.Action != "" && !slices.Contains(domain.ExternalActionTypes(), opts.Action) {
		v.Invalid("action", "not a known action type")
	}
	httpx.CanonicalIDFilterField(v, "actor", opts.ActorUserID, "user")
	httpx.CanonicalIDFilterField(v, "team_id", opts.TeamID, "team")
	opts.Since, opts.Until = resolveActivityWindow(v, f.Since, f.Until)
	return opts
}

// handleUsageTeamActions returns one team's external-action history, newest
// first. Same gates as the team artifacts feed. The team feed omits
// team_id/team_name (it's already one team) but DOES resolve the authorizing
// actor's display name — this is the audit surface a team admin reads, so raw
// actor UUIDs would be poor UX, and resolving over one page's small
// distinct-actor set is cheap.
//
// POST /api/teams/{team_id}/usage/actions/list
func (h *Handler) handleUsageTeamActions(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeamActivityCaller(w, r)
	if !ok {
		return
	}

	var req actionActivityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	opts := resolveActionActivityFilters(&v, actionActivityFilters{
		Provider: req.Provider, Action: req.Action, Actor: req.Actor, Since: req.Since, Until: req.Until,
	})
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(opts), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	opts.Limit, opts.Offset, opts.CountOnly = page.Limit, page.Offset, page.CountOnly

	var (
		actions    []domain.ExternalAction
		total      int
		actorNames map[string]string
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actions, total, e = tx.ExternalActions.ListByTeam(r.Context(), orgID, teamID, opts)
		if e != nil {
			return e
		}
		actorNames, e = resolveActionActorNames(r.Context(), tx, actions)
		return e
	}); err != nil {
		httpx.InternalError(w, "usage", err)
		return
	}
	out := make([]ActionJSON, len(actions))
	for i, a := range actions {
		j := ToActionJSON(a)
		j.ActorName = actorNames[a.ActorUserID]
		out[i] = j
	}
	httpx.WriteList(w, page, out, total)
}

// handleUsageOrgActions returns the org-wide external-action history across
// every team, newest first. It uses the System aggregate (ListByOrgSystem,
// admin pool) because crossing teams is the point, then resolves each row's
// team name (Teams.GetSystem, admin pool) and human actor's display name
// (Users.GetProfile, org-scoped under the caller's claims) so the feed reads
// "team X's bot did Y, authorized by Z". team_id keeps one team's rows.
//
// POST /api/orgs/{org_id}/usage/actions/list
func (h *Handler) handleUsageOrgActions(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveGovernedOrgAdmin(w, r)
	if !ok {
		return
	}

	var req orgActionActivityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	opts := resolveActionActivityFilters(&v, actionActivityFilters{
		Provider: req.Provider, Action: req.Action, Actor: req.Actor, TeamID: req.TeamID, Since: req.Since, Until: req.Until,
	})
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(opts), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	opts.Limit, opts.Offset, opts.CountOnly = page.Limit, page.Offset, page.CountOnly

	var (
		actions    []domain.ExternalAction
		total      int
		teamNames  map[string]string
		actorNames map[string]string
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actions, total, e = tx.ExternalActions.ListByOrgSystem(r.Context(), orgID, opts)
		if e != nil {
			return e
		}
		teamNames, e = resolveActionTeamNames(r.Context(), tx, orgID, actions)
		if e != nil {
			return e
		}
		actorNames, e = resolveActionActorNames(r.Context(), tx, actions)
		return e
	}); err != nil {
		httpx.InternalError(w, "usage", err)
		return
	}
	out := make([]ActionJSON, len(actions))
	for i, a := range actions {
		j := ToActionJSON(a)
		j.TeamID = a.TeamID
		j.TeamName = teamNames[a.TeamID]
		j.ActorName = actorNames[a.ActorUserID]
		out[i] = j
	}
	httpx.WriteList(w, page, out, total)
}

// resolveActionTeamNames maps each distinct team in the action set to its
// name. Actions with no team (a dashboard board-drag, a team-less mirror) are
// skipped — they carry no chip.
func resolveActionTeamNames(ctx context.Context, tx db.TxStores, orgID string, actions []domain.ExternalAction) (map[string]string, error) {
	teamIDs := make([]string, 0, len(actions))
	for _, a := range actions {
		teamIDs = append(teamIDs, a.TeamID)
	}
	return resolveTeamNames(ctx, tx, orgID, teamIDs)
}

// resolveTeamNames maps each distinct non-empty id of teamIDs to its team's
// name via the ADMIN pool (Teams.GetSystem): the org feeds cross teams the
// caller may not belong to. A team with no row maps to "". N+1 over the small
// distinct-id set is fine: a page is ~50 rows, far fewer distinct teams.
func resolveTeamNames(ctx context.Context, tx db.TxStores, orgID string, teamIDs []string) (map[string]string, error) {
	names := map[string]string{}
	for _, id := range teamIDs {
		if id == "" {
			continue
		}
		if _, done := names[id]; done {
			continue
		}
		t, err := tx.Teams.GetSystem(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		name := ""
		if t != nil {
			name = t.Name
		}
		names[id] = name
	}
	return names, nil
}

// resolveActionActorNames maps each distinct human actor in the action set to
// a display name (resolveActorNames). System actions (empty actor_user_id —
// event-triggered bot runs, the Jira mirror) carry no actor and are skipped.
func resolveActionActorNames(ctx context.Context, tx db.TxStores, actions []domain.ExternalAction) (map[string]string, error) {
	userIDs := make([]string, 0, len(actions))
	for _, a := range actions {
		userIDs = append(userIDs, a.ActorUserID)
	}
	return resolveActorNames(ctx, tx, userIDs)
}

// resolveActorNames maps each distinct non-empty id of userIDs to a display
// name via the app pool (Users.GetProfile, org-scoped RLS — a co-member's
// profile is readable, the same path the spend by-member breakdown uses). A
// user whose profile is not readable maps to "". N+1 over the small
// distinct-id set is fine: a page, or a facet cut, names few people.
func resolveActorNames(ctx context.Context, tx db.TxStores, userIDs []string) (map[string]string, error) {
	names := map[string]string{}
	for _, id := range userIDs {
		if id == "" {
			continue
		}
		if _, done := names[id]; done {
			continue
		}
		name, _, err := tx.Users.GetProfile(ctx, id)
		if err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, nil
}
