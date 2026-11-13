package postgres_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/google/uuid"

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
			Tasks:            stores.Tasks,
			Entities:         stores.Entities,
			StagedInjections: stores.StagedInjections,
			Conversations:    stores.Conversations,
			UserID:           userID,
		}, orgID, newPgCloseDeferralSeeder(h.AdminDB, orgID, userID)
	})
}

// TestTaskReopen_Postgres runs the shared reopen suite against the Postgres
// impl, on the held-close suite's stores and seeder. The reopen is the System
// mint doors' alone, so the stores run on the admin connection, as in
// production. Skips cleanly when Docker isn't available.
func TestTaskReopen_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunTaskReopenConformance(t, func(t *testing.T) (dbtest.TaskCloseDeferralStores, string, dbtest.TaskCloseDeferralSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		return dbtest.TaskCloseDeferralStores{
			Tasks:            stores.Tasks,
			Entities:         stores.Entities,
			StagedInjections: stores.StagedInjections,
			Conversations:    stores.Conversations,
			UserID:           userID,
		}, orgID, newPgCloseDeferralSeeder(h.AdminDB, orgID, userID)
	})
}

// TestSiblingWork_Postgres runs the shared overlapping-work suite against the
// Postgres impl, on the held-close suite's stores and seeder. Every read and
// the retire are admin-pool only, so the stores run on the admin connection,
// as in production. Skips cleanly when Docker isn't available.
func TestSiblingWork_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunSiblingWorkConformance(t, func(t *testing.T) (dbtest.TaskCloseDeferralStores, string, dbtest.SiblingWorkSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		return dbtest.TaskCloseDeferralStores{
				Tasks:            stores.Tasks,
				Entities:         stores.Entities,
				StagedInjections: stores.StagedInjections,
				Conversations:    stores.Conversations,
				UserID:           userID,
			}, orgID, dbtest.SiblingWorkSeeder{
				TaskCloseDeferralSeeder: newPgCloseDeferralSeeder(h.AdminDB, orgID, userID),
				Touch: func(t *testing.T, conversationID, entityID string) {
					t.Helper()
					if _, err := h.AdminDB.Exec(`
						INSERT INTO conversation_memory_entities (org_id, conversation_id, entity_id, role)
						VALUES ($1, $2, $3, 'touched')
					`, orgID, conversationID, entityID); err != nil {
						t.Fatalf("seed touch: %v", err)
					}
				},
				MoveToOtherTeam: func(t *testing.T, taskID string) {
					t.Helper()
					teamID := uuid.New().String()
					if _, err := h.AdminDB.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
						teamID, orgID, "other-"+teamID[:8], "Other"); err != nil {
						t.Fatalf("seed team: %v", err)
					}
					if _, err := h.AdminDB.Exec(`UPDATE tasks SET team_id = $3 WHERE org_id = $1 AND id = $2`, orgID, taskID, teamID); err != nil {
						t.Fatalf("move task: %v", err)
					}
				},
			}
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
		_, convID := seed.BlueprintAndConversation(t, taskID, "completed", dbtest.SeedConcluded)
		seed.DraftPullRequest(t, convID, seed.EntityForTask(t, seed.Task(t)))
		if out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, eventID); err != nil || !out.Held {
			t.Fatalf("CloseOrHoldSystem = %+v (err %v), want held", out, err)
		}
		// The person resolved the draft, so the sweep's list reads the hold.
		seed.ResolveDeliverables(t, convID)
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
		DraftPullRequest: func(t *testing.T, conversationID, entityID string) {
			var entity any
			if entityID != "" {
				entity = entityID
			}
			exec(t, "seed draft pull request", `
				INSERT INTO deliverables (conversation_id, org_id, team_id, provider, kind, scope, target, state, dedup_key, entity_id)
				SELECT id, org_id, team_id, 'github', 'pull_request', 'github.com', 'acme/widget#7', 'draft', 'held-close-' || id::text, $3
				  FROM conversations WHERE org_id = $1 AND id = $2
			`, orgID, conversationID, entity)
		},
		ResolveDeliverables: func(t *testing.T, conversationID string) {
			exec(t, "resolve drafts", `
				UPDATE deliverables SET state = 'open' WHERE org_id = $1 AND conversation_id = $2 AND state = 'draft'
			`, orgID, conversationID)
		},
	}
}
