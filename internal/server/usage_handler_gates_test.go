package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// usageRig is the multi-mode fixture for the Usage spend endpoints (TFAC-478):
// a real Postgres-backed Server (RLS live) with four actors —
//
//   - owner     : org owner + teamA admin (founder).
//   - orgAdmin  : org admin, member of teamB only (NOT teamA) — exercises the
//     org-admin-reads-a-team-they-don't-belong-to path (the System cross-RLS read).
//   - teamAdmin : teamA admin, plain org member.
//   - member    : teamA member, plain org member (no admin rights anywhere).
//
// Spend is seeded so the 200 cases have a real breakdown: teamA carries a manual
// run by member and an autonomous run fired by a named-blueprint trigger;
// teamB carries a manual run by orgAdmin; plus a system job. Skips without
// Docker via the pgtest harness.
type usageRig struct {
	h         *pgtest.Harness
	uh        *usageHandler
	orgID     string
	owner     string
	orgAdmin  string
	teamAdmin string
	member    string
	teamA     string
	teamB     string
	blueprint string // the trigger's blueprint name, surfaced in by_rule.
}

func newUsageRig(t *testing.T) *usageRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, owner, teamA := pgtest.SeedOrgWithUser(t, h, "usage-founder")
	teamB := pgtest.SeedTeam(t, h, orgID, "teamB")
	orgAdmin := pgtest.SeedUser(t, h, "usage-orgadmin")
	pgtest.AddOrgMember(t, h, orgAdmin, orgID, teamB, "admin", "member") // org admin, teamB only
	teamAdmin := pgtest.SeedUser(t, h, "usage-teamadmin")
	pgtest.AddOrgMember(t, h, teamAdmin, orgID, teamA, "member", "admin") // teamA admin, org member
	member := pgtest.SeedUser(t, h, "usage-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamA, "member", "member") // teamA member, org member

	r := &usageRig{
		h: h, uh: &usageHandler{tx: s.tx, az: s.az},
		orgID: orgID, owner: owner, orgAdmin: orgAdmin, teamAdmin: teamAdmin, member: member,
		teamA: teamA, teamB: teamB, blueprint: "CI Fix",
	}
	r.seedSpend(t)
	return r
}

// seedSpend stages the cross-team spend the 200-path assertions read. IDs are
// minted in Go so the FK references (trigger→blueprint, run→trigger) line up
// without a RETURNING round-trip.
func (r *usageRig) seedSpend(t *testing.T) {
	t.Helper()
	when := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	// Blueprint + trigger (teamA) so the autonomous run has a firing trigger and
	// by_rule resolves a human name (the trigger itself carries a NULL name).
	bpID := uuid.New().String()
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name) VALUES ($1, $2, $3, $4, $5)`,
		bpID, r.orgID, r.teamA, r.owner, r.blueprint)
	triggerID := uuid.New().String()
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability) VALUES ($1, $2, $3, $4, 'trigger', 'github:pr:ci_check_failed', $5, 3, 0.5)`,
		triggerID, r.orgID, r.teamA, r.owner, bpID)

	// teamA: manual run by member ($1.00), autonomous run via trigger ($0.25).
	// Spend is the messages ledger, so every conversation gets one
	// cost-stamped assistant row.
	seedSpendConv := func(convSQL string, args []any, model string, cost float64) {
		convID := uuid.New().String()
		pgtest.MustExec(t, r.h.AdminDB, convSQL, append([]any{convID}, args...)...)
		pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO messages (org_id, conversation_id, role, subtype, content, model, cost_usd, created_at) VALUES ($1, $2, 'assistant', '', 'work', NULLIF($3, ''), $4, $5)`,
			r.orgID, convID, model, cost, when)
	}
	seedSpendConv(`INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, model, status, started_at) VALUES ($1, $2, $3, $4, 'manual', 'manual', 'claude-opus-4-8', 'completed', $5)`,
		[]any{r.orgID, r.teamA, r.member, when}, "claude-opus-4-8", 1.00)
	seedSpendConv(`INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, trigger_id, model, status, started_at) VALUES ($1, $2, $3, NULL, 'event', 'manual', $4, 'claude-haiku-4-5', 'completed', $5)`,
		[]any{r.orgID, r.teamA, triggerID, when}, "claude-haiku-4-5", 0.25)

	// teamB: manual run by orgAdmin ($2.00).
	seedSpendConv(`INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, model, status, started_at) VALUES ($1, $2, $3, $4, 'manual', 'manual', 'claude-opus-4-8', 'completed', $5)`,
		[]any{r.orgID, r.teamB, r.orgAdmin, when}, "claude-opus-4-8", 2.00)

	// System job ($0.05, org-level).
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO system_llm_runs (id, org_id, job, model, total_cost_usd, started_at) VALUES ($1, $2, 'scorer', 'claude-haiku-4-5', 0.05, $3)`,
		uuid.New().String(), r.orgID, when)
}

// req builds a GET carrying the caller claims plus both scope segments — the
// org_id every org-scoped read authorizes against, and team_id when teamID is
// set — alongside the session org the viewer-relative and team reads take. That
// is the state withSession/withOrg + the mux would normally seed. The handler is
// invoked directly (not via the mux) so the test exercises authz + RLS without
// the cookie-session dance.
func (r *usageRig) req(caller, teamID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/usage?since=2000-01-01", nil)
	req.SetPathValue("org_id", r.orgID)
	if teamID != "" {
		req.SetPathValue("team_id", teamID)
	}
	ctx := httpx.WithOrgID(req.Context(), r.orgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
	return req.WithContext(ctx)
}

// TestUsageHandler_GatesAndScope_Postgres pins the role gates (multi-mode only,
// since they short-circuit in local) and the cross-RLS System reads the team /
// org endpoints depend on.
func TestUsageHandler_GatesAndScope_Postgres(t *testing.T) {
	r := newUsageRig(t)

	t.Run("me_returns_own_spend_only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.uh.handleUsageMe(rec, r.req(r.member, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("/me = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp usageMeResponse
		mustDecode(t, rec, &resp)
		// member created the teamA manual run ($1.00).
		if !floatEq(resp.TotalCostUSD, 1.00) {
			t.Errorf("/me total = %v, want 1.00 (member's own manual run)", resp.TotalCostUSD)
		}
	})

	t.Run("team_member_200_without_by_user", func(t *testing.T) {
		// Team spend is a member-visible team fact; the per-person cut is not.
		// A member gets the whole payload with by_user ABSENT — not empty,
		// which would read as "nobody spent anything".
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeam(rec, r.req(r.member, r.teamA))
		if rec.Code != http.StatusOK {
			t.Fatalf("/teams/{teamA} as plain member = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp usageTeamResponse
		mustDecode(t, rec, &resp)
		if resp.ByUser != nil {
			t.Errorf("/teams by_user = %+v as a plain member, want absent", *resp.ByUser)
		}
		if strings.Contains(rec.Body.String(), `"by_user"`) {
			t.Errorf("/teams body carries a by_user key for a plain member: %s", rec.Body.String())
		}
		// Every other cut is the admin's: the total, the models the donut needs,
		// the days, and the team's own rules.
		if !floatEq(resp.TotalCostUSD, 1.25) {
			t.Errorf("/teams total = %v, want 1.25 (teamA manual + autonomous)", resp.TotalCostUSD)
		}
		if len(resp.ByModel) != 2 {
			t.Errorf("/teams by_model = %+v, want both teamA models", resp.ByModel)
		}
		if len(resp.ByRule) != 1 || resp.ByRule[0].RuleName != r.blueprint {
			t.Errorf("/teams by_rule = %+v, want one rule %q (a rule is team config, not a person)", resp.ByRule, r.blueprint)
		}
	})

	t.Run("team_admin_200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeam(rec, r.req(r.teamAdmin, r.teamA))
		if rec.Code != http.StatusOK {
			t.Fatalf("/teams/{teamA} as team admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp usageTeamResponse
		mustDecode(t, rec, &resp)
		if !floatEq(resp.TotalCostUSD, 1.25) {
			t.Errorf("/teams total = %v, want 1.25 (teamA manual + autonomous)", resp.TotalCostUSD)
		}
		// by_rule resolves the blueprint name under the team admin's OWN claims —
		// a member can read their team's event_handlers/blueprints (no System read).
		if len(resp.ByRule) != 1 || resp.ByRule[0].RuleName != r.blueprint {
			t.Errorf("/teams by_rule = %+v, want one rule %q (resolved under member claims)", resp.ByRule, r.blueprint)
		}
		// by_user is present for an admin, and surfaces the member who created
		// the teamA spend.
		if resp.ByUser == nil {
			t.Fatalf("/teams by_user absent for a team admin, want the per-person cut")
		}
		var sawMember bool
		for _, u := range *resp.ByUser {
			if u.UserID == r.member {
				sawMember = true
			}
		}
		if !sawMember {
			t.Errorf("/teams by_user = %+v, want the member %s present", *resp.ByUser, r.member)
		}
	})

	t.Run("team_org_admin_not_member_200_without_by_user", func(t *testing.T) {
		// The org admin is a member of teamB, so teamA's spend is now readable to
		// them as an org member like any other. by_user is NOT: the predicate is
		// tf.user_is_team_admin, which org-admin does not satisfy — the org
		// rollup at /api/orgs/{org_id}/usage is where they get people.
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeam(rec, r.req(r.orgAdmin, r.teamA))
		if rec.Code != http.StatusOK {
			t.Fatalf("/teams/{teamA} as org admin (not on teamA) = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp usageTeamResponse
		mustDecode(t, rec, &resp)
		if resp.ByUser != nil {
			t.Errorf("/teams by_user = %+v for an org admin off the team, want absent", *resp.ByUser)
		}
	})

	t.Run("team_cross_org_404", func(t *testing.T) {
		// Non-disclosure is untouched by the widened gate: a team the caller's
		// org does not contain is not found, never a role refusal.
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeam(rec, r.req(r.member, uuid.New().String()))
		if rec.Code != http.StatusNotFound {
			t.Errorf("/teams/{stranger} = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_member_403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrg(rec, r.req(r.member, ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("/org as plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_team_admin_403", func(t *testing.T) {
		// A teamA admin is still only an org *member* — the org rollup is org-admin-only.
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrg(rec, r.req(r.teamAdmin, ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("/org as team admin (org member) = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_admin_200_rollup", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrg(rec, r.req(r.orgAdmin, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("/org as org admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp usageOrgResponse
		mustDecode(t, rec, &resp)
		// teamA (1.25) + teamB (2.00) + system (0.05).
		if !floatEq(resp.TotalCostUSD, 3.30) {
			t.Errorf("/org total = %v, want 3.30", resp.TotalCostUSD)
		}
		byTeam := map[string]float64{}
		for _, b := range resp.ByTeam {
			byTeam[b.TeamID] = b.Cost
		}
		if !floatEq(byTeam[r.teamA], 1.25) || !floatEq(byTeam[r.teamB], 2.00) {
			t.Errorf("/org by_team = %+v, want teamA 1.25 + teamB 2.00", resp.ByTeam)
		}
		// by_user (org-wide, manual by creator): member 1.00, orgAdmin 2.00.
		byUser := map[string]float64{}
		for _, u := range resp.ByUser {
			byUser[u.UserID] = u.Cost
		}
		if !floatEq(byUser[r.member], 1.00) || !floatEq(byUser[r.orgAdmin], 2.00) {
			t.Errorf("/org by_user = %+v, want member 1.00 + orgAdmin 2.00", resp.ByUser)
		}
		// org_level: the NULL-team system row.
		var sysCost float64
		for _, b := range resp.OrgLevel {
			if b.Category == "system_overhead" {
				sysCost = b.Cost
			}
		}
		if !floatEq(sysCost, 0.05) {
			t.Errorf("/org org_level system_overhead = %v, want 0.05; got %+v", sysCost, resp.OrgLevel)
		}
	})
}

func mustDecode(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}

// TestUsageAccessLog_GatesAndEntitlement_Postgres pins the EE viewer's two-axis
// gate under real RLS: the org-admin role gate (403 for a plain member or a mere
// team admin) AND the FeatureGovernance entitlement (404-and-hide when
// unlicensed, even for the org admin). The 200 path proves the org-admin read
// resolves actor/target names across the org.
func TestUsageAccessLog_GatesAndEntitlement_Postgres(t *testing.T) {
	r := newUsageRig(t)
	entitlements.Reset() // start unlicensed regardless of test order
	t.Cleanup(entitlements.Reset)

	// One audit row: the founder promoted the member to org admin. Written on the
	// admin pool (BYPASSRLS) so the fixture doesn't depend on a request context.
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO access_change_log (org_id, actor_user_id, action, target_user_id, detail_json)
		VALUES ($1, $2, $3, $4, $5)`,
		r.orgID, r.owner, domain.AccessActionOrgRoleChanged, r.member, `{"old_role":"member","new_role":"admin"}`)

	t.Run("unlicensed_404_even_for_org_admin", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.uh.handleUsageAccessLog(rec, r.req(r.orgAdmin, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("unlicensed org admin = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	// License governance for the remaining cases.
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))

	t.Run("org_member_403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.uh.handleUsageAccessLog(rec, r.activityReq(r.member, "", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin_org_member_403", func(t *testing.T) {
		// A team admin is still only an org member — the audit log is org-admin-only.
		rec := httptest.NewRecorder()
		r.uh.handleUsageAccessLog(rec, r.activityReq(r.teamAdmin, "", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("team admin (org member) = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_admin_200_reads_log_with_names", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.uh.handleUsageAccessLog(rec, r.activityReq(r.orgAdmin, "", ""))
		resp := decodeList[accessChangeJSON](t, rec)
		if len(resp.Items) != 1 || resp.Total() != 1 {
			t.Fatalf("items = %d / total %d, want 1/1: %+v", len(resp.Items), resp.Total(), resp.Items)
		}
		row := resp.Items[0]
		if row.Action != domain.AccessActionOrgRoleChanged {
			t.Errorf("action = %q, want %q", row.Action, domain.AccessActionOrgRoleChanged)
		}
		if !strings.Contains(row.ActionLabel, "from member to admin") {
			t.Errorf("action_label = %q, want it to render the member→admin transition", row.ActionLabel)
		}
		// Actor (founder) + target (member) resolve to non-empty display names
		// under the org admin's claims (users_select RLS is org-scoped).
		if row.ActorName == "" || row.TargetName == "" {
			t.Errorf("name resolution failed: actor=%q target=%q", row.ActorName, row.TargetName)
		}
	})
}
