package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
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
