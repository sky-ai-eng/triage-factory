package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// fixedLinearResolver hands every org a client keyed with a test API key and
// pointed at the fake workspace. The multi-mode credential path has its own
// tests; what this file exercises is the route over the Postgres stores.
type fixedLinearResolver struct {
	linear.Resolver
	endpoint string
}

func (r fixedLinearResolver) ForSystem(_ context.Context, orgID string) (*linear.Client, error) {
	cfg := linear.APIKey("lin_api_test")
	cfg.Endpoint = r.endpoint
	return linear.NewClient(cfg).WithOrg(orgID), nil
}

func decodeLinearTeamsResponse(t *testing.T, resp *http.Response) []linearTeamSettings {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, raw)
	}
	var out struct {
		LinearTeams []linearTeamSettings `json:"linear_teams"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v; body=%s", err, raw)
	}
	return out.LinearTeams
}

// TestLinearTeamsPut_Postgres_RoundTrips runs the write and the read back
// through a real session on the Postgres stores, under RLS: what the PUT
// echoes is what the team-settings read then shows, and a member who is not
// the team's admin can read the set but not replace it.
func TestLinearTeamsPut_Postgres_RoundTrips(t *testing.T) {
	rig := newAuthRig(t)
	alice := rig.seedUser()
	org, team := rig.seedOrg(alice, "linear-"+uuid.NewString()[:8])
	fake := newLinearCatalogFake(t, linearFixtureEng, linearFixtureOps)
	rig.srv.linearResolver = fixedLinearResolver{Resolver: rig.srv.linearResolver, endpoint: fake.URL}
	sid := rig.signIn(alice)

	teamPath := "/api/teams/" + team.String()
	body := map[string]any{"linear_teams": []map[string]any{
		{"id": linearTeamOps}, armedLinearTeam(linearTeamEng),
	}}
	echo := decodeLinearTeamsResponse(t, rig.postJSONWithSid(http.MethodPut, teamPath+"/linear-teams", sid, body))
	if len(echo) != 2 || echo[0].ID != linearTeamOps || echo[1].ID != linearTeamEng {
		t.Fatalf("echo = %+v, want OPS then ENG", echo)
	}
	if echo[0].Armed || !echo[1].Armed || echo[1].Key != "ENG" {
		t.Errorf("echo = %+v, want OPS watched and ENG armed", echo)
	}

	read := decodeLinearTeamsResponse(t, rig.requestWithSid(http.MethodGet, teamPath+"/settings", sid))
	if !reflect.DeepEqual(read, echo) {
		t.Errorf("settings read =\n%+v\nwant the echo\n%+v", read, echo)
	}

	bob := rig.seedUser()
	pgtest.AddOrgMember(t, rig.h, bob.String(), org.String(), team.String(), "member", "member")
	bobSid := rig.signIn(bob)
	if got := decodeLinearTeamsResponse(t, rig.requestWithSid(http.MethodGet, teamPath+"/settings", bobSid)); !reflect.DeepEqual(got, echo) {
		t.Errorf("a team member's read = %+v, want the stored set", got)
	}
	resp := rig.postJSONWithSid(http.MethodPut, teamPath+"/linear-teams", bobSid, map[string]any{"linear_teams": []any{}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a non-admin member's PUT = %d, want 403", resp.StatusCode)
	}
	if got := decodeLinearTeamsResponse(t, rig.requestWithSid(http.MethodGet, teamPath+"/settings", sid)); len(got) != 2 {
		t.Errorf("a refused PUT changed the set: %+v", got)
	}
}

// TestLinearCatalog_Postgres_AddressedAndGatedByOrg: the catalog routes answer
// for the org in the path, so a caller in two orgs reads the second without
// moving their active org; and the gate is live membership, so a member removed
// from the org gets a 404 from every route, with their session still carrying
// the org, and Linear is never asked.
func TestLinearCatalog_Postgres_AddressedAndGatedByOrg(t *testing.T) {
	rig := newAuthRig(t)
	alice := rig.seedUser()
	org, team := rig.seedOrg(alice, "linear-a-"+uuid.NewString()[:8])
	other, _ := rig.seedOrg(alice, "linear-b-"+uuid.NewString()[:8])
	fake := newLinearCatalogFake(t, linearFixtureEng)
	rig.srv.linearResolver = fixedLinearResolver{Resolver: rig.srv.linearResolver, endpoint: fake.URL}

	sid := rig.signIn(alice)
	resp := rig.postJSONWithSid(http.MethodPost, "/api/orgs/"+other.String()+"/linear/teams/list", sid, map[string]any{})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"key":"ENG"`) {
		t.Fatalf("a second org's teams = %d %s, want 200 listing ENG", resp.StatusCode, raw)
	}

	bob := rig.seedUser()
	pgtest.AddOrgMember(t, rig.h, bob.String(), org.String(), team.String(), "member", "member")
	bobSid := rig.signIn(bob)
	base := "/api/orgs/" + org.String() + "/linear/teams"
	resp = rig.postJSONWithSid(http.MethodPost, base+"/list", bobSid, map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a member's read = %d, want 200", resp.StatusCode)
	}

	pgtest.MustExec(t, rig.h.AdminDB, `DELETE FROM memberships WHERE user_id = $1`, bob)
	pgtest.MustExec(t, rig.h.AdminDB, `DELETE FROM org_memberships WHERE user_id = $1`, bob)
	before := fake.Calls()
	for name, resp := range map[string]*http.Response{
		"teams list":  rig.postJSONWithSid(http.MethodPost, base+"/list", bobSid, map[string]any{}),
		"team":        rig.requestWithSid(http.MethodGet, base+"/"+linearTeamEng, bobSid),
		"states list": rig.postJSONWithSid(http.MethodPost, base+"/"+linearTeamEng+"/states/list", bobSid, map[string]any{}),
		"state":       rig.requestWithSid(http.MethodGet, base+"/"+linearTeamEng+"/states/"+linearStateDone, bobSid),
	} {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s after removal = %d, want 404", name, resp.StatusCode)
		}
	}
	if got := fake.Calls() - before; got != 0 {
		t.Errorf("a removed member's reads reached Linear %d times", got)
	}
}
