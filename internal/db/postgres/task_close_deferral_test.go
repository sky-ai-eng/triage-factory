package postgres_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTaskCloseDeferral_Postgres runs the shared close-deferral suite against
// the Postgres impl. Every method under test is admin-pool only, so the
// stores run on the admin connection, as in production. Skips cleanly when
// Docker isn't available.
func TestTaskCloseDeferral_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunTaskCloseDeferralConformance(t, func(t *testing.T) (dbtest.TaskCloseDeferralStores, string, dbtest.TaskCloseDeferralSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		return dbtest.TaskCloseDeferralStores{
			Tasks:           stores.Tasks,
			Entities:        stores.Entities,
			Blueprints:      stores.Blueprints,
			ExternalActions: stores.ExternalActions,
		}, orgID, newPgCloseDeferralSeeder(h.AdminDB, orgID, userID)
	})
}

// TestTaskCloseDeferral_Postgres_ListsEveryOrg pins the list read's scope:
// the sweep is one loop over every org, so it reads every org's deferrals,
// each row naming the org it belongs to. SQLite holds one org and cannot
// show it.
func TestTaskCloseDeferral_Postgres_ListsEveryOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	want := map[string]string{} // task id → org id
	for range 2 {
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		seed := newPgCloseDeferralSeeder(h.AdminDB, orgID, userID)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
		if _, err := stores.Tasks.DeferCloseSystem(ctx, orgID, taskID, eventID, runID); err != nil {
			t.Fatalf("DeferCloseSystem: %v", err)
		}
		want[taskID] = orgID
	}

	all, err := stores.Tasks.ListActiveCloseDeferralsSystem(ctx)
	if err != nil {
		t.Fatalf("ListActiveCloseDeferralsSystem: %v", err)
	}
	got := map[string]string{}
	var orgs []string
	for _, d := range all {
		got[d.TaskID] = d.OrgID
		orgs = append(orgs, d.OrgID)
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d deferrals, want %d: %+v", len(got), len(want), all)
	}
	for taskID, orgID := range want {
		if got[taskID] != orgID {
			t.Errorf("task %s listed under org %q, want %q", taskID, got[taskID], orgID)
		}
	}
	if !slices.IsSorted(orgs) {
		t.Errorf("deferrals not ordered by org: %v", orgs)
	}
}

func newPgCloseDeferralSeeder(conn *sql.DB, orgID, userID string) dbtest.TaskCloseDeferralSeeder {
	return dbtest.TaskCloseDeferralSeeder{
		EntityTerminalCloseSeeder: newPgTerminalCloseSeeder(conn, orgID, userID),
		SetDeferralColumns: func(t *testing.T, taskID, eventID, blueprintRunID string) error {
			t.Helper()
			_, err := conn.Exec(`
				UPDATE tasks SET close_deferred_event_id = NULLIF($1, '')::uuid,
				                 close_deferred_blueprint_run_id = NULLIF($2, '')::uuid
				WHERE org_id = $3 AND id = $4
			`, eventID, blueprintRunID, orgID, taskID)
			return err
		},
	}
}
