package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// repoScopeRig is a real Postgres-backed Server (RLS live) with two teams, each
// tracking a distinct repo, plus a teamless org member: the shape a cross-team
// repository leak would show up in.
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
		return dbtest.TrackRepos(t.Context(), tx.Repos, tx.TeamGitHubRepos, r.orgID, teamID, dbtest.TestGitHubHost, repos)
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
func (r *repoScopeRig) listRepos(t *testing.T, callerID string) httpxtest.ListEnvelope[repoJSON] {
	t.Helper()
	rec := httptest.NewRecorder()
	r.s.handleRepositories(rec, r.req(http.MethodPost, "/api/repos/list", callerID, map[string]any{}))
	return httpxtest.DecodeList[repoJSON](t, rec)
}

// listTeamRepos calls the registry list as callerID, narrowed to teamID.
func (r *repoScopeRig) listTeamRepos(t *testing.T, callerID, teamID string) httpxtest.ListEnvelope[repoJSON] {
	t.Helper()
	rec := httptest.NewRecorder()
	r.s.handleRepositories(rec, r.req(http.MethodPost, "/api/repos/list", callerID, map[string]any{"team_id": teamID}))
	return httpxtest.DecodeList[repoJSON](t, rec)
}

func listedRepoSlugs(page httpxtest.ListEnvelope[repoJSON]) []string {
	out := make([]string, len(page.Items))
	for i, row := range page.Items {
		out[i] = row.Slug
	}
	return out
}

// TestHandleRepositories_TeamScoped: the registry list is the org-wide union
// for an org admin, the caller's own teams' tracked repos for a member, and
// nothing for a teamless member. Each row's can_edit mirrors the PATCH gate.
func TestHandleRepositories_TeamScoped(t *testing.T) {
	rig := newRepoScopeRig(t)

	canEdit := func(page httpxtest.ListEnvelope[repoJSON]) map[string]bool {
		out := make(map[string]bool, len(page.Items))
		for _, row := range page.Items {
			out[row.Slug] = row.CanEdit
		}
		return out
	}

	// Org owner (admin) sees the org-wide union and may edit every row. The
	// total_count counts what the caller may see, not what the table holds.
	owner := rig.listRepos(t, rig.orgOwner)
	if got := listedRepoSlugs(owner); !equalSlugs(got, []string{"acme/api", "acme/web"}) || owner.Total() != 2 {
		t.Errorf("owner (org admin) repos = %v (total %d), want org-wide [acme/api acme/web]", got, owner.Total())
	}
	if got := canEdit(owner); !got["acme/api"] || !got["acme/web"] {
		t.Errorf("org admin can_edit = %v, want both repos true", got)
	}

	// teamB's team admin sees only teamB's tracked repo (acme/web), and may
	// edit it.
	member := rig.listRepos(t, rig.memberB)
	if got := listedRepoSlugs(member); !equalSlugs(got, []string{"acme/web"}) || member.Total() != 1 {
		t.Errorf("memberB repos = %v (total %d), want only [acme/web] with total 1", got, member.Total())
	}
	if got := canEdit(member); !got["acme/web"] {
		t.Errorf("teamB admin can_edit[acme/web] = false, want true; got %v", got)
	}

	// A plain member of teamB sees the repo but may not edit it, which the page
	// renders read-only rather than as a control that 403s.
	got := canEdit(rig.listRepos(t, rig.plainB))
	if editable, listed := got["acme/web"]; !listed || editable || len(got) != 1 {
		t.Errorf("plain member can_edit = %v, want only acme/web, false", got)
	}

	teamless := rig.listRepos(t, rig.teamless)
	if got := listedRepoSlugs(teamless); len(got) != 0 || teamless.Total() != 0 {
		t.Errorf("teamless member repos = %v (total %d), want empty", got, teamless.Total())
	}
}

// TestHandleRepositories_TeamFilter_Postgres pins team_id under RLS and real
// roles: an org admin reads any team's tracked repositories, including a team
// they are not on; a member reads their own team's; and a member asking for
// another team's gets nothing, total included.
func TestHandleRepositories_TeamFilter_Postgres(t *testing.T) {
	rig := newRepoScopeRig(t)

	for _, c := range []struct {
		name, caller, team string
		want               []string
	}{
		{"admin, own team", rig.orgOwner, rig.teamA, []string{"acme/api"}},
		{"admin, a team they are not on", rig.orgOwner, rig.teamB, []string{"acme/web"}},
		{"member, own team", rig.plainB, rig.teamB, []string{"acme/web"}},
		{"member, another team", rig.plainB, rig.teamA, []string{}},
		{"teamless member", rig.teamless, rig.teamA, []string{}},
	} {
		page := rig.listTeamRepos(t, c.caller, c.team)
		if got := listedRepoSlugs(page); !equalSlugs(got, c.want) || page.Total() != len(c.want) {
			t.Errorf("%s: repos = %v (total %d), want %v", c.name, got, page.Total(), c.want)
		}
	}
}

// TestHandleRepositories_AdminListLeavesOutUntrackedRows pins that the org
// admin's union is the tracked set, not the registry: a repository its only
// team stopped tracking keeps its row but drops out of the list, while a repo
// tracked only by a team the admin is not on stays in it.
func TestHandleRepositories_AdminListLeavesOutUntrackedRows(t *testing.T) {
	rig := newRepoScopeRig(t)
	rig.replaceTeamRepos(t, rig.orgOwner, rig.teamA,
		domain.TeamGitHubRepo{Owner: "acme", Repo: "api"},
		domain.TeamGitHubRepo{Owner: "acme", Repo: "old"})
	rig.replaceTeamRepos(t, rig.orgOwner, rig.teamA, domain.TeamGitHubRepo{Owner: "acme", Repo: "api"})
	oldID := rig.repoID(t, "acme", "old") // the registry row survives untracking

	owner := rig.listRepos(t, rig.orgOwner)
	if got := listedRepoSlugs(owner); !equalSlugs(got, []string{"acme/api", "acme/web"}) || owner.Total() != 2 {
		t.Errorf("owner (org admin) repos = %v (total %d), want the tracked set [acme/api acme/web]", got, owner.Total())
	}

	// The row is still the admin's to read by id; it is only no longer listed.
	rec := httptest.NewRecorder()
	req := rig.req(http.MethodGet, "/api/repos/"+oldID, rig.orgOwner, nil)
	req.SetPathValue("id", oldID)
	rig.s.handleRepoGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET the untracked row by id as the org admin = %d, want 200; body=%s", rec.Code, rec.Body.String())
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

// TestHandleRepoUpdate_RequiresAdmin: changing an org-wide repository row takes
// an admin of a tracking team, not just membership. A member is 403 on a repo in
// their own list and 404 on one outside it, which discloses nothing.
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

// TestHandleRepoBranches_TeamScoped: the branches listing 404s a repo outside
// the caller's tracked set before resolving any credential, and passes the gate
// for one it tracks, reaching the 409 for this org's unconfigured GitHub.
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

// TestRepoRoutes_RowOnAnotherHostIsInvisible_Postgres: under RLS and real roles,
// rows on a host the org left are 404 to every caller, the owner included, on
// every repository route, and the PATCH writes nothing. Pointing back restores them.
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

	rig.setGitHubHost(t, "https://github.com")
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

// TestHandleRepoRegister_GateAndTrack_Postgres: under real RLS only an org admin
// or a team admin may register a repository by GitHub id, and a team admin who
// is no org admin can then track it in their team by the returned registry id.
func TestHandleRepoRegister_GateAndTrack_Postgres(t *testing.T) {
	r := newRepoScopeRig(t)
	if err := r.s.reachableRepos.ReplaceForPATSystem(t.Context(), r.orgID, dbtest.TestGitHubHost, []domain.ReachableRepository{
		{OrgID: r.orgID, Owner: "acme", Repo: "infra", ExternalID: "501"},
	}); err != nil {
		t.Fatalf("seed the reachable mirror: %v", err)
	}

	register := func(caller string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		r.s.handleRepoRegister(rec, r.req(http.MethodPost, "/api/repos", caller, map[string]any{"github_id": "501"}))
		return rec
	}
	for name, caller := range map[string]string{"plain member": r.plainB, "teamless member": r.teamless} {
		if rec := register(caller); rec.Code != http.StatusForbidden {
			t.Errorf("%s registering = %d, want 403; body=%s", name, rec.Code, rec.Body.String())
		}
	}
	if n := registryCount(t, r); n != 2 {
		t.Fatalf("registry rows after the refusals = %d, want the rig's 2", n)
	}

	rec := register(r.memberB)
	if rec.Code != http.StatusOK {
		t.Fatalf("team admin registering = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var infra repoJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &infra); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if infra.Slug != "acme/infra" || infra.GitHubID != "501" || infra.CanEdit {
		t.Errorf("registered = %+v, want acme/infra carrying 501, not yet editable by a team admin whose team does not track it", infra)
	}
	if again := register(r.orgOwner); again.Code != http.StatusOK || !strings.Contains(again.Body.String(), infra.ID) {
		t.Errorf("org admin registering the same id = %d %s, want 200 with row %s", again.Code, again.Body.String(), infra.ID)
	}

	put := func(caller string, ids ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := r.req(http.MethodPut, "/api/teams/"+r.teamB+"/github-repos", caller, map[string]any{"repository_ids": ids})
		req.SetPathValue("team_id", r.teamB)
		rec := httptest.NewRecorder()
		r.s.handleTeamReposPut(rec, req)
		return rec
	}
	web := r.repoID(t, "acme", "web")
	if rec := put(r.plainB, web, infra.ID); rec.Code != http.StatusForbidden {
		t.Errorf("plain member PUT = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	rec = put(r.memberB, web, infra.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("team admin PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var saved teamReposPutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := trackedNames(saved.Repos); !equalSlugs(got, []string{"acme/infra", "acme/web"}) {
		t.Errorf("teamB tracked = %v, want [acme/infra acme/web]", got)
	}
}

// registryCount counts the rig org's registry rows off the admin pool.
func registryCount(t *testing.T, r *repoScopeRig) int {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM repositories WHERE org_id = $1`, r.orgID).Scan(&n); err != nil {
		t.Fatalf("count registry rows: %v", err)
	}
	return n
}
