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

// The /usage activity feeds (TFAC-483) are the EE governance surface, behind
// FeatureGovernance. There are TWO resources, and they are two routes:
//
//   - .../artifacts/list → the artifacts head: each object the bot produced and
//     its CURRENT state, reconciler-maintained. Includes externally-driven
//     transitions (a human merging a PR — not a TF action, so it never appears
//     in the actions log).
//   - .../actions/list → the append-only external-action audit log of record:
//     one row per external WRITE TF performed under an org credential (the
//     bot's GitHub/Jira mutations, the human-authorized GitHub approval
//     lifecycle, the Jira board mirror), who/what/when/from→to. The source of
//     truth for "what did TF do." Includes actions with no artifact (the Jira
//     mirror's transitions).
//
// The coverage difference between the two is intentional, and it is exactly
// why they are separate routes now. They used to be one route multiplexed by
// ?view=, which meant one address answering with two different row shapes and
// two different filter vocabularies — a caller's `state=` filter silently did
// nothing on the actions lens, and a typo'd view answered with the other
// resource entirely.
//
// Each resource has the same two scopes + gates (mirroring the spend
// endpoints):
//
//   - POST /api/teams/{team_id}/usage/{artifacts,actions}/list — one team's
//     history (team admin OR org admin), RLS/team-scoped under the caller's
//     claims.
//   - POST /api/orgs/{org_id}/usage/{artifacts,actions}/list — every team's history (org
//     admin), the System cross-team read; rows carry team_id + team_name (+
//     actor_name for actions).
//
// Unlicensed builds (local mode included) 404, agreeing with the FE
// useEntitlements gate that hides the surface entirely.

// activityArtifactJSON is the bot-activity feed's wire shape: the shared run
// artifact projection (artifactJSON) plus the owning team's id + name. The team
// fields populate ONLY the org-wide feed — so an org-admin row shows which
// team's bot acted — and are omitted (omitempty) on the team-scoped feed, which
// is already one team, so that response is exactly []artifactJSON.
type activityArtifactJSON struct {
	artifactJSON
	TeamID   string `json:"team_id,omitempty"`
	TeamName string `json:"team_name,omitempty"`
}

// artifactActivityListRequest is the body of the artifacts-head feeds. The
// filter names are the query params they replaced, unchanged in meaning.
type artifactActivityListRequest struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	// Since / Until bound created_at to the half-open window [since, until).
	// Both are RFC3339 or YYYY-MM-DD, and both default to UNBOUNDED: the feed
	// is a full history, not a calendar month.
	Since string `json:"since"`
	Until string `json:"until"`

	httpx.PageRequest
}

// resolveArtifactActivityFilters validates the body's filters into the store's
// opts. Every vocabulary is closed: an unrecognized value used to reach the
// store as a literal, match nothing, and answer 200 with an empty page — a typo
// that reads exactly like "your bot did nothing".
func resolveArtifactActivityFilters(v *httpx.Validation, req artifactActivityListRequest) db.ArtifactListOpts {
	opts := db.ArtifactListOpts{
		Provider: strings.TrimSpace(req.Provider),
		Kind:     strings.TrimSpace(req.Kind),
		State:    strings.TrimSpace(req.State),
	}
	for _, f := range []struct {
		name, value string
		vocab       []string
	}{
		{"provider", opts.Provider, domain.ArtifactProviders()},
		{"kind", opts.Kind, domain.ArtifactKinds()},
		{"state", opts.State, domain.ArtifactStates()},
	} {
		if f.value != "" && !slices.Contains(f.vocab, f.value) {
			v.Invalid(f.name, "must be one of: "+strings.Join(f.vocab, ", "))
		}
	}
	opts.Since, opts.Until = resolveActivityWindow(v, req.Since, req.Until)
	return opts
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

// handleUsageTeamArtifacts returns one team's artifacts head, newest first.
// Gate: FeatureGovernance AND (team admin OR org admin) — the org-governance
// lens may inspect any team's bot activity, while a team's own admin sees their
// team's. The read uses the RLS-scoped ListByTeam under the caller's claims
// (defense in depth alongside the role gate). The team feed omits
// team_id/team_name (already one team).
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
	opts.Limit, opts.Offset = page.Limit, page.Offset

	var (
		arts  []domain.Artifact
		total int
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		arts, total, e = tx.Artifacts.ListByTeam(r.Context(), orgID, teamID, opts)
		return e
	}); err != nil {
		internalError(w, "usage", err)
		return
	}

	out := make([]activityArtifactJSON, len(arts))
	for i, a := range arts {
		out[i] = activityArtifactJSON{artifactJSON: toArtifactJSON(a)}
	}
	httpx.WriteList(w, page, out, total)
}

// handleUsageOrgArtifacts returns the org-wide artifacts head across every
// team, newest first. Gate: FeatureGovernance AND org admin — a cross-team read
// (the authorized governance intent). It uses the System aggregate
// (ListByOrgSystem, admin pool / BYPASSRLS) because crossing teams is the point,
// then resolves each row's team name (via Teams.GetSystem) so the feed shows
// which team's bot acted.
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
	opts.Limit, opts.Offset = page.Limit, page.Offset

	var (
		arts      []domain.Artifact
		total     int
		teamNames map[string]string
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		arts, total, e = tx.Artifacts.ListByOrgSystem(r.Context(), orgID, opts)
		if e != nil {
			return e
		}
		teamNames, e = resolveArtifactTeamNames(r.Context(), tx, orgID, arts)
		return e
	}); err != nil {
		internalError(w, "usage", err)
		return
	}

	out := make([]activityArtifactJSON, len(arts))
	for i, a := range arts {
		out[i] = activityArtifactJSON{
			artifactJSON: toArtifactJSON(a),
			TeamID:       a.TeamID,
			TeamName:     teamNames[a.TeamID],
		}
	}
	httpx.WriteList(w, page, out, total)
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

// resolveArtifactTeamNames maps each distinct team in the artifact set to its
// name via the ADMIN pool (Teams.GetSystem) — the org feed crosses teams the
// caller may not belong to. Mirrors resolveSpendTeamNames; artifact TeamID is
// NOT NULL, so every row has a team. N+1 over the small distinct-id set is fine
// for v1 (a page is ~50 rows, far fewer distinct teams).
func resolveArtifactTeamNames(ctx context.Context, tx db.TxStores, orgID string, arts []domain.Artifact) (map[string]string, error) {
	names := map[string]string{}
	for _, a := range arts {
		if a.TeamID == "" {
			continue
		}
		if _, done := names[a.TeamID]; done {
			continue
		}
		t, err := tx.Teams.GetSystem(ctx, orgID, a.TeamID)
		if err != nil {
			return nil, err
		}
		name := ""
		if t != nil {
			name = t.Name
		}
		names[a.TeamID] = name
	}
	return names, nil
}

// --- The actions feed (the external-action audit log) ---

// actionJSON is the action-log feed's wire shape — one external_actions row
// projected for the FE. The team_id/team_name/actor_name fields populate ONLY the
// org-wide feed (so an org-admin row shows which team's bot acted and who
// authorized it) and are omitted (omitempty) on the team-scoped feed.
type actionJSON struct {
	ID             string          `json:"id"`
	Provider       string          `json:"provider"`
	Action         string          `json:"action"`
	Target         string          `json:"target"`
	ExternalID     string          `json:"external_id,omitempty"`
	URL            string          `json:"url,omitempty"`
	FromState      string          `json:"from_state,omitempty"`
	ToState        string          `json:"to_state,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	ActorUserID    string          `json:"actor_user_id,omitempty"`
	Credential     string          `json:"credential"`
	Details        json.RawMessage `json:"details,omitempty"`
	OccurredAt     time.Time       `json:"occurred_at"`
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
	return actionJSON{
		ID:             a.ID,
		Provider:       a.Provider,
		Action:         a.Action,
		Target:         a.Target,
		ExternalID:     a.ExternalID,
		URL:            a.URL,
		FromState:      a.FromState,
		ToState:        a.ToState,
		ConversationID: a.ConversationID,
		ActorUserID:    a.ActorUserID,
		Credential:     a.Credential,
		Details:        details,
		OccurredAt:     a.OccurredAt,
	}
}

// actionActivityListRequest is the body of the action-log feeds. It mirrors
// artifactActivityListRequest (provider and the time window are identical) but
// filters on the action discriminator and the actor user id instead of
// kind/state — the two resources' filter vocabularies are genuinely different,
// which is why they are two routes rather than one with a lens selector.
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
		Provider:    strings.TrimSpace(req.Provider),
		Action:      strings.TrimSpace(req.Action),
		ActorUserID: strings.TrimSpace(req.Actor),
	}
	// Same closed-vocabulary rule as the artifacts feed.
	if opts.Provider != "" && !slices.Contains(domain.ArtifactProviders(), opts.Provider) {
		v.Invalid("provider", "must be one of: "+strings.Join(domain.ArtifactProviders(), ", "))
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
	opts.Limit, opts.Offset = page.Limit, page.Offset

	var (
		actions    []domain.ExternalAction
		total      int
		actorNames map[string]string
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	opts.Limit, opts.Offset = page.Limit, page.Offset

	var (
		actions    []domain.ExternalAction
		total      int
		teamNames  map[string]string
		actorNames map[string]string
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

// resolveActionTeamNames maps each distinct team in the action set to its name
// via the ADMIN pool (Teams.GetSystem) — the org feed crosses teams the caller
// may not belong to. Mirrors resolveArtifactTeamNames. Actions with no team (a
// dashboard board-drag, a team-less mirror) are skipped — they carry no chip.
func resolveActionTeamNames(ctx context.Context, tx db.TxStores, orgID string, actions []domain.ExternalAction) (map[string]string, error) {
	names := map[string]string{}
	for _, a := range actions {
		if a.TeamID == "" {
			continue
		}
		if _, done := names[a.TeamID]; done {
			continue
		}
		t, err := tx.Teams.GetSystem(ctx, orgID, a.TeamID)
		if err != nil {
			return nil, err
		}
		name := ""
		if t != nil {
			name = t.Name
		}
		names[a.TeamID] = name
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
