package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// The two member lists narrow on the server: the team roster by `search`, the
// org roster by `search` and `excluding_team_id`. These run under real claims,
// so the RLS arm is live — which is what the team roster's handle search has
// to get past, since a teammate's identity binding is invisible to the caller
// on the app pool.

func (r *teamMembersRig) roster(t *testing.T, callerID string, body map[string]any) teamRosterListResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	r.tmh.handleTeamRosterList(rec, r.req(http.MethodPost, callerID, "", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamRosterListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func rosterIDs(items []teamRosterRow) []string {
	out := make([]string, len(items))
	for i, m := range items {
		out[i] = m.UserID
	}
	return out
}

func memberIDs(items []orgMemberRow) []string {
	out := make([]string, len(items))
	for i, m := range items {
		out[i] = m.UserID
	}
	return out
}

func assertIDs(t *testing.T, label string, got []string, total int, want ...string) {
	t.Helper()
	if total != len(want) {
		t.Errorf("%s total_count = %d, want %d", label, total, len(want))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func TestTeamRosterList_SearchFindsATeammateByHandle(t *testing.T) {
	r := newTeamMembersRig(t)
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO user_github_identities (user_id, github_base_url, login, source, verified_at)
		 VALUES ($1, 'https://github.com', 'octo-founder', 'pat', now())`, r.admin)

	// The member searches the founder's GitHub login: a peer's binding.
	resp := r.roster(t, r.memb, map[string]any{"search": "OCTO-F"})
	assertIDs(t, "search a teammate's login", rosterIDs(resp.Items), resp.TotalCount, r.admin)
	if resp.Items[0].GitHubUsername == nil || *resp.Items[0].GitHubUsername != "octo-founder" {
		t.Errorf("matched row github_username = %v, want octo-founder", resp.Items[0].GitHubUsername)
	}

	resp = r.roster(t, r.memb, map[string]any{"search": "memb"})
	assertIDs(t, "search a display name", rosterIDs(resp.Items), resp.TotalCount, r.memb)

	resp = r.roster(t, r.memb, map[string]any{"search": "nobody"})
	assertIDs(t, "search nothing matches", rosterIDs(resp.Items), resp.TotalCount)

	// A needle of blanks is no search.
	resp = r.roster(t, r.memb, map[string]any{"search": "   "})
	if resp.TotalCount != 2 {
		t.Errorf("blank search total_count = %d, want 2 (the whole roster)", resp.TotalCount)
	}
}

func TestTeamRosterList_SearchValidation(t *testing.T) {
	r := newTeamMembersRig(t)

	rec := httptest.NewRecorder()
	r.tmh.handleTeamRosterList(rec, r.req(http.MethodPost, r.memb, "", map[string]any{"search": strings.Repeat("x", 201)}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("201-rune search status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, "INVALID_FIELD", "search")

	// The cap counts runes, so 200 multi-byte characters is a search.
	r.roster(t, r.memb, map[string]any{"search": strings.Repeat("é", 200)})

	rec = httptest.NewRecorder()
	r.tmh.handleTeamRosterList(rec, r.req(http.MethodPost, r.memb, "", map[string]any{"search": 7}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("numeric search status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.tmh.handleTeamRosterList(rec, r.req(http.MethodPost, r.memb, "", map[string]any{"q": "founder"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, "UNKNOWN_FIELD", "q")
}

func TestTeamRosterList_PageTokenFollowsTheSearch(t *testing.T) {
	r := newTeamMembersRig(t)

	// founder and member both carry an "e", so the search pages at size one.
	first := r.roster(t, r.memb, map[string]any{"search": "e", "page_size": 1})
	if first.NextPageToken == "" || first.TotalCount != 2 {
		t.Fatalf("first page = %+v, want a token and a total of 2", first)
	}

	// The same needle in another case is the same query.
	second := r.roster(t, r.memb, map[string]any{"search": "E", "page_size": 1, "page_token": first.NextPageToken})
	if len(second.Items) != 1 || second.Items[0].UserID == first.Items[0].UserID {
		t.Errorf("second page = %v, want the other match", rosterIDs(second.Items))
	}

	for _, body := range []map[string]any{
		{"search": "founder", "page_size": 1, "page_token": first.NextPageToken},
		{"page_size": 1, "page_token": first.NextPageToken},
	} {
		rec := httptest.NewRecorder()
		r.tmh.handleTeamRosterList(rec, r.req(http.MethodPost, r.memb, "", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("token across searches (%v) status = %d, want 400; body=%s", body["search"], rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, "INVALID_PARAM", "page_token")
	}
}

// orgList calls the org roster as callerID and decodes the page.
func (r *orgMembersRig) orgList(t *testing.T, callerID string, body map[string]any) httpxtest.ListEnvelope[orgMemberRow] {
	t.Helper()
	rec := httptest.NewRecorder()
	r.omh.handleOrgMembersList(rec, r.req(http.MethodPost, callerID, "", body))
	return httpxtest.DecodeList[orgMemberRow](t, rec)
}

// orgListFault calls the org roster expecting a refusal with status.
func (r *orgMembersRig) orgListFault(t *testing.T, callerID string, body map[string]any, status int, reason, field string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.omh.handleOrgMembersList(rec, r.req(http.MethodPost, callerID, "", body))
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, reason, field)
}

// teamOf returns the org's one seeded team.
func (r *orgMembersRig) teamOf(t *testing.T) string {
	t.Helper()
	var teamID string
	if err := r.h.AdminDB.QueryRow(`SELECT id::text FROM teams WHERE org_id = $1`, r.orgID).Scan(&teamID); err != nil {
		t.Fatalf("read team: %v", err)
	}
	return teamID
}

func TestOrgMembersList_SearchByName(t *testing.T) {
	r := newOrgMembersRig(t)

	page := r.orgList(t, r.memb, map[string]any{"search": "ADM"})
	assertIDs(t, "search adm", memberIDs(page.Items), page.Total(), r.admin)

	page = r.orgList(t, r.memb, map[string]any{"search": "zzz"})
	assertIDs(t, "search zzz", memberIDs(page.Items), page.Total())
}

func TestOrgMembersList_SearchMatchesAddressesOnlyForATeamManager(t *testing.T) {
	r := newOrgMembersRig(t)
	pgtest.SeedIdentity(t, r.h, r.admin, "ops-lead@corp.example", true)
	// An admin of a team who is no org admin: someone the add-teammate field
	// is for.
	lead := pgtest.SeedUser(t, r.h, "lead")
	pgtest.AddOrgMember(t, r.h, lead, r.orgID, pgtest.SeedTeam(t, r.h, r.orgID, "second"), "member", "admin")

	// A plain member is not shown addresses anywhere, so a search does not
	// match on them either.
	page := r.orgList(t, r.memb, map[string]any{"search": "corp.example"})
	assertIDs(t, "member searches an address", memberIDs(page.Items), page.Total())

	page = r.orgList(t, lead, map[string]any{"search": "OPS-LEAD@"})
	assertIDs(t, "team admin searches an address", memberIDs(page.Items), page.Total(), r.admin)

	page = r.orgList(t, r.owner, map[string]any{"search": "corp.example"})
	assertIDs(t, "org owner searches an address", memberIDs(page.Items), page.Total(), r.admin)
}

func TestOrgMembersList_ExcludingTeam(t *testing.T) {
	r := newOrgMembersRig(t)
	teamID := r.teamOf(t)
	// Everyone in the rig is on its one team; a newcomer is not.
	newcomer := pgtest.SeedUser(t, r.h, "newcomer")
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, newcomer, r.orgID)

	page := r.orgList(t, r.memb, map[string]any{"excluding_team_id": teamID})
	assertIDs(t, "excluding the team", memberIDs(page.Items), page.Total(), newcomer)

	page = r.orgList(t, r.memb, map[string]any{"excluding_team_id": teamID, "search": "member"})
	assertIDs(t, "excluding the team, searching a member of it", memberIDs(page.Items), page.Total())
}

func TestOrgMembersList_ExcludingTeamValidation(t *testing.T) {
	r := newOrgMembersRig(t)

	r.orgListFault(t, r.memb, map[string]any{"excluding_team_id": "not-a-team"},
		http.StatusBadRequest, "INVALID_FIELD", "excluding_team_id")

	// A team in another org and a team that does not exist answer alike, so
	// the field tells the caller nothing about another org.
	_, _, otherTeam := pgtest.SeedOrgWithUser(t, r.h, "rival")
	for _, id := range []string{otherTeam, uuid.NewString()} {
		r.orgListFault(t, r.memb, map[string]any{"excluding_team_id": id},
			http.StatusUnprocessableEntity, "CROSS_TEAM_REF", "excluding_team_id")
	}

	r.orgListFault(t, r.memb, map[string]any{"search": strings.Repeat("x", 201)},
		http.StatusBadRequest, "INVALID_FIELD", "search")
	r.orgListFault(t, r.memb, map[string]any{"excluding_team": r.teamOf(t)},
		http.StatusBadRequest, "UNKNOWN_FIELD", "excluding_team")
}

func TestOrgMembersList_PageTokenFollowsTheFilters(t *testing.T) {
	r := newOrgMembersRig(t)
	teamID := r.teamOf(t)
	for _, name := range []string{"newcomer-a", "newcomer-b"} {
		u := pgtest.SeedUser(t, r.h, name)
		pgtest.MustExec(t, r.h.AdminDB,
			`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, u, r.orgID)
	}

	first := r.orgList(t, r.memb, map[string]any{"excluding_team_id": teamID, "page_size": 1})
	if first.NextPageToken == "" || first.Total() != 2 {
		t.Fatalf("first page = %+v, want a token and a total of 2", first)
	}
	r.orgList(t, r.memb, map[string]any{"excluding_team_id": teamID, "page_size": 1, "page_token": first.NextPageToken})

	for _, body := range []map[string]any{
		{"page_size": 1, "page_token": first.NextPageToken},
		{"excluding_team_id": teamID, "search": "newcomer", "page_size": 1, "page_token": first.NextPageToken},
	} {
		r.orgListFault(t, r.memb, body, http.StatusBadRequest, "INVALID_PARAM", "page_token")
	}
}
