package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// TestLinearTeamUnreadable_Postgres_StopTracking runs linear.team_unreadable's
// "Stop tracking" through the invoke route on the Postgres stores: the org's
// admin is sent the org row, the tracking team's admin the team row with the
// action first, and the action removes that one Linear team from the team's
// set without asking Linear, keeping the rest as stored. The team row resolves
// at once; the org row waits for the producer, whose next pass no longer names
// the team that stopped tracking.
func TestLinearTeamUnreadable_Postgres_StopTracking(t *testing.T) {
	rig := newAuthRig(t)
	owner := rig.seedUser()
	org, _ := rig.seedOrg(owner, "linear-unreadable-"+uuid.NewString()[:8])
	pgtest.MustExec(t, rig.h.AdminDB,
		`INSERT INTO org_settings (org_id, linear_workspace_id) VALUES ($1, 'ws-test')
		 ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = EXCLUDED.linear_workspace_id`, org)
	fake := newLinearCatalogFake(t, linearFixtureEng, linearFixtureOps)
	rig.srv.linearResolver = fixedLinearResolver{Resolver: rig.srv.linearResolver, endpoint: fake.URL}

	infraAdmin := rig.seedUser()
	pgtest.MustExec(t, rig.h.AdminDB, `INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, infraAdmin, org)
	infra := rig.seedTeam(org, "infra", "admin", infraAdmin)
	ownerSid, adminSid := rig.signIn(owner), rig.signIn(infraAdmin)
	rig.setActiveOrg(t, ownerSid, org)
	rig.setActiveOrg(t, adminSid, org)

	teamPath := "/api/teams/" + infra.String()
	decodeLinearTeamsResponse(t, rig.postJSONWithSid(http.MethodPut, teamPath+"/linear-teams", adminSid, map[string]any{
		"linear_teams": []map[string]any{armedLinearTeam(linearTeamEng), {"id": linearTeamOps}},
	}))

	installNotify(t, rig.srv)
	ctx := context.Background()
	kinds.LinearTeamUnreadable.Sync(ctx, org.String(), []kinds.LinearTeam{{LinearTeamID: linearTeamEng, TeamKey: "ENG"}})

	openInbox := func(sid string) listEnvelope[notificationItemWire] {
		t.Helper()
		return decodeList[notificationItemWire](t, rig.tokensJSON(http.MethodPost, meNotifications+"/list", map[string]any{"status": "open"}, sid, ""))
	}
	ownerInbox := openInbox(ownerSid)
	if ownerInbox.Total() != 1 || ownerInbox.Items[0].Audience.Scope != "org" {
		t.Fatalf("org admin's inbox = %+v, want the org row alone", ownerInbox.Items)
	}
	if acts := ownerInbox.Items[0].Actions; len(acts) != 1 || acts[0].ID != "org_settings" {
		t.Errorf("org row actions = %+v, want the settings link alone", acts)
	}
	adminInbox := openInbox(adminSid)
	if adminInbox.Total() != 1 || adminInbox.Items[0].Audience.Scope != "team" {
		t.Fatalf("infra admin's inbox = %+v, want the team row alone", adminInbox.Items)
	}
	row := adminInbox.Items[0]
	if acts := row.Actions; len(acts) != 2 || acts[0].ID != "untrack" || acts[0].Allowed == nil || !*acts[0].Allowed || acts[0].Confirm == "" {
		t.Fatalf("team row actions = %+v, want untrack first, allowed, with a confirmation", acts)
	}

	before := fake.Calls()
	rec := rig.tokensJSON(http.MethodPost, meNotifications+"/"+row.ID+"/actions/untrack", nil, adminSid, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("untrack = %d: %s", rec.Code, rec.Body.String())
	}
	if it := decodeNotificationItem(t, rec.Body.Bytes()); it.ResolvedAt == nil {
		t.Errorf("invoked row = %+v, want it resolved", it)
	}
	if got := fake.Calls(); got != before {
		t.Errorf("Linear calls during the untrack = %d, want none", got-before)
	}
	set := decodeLinearTeamsResponse(t, rig.requestWithSid(http.MethodGet, teamPath+"/settings", adminSid))
	if len(set) != 1 || set[0].ID != linearTeamOps || set[0].Armed {
		t.Errorf("infra's set after the untrack = %+v, want OPS alone, still watched", set)
	}
	if got := openInbox(ownerSid); got.Total() != 1 {
		t.Errorf("org admin's open inbox = %d rows, want the org row still open", got.Total())
	}

	kinds.LinearTeamUnreadable.Sync(ctx, org.String(), []kinds.LinearTeam{{LinearTeamID: linearTeamEng, TeamKey: "ENG"}})
	if got := openInbox(adminSid); got.Total() != 0 {
		t.Errorf("infra admin's open inbox after the next pass = %+v, want nothing", got.Items)
	}
	if got := openInbox(ownerSid); got.Total() != 1 {
		t.Errorf("org admin's open inbox after the next pass = %d rows, want the org row", got.Total())
	}
}
