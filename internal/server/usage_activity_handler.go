package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The /usage activity feed (TFAC-483) is the EE governance surface, behind
// FeatureGovernance, with TWO lenses on the org's external footprint — selected
// per request by ?view=:
//
//   - view=actions → the append-only external-action audit log of record: one
//     row per external WRITE TF performed under an org credential (the bot's
//     GitHub/Jira mutations, the human-authorized GitHub approval lifecycle, the
//     Jira board mirror), who/what/when/from→to. The source of truth for "what
//     did TF do." Includes actions with no artifact (the Jira mirror's
//     transitions).
//   - view=objects (default) → the artifacts head: each object the bot produced
//     and its CURRENT state, reconciler-maintained. Includes externally-driven
//     transitions (a human merging a PR — not a TF action, so it never appears in
//     the Actions log). The coverage difference between the two is intentional.
//
// Both lenses share the same two scopes + gates (mirroring the spend endpoints):
//
//   - GET /api/usage/teams/{team_id}/activity — one team's history (team admin
//     OR org admin), RLS/team-scoped under the caller's claims.
//   - GET /api/usage/org/activity — every team's history (org admin), the System
//     cross-team read; rows carry team_id + team_name (+ actor_name for actions).
//
// Unlicensed builds (local mode included) 404, agreeing with the FE
// useEntitlements gate that hides the surface entirely.

// activity feed lens selector. ?view=actions selects the external-action log;
// anything else (the default) selects the artifacts head.
const (
	viewObjects = "objects"
	viewActions = "actions"
)

func activityView(r *http.Request) string {
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("view")), viewActions) {
		return viewActions
	}
	return viewObjects
}

// activity feed page sizing. Limit defaults to activityPageDefault and is
// clamped to activityPageMax so a caller can't request an unbounded scan.
const (
	activityPageDefault = 50
	activityPageMax     = 200
)

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

// handleUsageTeamActivity returns one team's bot-activity history, newest first.
// Gate: FeatureGovernance AND (team admin OR org admin) — the org-governance
// lens may inspect any team's bot activity, while a team's own admin sees their
// team's. The read uses the RLS-scoped ListByTeam under the caller's claims
// (defense in depth alongside the role gate); filter/paging opts come from the
// query string. The team feed omits team_id/team_name (already one team).
//
// GET /api/usage/teams/{team_id}/activity?provider=&kind=&state=&since=&until=&limit=&offset=
func (h *usageHandler) handleUsageTeamActivity(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveCaller(w, r)
	if !ok {
		return
	}
	if !requireGovernance(w, r, orgID) {
		return
	}
	teamID := r.PathValue("team_id")
	if _, err := uuid.Parse(teamID); err != nil {
		// A malformed id is "not found" (parity with handleUsageTeam), not a role
		// failure — don't surface it as a 500 from a uuid cast downstream.
		http.NotFound(w, r)
		return
	}
	// Confirm the team is in the caller's org first so a cross-org id 404s cleanly
	// (non-disclosure) rather than falling through to the 403.
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !h.requireTeamOrOrgAdmin(w, r, orgID, userID, teamID) {
		return
	}

	// Actions lens (the append-only external-action log) vs Objects lens (the
	// artifacts head, below). Same gates; different source + row shape.
	if activityView(r) == viewActions {
		h.handleTeamActionsActivity(w, r, orgID, userID, teamID)
		return
	}

	opts, errMsg := parseArtifactListOpts(r.URL.Query())
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}

	var arts []domain.Artifact
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		arts, e = tx.Artifacts.ListByTeam(r.Context(), orgID, teamID, opts)
		return e
	}); err != nil {
		internalError(w, "usage", err)
		return
	}

	out := make([]activityArtifactJSON, len(arts))
	for i, a := range arts {
		out[i] = activityArtifactJSON{artifactJSON: toArtifactJSON(a)}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUsageOrgActivity returns the org-wide bot-activity history across every
// team, newest first. Gate: FeatureGovernance AND org admin — a cross-team read
// (the authorized governance intent). It uses the System aggregate
// (ListByOrgSystem, admin pool / BYPASSRLS) because crossing teams is the point,
// then resolves each row's team name (via Teams.GetSystem) so the feed shows
// which team's bot acted. Filter/paging opts come from the query string.
//
// GET /api/usage/org/activity?provider=&kind=&state=&since=&until=&limit=&offset=
func (h *usageHandler) handleUsageOrgActivity(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveCaller(w, r)
	if !ok {
		return
	}
	if !requireGovernance(w, r, orgID) {
		return
	}
	if !h.az.RequireOrgAdminRole(w, r, orgID, userID) {
		return
	}

	// Actions lens (the append-only external-action log) vs Objects lens (the
	// artifacts head, below). Same org-admin gate; different source + row shape.
	if activityView(r) == viewActions {
		h.handleOrgActionsActivity(w, r, orgID, userID)
		return
	}

	opts, errMsg := parseArtifactListOpts(r.URL.Query())
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}

	var (
		arts      []domain.Artifact
		teamNames map[string]string
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		arts, e = tx.Artifacts.ListByOrgSystem(r.Context(), orgID, opts)
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
	writeJSON(w, http.StatusOK, out)
}

// requireGovernance gates a handler on the FeatureGovernance entitlement for
// orgID, writing a 404 (not 403) and returning false when it's unlicensed.
// The 404 matches the FE useEntitlements gate, which hides the surface
// entirely in an unlicensed build — so the route reads as "not here", not
// "forbidden". The check is mode-agnostic (entitlements never carve out by
// runmode): a community / unlicensed build — local mode included — answers
// false and 404s, which is why the role checks that follow are only ever
// reached in a licensed multi deploy.
func requireGovernance(w http.ResponseWriter, r *http.Request, orgID string) bool {
	if !entitlements.For(orgID).Has(entitlements.FeatureGovernance) {
		http.NotFound(w, r)
		return false
	}
	return true
}

// requireTeamOrOrgAdmin authorizes the team-scoped bot-activity feed: the caller
// must be an admin of teamID OR an org admin. It writes a 403 and returns false
// otherwise. Local mode short-circuits to allowed for safety — though the
// governance gate (which 404s an unlicensed build, local included) runs first,
// so this is only reached under a license. VerifyTeamInOrg has already confirmed
// teamID is in the caller's org, so a non-admin here is a clean 403, never a
// cross-org probe. The raw probes don't short-circuit local, hence the guard.
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
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "team admin or org admin role required"})
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

// parseArtifactListOpts builds the store filter/paging opts from the activity
// feed's query string: ?provider=&kind=&state=&since=&until=&limit=&offset=.
// provider/kind/state pass through as exact-match filters (empty = no filter on
// that column). since/until reuse the usage time parser (RFC3339 or YYYY-MM-DD)
// and — unlike the spend window — default to UNBOUNDED: the feed is a full
// history, not a calendar month. limit defaults to activityPageDefault and is
// clamped to activityPageMax; offset is non-negative. Returns a non-empty errMsg
// on a malformed value (the handler 400s with it).
func parseArtifactListOpts(q url.Values) (db.ArtifactListOpts, string) {
	opts := db.ArtifactListOpts{
		Limit:    activityPageDefault,
		Provider: strings.TrimSpace(q.Get("provider")),
		Kind:     strings.TrimSpace(q.Get("kind")),
		State:    strings.TrimSpace(q.Get("state")),
	}
	if s := strings.TrimSpace(q.Get("since")); s != "" {
		t, err := parseUsageTime(s)
		if err != nil {
			return db.ArtifactListOpts{}, "invalid 'since': want RFC3339 or YYYY-MM-DD"
		}
		opts.Since = t
	}
	if s := strings.TrimSpace(q.Get("until")); s != "" {
		t, err := parseUsageTime(s)
		if err != nil {
			return db.ArtifactListOpts{}, "invalid 'until': want RFC3339 or YYYY-MM-DD"
		}
		opts.Until = t
	}
	// Half-open [since, until): reject a non-positive window only when both bounds
	// are supplied (a single open side is fine), mirroring parseUsageWindow.
	if !opts.Since.IsZero() && !opts.Until.IsZero() && !opts.Since.Before(opts.Until) {
		return db.ArtifactListOpts{}, "'since' must be before 'until'"
	}
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return db.ArtifactListOpts{}, "invalid 'limit': want a positive integer"
		}
		if n > activityPageMax {
			n = activityPageMax
		}
		opts.Limit = n
	}
	if s := strings.TrimSpace(q.Get("offset")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return db.ArtifactListOpts{}, "invalid 'offset': want a non-negative integer"
		}
		opts.Offset = n
	}
	return opts, ""
}

// --- Actions lens (the external-action audit log) ---

// actionJSON is the action-log feed's wire shape — one external_actions row
// projected for the FE. The team_id/team_name/actor_name fields populate ONLY the
// org-wide feed (so an org-admin row shows which team's bot acted and who
// authorized it) and are omitted (omitempty) on the team-scoped feed.
type actionJSON struct {
	ID          string          `json:"id"`
	Provider    string          `json:"provider"`
	Action      string          `json:"action"`
	Target      string          `json:"target"`
	ExternalID  string          `json:"external_id,omitempty"`
	URL         string          `json:"url,omitempty"`
	FromState   string          `json:"from_state,omitempty"`
	ToState     string          `json:"to_state,omitempty"`
	RunID       string          `json:"run_id,omitempty"`
	ActorUserID string          `json:"actor_user_id,omitempty"`
	Credential  string          `json:"credential"`
	Details     json.RawMessage `json:"details,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
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
		ID:          a.ID,
		Provider:    a.Provider,
		Action:      a.Action,
		Target:      a.Target,
		ExternalID:  a.ExternalID,
		URL:         a.URL,
		FromState:   a.FromState,
		ToState:     a.ToState,
		RunID:       a.RunID,
		ActorUserID: a.ActorUserID,
		Credential:  a.Credential,
		Details:     details,
		OccurredAt:  a.OccurredAt,
	}
}

// handleTeamActionsActivity returns one team's external-action history, newest
// first — the Actions lens of the team feed. Same gates as the Objects lens (run
// already by the caller). The team feed omits team_id/team_name (it's already one
// team) but DOES resolve the authorizing actor's display name — this is the audit
// surface a team admin reads, so raw actor UUIDs would be poor UX, and resolving
// over one team's small distinct-actor set is cheap.
func (h *usageHandler) handleTeamActionsActivity(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) {
	opts, errMsg := parseExternalActionListOpts(r.URL.Query())
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}
	var (
		actions    []domain.ExternalAction
		actorNames map[string]string
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actions, e = tx.ExternalActions.ListByTeam(r.Context(), orgID, teamID, opts)
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
	writeJSON(w, http.StatusOK, out)
}

// handleOrgActionsActivity returns the org-wide external-action history across
// every team, newest first — the Actions lens of the org feed. It uses the System
// aggregate (ListByOrgSystem, admin pool) because crossing teams is the point,
// then resolves each row's team name (Teams.GetSystem, admin pool) and human
// actor's display name (Users.GetProfile, org-scoped under the caller's claims) so
// the feed reads "team X's bot did Y, authorized by Z".
func (h *usageHandler) handleOrgActionsActivity(w http.ResponseWriter, r *http.Request, orgID, userID string) {
	opts, errMsg := parseExternalActionListOpts(r.URL.Query())
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}
	var (
		actions    []domain.ExternalAction
		teamNames  map[string]string
		actorNames map[string]string
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actions, e = tx.ExternalActions.ListByOrgSystem(r.Context(), orgID, opts)
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
	writeJSON(w, http.StatusOK, out)
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

// parseExternalActionListOpts builds the action-log filter/paging opts from the
// query string: ?provider=&action=&actor=&since=&until=&limit=&offset=. Mirrors
// parseArtifactListOpts (provider/time/limit/offset identical) but filters on the
// action discriminator + the actor user id instead of kind/state, and bounds
// occurred_at rather than created_at. limit defaults to activityPageDefault,
// clamped to activityPageMax; offset is non-negative.
func parseExternalActionListOpts(q url.Values) (domain.ExternalActionListOpts, string) {
	opts := domain.ExternalActionListOpts{
		Limit:       activityPageDefault,
		Provider:    strings.TrimSpace(q.Get("provider")),
		Action:      strings.TrimSpace(q.Get("action")),
		ActorUserID: strings.TrimSpace(q.Get("actor")),
	}
	if s := strings.TrimSpace(q.Get("since")); s != "" {
		t, err := parseUsageTime(s)
		if err != nil {
			return domain.ExternalActionListOpts{}, "invalid 'since': want RFC3339 or YYYY-MM-DD"
		}
		opts.Since = t
	}
	if s := strings.TrimSpace(q.Get("until")); s != "" {
		t, err := parseUsageTime(s)
		if err != nil {
			return domain.ExternalActionListOpts{}, "invalid 'until': want RFC3339 or YYYY-MM-DD"
		}
		opts.Until = t
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && !opts.Since.Before(opts.Until) {
		return domain.ExternalActionListOpts{}, "'since' must be before 'until'"
	}
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return domain.ExternalActionListOpts{}, "invalid 'limit': want a positive integer"
		}
		if n > activityPageMax {
			n = activityPageMax
		}
		opts.Limit = n
	}
	if s := strings.TrimSpace(q.Get("offset")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return domain.ExternalActionListOpts{}, "invalid 'offset': want a non-negative integer"
		}
		opts.Offset = n
	}
	return opts, ""
}
