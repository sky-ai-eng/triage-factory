package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// TestLinearTeamUnreadable_Postgres_StopTracking: the org admin gets the org row
// and the team admin the team row, whose "Stop tracking" drops only that Linear
// team without asking Linear and resolves it; the org row waits for the producer.
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

// TestLinearTeamUnreadable_Postgres_StopTrackingRebuildsAfterAConcurrentSave: a
// save that commits while "Stop tracking" waits for the settings lock is kept,
// because the action rebuilds from the changed set: OPS keeps its rules, ENG goes.
func TestLinearTeamUnreadable_Postgres_StopTrackingRebuildsAfterAConcurrentSave(t *testing.T) {
	rig := newAuthRig(t)
	owner := rig.seedUser()
	org, _ := rig.seedOrg(owner, "linear-unreadable-race-"+uuid.NewString()[:8])
	pgtest.MustExec(t, rig.h.AdminDB,
		`INSERT INTO org_settings (org_id, linear_workspace_id) VALUES ($1, 'ws-test')
		 ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = EXCLUDED.linear_workspace_id`, org)
	fake := newLinearCatalogFake(t, linearFixtureEng, linearFixtureOps)
	rig.srv.linearResolver = fixedLinearResolver{Resolver: rig.srv.linearResolver, endpoint: fake.URL}

	infraAdmin := rig.seedUser()
	pgtest.MustExec(t, rig.h.AdminDB, `INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, infraAdmin, org)
	infra := rig.seedTeam(org, "infra", "admin", infraAdmin)
	adminSid := rig.signIn(infraAdmin)
	rig.setActiveOrg(t, adminSid, org)

	teamPath := "/api/teams/" + infra.String()
	decodeLinearTeamsResponse(t, rig.postJSONWithSid(http.MethodPut, teamPath+"/linear-teams", adminSid, map[string]any{
		"linear_teams": []map[string]any{armedLinearTeam(linearTeamEng), {"id": linearTeamOps}},
	}))
	installNotify(t, rig.srv)
	ctx := context.Background()
	kinds.LinearTeamUnreadable.Sync(ctx, org.String(), []kinds.LinearTeam{{LinearTeamID: linearTeamEng, TeamKey: "ENG"}})
	inbox := decodeList[notificationItemWire](t, rig.tokensJSON(http.MethodPost, meNotifications+"/list", map[string]any{"status": "open"}, adminSid, ""))
	if inbox.Total() != 1 {
		t.Fatalf("infra admin's inbox = %+v, want the team row alone", inbox.Items)
	}

	// The other save holds the lock and arms OPS with ENG's rules, and commits
	// only once the action is waiting for the lock, after it read the set.
	locked, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- rig.srv.tx.WithTx(ctx, org.String(), infraAdmin.String(), func(tx db.TxStores) error {
			if err := tx.Teams.LockSettings(ctx, infra.String()); err != nil {
				return err
			}
			rules, err := tx.LinearTeamRules.ListForTeam(ctx, infra.String(), "ws-test")
			if err != nil {
				return err
			}
			var eng domain.LinearTeamRules
			for _, r := range rules {
				if r.LinearTeamID == linearTeamEng {
					eng = r
				}
			}
			ops := eng
			ops.LinearTeamID, ops.LinearTeamKey, ops.LinearTeamName = linearTeamOps, "OPS", "Operations"
			if _, err := tx.LinearTeamRules.ReplaceForTeam(ctx, infra.String(), "ws-test", []domain.LinearTeamRules{eng, ops}); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-held:
		t.Fatalf("the other save ended before holding the lock: %v", err)
	}

	before := fake.Calls()
	invoked := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		invoked <- rig.tokensJSON(http.MethodPost, meNotifications+"/"+inbox.Items[0].ID+"/actions/untrack", nil, adminSid, "")
	}()
	waitForLockWaiter(t, rig.h.AdminDB)
	close(release)
	if err := <-held; err != nil {
		t.Fatalf("the other save: %v", err)
	}
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-invoked:
	case <-time.After(10 * time.Second):
		t.Fatal("the untrack never finished after the other save committed")
	}
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
	if len(set) != 1 || set[0].ID != linearTeamOps || !set[0].Armed {
		t.Errorf("infra's set after the untrack = %+v, want OPS alone, armed as the other save left it", set)
	}
}

// waitForLockWaiter returns once some session is waiting for a lock, failing
// the test if none is within ten seconds. Any lock counts, so a write that
// skips the lock it should take and waits on a row instead still gets this
// far, and the test fails on what it stored.
func waitForLockWaiter(t *testing.T, admin *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := admin.QueryRow(`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nothing waited for a lock")
}
