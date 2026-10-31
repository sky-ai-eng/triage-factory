package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TestTeamSettingsPatch_Postgres_KeepsAnOrderSavedMidPatch: the team-settings
// PATCH stores the whole settings row, so a tracked-set save that commits
// while the PATCH waits for the team's settings lock is kept. The PATCH reads
// the row only once it holds the lock, and stores the order that save left
// beside its own field.
func TestTeamSettingsPatch_Postgres_KeepsAnOrderSavedMidPatch(t *testing.T) {
	rig := newAuthRig(t)
	alice := rig.seedUser()
	org, team := rig.seedOrg(alice, "settings-patch-"+uuid.NewString()[:8])
	sid := rig.signIn(alice)
	rig.setActiveOrg(t, sid, org)
	ctx := context.Background()

	// The other save holds the lock and reorders the Linear teams, and commits
	// only once the PATCH is waiting for the lock.
	locked, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- rig.srv.tx.WithTx(ctx, org.String(), alice.String(), func(tx db.TxStores) error {
			if err := tx.Teams.LockSettings(ctx, team.String()); err != nil {
				return err
			}
			set, err := tx.Teams.GetSettings(ctx, team.String())
			if err != nil {
				return err
			}
			set.LinearTeams = []string{linearTeamOps, linearTeamEng}
			if _, err := tx.Teams.UpdateSettings(ctx, team.String(), set); err != nil {
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

	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		patched <- rig.tokensJSON(http.MethodPatch, "/api/teams/"+team.String()+"/settings",
			map[string]any{"ai_auto_delegate_enabled": false}, sid, "")
	}()
	waitForLockWaiter(t, rig.h.AdminDB)
	close(release)
	if err := <-held; err != nil {
		t.Fatalf("the other save: %v", err)
	}
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-patched:
	case <-time.After(10 * time.Second):
		t.Fatal("the PATCH never finished after the other save committed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body.String())
	}

	var (
		order        string
		autoDelegate bool
	)
	if err := rig.h.AdminDB.QueryRow(
		`SELECT array_to_json(linear_teams)::text, auto_delegate_enabled FROM team_settings WHERE team_id = $1`, team,
	).Scan(&order, &autoDelegate); err != nil {
		t.Fatalf("read team_settings: %v", err)
	}
	if want := `["` + linearTeamOps + `","` + linearTeamEng + `"]`; order != want {
		t.Errorf("linear_teams = %s, want the other save's %s", order, want)
	}
	if autoDelegate {
		t.Error("auto_delegate_enabled = true, want the PATCH's false")
	}
}
