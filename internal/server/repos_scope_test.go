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
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
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
	plainB       string // teamB plain member: no admin role anywhere
	teamless     string // org member on zero teams
}

func newRepoScopeRig(t *testing.T) *repoScopeRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, owner, teamA := pgtest.SeedOrgWithUser(t, h, "owner")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")
	memberB := pgtest.SeedUser(t, h, "member-b")
	// memberB is teamB's *team* admin (needed so replaceTeamRepos below, run
	// as memberB, satisfies team_github_repos_insert's tf.user_is_team_admin
	// check) but an org-level plain "member" — the role isOrgAdmin/
	// repoVisible actually gate on. Team role and org role are
	// orthogonal; this is a common real shape (team admins who aren't org
	// admins).
	pgtest.AddOrgMember(t, h, memberB, orgID, teamB, "member", "admin")
	// plainB is on teamB with no admin role at either grain — the shape the
	// read gate and the write gate answer differently: they can *see*
	// teamB's repos (membership), but must not be able to mutate an
	// org-wide repository row every tracking team's runs read.
	plainB := pgtest.SeedUser(t, h, "plain-b")
	pgtest.AddOrgMember(t, h, plainB, orgID, teamB, "member", "member")
	teamless := pgtest.SeedUser(t, h, "teamless")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, teamless, orgID)

	rig := &repoScopeRig{
		h: h, s: s, orgID: orgID,
		teamA: teamA, teamB: teamB,
		orgOwner: owner, memberB: memberB, plainB: plainB, teamless: teamless,
	}

	// teamA (owner's team) tracks acme/api; teamB tracks acme/web.
	rig.replaceTeamRepos(t, owner, teamA, domain.TeamGitHubRepo{Owner: "acme", Repo: "api"})
	rig.replaceTeamRepos(t, memberB, teamB, domain.TeamGitHubRepo{Owner: "acme", Repo: "web"})

	return rig
}

func (r *repoScopeRig) replaceTeamRepos(t *testing.T, actingUser, teamID string, repos ...domain.TeamGitHubRepo) {
	t.Helper()
	if err := r.s.tx.WithTx(t.Context(), r.orgID, actingUser, func(tx db.TxStores) error {
		return tx.TeamGitHubRepos.ReplaceForTeam(t.Context(), r.orgID, teamID, dbtest.TestGitHubHost, repos)
	}); err != nil {
		t.Fatalf("ReplaceForTeam(%s): %v", teamID, err)
	}
}

// repoID resolves a seeded repository's registry row id — how every repo
// route addresses one. Read off the admin pool so the lookup itself is not
// subject to the gate under test.
func (r *repoScopeRig) repoID(t *testing.T, owner, repo string) string {
	t.Helper()
	var id string
	if err := r.h.AdminDB.QueryRow(
		`SELECT id::text FROM repositories WHERE org_id = $1 AND lower(owner) = lower($2) AND lower(repo) = lower($3)`,
		r.orgID, owner, repo,
	).Scan(&id); err != nil {
		t.Fatalf("resolve repository id for %s/%s: %v", owner, repo, err)
	}
	return id
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

// listRepos calls the registry list as callerID and returns the page.
func (r *repoScopeRig) listRepos(t *testing.T, callerID string) listEnvelope[repoJSON] {
	t.Helper()
	rec := httptest.NewRecorder()
	r.s.handleRepositories(rec, r.req(http.MethodPost, "/api/repos/list", callerID, map[string]any{}))
	return decodeList[repoJSON](t, rec)
}

func listedRepoSlugs(page listEnvelope[repoJSON]) []string {
	out := make([]string, len(page.Items))
	for i, row := range page.Items {
		out[i] = row.Slug
	}
	return out
}

// TestHandleRepositories_TeamScoped pins the TFAC-559 fix: the registry list
// returns the org-wide union for an org admin, only the caller's own team's
// tracked repos for a plain member, and nothing for a teamless member —
// where before the fix every caller saw the full org-wide list regardless
// of role or team membership.
func TestHandleRepositories_TeamScoped(t *testing.T) {
	rig := newRepoScopeRig(t)

	// Org owner (admin) sees the org-wide union: both teams' repos. The
	// total_count is scoped the same way the rows are — it counts what the
	// caller may see, not what the table holds.
	owner := rig.listRepos(t, rig.orgOwner)
	if got := listedRepoSlugs(owner); !equalSlugs(got, []string{"acme/api", "acme/web"}) || owner.Total() != 2 {
		t.Errorf("owner (org admin) repos = %v (total %d), want org-wide [acme/api acme/web]", got, owner.Total())
	}

	// teamB member sees only teamB's tracked repo (acme/web), not teamA's
	// (acme/api) — the cross-team leak this ticket fixes.
	member := rig.listRepos(t, rig.memberB)
	if got := listedRepoSlugs(member); !equalSlugs(got, []string{"acme/web"}) || member.Total() != 1 {
		t.Errorf("memberB repos = %v (total %d), want only [acme/web] with total 1", got, member.Total())
	}

	// A teamless member sees zero repos — before the fix this returned the
	// full org-wide list to any org member regardless of team membership.
	teamless := rig.listRepos(t, rig.teamless)
	if got := listedRepoSlugs(teamless); len(got) != 0 || teamless.Total() != 0 {
		t.Errorf("teamless member repos = %v (total %d), want empty", got, teamless.Total())
	}
}

// baseBranch reads a repository row's stored base_branch straight off the
// admin pool, so a "the write was rejected" assertion checks the row rather
// than trusting the status code.
func (r *repoScopeRig) baseBranch(t *testing.T, owner, repo string) string {
	t.Helper()
	var got *string
	if err := r.h.AdminDB.QueryRow(
		`SELECT base_branch FROM repositories WHERE org_id = $1 AND lower(owner) = lower($2) AND lower(repo) = lower($3)`,
		r.orgID, owner, repo,
	).Scan(&got); err != nil {
		t.Fatalf("read base_branch for %s/%s: %v", owner, repo, err)
	}
	if got == nil {
		return ""
	}
	return *got
}

func (r *repoScopeRig) patchBaseBranch(t *testing.T, callerID, owner, repo, branch string) *httptest.ResponseRecorder {
	t.Helper()
	id := r.repoID(t, owner, repo)
	rec := httptest.NewRecorder()
	req := r.req(http.MethodPatch, "/api/repos/"+id, callerID, map[string]string{"base_branch": branch})
	req.SetPathValue("id", id)
	r.s.handleRepoUpdate(rec, req)
	return rec
}

// TestHandleRepoUpdate_TeamScoped pins the PATCH-side visibility boundary:
// a repo outside the caller's tracked set gets 404 (not disclosed, matching
// the GET-side filtering) rather than a silent cross-team write, and an org
// admin may update any repo.
func TestHandleRepoUpdate_TeamScoped(t *testing.T) {
	rig := newRepoScopeRig(t)

	// memberB is teamB's team admin, so their own team's repo is writable.
	if rec := rig.patchBaseBranch(t, rig.memberB, "acme", "web", "develop"); rec.Code != http.StatusOK {
		t.Fatalf("memberB PATCH acme/web: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// memberB reaches for teamA's repo (acme/api) — blocked with 404, not
	// leaked as a 403 (which would confirm the repo's existence).
	if rec := rig.patchBaseBranch(t, rig.memberB, "acme", "api", "develop"); rec.Code != http.StatusNotFound {
		t.Fatalf("memberB PATCH acme/api (untracked by their team): status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// The org owner (admin) may update any repo, including one outside
	// their own team's tracked set.
	if rec := rig.patchBaseBranch(t, rig.orgOwner, "acme", "web", "release"); rec.Code != http.StatusOK {
		t.Fatalf("org admin PATCH acme/web: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// A teamless member is blocked from every repo.
	if rec := rig.patchBaseBranch(t, rig.teamless, "acme", "api", "develop"); rec.Code != http.StatusNotFound {
		t.Fatalf("teamless PATCH acme/api: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleRepoUpdate_RequiresAdmin pins the mutation gate: changing an
// org-wide repository row takes an admin, not just membership of a team that
// tracks it.
// The two rejection codes are load-bearing and different — 403 for a repo
// the caller can see in their own GET /api/repos list (404 there would read
// as a bug), 404 for one they can't (disclosing nothing).
func TestHandleRepoUpdate_RequiresAdmin(t *testing.T) {
	rig := newRepoScopeRig(t)

	// Baseline the row so "unchanged" means something.
	if rec := rig.patchBaseBranch(t, rig.orgOwner, "acme", "web", "release"); rec.Code != http.StatusOK {
		t.Fatalf("seed base_branch: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// plainB is on teamB, which tracks acme/web — they can read it, so this
	// is a permission boundary, not a visibility one: 403, and no write.
	rec := rig.patchBaseBranch(t, rig.plainB, "acme", "web", "hijacked")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plain member PATCH acme/web: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if got := rig.baseBranch(t, "acme", "web"); got != "release" {
		t.Errorf("base_branch = %q after a rejected PATCH, want it unchanged at %q", got, "release")
	}

	// The same caller reaching a repo no team of theirs tracks still gets
	// 404 — the admin check must not turn a non-disclosure into a
	// disclosure.
	if rec := rig.patchBaseBranch(t, rig.plainB, "acme", "api", "hijacked"); rec.Code != http.StatusNotFound {
		t.Fatalf("plain member PATCH acme/api (untracked by their team): status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// Team admin of a tracking team, with no org-admin role at all, still
	// succeeds — this gate is admin-of-a-tracking-team, not org-admin-only.
	if rec := rig.patchBaseBranch(t, rig.memberB, "acme", "web", "develop"); rec.Code != http.StatusOK {
		t.Fatalf("team admin PATCH acme/web: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rig.baseBranch(t, "acme", "web"); got != "develop" {
		t.Errorf("base_branch = %q after an allowed PATCH, want develop", got)
	}
}

// TestHandleRepositories_CanEdit pins the projection the Repos page gates
// its base-branch control on: it mirrors the PATCH gate per row, so the
// client never has to re-derive authz from role (org admin and team admin
// are orthogonal, and only the server knows which tracking teams the caller
// administers).
func TestHandleRepositories_CanEdit(t *testing.T) {
	rig := newRepoScopeRig(t)

	canEdit := func(callerID string) map[string]bool {
		t.Helper()
		page := rig.listRepos(t, callerID)
		out := make(map[string]bool, len(page.Items))
		for _, row := range page.Items {
			out[row.Slug] = row.CanEdit
		}
		return out
	}

	// Org admin: every repo in the org-wide union is editable.
	if got := canEdit(rig.orgOwner); !got["acme/api"] || !got["acme/web"] {
		t.Errorf("org admin can_edit = %v, want both repos true", got)
	}

	// Team admin of teamB: teamB's repo is editable, and teamA's isn't even
	// in their list.
	got := canEdit(rig.memberB)
	if !got["acme/web"] {
		t.Errorf("teamB admin can_edit[acme/web] = false, want true; got %v", got)
	}
	if _, listed := got["acme/api"]; listed {
		t.Errorf("teamB admin should not see acme/api at all; got %v", got)
	}

	// Plain member of teamB: sees the repo, may not edit it — exactly the
	// state the page renders read-only instead of a control that 403s.
	got = canEdit(rig.plainB)
	if _, listed := got["acme/web"]; !listed {
		t.Fatalf("plain member should still see acme/web; got %v", got)
	}
	if got["acme/web"] {
		t.Errorf("plain member can_edit[acme/web] = true, want false")
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
		id := rig.repoID(t, owner, repo)
		rec := httptest.NewRecorder()
		req := rig.req(http.MethodPost, "/api/repos/"+id+"/branches/list", callerID, map[string]any{})
		req.SetPathValue("id", id)
		rig.s.handleRepoBranches(rec, req)
		return rec
	}

	// memberB's team doesn't track acme/api — blocked at the gate, 404,
	// before any GitHub credential resolution is attempted.
	if rec := branches(rig.memberB, "acme", "api"); rec.Code != http.StatusNotFound {
		t.Fatalf("memberB branches list for acme/api: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// memberB's team does track acme/web — passes the gate and reaches the
	// (unconfigured, in this test) GitHub resolver, which answers 409
	// NOT_CONFIGURED, distinctly from the gate's 404.
	if rec := branches(rig.memberB, "acme", "web"); rec.Code != http.StatusConflict {
		t.Fatalf("memberB branches list for acme/web: status = %d, want 409 (gate passed, no GitHub creds); body=%s", rec.Code, rec.Body.String())
	}
}

// setGitHubHost points the org at base through the settings writer, as the org
// owner — the one door that owns the column.
func (r *repoScopeRig) setGitHubHost(t *testing.T, base string) {
	t.Helper()
	if err := r.s.tx.WithTx(t.Context(), r.orgID, r.orgOwner, func(tx db.TxStores) error {
		cur, err := tx.Orgs.GetSettings(t.Context(), r.orgID)
		if err != nil {
			return err
		}
		cur.GitHubBaseURL = base
		_, err = tx.Orgs.UpdateSettingsVersioned(t.Context(), r.orgID, cur, cur.Version)
		return err
	}); err != nil {
		t.Fatalf("set github host %q: %v", base, err)
	}
}

// TestRepoRoutes_RowOnAnotherHostIsInvisible_Postgres pins the host gate under
// RLS and real roles. Once the org points at another GitHub host, its rows on
// the host it left are 404 to every caller — the org owner included, who sees
// every row on the current host — on the id read, the by-name read, the PATCH
// (no write, and a 404 rather than the 403 a tracking team's non-admin gets on
// the current host), and the branch list. Pointing the org back restores them.
func TestRepoRoutes_RowOnAnotherHostIsInvisible_Postgres(t *testing.T) {
	rig := newRepoScopeRig(t)
	apiID := rig.repoID(t, "acme", "api")
	if rec := rig.patchBaseBranch(t, rig.orgOwner, "acme", "api", "release"); rec.Code != http.StatusOK {
		t.Fatalf("seed base_branch: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rig.setGitHubHost(t, "https://ghe.example.com")

	call := func(handler http.HandlerFunc, method, path, callerID string, body any, values map[string]string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := rig.req(method, path, callerID, body)
		for k, v := range values {
			req.SetPathValue(k, v)
		}
		handler(rec, req)
		return rec
	}
	for _, caller := range []struct{ name, id string }{
		{"org owner", rig.orgOwner},
		{"tracking team admin", rig.memberB},
		{"tracking team member", rig.plainB},
	} {
		t.Run(caller.name, func(t *testing.T) {
			if rec := call(rig.s.handleRepoGet, http.MethodGet, "/api/repos/"+apiID, caller.id, nil,
				map[string]string{"id": apiID}); rec.Code != http.StatusNotFound {
				t.Errorf("GET by id = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			if rec := call(rig.s.handleRepoGetByName, http.MethodGet, "/api/repos/by-name/acme/web", caller.id, nil,
				map[string]string{"owner": "acme", "repo": "web"}); rec.Code != http.StatusNotFound {
				t.Errorf("GET by name = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			if rec := rig.patchBaseBranch(t, caller.id, "acme", "web", "hijacked"); rec.Code != http.StatusNotFound {
				t.Errorf("PATCH = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			webID := rig.repoID(t, "acme", "web")
			if rec := call(rig.s.handleRepoBranches, http.MethodPost, "/api/repos/"+webID+"/branches/list", caller.id, map[string]any{},
				map[string]string{"id": webID}); rec.Code != http.StatusNotFound {
				t.Errorf("branches = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
			if page := rig.listRepos(t, caller.id); len(page.Items) != 0 {
				t.Errorf("list = %v, want no rows from the host the org left", listedRepoSlugs(page))
			}
		})
	}
	if got := rig.baseBranch(t, "acme", "web"); got != "" {
		t.Errorf("acme/web base_branch = %q after refused PATCHes, want unset", got)
	}

	rig.setGitHubHost(t, "")
	if got := listedRepoSlugs(rig.listRepos(t, rig.orgOwner)); !equalSlugs(got, []string{"acme/api", "acme/web"}) {
		t.Errorf("org owner list back on the original host = %v, want both rows again", got)
	}
	if got := rig.baseBranch(t, "acme", "api"); got != "release" {
		t.Errorf("acme/api base_branch = %q, want the row kept unchanged at release", got)
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
