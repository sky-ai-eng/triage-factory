package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestConversationStore_Postgres_CalledOffRunRefusesAnOpenWakeForANonCreator
// runs the wake's called-off guard on the pool it runs on in production, as a
// teammate who cannot see the creator's manual blueprint run. The shared
// conformance suite wires both pools to the admin connection, where every run
// is visible, so it cannot tell a guard that reads the run past its policy
// from one that reads an empty result as "not called off".
//
// Both directions are asserted, because a guard that refused every invisible
// run would pass the first half and break a teammate's follow-up.
func TestConversationStore_Postgres_CalledOffRunRefusesAnOpenWakeForANonCreator(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, creator, teamID := pgtest.SeedOrgWithUser(t, h, "creator")
	teammate := seedPgMember(t, h, orgID, "teammate", "member")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, teammate, teamID)
	seedPgConversationPromptIn(t, h, "p_called_off_wake", orgID, creator)

	entityID, eventID, taskID := uuid.New().String(), uuid.New().String(), uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Called-off wake', '', '{}'::jsonb, now())
	`, entityID, orgID, "called-off-"+orgID[:8])
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
		                   primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:ci_check_failed', '', $6, 'queued', 'pending', 0.5)
	`, taskID, orgID, creator, teamID, entityID, eventID)

	// The creator's manual blueprint, cancel requested, with its step parked.
	brID := seedPgBlueprintRun(t, h, orgID, creator, taskID)
	pgtest.MustExec(t, h.AdminDB, `UPDATE blueprint_runs SET cancel_requested = true WHERE id = $1`, brID)
	stepIdx := 0
	convID := seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		TaskID: taskID, PromptID: "p_called_off_wake", Status: domain.StatusOpen, Model: "m",
		TriggerType: "manual", CreatorUserID: creator,
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})

	// The precondition: the teammate cannot see the run.
	if err := h.WithUser(t, teammate, orgID, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE id = $1`, brID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("the teammate can see %d blueprint_runs rows; this test needs the creator-scoped policy to hide it", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("read blueprint_runs as the teammate: %v", err)
	}

	wake := func(t *testing.T) bool {
		t.Helper()
		var flipped bool
		if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, teammate, func(tx db.TxStores) error {
			f, mErr := tx.Conversations.MarkQueuedForResume(ctx, orgID, convID)
			flipped = f
			return mErr
		}); err != nil {
			t.Fatalf("MarkQueuedForResume as a non-creator: %v", err)
		}
		return flipped
	}

	if wake(t) {
		t.Error("a non-creator's wake flipped an open step under a cancel-requested run; the guard read the hidden run as not called off")
	}
	var status string
	if err := h.AdminDB.QueryRow(`SELECT status FROM conversations WHERE id = $1`, convID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != domain.StatusOpen {
		t.Errorf("status = %q, want open — a refused wake writes nothing", status)
	}

	// With the cancel withdrawn, the same teammate's wake lands.
	pgtest.MustExec(t, h.AdminDB, `UPDATE blueprint_runs SET cancel_requested = false WHERE id = $1`, brID)
	if !wake(t) {
		t.Error("the teammate's wake was refused under a running run; the guard must not refuse a run it cannot see")
	}
}
