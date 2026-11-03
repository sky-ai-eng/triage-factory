package usage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// The activity feeds' team_id and actor filters, and the facets nodes beside
// each list: the values the client's team and actor selects offer.

// decodeFacets asserts a 200 and decodes a facets node's body into out.
func decodeFacets(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode facets: %v (body=%s)", err, rec.Body.String())
	}
}

// facetsOf renders a cut as (value, count, name) triples, in its served order.
func facetsOf(in []activityFacetJSON) [][3]any {
	out := make([][3]any, len(in))
	for i, f := range in {
		out[i] = [3]any{f.Value, f.Count, f.Name}
	}
	return out
}

func TestUsageActionsNarrowingAndFacets_Postgres(t *testing.T) {
	r := newUsageRig(t)
	r.seedExternalActions(t)
	t.Cleanup(entitlements.Reset)
	license := func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance)) }

	t.Run("team_id_narrows_the_org_list", func(t *testing.T) {
		license()
		for team, want := range map[string][]string{
			r.teamA: {"pr_created", "pr_marked_ready"},
			r.teamB: {"issue_transitioned"},
		} {
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", `{"team_id":"`+team+`"}`))
			out := httpxtest.DecodeList[ActionJSON](t, rec)
			var got []string
			for _, a := range out.Items {
				if a.TeamID != team {
					t.Errorf("team_id=%s served a row of team %s", team, a.TeamID)
				}
				got = append(got, a.Action)
			}
			slices.Sort(got)
			if !slices.Equal(got, want) || out.Total() != len(want) {
				t.Errorf("team_id=%s = %v (total %d), want %v", team, got, out.Total(), want)
			}
		}
	})

	t.Run("actor_takes_the_id_the_rows_carry", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", ""))
		var actor string
		for _, a := range httpxtest.DecodeList[ActionJSON](t, rec).Items {
			if a.ActorUserID != "" {
				actor = a.ActorUserID
			}
		}
		if actor != r.orgAdmin {
			t.Fatalf("the human row's actor_user_id = %q, want %q", actor, r.orgAdmin)
		}
		for _, call := range []func(*httptest.ResponseRecorder){
			func(rec *httptest.ResponseRecorder) {
				r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", `{"actor":"`+actor+`"}`))
			},
			func(rec *httptest.ResponseRecorder) {
				r.uh.handleUsageTeamActions(rec, r.activityReq(r.teamAdmin, r.teamA, `{"actor":"`+actor+`"}`))
			},
		} {
			rec := httptest.NewRecorder()
			call(rec)
			out := httpxtest.DecodeList[ActionJSON](t, rec)
			if len(out.Items) != 1 || out.Items[0].Action != "pr_marked_ready" || out.Total() != 1 {
				t.Errorf("actor=%s = %+v (total %d), want the one marked-ready row", actor, out.Items, out.Total())
			}
		}
	})

	t.Run("bad_filters_400", func(t *testing.T) {
		license()
		for body, want := range map[string][2]string{
			`{"team_id":"platform"}`:                             {httpx.ReasonInvalidField, "team_id"},
			`{"team_id":"6BA7B810-9DAD-11D1-80B4-00C04FD430C8"}`: {httpx.ReasonInvalidField, "team_id"},
			`{"team_id":7}`:                                      {httpx.ReasonInvalidField, "team_id"},
			`{"actor":"someone"}`:                                {httpx.ReasonInvalidField, "actor"},
		} {
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("org %s = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
				continue
			}
			httpxtest.AssertFirstError(t, rec, want[0], want[1])
		}
		// The team feed names its team in the path: team_id is not one of
		// its fields.
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActions(rec, r.activityReq(r.teamAdmin, r.teamA, `{"team_id":"`+r.teamA+`"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("team feed with team_id = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonUnknownField, "team_id")
	})

	t.Run("page_token_is_bound_to_team_and_actor", func(t *testing.T) {
		license()
		mint := func(t *testing.T, body string) string {
			t.Helper()
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", body))
			out := httpxtest.DecodeList[ActionJSON](t, rec)
			if out.NextPageToken == "" {
				t.Fatalf("%s minted no next page token: %+v", body, out)
			}
			return out.NextPageToken
		}
		for _, tc := range []struct{ mintBody, reuse string }{
			{`{"page_size":1,"team_id":"` + r.teamA + `"}`, `"team_id":"` + r.teamB + `"`},
			{`{"page_size":1}`, `"team_id":"` + r.teamA + `"`},
			{`{"page_size":1}`, `"actor":"` + r.orgAdmin + `"`},
		} {
			token := mint(t, tc.mintBody)
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgActions(rec, r.activityReq(r.orgAdmin, "", `{"page_size":1,"page_token":"`+token+`",`+tc.reuse+`}`))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("token from %s reused with %s = %d, want 400; body=%s", tc.mintBody, tc.reuse, rec.Code, rec.Body.String())
				continue
			}
			httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidParam, "page_token")
		}
	})

	t.Run("org_facets_compute_each_cut_without_its_own_filter", func(t *testing.T) {
		license()
		facets := func(t *testing.T, body string) actionActivityFacetsJSON {
			t.Helper()
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgActionFacets(rec, r.activityReq(r.orgAdmin, "", body))
			var out actionActivityFacetsJSON
			decodeFacets(t, rec, &out)
			return out
		}
		admin := [3]any{r.orgAdmin, 1, "Org Admin"}
		// By name: teamA is "default", teamB "teamB".
		teams := [][3]any{{r.teamA, 2, "default"}, {r.teamB, 1, "teamB"}}
		for _, tc := range []struct {
			body          string
			actors, teams [][3]any
		}{
			{`{}`, [][3]any{admin}, teams},
			{`{"team_id":"` + r.teamB + `"}`, [][3]any{}, teams},
			{`{"actor":"` + r.orgAdmin + `"}`, [][3]any{admin}, [][3]any{{r.teamA, 1, "default"}}},
			{`{"provider":"jira"}`, [][3]any{}, [][3]any{{r.teamB, 1, "teamB"}}},
		} {
			got := facets(t, tc.body)
			if a := facetsOf(got.ByActor); !slices.Equal(a, tc.actors) {
				t.Errorf("%s by_actor = %v, want %v", tc.body, a, tc.actors)
			}
			if b := facetsOf(got.ByTeam); !slices.Equal(b, tc.teams) {
				t.Errorf("%s by_team = %v, want %v", tc.body, b, tc.teams)
			}
		}
	})

	t.Run("team_facets_answer_the_same_cuts", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActionFacets(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		var out actionActivityFacetsJSON
		decodeFacets(t, rec, &out)
		if a := facetsOf(out.ByActor); !slices.Equal(a, [][3]any{{r.orgAdmin, 1, "Org Admin"}}) {
			t.Errorf("team by_actor = %v, want the org admin's one row", a)
		}
		if b := facetsOf(out.ByTeam); !slices.Equal(b, [][3]any{{r.teamA, 2, "default"}}) {
			t.Errorf("team by_team = %v, want the team's two rows", b)
		}
	})

	t.Run("facets_refuse_paging_and_other_fields", func(t *testing.T) {
		license()
		for body, want := range map[string][2]string{
			`{"page_size":10}`:               {httpx.ReasonInvalidField, "page_size"},
			`{"page_token":"x"}`:             {httpx.ReasonInvalidField, "page_token"},
			`{"search":"pr"}`:                {httpx.ReasonUnknownField, "search"},
			`{"object_kind":"pull_request"}`: {httpx.ReasonUnknownField, "object_kind"},
			`{"team_id":"platform"}`:         {httpx.ReasonInvalidField, "team_id"},
		} {
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgActionFacets(rec, r.activityReq(r.orgAdmin, "", body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("org facets %s = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
				continue
			}
			httpxtest.AssertFirstError(t, rec, want[0], want[1])
		}
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamActionFacets(rec, r.activityReq(r.teamAdmin, r.teamA, `{"team_id":"`+r.teamA+`"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("team facets with team_id = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonUnknownField, "team_id")
	})

	t.Run("facets_gates", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgActionFacets(rec, r.activityReq(r.orgAdmin, "", ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("org facets unlicensed = %d, want 404", rec.Code)
		}
		license()
		for name, call := range map[string]func(*httptest.ResponseRecorder){
			"org_member": func(rec *httptest.ResponseRecorder) {
				r.uh.handleUsageOrgActionFacets(rec, r.activityReq(r.member, "", ""))
			},
			"team_plain_member": func(rec *httptest.ResponseRecorder) {
				r.uh.handleUsageTeamActionFacets(rec, r.activityReq(r.member, r.teamA, ""))
			},
		} {
			rec := httptest.NewRecorder()
			call(rec)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s = %d, want 403; body=%s", name, rec.Code, rec.Body.String())
			}
		}
	})
}

func TestUsageArtifactsNarrowingAndFacets_Postgres(t *testing.T) {
	r := newUsageRig(t)
	seed := r.seedArtifacts(t)
	t.Cleanup(entitlements.Reset)
	license := func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance)) }

	t.Run("team_id_selects_the_teams_rows", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"team_id":"`+r.teamB+`"}`))
		out := httpxtest.DecodeList[orgObjectActivityJSON](t, rec)
		if len(out.Items) != 2 || out.Total() != 2 || out.Items[0].ObjectID != seed.issue || out.Items[1].ObjectID != seed.branch {
			t.Fatalf("teamB = %+v (total %d), want the issue then the branch", out.Items, out.Total())
		}
		// The branch three runs of two teams pushed reads as teamB's one run.
		if b := out.Items[1]; b.ConversationCount != 1 || b.ActionCount != 1 || len(b.Teams) != 1 || b.Teams[0].ID != r.teamB {
			t.Errorf("teamB's branch = %+v, want its one run on teamB alone", b)
		}
	})

	t.Run("bad_filters_and_the_team_feed_400", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"team_id":"teamB"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad team_id = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidField, "team_id")
		rec = httptest.NewRecorder()
		r.uh.handleUsageTeamArtifacts(rec, r.activityReq(r.teamAdmin, r.teamA, `{"team_id":"`+r.teamA+`"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("team feed with team_id = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonUnknownField, "team_id")
	})

	t.Run("page_token_is_bound_to_team_id", func(t *testing.T) {
		license()
		rec := httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"page_size":1,"team_id":"`+r.teamA+`"}`))
		token := httpxtest.DecodeList[orgObjectActivityJSON](t, rec).NextPageToken
		if token == "" {
			t.Fatal("teamA's first page minted no token")
		}
		rec = httptest.NewRecorder()
		r.uh.handleUsageOrgArtifacts(rec, r.activityReq(r.orgAdmin, "", `{"page_size":1,"page_token":"`+token+`","team_id":"`+r.teamB+`"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("teamA's token under teamB = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidParam, "page_token")
	})

	t.Run("facets_count_objects_per_team", func(t *testing.T) {
		license()
		org := func(t *testing.T, body string) [][3]any {
			t.Helper()
			rec := httptest.NewRecorder()
			r.uh.handleUsageOrgArtifactFacets(rec, r.activityReq(r.orgAdmin, "", body))
			var out artifactActivityFacetsJSON
			decodeFacets(t, rec, &out)
			return facetsOf(out.ByTeam)
		}
		// teamA's runs changed the branch, the PR and the review; teamB's the
		// branch and the issue. A selected team still offers both.
		both := [][3]any{{r.teamA, 3, "default"}, {r.teamB, 2, "teamB"}}
		for _, body := range []string{`{}`, `{"team_id":"` + r.teamA + `"}`} {
			if got := org(t, body); !slices.Equal(got, both) {
				t.Errorf("org by_team %s = %v, want %v", body, got, both)
			}
		}
		if got := org(t, `{"object_kind":"branch"}`); !slices.Equal(got, [][3]any{{r.teamA, 1, "default"}, {r.teamB, 1, "teamB"}}) {
			t.Errorf("org by_team over branches = %v, want one object per team", got)
		}
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamArtifactFacets(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		var team artifactActivityFacetsJSON
		decodeFacets(t, rec, &team)
		if got := facetsOf(team.ByTeam); !slices.Equal(got, [][3]any{{r.teamA, 3, "default"}}) {
			t.Errorf("team by_team = %v, want the team's three objects", got)
		}
		rec = httptest.NewRecorder()
		r.uh.handleUsageOrgArtifactFacets(rec, r.activityReq(r.orgAdmin, "", `{"page_size":0}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("facets with page_size = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidField, "page_size")
		rec = httptest.NewRecorder()
		r.uh.handleUsageOrgArtifactFacets(rec, r.activityReq(r.orgAdmin, "", `{"action":"pr_created"}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("artifacts facets with the actions' filter = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonUnknownField, "action")
	})

	t.Run("facets_gates", func(t *testing.T) {
		entitlements.Reset()
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamArtifactFacets(rec, r.activityReq(r.teamAdmin, r.teamA, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("team facets unlicensed = %d, want 404", rec.Code)
		}
		license()
		rec = httptest.NewRecorder()
		r.uh.handleUsageOrgArtifactFacets(rec, r.activityReq(r.teamAdmin, "", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("org facets as a team admin = %d, want 403", rec.Code)
		}
	})

}
