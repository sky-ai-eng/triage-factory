package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// repoScopeRig is the multi-mode fixture for TFAC-559: a real Postgres-backed
// Server (RLS live) with two teams, each tracking a distinct repo, plus a
// teamless org member — the exact leak shape the ticket describes.
type repoScopeRig struct {
	h            *pgtest.Harness
	s            *Server
	orgID        string
	teamA, teamB string
	orgOwner     string // founder: org role 'owner', teamA admin
	memberB      string // teamB *team* admin but org-level plain "member" (no org-admin role)
	teamless     string // org member on zero teams
}

func newRepoScopeRig(t *testing.T) *repoScopeRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores, 3000)

	orgID, owner, teamA := pgtest.SeedOrgWithUser(t, h, "owner")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")
	memberB := pgtest.SeedUser(t, h, "member-b")
	// memberB is teamB's *team* admin (needed so replaceTeamRepos below, run
	// as memberB, satisfies team_github_repos_insert's tf.user_is_team_admin
	// check) but an org-level plain "member" — the role isOrgAdmin/
	// repoAccessAllowed actually gate on. Team role and org role are
	// orthogonal; this is a common real shape (team admins who aren't org
	// admins).
	pgtest.AddOrgMember(t, h, memberB, orgID, teamB, "member", "admin")
	teamless := pgtest.SeedUser(t, h, "teamless")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, teamless, orgID)

	rig := &repoScopeRig{
		h: h, s: s, orgID: orgID,
		teamA: teamA, teamB: teamB,
		orgOwner: owner, memberB: memberB, teamless: teamless,
	}

	// teamA (owner's team) tracks acme/api; teamB tracks acme/web.
	rig.replaceTeamRepos(t, owner, teamA, domain.TeamGitHubRepo{Owner: "acme", Repo: "api"})
	rig.replaceTeamRepos(t, memberB, teamB, domain.TeamGitHubRepo{Owner: "acme", Repo: "web"})

	return rig
}

func (r *repoScopeRig) replaceTeamRepos(t *testing.T, actingUser, teamID string, repos ...domain.TeamGitHubRepo) {
	t.Helper()
	if err := r.s.tx.WithTx(t.Context(), r.orgID, actingUser, func(tx db.TxStores) error {
		return tx.TeamGitHubRepos.ReplaceForTeam(t.Context(), r.orgID, teamID, repos)
	}); err != nil {
		t.Fatalf("ReplaceForTeam(%s): %v", teamID, err)
	}
}

// req builds a request to path as callerID with claims + active org
// injected, mirroring viewerRig.req.
func (r *repoScopeRig) req(method, path, callerID string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

func repoSlugsFromJSON(t *testing.T, body []byte) []string {
	t.Helper()
	var rows []struct {
		Owner string `json:"owner"`
		Repo  string `json:"repo"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode repo list: %v; body=%s", err, body)
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Owner + "/" + row.Repo
	}
	return out
}

// TestHandleRepoProfiles_TeamScoped pins the TFAC-559 fix: GET /api/repos
// returns the org-wide union for an org admin, only the caller's own team's
// tracked repos for a plain member, and nothing for a teamless member —
// where before the fix every caller saw the full org-wide list regardless
// of role or team membership.
func TestHandleRepoProfiles_TeamScoped(t *testing.T) {
	rig := newRepoScopeRig(t)

	// Org owner (admin) sees the org-wide union: both teams' repos.
	rec := httptest.NewRecorder()
	rig.s.handleRepoProfiles(rec, rig.req(http.MethodGet, "/api/repos", rig.orgOwner, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET /api/repos: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoSlugsFromJSON(t, rec.Body.Bytes()); !equalSlugs(got, []string{"acme/api", "acme/web"}) {
		t.Errorf("owner (org admin) repos = %v, want org-wide [acme/api acme/web]", got)
	}

	// teamB member sees only teamB's tracked repo (acme/web), not teamA's
	// (acme/api) — the cross-team leak this ticket fixes.
	rec = httptest.NewRecorder()
	rig.s.handleRepoProfiles(rec, rig.req(http.MethodGet, "/api/repos", rig.memberB, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("memberB GET /api/repos: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoSlugsFromJSON(t, rec.Body.Bytes()); !equalSlugs(got, []string{"acme/web"}) {
		t.Errorf("memberB repos = %v, want only [acme/web]", got)
	}

	// A teamless member sees zero repos — before the fix this returned the
	// full org-wide list to any org member regardless of team membership.
	rec = httptest.NewRecorder()
	rig.s.handleRepoProfiles(rec, rig.req(http.MethodGet, "/api/repos", rig.teamless, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("teamless GET /api/repos: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoSlugsFromJSON(t, rec.Body.Bytes()); len(got) != 0 {
		t.Errorf("teamless member repos = %v, want empty", got)
	}
}

// TestHandleRepoUpdate_TeamScoped pins the PATCH-side fix: a non-admin
// member may only update base_branch on a repo their own team tracks; an
// org admin may update any repo; a member reaching for another team's repo
// gets 404 (not disclosed, matching the GET-side filtering) rather than a
// silent cross-team write.
func TestHandleRepoUpdate_TeamScoped(t *testing.T) {
	rig := newRepoScopeRig(t)

	patch := func(callerID, owner, repo, branch string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := rig.req(http.MethodPatch, "/api/repos/"+owner+"/"+repo, callerID, map[string]string{"base_branch": branch})
		req.SetPathValue("owner", owner)
		req.SetPathValue("repo", repo)
		rig.s.handleRepoUpdate(rec, req)
		return rec
	}

	// memberB updates their own team's repo (acme/web) — allowed.
	if rec := patch(rig.memberB, "acme", "web", "develop"); rec.Code != http.StatusOK {
		t.Fatalf("memberB PATCH acme/web: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// memberB reaches for teamA's repo (acme/api) — blocked with 404, not
	// leaked as a 403 (which would confirm the repo's existence).
	if rec := patch(rig.memberB, "acme", "api", "develop"); rec.Code != http.StatusNotFound {
		t.Fatalf("memberB PATCH acme/api (untracked by their team): status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// The org owner (admin) may update any repo, including one outside
	// their own team's tracked set.
	if rec := patch(rig.orgOwner, "acme", "web", "release"); rec.Code != http.StatusOK {
		t.Fatalf("org admin PATCH acme/web: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// A teamless member is blocked from every repo.
	if rec := patch(rig.teamless, "acme", "api", "develop"); rec.Code != http.StatusNotFound {
		t.Fatalf("teamless PATCH acme/api: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleRepoBranches_TeamScoped pins the same gate on the branches
// listing: a member is blocked (404) before any GitHub call is attempted for
// a repo outside their team's tracked set, but passes the gate (reaching the
// credential-resolution step, which then 400s on this org's unconfigured
// GitHub creds) for a repo their team does track. The distinct status codes
// prove the gate — not credential availability — is what's being exercised.
func TestHandleRepoBranches_TeamScoped(t *testing.T) {
	rig := newRepoScopeRig(t)

	branches := func(callerID, owner, repo string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := rig.req(http.MethodGet, "/api/repos/"+owner+"/"+repo+"/branches", callerID, nil)
		req.SetPathValue("owner", owner)
		req.SetPathValue("repo", repo)
		rig.s.handleRepoBranches(rec, req)
		return rec
	}

	// memberB's team doesn't track acme/api — blocked at the gate, 404,
	// before any GitHub credential resolution is attempted.
	if rec := branches(rig.memberB, "acme", "api"); rec.Code != http.StatusNotFound {
		t.Fatalf("memberB GET acme/api/branches: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// memberB's team does track acme/web — passes the gate and reaches the
	// (unconfigured, in this test) GitHub resolver, which 400s distinctly
	// from the gate's 404.
	if rec := branches(rig.memberB, "acme", "web"); rec.Code != http.StatusBadRequest {
		t.Fatalf("memberB GET acme/web/branches: status = %d, want 400 (gate passed, no GitHub creds); body=%s", rec.Code, rec.Body.String())
	}
}

// equalSlugs compares two "owner/repo" slug slices order-independently.
func equalSlugs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
