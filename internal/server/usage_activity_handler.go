package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The bot-activity audit feed (TFAC-483) is the EE governance lens over the
// artifacts table: a time-ordered, filterable history of EVERY external action
// the org's bot took with org credentials — branches, PRs, reviews, comments,
// issues — newest first, terminal rows included (merged PRs, deleted branches,
// dismissed reviews). It is a LOG, not a worklist (that's the board / PRs page),
// so it reads the full artifact list, not the reconciler's non-terminal set.
//
// Two scopes mirror the spend endpoints' gates, both behind FeatureGovernance:
//
//   - GET /api/usage/teams/{team_id}/activity — one team's history (team admin
//     OR org admin). ListByTeam, RLS-scoped under the caller's claims.
//   - GET /api/usage/org/activity — every team's history (org admin). The
//     System cross-RLS read (ListByOrgSystem); rows carry team_id + team_name.
//
// Unlicensed builds (local mode included) 404, agreeing with the FE
// useEntitlements gate that hides the surface entirely.

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
	if !requireGovernance(w, r) {
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
	if !requireGovernance(w, r) {
		return
	}
	if !h.az.RequireOrgAdminRole(w, r, orgID, userID) {
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

// requireGovernance gates a handler on the FeatureGovernance entitlement,
// writing a 404 (not 403) and returning false when it's unlicensed. The 404
// matches the FE useEntitlements gate, which hides the surface entirely in an
// unlicensed build — so the route reads as "not here", not "forbidden". The
// check is mode-agnostic (entitlements never carve out by runmode): a community
// / unlicensed build — local mode included — answers false and 404s, which is
// why the role checks that follow are only ever reached in a licensed multi
// deploy.
func requireGovernance(w http.ResponseWriter, r *http.Request) bool {
	if !entitlements.Active().Has(entitlements.FeatureGovernance) {
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
