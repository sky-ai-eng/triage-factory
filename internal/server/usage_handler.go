package server

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// usageHandler serves the core Usage page's spend layer: three role-gated read
// endpoints over the llm_spend view. Spend is core at every scope; the SCOPE is
// what's role-gated, and each route is addressed by the scope it is about — a
// team's spend is a fact about the team, like its roster:
//
//   - GET /api/me/usage           — the caller's own spend (any org member).
//   - GET /api/teams/{id}/usage   — one team's breakdown (team member; the
//     by_user cut alone is team admin).
//   - GET /api/orgs/{id}/usage    — the org rollup (org admin).
//
// /me is viewer-relative, so its org comes from the session claims and its
// subject from the principal; it runs on the APP pool under those claims (RLS
// + a creator filter scope it to them). /teams names the team in the path and
// resolves the org from the session, like every other {team_id} route; its
// names resolve under the caller's own claims, while its spend read uses
// ListSpendSystem — the authorized cross-RLS read — since the route's
// membership gate is what authorizes it, not the caller's own claims.
// /orgs names the org in
// the path and authorizes the caller against THAT org, so an admin of three
// orgs can read all three without first moving a session cursor: it is the
// org-admin governance rollup, a cross-team ListSpendSystem read with no
// per-rule detail. Aggregation happens in Go from the rows; per-org/-team/-month
// volumes are modest, so we materialize nothing here.
type usageHandler struct {
	tx db.TxRunner
	az *authz.Checker
	// conversationQueue backs the org-scoped operations subset (TFAC-589): an org
	// admin's own queue waits + run durations. Admin-pool reads with orgID
	// bound by argument, gated by the HTTP org-admin check — SaaS-safe, no
	// cross-tenant machine truth.
	conversationQueue db.ConversationQueueStore
}

// --- response shapes ---

// usageCategoryBucket is one spend category's cost + token totals over the
// window. cost is real dollars (SDK token counts × list price; see TFAC-449).
type usageCategoryBucket struct {
	Category            string  `json:"category"`
	Cost                float64 `json:"cost"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
}

type usageModelBucket struct {
	Model string  `json:"model"`
	Cost  float64 `json:"cost"`
}

// usageDayBucket is one UTC calendar day's total cost (date as YYYY-MM-DD).
type usageDayBucket struct {
	Date string  `json:"date"`
	Cost float64 `json:"cost"`
}

// usageDayModelBucket is one (UTC day, model) cell — the long-format series the
// FE pivots into a stacked-by-model area over time. Rows naming no model
// (NULL) or a runtime-composed one (domain.ModelSynthetic) are excluded,
// same as by_model, so the stack covers model-attributed spend; the per-day
// total (by_day) still includes their share.
type usageDayModelBucket struct {
	Date  string  `json:"date"`
	Model string  `json:"model"`
	Cost  float64 `json:"cost"`
}

type usageUserBucket struct {
	UserID      string  `json:"user_id"`
	DisplayName string  `json:"display_name"`
	AvatarURL   string  `json:"avatar_url,omitempty"`
	Cost        float64 `json:"cost"`
}

type usageRuleBucket struct {
	TriggerID string  `json:"trigger_id"`
	RuleName  string  `json:"rule_name"`
	Cost      float64 `json:"cost"`
}

type usageTeamBucket struct {
	TeamID   string  `json:"team_id"`
	TeamName string  `json:"team_name"`
	Cost     float64 `json:"cost"`
}

type usageOrgLevelBucket struct {
	Category string  `json:"category"`
	Cost     float64 `json:"cost"`
}

type usageMeResponse struct {
	TotalCostUSD float64               `json:"total_cost_usd"`
	ByCategory   []usageCategoryBucket `json:"by_category"`
	ByModel      []usageModelBucket    `json:"by_model"`
	ByDay        []usageDayBucket      `json:"by_day"`
	ByDayModel   []usageDayModelBucket `json:"by_day_model"`
}

// usageTeamResponse is one team's breakdown. Every cut here is a fact about
// the team — money, days, models, categories, the team's own rules — except
// by_user, which names people.
//
// ByUser is a POINTER because absent and empty are different answers, and a
// client must not read one as the other: absent means the caller is not
// authorized for the per-person cut (they are a member, not an admin), while
// `[]` means nobody on the team has attributed spend in the window. A reader
// renders the first as "unknown" and only the second as zero.
type usageTeamResponse struct {
	TeamID       string                `json:"team_id"`
	TeamName     string                `json:"team_name"`
	TotalCostUSD float64               `json:"total_cost_usd"`
	ByCategory   []usageCategoryBucket `json:"by_category"`
	ByUser       *[]usageUserBucket    `json:"by_user,omitempty"`
	ByRule       []usageRuleBucket     `json:"by_rule"`
	ByModel      []usageModelBucket    `json:"by_model"`
	ByDay        []usageDayBucket      `json:"by_day"`
	ByDayModel   []usageDayModelBucket `json:"by_day_model"`
}

// usageOrgResponse is the org rollup. Partition invariant: total_cost_usd ==
// sum(by_team[*].cost) + sum(org_level[*].cost) — by_team covers the team-
// attributed rows, org_level the NULL-team rows (system_overhead). by_user
// and by_category slice the SAME total on different
// axes (creator / category), so neither sums to total on its own; a consumer
// reproducing the total must add by_team + org_level, not by_team alone.
type usageOrgResponse struct {
	TotalCostUSD float64               `json:"total_cost_usd"`
	ByTeam       []usageTeamBucket     `json:"by_team"`
	ByUser       []usageUserBucket     `json:"by_user"`
	OrgLevel     []usageOrgLevelBucket `json:"org_level"`
	ByCategory   []usageCategoryBucket `json:"by_category"`
	ByModel      []usageModelBucket    `json:"by_model"`
	ByDay        []usageDayBucket      `json:"by_day"`
	ByDayModel   []usageDayModelBucket `json:"by_day_model"`
	// ByRule is populated ONLY in local mode (N=1): there's a single team, so the
	// cross-team boundary that keeps per-rule detail off the org rollup in multi
	// mode doesn't apply, and the local console can read everything in one request.
	// omitempty → absent entirely in multi mode.
	ByRule []usageRuleBucket `json:"by_rule,omitempty"`
}

// --- handlers ---

// handleUsageMe returns the caller's own spend: manual runs they created
// (autonomous/system rows carry a NULL creator and are excluded by
// the filter). Gate: any authenticated org member. Read on the app pool under
// the caller's claims, narrowed by CreatorUserID = self.
//
// GET /api/me/usage?since=&until=
func (h *usageHandler) handleUsageMe(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveCaller(w, r)
	if !ok {
		return
	}
	since, until, errMsg := parseUsageWindow(r.URL.Query(), time.Now().UTC())
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}

	self := userID
	var rows []domain.SpendRow
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		rows, e = tx.Spend.ListSpend(r.Context(), orgID, domain.SpendFilter{
			CreatorUserID: &self, Since: since, Until: until,
		})
		return e
	}); err != nil {
		internalError(w, "usage", err)
		return
	}

	writeJSON(w, http.StatusOK, usageMeResponse{
		TotalCostUSD: sumSpendCost(rows),
		ByCategory:   spendByCategory(rows),
		ByModel:      spendByModel(rows),
		ByDay:        spendByDay(rows),
		ByDayModel:   spendByDayModel(rows),
	})
}

// handleUsageTeam returns one team's breakdown. Gate: MEMBERSHIP — the same
// resolution the team's activity node uses, because team spend is a fact about
// the team in exactly the way its events and failures are, and the two nodes
// share one audience line. Per-rule (per-blueprint) detail is operational, so
// it stays with the team; org admins get the cross-team rollup from
// /api/orgs/{org_id}/usage instead, never another team's per-rule view.
//
// by_user is the one cut that names PEOPLE, and it stays team-admin-only: the
// admin check runs as a predicate rather than a refusal, so a member gets the
// whole payload minus that cut. Absent is not empty — see usageTeamResponse.
//
// Names resolve under the caller's own claims: by_rule reads the team's
// event_handlers / blueprints through the app pool (team-scoped RLS permits a
// member; a rule an org member outside the team may not read comes back
// unnamed rather than failing the request), and by_user reads org-scoped
// display names. The spend READ uses ListSpendSystem — the authorized
// cross-RLS read, since the route's gate is what authorizes it, not the
// caller's own claims. by_user groups manual rows by creator; by_rule groups
// autonomous rows by the firing trigger; system rows (NULL team) never appear
// (the TeamID filter excludes them).
//
// GET /api/teams/{team_id}/usage?since=&until=
func (h *usageHandler) handleUsageTeam(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveCaller(w, r)
	if !ok {
		return
	}
	// The one {team_id} grammar: a uuid, plus the literal "default" in local
	// mode. A segment that resolves to nothing is "not found" (parity with the
	// team handlers), not a role failure or a 500 from a uuid cast.
	teamID, ok := h.az.TeamIDFromPath(w, r, "usage", orgID, userID)
	if !ok {
		return
	}
	// Confirm the team is in the caller's org: a cross-org id 404s cleanly
	// (non-disclosure), and that answer is the whole route gate — this is the
	// activity node's resolution, and membership is what authorizes the read.
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	// The team-admin check, as a predicate rather than a gate: it decides
	// whether by_user rides along, never whether the request is answered. A
	// failed PROBE is still a 500 — a cut dropped because a query errored would
	// be indistinguishable from a cut withheld by role. Local short-circuits to
	// admin (N=1).
	isTeamAdmin, err := h.az.UserIsTeamAdmin(r.Context(), userID, orgID, teamID)
	if err != nil {
		internalError(w, "usage", err)
		return
	}
	since, until, errMsg := parseUsageWindow(r.URL.Query(), time.Now().UTC())
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}

	var (
		rows         []domain.SpendRow
		teamName     string
		userProfiles map[string]userProfile
		ruleNames    map[string]string
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		rows, e = tx.Spend.ListSpendSystem(r.Context(), orgID, domain.SpendFilter{
			TeamID: &teamID, Since: since, Until: until,
		})
		if e != nil {
			return e
		}
		// Team name via the by-id System getter. Unlike by_rule/by_user below
		// (team- and creator-scoped data, resolved under the member's own claims),
		// team rows are org-visible (teams_select is org-scoped) so this is no
		// privilege bump — and TeamsStore exposes no app-pool Get-by-id, so
		// GetSystem is the only by-id team getter.
		t, e := tx.Teams.GetSystem(r.Context(), orgID, teamID)
		if e != nil {
			return e
		}
		if t != nil {
			teamName = t.Name
		}
		// Display names are resolved only for the cut that renders them, so a
		// member's request never reads the roster it isn't shown.
		if isTeamAdmin {
			if userProfiles, e = resolveSpendUserProfiles(r.Context(), tx, rows); e != nil {
				return e
			}
		}
		ruleNames, e = resolveSpendRuleNames(r.Context(), tx, orgID, rows)
		return e
	}); err != nil {
		internalError(w, "usage", err)
		return
	}

	resp := usageTeamResponse{
		TeamID:       teamID,
		TeamName:     teamName,
		TotalCostUSD: sumSpendCost(rows),
		ByCategory:   spendByCategory(rows),
		ByRule:       spendByRule(rows, ruleNames),
		ByModel:      spendByModel(rows),
		ByDay:        spendByDay(rows),
		ByDayModel:   spendByDayModel(rows),
	}
	if isTeamAdmin {
		byUser := spendByUser(rows, userProfiles)
		resp.ByUser = &byUser
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsageOrg returns the org rollup across every team + system job. Gate:
// org admin (a cross-team read — the authorized intent, not a workaround).
// This is the governance lens: by_team groups the team-attributed rows (and
// drives the FE team dropdown), by_user groups the human creators org-wide,
// by_category splits automated / delegated / system, and org_level is the
// NULL-team rows (system_overhead) by category. Deliberately NO by_rule —
// per-rule detail stays with
// the owning team (/api/teams/{team_id}/usage), so the org view never reaches
// into another team's event_handlers.
//
// GET /api/orgs/{org_id}/usage?since=&until=
func (h *usageHandler) handleUsageOrg(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	since, until, errMsg := parseUsageWindow(r.URL.Query(), time.Now().UTC())
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}

	// Local mode (N=1) carries by_rule too — see usageOrgResponse.ByRule.
	local := runmode.Current() == runmode.ModeLocal
	var (
		rows         []domain.SpendRow
		teamNames    map[string]string
		userProfiles map[string]userProfile
		ruleNames    map[string]string // local mode only
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		rows, e = tx.Spend.ListSpendSystem(r.Context(), orgID, domain.SpendFilter{
			Since: since, Until: until,
		})
		if e != nil {
			return e
		}
		if teamNames, e = resolveSpendTeamNames(r.Context(), tx, orgID, rows); e != nil {
			return e
		}
		// Org-wide creators are co-org-members, so profiles resolve under the
		// org admin's claims (users_select is org-scoped) — no System read needed.
		if userProfiles, e = resolveSpendUserProfiles(r.Context(), tx, rows); e != nil {
			return e
		}
		if local {
			ruleNames, e = resolveSpendRuleNames(r.Context(), tx, orgID, rows)
		}
		return e
	}); err != nil {
		internalError(w, "usage", err)
		return
	}

	resp := usageOrgResponse{
		TotalCostUSD: sumSpendCost(rows),
		ByTeam:       spendByTeam(rows, teamNames),
		ByUser:       spendByUser(rows, userProfiles),
		OrgLevel:     spendOrgLevel(rows),
		ByCategory:   spendByCategory(rows),
		ByModel:      spendByModel(rows),
		ByDay:        spendByDay(rows),
		ByDayModel:   spendByDayModel(rows),
	}
	if local {
		resp.ByRule = spendByRule(rows, ruleNames)
	}
	writeJSON(w, http.StatusOK, resp)
}

// teamCapUpdate is the PUT /api/teams/{team_id}/usage/cap body. A pointer so an
// explicit null and an omitted field both decode to nil → 0 → "clear the cap".
type teamCapUpdate struct {
	MaxDailyCostUSD *float64 `json:"max_daily_cost_usd"`
}

// handleUsageTeamCap sets one team's per-team daily LLM spend cap (TFAC-482).
// Gate: org admin AND the governance entitlement, in that order — a non-admin
// gets the same 403 licensed or not, so the status can't be read as a licence
// tier, and an admin on an unlicensed deployment gets the 404 that hides the
// feature (per-team caps are dormant without governance, with the org cap as
// the safety net). A team admin can NOT set their own team's cap — this is org-admin-only by
// design, so the write goes through the admin pool (SetDailyCostCapSystem): the
// org admin need not be a member of the team they're capping. The org comes from
// the session (like the GET reads), the team from the path.
//
// Body: {"max_daily_cost_usd": number|null}. null or 0 clears the cap; a
// negative value 400s. Echoes the stored value (null when cleared) so the FE can
// update in place.
//
// PUT /api/teams/{team_id}/usage/cap
func (h *usageHandler) handleUsageTeamCap(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveCaller(w, r)
	if !ok {
		return
	}
	// Role, then the EE gate — the licence must not be readable off a status
	// code by someone who isn't an org admin (see resolveGovernedOrgAdmin).
	// The entitlement check is mode-agnostic: local is unlicensed, so the route
	// 404s an admin there too (per-team caps are a multi-tenant EE concept).
	if !h.az.RequireOrgAdminRole(w, r, orgID, userID) {
		return
	}
	if !requireGovernance(w, r, orgID) {
		return
	}
	// The one {team_id} grammar; a segment that resolves to nothing is "not
	// found" (parity with handleUsageTeam), not a 500.
	teamID, ok := h.az.TeamIDFromPath(w, r, "usage", orgID, userID)
	if !ok {
		return
	}
	// Confirm the team is in the caller's org so a cross-org id 404s (non-
	// disclosure) rather than writing another org's team_settings row.
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	// Block writes to an archived team (403), consistent with the rest of the
	// team-settings write family — a cap on a force-stopped team can never be
	// enforced (no runs), so it'd be a silently-inert write.
	if !h.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}

	var req teamCapUpdate
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	capUSD := 0.0
	if req.MaxDailyCostUSD != nil {
		capUSD = *req.MaxDailyCostUSD
	}
	if capUSD < 0 {
		badRequest(w, "max_daily_cost_usd must be >= 0 (or null to clear the cap)")
		return
	}

	// The write returns the settings row it persisted, so the echo reflects what
	// actually landed in team_settings rather than this caller's request body.
	// That is what keeps two org admins racing on the same team's cap
	// convergent — each echoes the state its own statement produced, not its own
	// request — instead of diverging until the next poll. capUSD <= 0 stored
	// NULL, which comes back as 0 (no cap) → a null echo.
	var stored *float64
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		set, e := tx.Teams.SetDailyCostCapSystem(r.Context(), teamID, capUSD)
		if e != nil {
			return e
		}
		if set.MaxDailyCostUSD > 0 {
			v := set.MaxDailyCostUSD
			stored = &v
		}
		return nil
	}); err != nil {
		internalError(w, "usage", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"max_daily_cost_usd": stored})
}

// usageTeamCapEntry is one team in the team-caps list: its id, name, and
// configured per-team daily cap (null = no cap).
type usageTeamCapEntry struct {
	TeamID   string   `json:"team_id"`
	TeamName string   `json:"team_name"`
	Cap      *float64 `json:"cap"`
}

// handleUsageTeamCaps lists EVERY active team in the org with its per-team
// daily cap, for the governance cap editor. Gate: org admin (403) then
// governance (404 unlicensed) — same posture as the PUT. Unlike the spend rollup's
// by_team (only teams with spend in the window), this lists all active teams
// so an org admin can pre-cap an idle team before any runaway happens. Admin
// pool throughout: an org admin may not be a member of every team. Archived
// teams are excluded — they can't be capped (the PUT 403s on archived). The FE
// pairs each entry with its window spend looked up from
// /api/orgs/{org_id}/usage by_team (0 if idle).
//
// POST /api/orgs/{org_id}/usage/team-caps/list
func (h *usageHandler) handleUsageTeamCaps(w http.ResponseWriter, r *http.Request) {
	// Org admin (403) then the EE gate (404 unlicensed) — mirrors the PUT.
	orgID, userID, ok := h.resolveGovernedOrgAdmin(w, r)
	if !ok {
		return
	}

	var req httpx.PageRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	// The list has no filters — it is every active team in the caller's org —
	// so every request fingerprints the same.
	page := httpx.ResolvePage(&v, req, httpx.FilterFingerprint("org-team-caps"), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		caps  []domain.TeamCap
		total int
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		caps, total, e = tx.Teams.ListActiveCapsForOrgSystem(r.Context(), orgID, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "usage", err)
		return
	}

	entries := make([]usageTeamCapEntry, len(caps))
	for i, c := range caps {
		entries[i] = usageTeamCapEntry{TeamID: c.TeamID, TeamName: c.TeamName, Cap: c.Cap}
	}
	httpx.WriteList(w, page, entries, total)
}

// --- gating ---

// resolveCaller pulls the active org from the session and the caller's user id
// from the claims — the shared prefix for the viewer-relative read and for the
// team-scoped ones, which name their team in the path but still resolve it
// within the caller's org. The active org is one the user belongs to (set at
// login / active-org switch), so requireOrg is the org-member floor for /me.
//
// The org-scoped reads do NOT use it: they name their org in the path and
// authorize against that value — see resolveGovernedOrgAdmin and the direct
// az.RequireOrgAdmin calls.
func (h *usageHandler) resolveCaller(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", false
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return "", "", false
	}
	return orgID, claims.Subject, true
}

// resolveGovernedOrgAdmin is the shared prefix of the org-scoped usage reads a
// governance licence gates: the org-admin role against the {org_id} in the
// path, THEN the entitlement.
//
// Role first, and the order is the whole point: a caller who is not an org
// admin gets the same 403 whether or not the deployment is licensed, so the
// status code they get back cannot be used to read off the org's plan tier.
// Entitlement-first would answer them 404 on an unlicensed deployment and 403
// on a licensed one — a licence oracle for anyone who can hit the route. What
// it gives up is that a non-admin learns the route exists at all, which in a
// public codebase is not a secret worth buying with the other one.
//
// An org admin on an unlicensed deployment still gets the 404 that hides the
// feature, which is what the frontend's own entitlement gate renders.
func (h *usageHandler) resolveGovernedOrgAdmin(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	orgID, userID, ok = h.az.RequireOrgAdmin(w, r)
	if !ok {
		return "", "", false
	}
	if !requireGovernance(w, r, orgID) {
		return "", "", false
	}
	return orgID, userID, true
}

// --- window parsing ---

// parseUsageWindow resolves the [since, until) read window from the query
// string. Both absent → the current calendar month (UTC) up to now. Each
// present value parses as RFC3339 or YYYY-MM-DD (a bare date → UTC midnight).
// Returns a non-empty errMsg on a malformed value (the handler 400s with it).
func parseUsageWindow(q url.Values, now time.Time) (since, until time.Time, errMsg string) {
	sinceStr := strings.TrimSpace(q.Get("since"))
	untilStr := strings.TrimSpace(q.Get("until"))
	if sinceStr == "" && untilStr == "" {
		since = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		until = now
		return since, until, ""
	}
	if sinceStr != "" {
		t, err := parseUsageTime(sinceStr)
		if err != nil {
			return time.Time{}, time.Time{}, "invalid 'since': want RFC3339 or YYYY-MM-DD"
		}
		since = t
	}
	if untilStr != "" {
		t, err := parseUsageTime(untilStr)
		if err != nil {
			return time.Time{}, time.Time{}, "invalid 'until': want RFC3339 or YYYY-MM-DD"
		}
		until = t
	}
	// When both bounds are supplied, reject a non-positive window (since >=
	// until). The read is half-open [since, until), so since == until is empty
	// and since > until is inverted — both would otherwise return a misleading
	// 200 with no rows and mask a client bug. Single-bound requests leave the
	// other side open (zero), so there's no ordering to check.
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return time.Time{}, time.Time{}, "'since' must be before 'until'"
	}
	return since, until, ""
}

// parseUsageTime accepts a full RFC3339 timestamp or a bare YYYY-MM-DD date
// (interpreted as UTC midnight). Always returns UTC so the day-bucketing and
// the view's occurred_at compare in one zone.
func parseUsageTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// --- aggregation (pure) ---

func sumSpendCost(rows []domain.SpendRow) float64 {
	var total float64
	for _, r := range rows {
		total += r.TotalCostUSD
	}
	return total
}

// spendByCategory sums cost + the four token totals per category, sorted by
// category name for a stable response.
func spendByCategory(rows []domain.SpendRow) []usageCategoryBucket {
	byCat := map[string]*usageCategoryBucket{}
	for _, r := range rows {
		b := byCat[r.Category]
		if b == nil {
			b = &usageCategoryBucket{Category: r.Category}
			byCat[r.Category] = b
		}
		b.Cost += r.TotalCostUSD
		b.InputTokens += r.InputTokens
		b.OutputTokens += r.OutputTokens
		b.CacheReadTokens += r.CacheReadTokens
		b.CacheCreationTokens += r.CacheCreationTokens
	}
	out := make([]usageCategoryBucket, 0, len(byCat))
	for _, b := range byCat {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Category < out[j].Category })
	return out
}

// spendByModel sums cost per model, highest-cost-first (ties broken by model
// name). Rows with a NULL model are excluded — by_model is a
// per-model breakdown, not a total, so their share lives only in
// by_category. Runtime-composed rows (domain.ModelSynthetic) are excluded on
// the same grounds: they name no model. Settlement no longer targets those
// rows, so this covers rows stamped before that fix plus the last-resort
// corner where a conversation has nothing else to settle on.
func spendByModel(rows []domain.SpendRow) []usageModelBucket {
	byModel := map[string]float64{}
	for _, r := range rows {
		if r.Model == nil || *r.Model == domain.ModelSynthetic {
			continue
		}
		byModel[*r.Model] += r.TotalCostUSD
	}
	out := make([]usageModelBucket, 0, len(byModel))
	for m, c := range byModel {
		out = append(out, usageModelBucket{Model: m, Cost: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// spendByDay sums cost per UTC calendar day, oldest-first (the time-series the
// dashboard plots).
func spendByDay(rows []domain.SpendRow) []usageDayBucket {
	byDay := map[string]float64{}
	for _, r := range rows {
		byDay[r.OccurredAt.UTC().Format("2006-01-02")] += r.TotalCostUSD
	}
	out := make([]usageDayBucket, 0, len(byDay))
	for d, c := range byDay {
		out = append(out, usageDayBucket{Date: d, Cost: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// spendByDayModel sums cost per (UTC calendar day, model) — the long-format
// series the FE pivots into a stacked-by-model area. Rows with a NULL model
// or a runtime-composed one (domain.ModelSynthetic) are excluded,
// matching by_model; their share still lands in by_day's per-day total. Sorted
// by date then model for a stable response.
func spendByDayModel(rows []domain.SpendRow) []usageDayModelBucket {
	type key struct{ date, model string }
	byCell := map[key]float64{}
	for _, r := range rows {
		if r.Model == nil || *r.Model == domain.ModelSynthetic {
			continue
		}
		byCell[key{r.OccurredAt.UTC().Format("2006-01-02"), *r.Model}] += r.TotalCostUSD
	}
	out := make([]usageDayModelBucket, 0, len(byCell))
	for k, c := range byCell {
		out = append(out, usageDayModelBucket{Date: k.date, Model: k.model, Cost: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// userProfile is a resolved (display name, avatar URL) pair for one creator —
// the value type of the profiles map spendByUser consumes.
type userProfile struct {
	name   string
	avatar string
}

// spendByUser sums cost per human creator (manual runs),
// resolving display name + avatar from the supplied map. Rows with a NULL creator
// (autonomous/system) are excluded. Sorted cost-desc.
func spendByUser(rows []domain.SpendRow, profiles map[string]userProfile) []usageUserBucket {
	byUser := map[string]float64{}
	for _, r := range rows {
		if r.CreatorUserID == nil {
			continue
		}
		byUser[*r.CreatorUserID] += r.TotalCostUSD
	}
	out := make([]usageUserBucket, 0, len(byUser))
	for uid, c := range byUser {
		p := profiles[uid]
		out = append(out, usageUserBucket{UserID: uid, DisplayName: p.name, AvatarURL: p.avatar, Cost: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].UserID < out[j].UserID
	})
	return out
}

// spendByRule sums cost per firing trigger across the autonomous rows,
// resolving rule names from the supplied map. Sorted cost-desc.
func spendByRule(rows []domain.SpendRow, names map[string]string) []usageRuleBucket {
	byRule := map[string]float64{}
	for _, r := range rows {
		if r.Category != domain.SpendCategoryAutonomous || r.TriggerID == nil {
			continue
		}
		byRule[*r.TriggerID] += r.TotalCostUSD
	}
	out := make([]usageRuleBucket, 0, len(byRule))
	for tid, c := range byRule {
		out = append(out, usageRuleBucket{TriggerID: tid, RuleName: names[tid], Cost: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].TriggerID < out[j].TriggerID
	})
	return out
}

// spendByTeam sums cost per team across the team-attributed rows (NULL-team
// rows go to org_level instead), resolving team names from the supplied map.
// Sorted cost-desc. Per-team caps are NOT carried here — the governance cap
// editor reads the full team list (incl. idle teams absent from this spend
// rollup) from GET /api/orgs/{org_id}/usage/team-caps instead (TFAC-482).
func spendByTeam(rows []domain.SpendRow, names map[string]string) []usageTeamBucket {
	byTeam := map[string]float64{}
	for _, r := range rows {
		if r.TeamID == nil {
			continue
		}
		byTeam[*r.TeamID] += r.TotalCostUSD
	}
	out := make([]usageTeamBucket, 0, len(byTeam))
	for tid, c := range byTeam {
		out = append(out, usageTeamBucket{TeamID: tid, TeamName: names[tid], Cost: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].TeamID < out[j].TeamID
	})
	return out
}

// spendOrgLevel sums the NULL-team rows (system_overhead) per category —
// the org-level spend that isn't attributable to any one team. Sorted by
// category.
func spendOrgLevel(rows []domain.SpendRow) []usageOrgLevelBucket {
	byCat := map[string]float64{}
	for _, r := range rows {
		if r.TeamID != nil {
			continue
		}
		byCat[r.Category] += r.TotalCostUSD
	}
	out := make([]usageOrgLevelBucket, 0, len(byCat))
	for cat, c := range byCat {
		out = append(out, usageOrgLevelBucket{Category: cat, Cost: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Category < out[j].Category })
	return out
}

// --- name resolution (N+1 over the small distinct-id sets; fine for v1) ---

// resolveSpendUserProfiles maps each distinct human creator in rows to a (display
// name, avatar URL) pair. Runs on the app pool under the caller's claims —
// users_select RLS is org-scoped, so a caller (always an org member here)
// resolves any co-member's profile, which covers every run creator in
// their org. avatar_url is whatever the OAuth login captured (often empty in
// local mode); the FE roster falls back to a monogram when it's blank.
func resolveSpendUserProfiles(ctx context.Context, tx db.TxStores, rows []domain.SpendRow) (map[string]userProfile, error) {
	profiles := map[string]userProfile{}
	for _, r := range rows {
		if r.CreatorUserID == nil {
			continue
		}
		if _, done := profiles[*r.CreatorUserID]; done {
			continue
		}
		name, avatar, err := tx.Users.GetProfile(ctx, *r.CreatorUserID)
		if err != nil {
			return nil, err
		}
		profiles[*r.CreatorUserID] = userProfile{name: name, avatar: avatar}
	}
	return profiles, nil
}

// resolveSpendRuleNames maps each distinct firing trigger (autonomous rows) to a
// human-readable rule name. It uses the plain app-pool Get (RLS-scoped), NOT
// GetSystem, and runs for two callers — both safe under that scoping:
//   - /api/teams/{team_id}/usage (any mode): the caller is a team member (team-admin gate),
//     and event_handlers / blueprints RLS lets a member read their own team's rows.
//   - /api/orgs/{org_id}/usage in LOCAL mode (N=1): there's one team and no RLS, so reading
//     its rule names is unrestricted — and the multi-tenant boundary that keeps
//     per-rule detail off the org rollup is moot at N=1.
//
// The MULTI-mode org rollup never calls this (it omits by_rule), so no cross-team
// per-rule read ever happens. A trigger event_handler always carries a NULL name
// (the trigger_shape CHECK forces it), so the meaningful label is the blueprint it
// fires; we fall back to that, then to "" (the FE shows the id).
func resolveSpendRuleNames(ctx context.Context, tx db.TxStores, orgID string, rows []domain.SpendRow) (map[string]string, error) {
	names := map[string]string{}
	for _, r := range rows {
		if r.Category != domain.SpendCategoryAutonomous || r.TriggerID == nil {
			continue
		}
		tid := *r.TriggerID
		if _, done := names[tid]; done {
			continue
		}
		eh, err := tx.EventHandlers.Get(ctx, orgID, tid)
		if err != nil {
			return nil, err
		}
		name := ""
		if eh != nil {
			name = eh.Name // non-empty only for rules; triggers carry NULL.
			if name == "" && eh.BlueprintID != "" {
				bp, err := tx.Blueprints.Get(ctx, orgID, eh.BlueprintID)
				if err != nil {
					return nil, err
				}
				if bp != nil {
					name = bp.Name
				}
			}
		}
		names[tid] = name
	}
	return names, nil
}

// resolveSpendTeamNames maps each distinct team in rows to its name via the
// ADMIN pool (GetSystem) — the org rollup crosses teams the caller may not
// belong to.
func resolveSpendTeamNames(ctx context.Context, tx db.TxStores, orgID string, rows []domain.SpendRow) (map[string]string, error) {
	names := map[string]string{}
	for _, r := range rows {
		if r.TeamID == nil {
			continue
		}
		if _, done := names[*r.TeamID]; done {
			continue
		}
		t, err := tx.Teams.GetSystem(ctx, orgID, *r.TeamID)
		if err != nil {
			return nil, err
		}
		name := ""
		if t != nil {
			name = t.Name
		}
		names[*r.TeamID] = name
	}
	return names, nil
}
