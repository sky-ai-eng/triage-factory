package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

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
//     (+ actor_name for actions).
//
// Unlicensed builds (local mode included) 404, agreeing with the FE
// useEntitlements gate that hides the surface entirely.

// objectActivityJSON is one item of the artifacts feeds: an object the
// scope's runs changed (domain.ObjectActivity). object_id is the spelling the
// actions lists' object_id filter takes. deliverable is the most recently
// created deliverable in the scope that is the same object
// (domain.ArtifactDeliverable), or null.
type objectActivityJSON struct {
	ObjectID    domain.ObjectRef         `json:"object_id"`
	ObjectKind  string                   `json:"object_kind"`
	Provider    string                   `json:"provider"`
	Target      string                   `json:"target"`
	URL         string                   `json:"url"`
	LastAction  string                   `json:"last_action"`
	ActionCount int                      `json:"action_count"`
	RunCount    int                      `json:"run_count"`
	LastAt      time.Time                `json:"last_at"`
	Deliverable *artifactDeliverableJSON `json:"deliverable"`
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
		ObjectID:    a.ObjectID,
		ObjectKind:  a.ObjectKind,
		Provider:    a.Provider,
		Target:      a.Target,
		URL:         a.URL,
		LastAction:  a.LastAction,
		ActionCount: a.ActionCount,
		RunCount:    a.RunCount,
		LastAt:      a.LastAt,
	}
	if d, ok := domain.ArtifactDeliverable(a.Artifact, deliverables); ok {
		out.Deliverable = &artifactDeliverableJSON{ID: d.ID, Kind: d.Kind, State: d.State}
	}
	return out
}

// artifactActivityListRequest is the body of the artifacts feeds. The filters
// select ledger rows (db.ArtifactListOpts): Since / Until bound occurred_at to
// the half-open window [since, until), RFC3339 or YYYY-MM-DD, and both default
// to UNBOUNDED, since the feed is a full history, not a calendar month.
type artifactActivityListRequest struct {
	Provider   string `json:"provider"`
	ObjectKind string `json:"object_kind"`
	Since      string `json:"since"`
	Until      string `json:"until"`

	httpx.PageRequest
}

// resolveArtifactActivityFilters validates the body's filters into the store's
// opts. Both vocabularies are closed: an unknown value is a 400 naming the
// field, never a filter that matches nothing and reads as "the bot did
// nothing".
func resolveArtifactActivityFilters(v *httpx.Validation, req artifactActivityListRequest) db.ArtifactListOpts {
	opts := db.ArtifactListOpts{
		Provider:   resolveActivityProvider(v, req.Provider),
		ObjectKind: strings.TrimSpace(req.ObjectKind),
	}
	if opts.ObjectKind != "" && !slices.Contains(domain.ObjectKinds(), opts.ObjectKind) {
		v.Invalid("object_kind", "must be one of: "+strings.Join(domain.ObjectKinds(), ", "))
	}
	opts.Since, opts.Until = resolveActivityWindow(v, req.Since, req.Until)
	return opts
}

// resolveActivityProvider validates both feeds' provider filter against
// domain.Providers.
func resolveActivityProvider(v *httpx.Validation, raw string) string {
	provider := strings.TrimSpace(raw)
	if provider != "" && !slices.Contains(domain.Providers(), provider) {
		v.Invalid("provider", "must be one of: "+strings.Join(domain.Providers(), ", "))
	}
	return provider
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
func (h *usageHandler) handleUsageTeamArtifacts(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeamActivityCaller(w, r)
	if !ok {
		return
	}
	var req artifactActivityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	opts := resolveArtifactActivityFilters(&v, req)
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
		internalError(w, "usage", err)
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
// Teams.GetSystem.
//
// POST /api/orgs/{org_id}/usage/artifacts/list
func (h *usageHandler) handleUsageOrgArtifacts(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveGovernedOrgAdmin(w, r)
	if !ok {
		return
	}
	var req artifactActivityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	opts := resolveArtifactActivityFilters(&v, req)
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
		internalError(w, "usage", err)
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

// resolveActivityWindow parses the shared half-open [since, until) filter. Each
// side applies only when supplied, so the zero value is unbounded; a
// non-positive window is rejected only when BOTH bounds are present (a single
// open side is fine), mirroring parseUsageWindow.
func resolveActivityWindow(v *httpx.Validation, since, until string) (time.Time, time.Time) {
	var sinceT, untilT time.Time
	if raw := strings.TrimSpace(since); raw != "" {
		t, err := parseUsageTime(raw)
		if err != nil {
			v.Invalid("since", "must be RFC3339 or YYYY-MM-DD")
		} else {
			sinceT = t
		}
	}
	if raw := strings.TrimSpace(until); raw != "" {
		t, err := parseUsageTime(raw)
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
func (h *usageHandler) resolveTeamActivityCaller(w http.ResponseWriter, r *http.Request) (orgID, userID, teamID string, ok bool) {
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
		notFound(w, "route")
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
func (h *usageHandler) requireTeamOrOrgAdmin(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	isTeamAdmin, err := h.az.UserIsTeamAdmin(r.Context(), userID, orgID, teamID)
	if err != nil {
		internalError(w, "usage", err)
		return false
	}
	if isTeamAdmin {
		return true
	}
	isOrgAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "usage", err)
		return false
	}
	if isOrgAdmin {
		return true
	}
	forbidden(w, "team admin or org admin role required")
	return false
}

// --- The actions feed (the external-action audit log) ---

// actionJSON is the action-log feed's wire shape — one external_actions row
// projected for the FE. The team_id/team_name/actor_name fields populate ONLY the
// org-wide feed (so an org-admin row shows which team's bot acted and who
// authorized it) and are omitted (omitempty) on the team-scoped feed.
type actionJSON struct {
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

// toActionJSON projects a stored action onto the wire shape, emitting detail_json
// as embedded JSON (omitted when absent/corrupt — the row's who/what/when/from→to
// stand on their own).
func toActionJSON(a domain.ExternalAction) actionJSON {
	var details json.RawMessage
	if a.DetailJSON != "" && json.Valid([]byte(a.DetailJSON)) {
		details = json.RawMessage(a.DetailJSON)
	}
	objectID, _ := domain.ObjectID(a)
	return actionJSON{
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

// actionActivityListRequest is the body of the action-log feeds. It shares
// provider and the time window with artifactActivityListRequest but filters on
// the action discriminator and the actor user id instead of the object kind —
// the two resources' filter vocabularies are genuinely different, which is why
// they are two routes rather than one with a lens selector.
type actionActivityListRequest struct {
	Provider string `json:"provider"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	// Since / Until bound occurred_at (not created_at) to [since, until).
	Since string `json:"since"`
	Until string `json:"until"`

	httpx.PageRequest
}

func resolveActionActivityFilters(v *httpx.Validation, req actionActivityListRequest) domain.ExternalActionListOpts {
	opts := domain.ExternalActionListOpts{
		Provider:    resolveActivityProvider(v, req.Provider),
		Action:      strings.TrimSpace(req.Action),
		ActorUserID: strings.TrimSpace(req.Actor),
	}
	if opts.Action != "" && !slices.Contains(domain.ExternalActionTypes(), opts.Action) {
		v.Invalid("action", "not a known action type")
	}
	if opts.ActorUserID != "" {
		if _, err := uuid.Parse(opts.ActorUserID); err != nil {
			v.Invalid("actor", "must be a user id")
		}
	}
	opts.Since, opts.Until = resolveActivityWindow(v, req.Since, req.Until)
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
func (h *usageHandler) handleUsageTeamActions(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, ok := h.resolveTeamActivityCaller(w, r)
	if !ok {
		return
	}

	var req actionActivityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	opts := resolveActionActivityFilters(&v, req)
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
		internalError(w, "usage", err)
		return
	}
	out := make([]actionJSON, len(actions))
	for i, a := range actions {
		j := toActionJSON(a)
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
// "team X's bot did Y, authorized by Z".
//
// POST /api/orgs/{org_id}/usage/actions/list
func (h *usageHandler) handleUsageOrgActions(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveGovernedOrgAdmin(w, r)
	if !ok {
		return
	}

	var req actionActivityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	opts := resolveActionActivityFilters(&v, req)
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
		internalError(w, "usage", err)
		return
	}
	out := make([]actionJSON, len(actions))
	for i, a := range actions {
		j := toActionJSON(a)
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

// resolveActionActorNames maps each distinct human actor to a display name via the
// app pool (Users.GetProfile, org-scoped RLS — a co-member's profile is readable,
// the same path the spend by-member breakdown uses). System actions (empty
// actor_user_id — event-triggered bot runs, the Jira mirror) carry no actor and
// are skipped. N+1 over the small distinct-id set is fine for v1.
func resolveActionActorNames(ctx context.Context, tx db.TxStores, actions []domain.ExternalAction) (map[string]string, error) {
	names := map[string]string{}
	for _, a := range actions {
		if a.ActorUserID == "" {
			continue
		}
		if _, done := names[a.ActorUserID]; done {
			continue
		}
		name, _, err := tx.Users.GetProfile(ctx, a.ActorUserID)
		if err != nil {
			return nil, err
		}
		names[a.ActorUserID] = name
	}
	return names, nil
}
