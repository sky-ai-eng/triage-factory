package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// postJSONWithSid drives an authed mutating request: sid cookie +
// same-origin Origin header (so withCSRFOriginCheck passes) + JSON body.
func (r *authRig) postJSONWithSid(method, path, sid string, body any) *http.Response {
	r.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		r.t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	}
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// TestOrgCreate_HappyPath provisions a net-new org from the founder's
// session and asserts the full tenant-row set landed on the admin pool,
// the shipped defaults were materialized post-commit, and the session's
// active org now points at the new org.
func TestOrgCreate_HappyPath(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	runmode.SetOrgCreationEnabledForTest(t, true)

	r := newAuthRig(t)
	founder := r.seedUser()
	resp, _ := r.driveCallback(founder)
	sid := r.sidFromResp(resp)

	got := r.postJSONWithSid("POST", "/api/orgs", sid, map[string]string{"name": "Acme Industries"})
	if got.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d, want 201", got.StatusCode)
	}
	var out orgCreateResponse
	if err := json.NewDecoder(got.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Slug != "acme-industries" {
		t.Errorf("slug=%q, want %q", out.Slug, "acme-industries")
	}
	orgID := uuid.MustParse(out.ID)
	teamID := uuid.MustParse(out.TeamID)

	// Org row: name + owner.
	var (
		gotName  string
		gotOwner uuid.UUID
	)
	if err := r.h.AdminDB.QueryRow(
		`SELECT name, owner_user_id FROM public.orgs WHERE id = $1`, orgID,
	).Scan(&gotName, &gotOwner); err != nil {
		t.Fatalf("select org: %v", err)
	}
	if gotName != "Acme Industries" {
		t.Errorf("org name=%q, want %q", gotName, "Acme Industries")
	}
	if gotOwner != founder {
		t.Errorf("org owner=%s, want %s", gotOwner, founder)
	}

	// Default team.
	var (
		teamSlug string
		teamName string
		teamOrg  uuid.UUID
	)
	if err := r.h.AdminDB.QueryRow(
		`SELECT slug, name, org_id FROM public.teams WHERE id = $1`, teamID,
	).Scan(&teamSlug, &teamName, &teamOrg); err != nil {
		t.Fatalf("select team: %v", err)
	}
	if teamSlug != "default" || teamName != "Default" || teamOrg != orgID {
		t.Errorf("team = (%q,%q,%s); want (default, Default, %s)", teamSlug, teamName, teamOrg, orgID)
	}

	// Founder memberships: org owner + team admin.
	var orgRole string
	if err := r.h.AdminDB.QueryRow(
		`SELECT role FROM public.org_memberships WHERE user_id = $1 AND org_id = $2`, founder, orgID,
	).Scan(&orgRole); err != nil {
		t.Fatalf("select org_membership: %v", err)
	}
	if orgRole != "owner" {
		t.Errorf("org role=%q, want owner", orgRole)
	}
	var teamRole string
	if err := r.h.AdminDB.QueryRow(
		`SELECT role FROM public.memberships WHERE user_id = $1 AND team_id = $2`, founder, teamID,
	).Scan(&teamRole); err != nil {
		t.Fatalf("select membership: %v", err)
	}
	if teamRole != "admin" {
		t.Errorf("team role=%q, want admin", teamRole)
	}

	// Default settings rows.
	assertRowExists(t, r, `SELECT 1 FROM public.org_settings WHERE org_id = $1`, orgID, "org_settings")
	assertRowExists(t, r, `SELECT 1 FROM public.team_settings WHERE team_id = $1`, teamID, "team_settings")

	// BootstrapNewOrg ran post-commit: the org's agent + the team's bot
	// row exist (the load-bearing materialization for auto-delegation).
	agent, err := r.srv.allStores.Agents.GetForOrgSystem(t.Context(), orgID.String())
	if err != nil {
		t.Fatalf("GetForOrgSystem: %v", err)
	}
	if agent == nil {
		t.Fatal("no agents row after create — BootstrapNewOrg did not run")
	}
	ta, err := r.srv.allStores.TeamAgents.GetForTeamSystem(t.Context(), orgID.String(), teamID.String(), agent.ID)
	if err != nil {
		t.Fatalf("GetForTeamSystem: %v", err)
	}
	if ta == nil {
		t.Fatal("default team has no team_agents bot row after create")
	}

	// Session active org now points at the new org.
	var active uuid.NullUUID
	if err := r.h.AdminDB.QueryRow(
		`SELECT active_org_id FROM public.sessions WHERE id = $1`, uuid.MustParse(sid),
	).Scan(&active); err != nil {
		t.Fatalf("select active_org_id: %v", err)
	}
	if !active.Valid || active.UUID != orgID {
		t.Errorf("session active_org_id=%+v, want %s", active, orgID)
	}
}

// TestOrgCreate_InvalidName_400 pins the input-validation gates: a name
// with no slug-able characters (all symbols) and an over-long name both
// 400 before any provisioning, and neither persists an org.
func TestOrgCreate_InvalidName_400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	runmode.SetOrgCreationEnabledForTest(t, true)

	r := newAuthRig(t)
	founder := r.seedUser()
	resp, _ := r.driveCallback(founder)
	sid := r.sidFromResp(resp)

	cases := map[string]string{
		"no alphanumerics": "---",
		"over 200 chars":   strings.Repeat("a", 201),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			got := r.postJSONWithSid("POST", "/api/orgs", sid, map[string]string{"name": value})
			if got.StatusCode != http.StatusBadRequest {
				t.Errorf("status=%d, want 400", got.StatusCode)
			}
		})
	}

	var cnt int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM public.orgs WHERE owner_user_id = $1`, founder,
	).Scan(&cnt); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if cnt != 0 {
		t.Errorf("orgs created=%d on invalid input, want 0", cnt)
	}
}

// TestOrgCreate_LocalModeIsNotFound pins the multi-only gate: in local
// mode (N=1, provisions via POST /api/setup/start) POST /api/orgs is
// 404. Uses the SQLite test server so it runs without Docker.
func TestOrgCreate_LocalModeIsNotFound(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs", map[string]string{"name": "Acme"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (multi-only); body=%s", rec.Code, rec.Body.String())
	}
}

// TestOrgCreate_Unauthenticated_401 pins the auth gate: no sid cookie →
// withSession 401s before the handler runs.
func TestOrgCreate_Unauthenticated_401(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	runmode.SetOrgCreationEnabledForTest(t, true)

	r := newAuthRig(t)
	got := r.postJSONWithSid("POST", "/api/orgs", "", map[string]string{"name": "Acme"})
	if got.StatusCode != http.StatusUnauthorized {
		t.Errorf("no sid status=%d, want 401", got.StatusCode)
	}
}

// TestOrgCreate_CreationDisabled_403 pins the defense-in-depth behind the
// UI gate: with TF_PREVENT_ORG_CREATION on, an authed POST still 403s.
func TestOrgCreate_CreationDisabled_403(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	runmode.SetOrgCreationEnabledForTest(t, false)

	r := newAuthRig(t)
	founder := r.seedUser()
	resp, _ := r.driveCallback(founder)
	sid := r.sidFromResp(resp)

	got := r.postJSONWithSid("POST", "/api/orgs", sid, map[string]string{"name": "Acme"})
	if got.StatusCode != http.StatusForbidden {
		t.Errorf("creation-disabled status=%d, want 403", got.StatusCode)
	}

	// And nothing was created.
	var cnt int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM public.orgs WHERE owner_user_id = $1`, founder,
	).Scan(&cnt); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if cnt != 0 {
		t.Errorf("orgs created=%d despite 403, want 0", cnt)
	}
}

// TestOrgCreate_DuplicateName_DistinctSlug pins race-safe slug
// allocation: two orgs created with the same name resolve to distinct
// slugs (the second gets a -2 suffix).
func TestOrgCreate_DuplicateName_DistinctSlug(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	runmode.SetOrgCreationEnabledForTest(t, true)

	r := newAuthRig(t)
	founder := r.seedUser()
	resp, _ := r.driveCallback(founder)
	sid := r.sidFromResp(resp)

	first := r.postJSONWithSid("POST", "/api/orgs", sid, map[string]string{"name": "Acme Industries"})
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create status=%d, want 201", first.StatusCode)
	}
	var firstOut orgCreateResponse
	_ = json.NewDecoder(first.Body).Decode(&firstOut)

	second := r.postJSONWithSid("POST", "/api/orgs", sid, map[string]string{"name": "Acme Industries"})
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second create status=%d, want 201", second.StatusCode)
	}
	var secondOut orgCreateResponse
	_ = json.NewDecoder(second.Body).Decode(&secondOut)

	if firstOut.Slug != "acme-industries" {
		t.Errorf("first slug=%q, want acme-industries", firstOut.Slug)
	}
	if secondOut.Slug == firstOut.Slug {
		t.Errorf("second slug=%q collided with first", secondOut.Slug)
	}
	if secondOut.Slug != "acme-industries-2" {
		t.Errorf("second slug=%q, want acme-industries-2", secondOut.Slug)
	}
}

// TestOrgCreate_RLS_NonMemberCannotAccess proves the new org is RLS-
// isolated: the founder reads + writes it through the app pool (tf_app),
// a non-member sees nothing and writes nothing.
func TestOrgCreate_RLS_NonMemberCannotAccess(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	runmode.SetOrgCreationEnabledForTest(t, true)

	r := newAuthRig(t)
	founder := r.seedUser()
	resp, _ := r.driveCallback(founder)
	sid := r.sidFromResp(resp)

	got := r.postJSONWithSid("POST", "/api/orgs", sid, map[string]string{"name": "Private Co"})
	if got.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d, want 201", got.StatusCode)
	}
	var out orgCreateResponse
	_ = json.NewDecoder(got.Body).Decode(&out)
	orgID := out.ID

	nonMember := r.seedUser()

	// Founder reads the org through the app pool under RLS.
	if err := r.h.WithUser(t, founder.String(), orgID, func(tx *sql.Tx) error {
		var cnt int
		if err := tx.QueryRow(`SELECT count(*) FROM public.orgs WHERE id = $1`, orgID).Scan(&cnt); err != nil {
			return err
		}
		if cnt != 1 {
			t.Errorf("founder sees %d org rows, want 1", cnt)
		}
		return nil
	}); err != nil {
		t.Fatalf("founder read tx: %v", err)
	}

	// Non-member sees nothing (orgs_select: not owner, not a member).
	if err := r.h.WithUser(t, nonMember.String(), orgID, func(tx *sql.Tx) error {
		var cnt int
		if err := tx.QueryRow(`SELECT count(*) FROM public.orgs WHERE id = $1`, orgID).Scan(&cnt); err != nil {
			return err
		}
		if cnt != 0 {
			t.Errorf("non-member sees %d org rows, want 0 (RLS leak)", cnt)
		}
		return nil
	}); err != nil {
		t.Fatalf("non-member read tx: %v", err)
	}

	// Non-member write is a no-op under RLS (orgs_update requires admin
	// of the active org) — zero rows affected, no error.
	if err := r.h.WithUser(t, nonMember.String(), orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE public.orgs SET name = 'hijacked' WHERE id = $1`, orgID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("non-member UPDATE affected %d rows, want 0 (RLS leak)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("non-member write tx: %v", err)
	}

	// The name is unchanged.
	var name string
	if err := r.h.AdminDB.QueryRow(`SELECT name FROM public.orgs WHERE id = $1`, orgID).Scan(&name); err != nil {
		t.Fatalf("select name: %v", err)
	}
	if name != "Private Co" {
		t.Errorf("org name=%q after non-member write attempt, want unchanged", name)
	}
}

// TestOrgGet_MemberVisibleAndNonDisclosing pins the org single read: a member
// gets the org's identity plus their OWN role (which is what makes it useful
// to a client that has to decide whether to render an admin surface), and a
// non-member gets 404 rather than a 403 — a 403 would confirm the org exists
// to somebody with no relationship to it.
func TestOrgGet_MemberVisibleAndNonDisclosing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	runmode.SetOrgCreationEnabledForTest(t, true)

	r := newAuthRig(t)
	founder := r.seedUser()
	resp, _ := r.driveCallback(founder)
	sid := r.sidFromResp(resp)

	created := r.postJSONWithSid("POST", "/api/orgs", sid, map[string]string{"name": "Acme Industries"})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d, want 201", created.StatusCode)
	}
	var out orgCreateResponse
	if err := json.NewDecoder(created.Body).Decode(&out); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	got := r.postJSONWithSid("GET", "/api/orgs/"+out.ID, sid, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/orgs/%s = %d, want 200", out.ID, got.StatusCode)
	}
	var org orgJSON
	if err := json.NewDecoder(got.Body).Decode(&org); err != nil {
		t.Fatalf("decode org: %v", err)
	}
	if org.ID != out.ID || org.Name != "Acme Industries" {
		t.Errorf("org = %+v, want id=%s name=Acme Industries", org, out.ID)
	}
	// The founder owns the org they just created, so the read reports the
	// role the client would gate an admin surface on.
	if org.Role != "owner" && org.Role != "admin" {
		t.Errorf("role = %q, want the founder's own role (owner/admin)", org.Role)
	}

	// A different signed-in user with no membership: 404, not 403.
	outsider := r.seedUser()
	outResp, _ := r.driveCallback(outsider)
	outSid := r.sidFromResp(outResp)
	if miss := r.postJSONWithSid("GET", "/api/orgs/"+out.ID, outSid, nil); miss.StatusCode != http.StatusNotFound {
		t.Errorf("non-member GET = %d, want 404 (non-disclosure)", miss.StatusCode)
	}
}

// assertRowExists fails the test if the single-column existence query
// returns no row.
func assertRowExists(t *testing.T, r *authRig, query string, arg any, label string) {
	t.Helper()
	var one int
	if err := r.h.AdminDB.QueryRow(query, arg).Scan(&one); err != nil {
		t.Fatalf("%s row missing: %v", label, err)
	}
}
