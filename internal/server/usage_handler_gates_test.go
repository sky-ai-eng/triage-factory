package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
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
// run by member, an autonomous run fired by a named-blueprint trigger, and a
// team curator by member; teamB carries a manual run by orgAdmin; plus a system
// job. Skips without Docker via the pgtest harness.
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
	s := New(h.AdminDB, stores, 3000)

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
// minted in Go so the FK references (trigger→blueprint, run→trigger,
// curator→project) line up without a RETURNING round-trip.
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
	// A teamA project backs the curator turn.
	projID := uuid.New().String()
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO projects (id, org_id, creator_user_id, team_id, name, visibility) VALUES ($1, $2, $3, $4, 'usage-proj', 'team')`,
		projID, r.orgID, r.member, r.teamA)

	// teamA: manual run by member ($1.00), autonomous run via trigger ($0.25),
	// team curator by member ($0.50).
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO runs (id, org_id, team_id, creator_user_id, trigger_type, origin, model, status, total_cost_usd, started_at) VALUES ($1, $2, $3, $4, 'manual', 'manual', 'claude-opus-4-8', 'completed', 1.00, $5)`,
		uuid.New().String(), r.orgID, r.teamA, r.member, when)
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO runs (id, org_id, team_id, creator_user_id, trigger_type, origin, trigger_id, model, status, total_cost_usd, started_at) VALUES ($1, $2, $3, NULL, 'event', 'manual', $4, 'claude-haiku-4-5', 'completed', 0.25, $5)`,
		uuid.New().String(), r.orgID, r.teamA, triggerID, when)
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO curator_requests (id, org_id, creator_user_id, project_id, team_id, status, user_input, cost_usd, created_at) VALUES ($1, $2, $3, $4, $5, 'completed', 'hi', 0.50, $6)`,
		uuid.New().String(), r.orgID, r.member, projID, r.teamA, when)

	// teamB: manual run by orgAdmin ($2.00).
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO runs (id, org_id, team_id, creator_user_id, trigger_type, origin, model, status, total_cost_usd, started_at) VALUES ($1, $2, $3, $4, 'manual', 'manual', 'claude-opus-4-8', 'completed', 2.00, $5)`,
		uuid.New().String(), r.orgID, r.teamB, r.orgAdmin, when)

	// System job ($0.05, org-level).
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO system_llm_runs (id, org_id, job, model, total_cost_usd, started_at) VALUES ($1, $2, 'scorer', 'claude-haiku-4-5', 0.05, $3)`,
		uuid.New().String(), r.orgID, when)
}

// req builds a GET carrying the active org (session-scoped) + caller claims, and
// the team_id path value when teamID is set — the state withSession/withOrg
// would normally seed. The handler is invoked directly (not via the mux) so the
// test exercises authz + RLS without the cookie-session dance.
func (r *usageRig) req(caller, teamID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/usage?since=2000-01-01", nil)
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
		// member created the teamA manual run ($1.00) + the team curator ($0.50).
		if !floatEq(resp.TotalCostUSD, 1.50) {
			t.Errorf("/me total = %v, want 1.50 (member's own manual + curator)", resp.TotalCostUSD)
		}
	})

	t.Run("team_member_non_admin_403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeam(rec, r.req(r.member, r.teamA))
		if rec.Code != http.StatusForbidden {
			t.Errorf("/teams/{teamA} as plain member = %d, want 403; body=%s", rec.Code, rec.Body.String())
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
		if !floatEq(resp.TotalCostUSD, 1.75) {
			t.Errorf("/teams total = %v, want 1.75 (teamA manual + autonomous + curator)", resp.TotalCostUSD)
		}
	})

	t.Run("team_org_admin_not_member_200_crossRLS", func(t *testing.T) {
		// The org admin is on teamB, NOT teamA. An app-pool read would see no
		// teamA rows; the role gate + System read are the authorized cross-RLS path.
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeam(rec, r.req(r.orgAdmin, r.teamA))
		if rec.Code != http.StatusOK {
			t.Fatalf("/teams/{teamA} as org admin (not on teamA) = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp usageTeamResponse
		mustDecode(t, rec, &resp)
		if !floatEq(resp.TotalCostUSD, 1.75) {
			t.Errorf("/teams total (org admin) = %v, want 1.75 — System read should cross RLS", resp.TotalCostUSD)
		}
		// by_rule resolves the blueprint name via the admin-pool GetSystem chain,
		// which an org admin not on teamA could not reach through app-pool RLS.
		if len(resp.ByRule) != 1 || resp.ByRule[0].RuleName != r.blueprint {
			t.Errorf("/teams by_rule = %+v, want one rule %q (blueprint-name fallback via GetSystem)", resp.ByRule, r.blueprint)
		}
		// by_user surfaces the member who created the teamA spend.
		var sawMember bool
		for _, u := range resp.ByUser {
			if u.UserID == r.member {
				sawMember = true
			}
		}
		if !sawMember {
			t.Errorf("/teams by_user = %+v, want the member %s present", resp.ByUser, r.member)
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
		// teamA (1.75) + teamB (2.00) + system (0.05).
		if !floatEq(resp.TotalCostUSD, 3.80) {
			t.Errorf("/org total = %v, want 3.80", resp.TotalCostUSD)
		}
		byTeam := map[string]float64{}
		for _, b := range resp.ByTeam {
			byTeam[b.TeamID] = b.Cost
		}
		if !floatEq(byTeam[r.teamA], 1.75) || !floatEq(byTeam[r.teamB], 2.00) {
			t.Errorf("/org by_team = %+v, want teamA 1.75 + teamB 2.00", resp.ByTeam)
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
