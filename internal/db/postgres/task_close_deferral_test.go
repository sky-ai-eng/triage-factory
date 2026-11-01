package postgres_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTaskCloseDeferral_Postgres runs the shared held-close suite against
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
			Tasks:         stores.Tasks,
			Entities:      stores.Entities,
			Conversations: stores.Conversations,
		}, orgID, newPgCloseDeferralSeeder(h.AdminDB, orgID, userID)
	})
}

// TestTaskCloseDeferral_Postgres_ListsEveryOrg pins the list read's scope:
// the sweep is one loop over every org, so it reads every live org's
// held closes, each row naming the org it belongs to, and none of a deleted
// org's. SQLite holds one org and has no soft delete, so it cannot show
// either.
func TestTaskCloseDeferral_Postgres_ListsEveryOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	want := map[string]string{} // task id → org id
	for i := range 3 {
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		seed := newPgCloseDeferralSeeder(h.AdminDB, orgID, userID)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		seed.BlueprintAndConversation(t, taskID, "running", "")
		if _, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, eventID); err != nil {
			t.Fatalf("CloseOrHoldSystem: %v", err)
		}
		if i == 2 {
			if _, err := h.AdminDB.Exec(`UPDATE orgs SET deleted_at = now() WHERE id = $1`, orgID); err != nil {
				t.Fatalf("soft-delete org: %v", err)
			}
			continue
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
	exec := func(t *testing.T, what, query string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	return dbtest.TaskCloseDeferralSeeder{
		EntityTerminalCloseSeeder: newPgTerminalCloseSeeder(conn, orgID, userID),
		Claim: func(t *testing.T, conversationID string) {
			exec(t, "seed claim", `
				INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch, lease_expires_at)
				VALUES ($1, $2, 'close-hold', 1, now() + interval '300 seconds')
			`, orgID, conversationID)
		},
		HoldUntilLater: func(t *testing.T, conversationID string) {
			exec(t, "hold until later", `
				UPDATE conversations SET next_attempt_at = now() + interval '1 hour' WHERE org_id = $1 AND id = $2
			`, orgID, conversationID)
		},
		CallOff: func(t *testing.T, blueprintRunID string) {
			exec(t, "call off", `UPDATE blueprint_runs SET cancel_requested = true WHERE org_id = $1 AND id = $2`, orgID, blueprintRunID)
		},
	}
}
