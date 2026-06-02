package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// createTriggerForTest is the raw-SQL replacement for SavePromptTrigger
// (now also for the unified event_handlers table, post-SKY-259). Used by
// tests in package db that need to seed a trigger row but can't import
// internal/db/sqlite (cycle: the sqlite package depends on db). Tests
// outside this package use stores.EventHandlers.Create directly.
//
// Always writes source='user' + kind='trigger'; the per-kind CHECK
// constraint on event_handlers enforces the trigger-only column set.
func createTriggerForTest(t *testing.T, database *sql.DB, trig domain.EventHandler) {
	t.Helper()
	now := time.Now().UTC()
	source := trig.Source
	if source == "" {
		source = domain.EventHandlerSourceUser
	}
	if trig.TriggerType == "" {
		trig.TriggerType = domain.TriggerTypeEvent
	}
	// creator_user_id is NULL for system rows, LocalDefaultUserID for
	// user rows — enforced by event_handlers_system_has_no_creator.
	var creatorUserID any
	if source == domain.EventHandlerSourceUser {
		creatorUserID = runmode.LocalDefaultUserID
	}
	// A trigger FK-references a blueprint its own team owns. Materialize a
	// user-source blueprint owned by LocalDefaultTeamID with the id the
	// trigger points at so the same-team (blueprint_id, team_id) FK is
	// satisfied. The blueprint wraps no steps here — the helper exists only
	// to satisfy the FK shape, not to drive a delegation.
	if _, err := database.Exec(`
		INSERT INTO blueprints (id, name, source, org_id, team_id, creator_user_id, created_at, updated_at)
		VALUES (?, ?, 'user', ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING
	`, trig.BlueprintID, "trigger-blueprint", runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		runmode.LocalDefaultUserID, now, now,
	); err != nil {
		t.Fatalf("createTriggerForTest blueprint %s: %v", trig.BlueprintID, err)
	}
	if _, err := database.Exec(`
		INSERT INTO event_handlers (id, kind, event_type, scope_predicate_json,
		                            breaker_threshold, min_autonomy_suitability,
		                            blueprint_id, enabled, source, team_id, creator_user_id,
		                            created_at, updated_at)
		VALUES (?, 'trigger', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, trig.ID, trig.EventType, trig.ScopePredicateJSON,
		ptrDerefInt(trig.BreakerThreshold), ptrDerefFloat(trig.MinAutonomySuitability),
		trig.BlueprintID, trig.Enabled, source,
		runmode.LocalDefaultTeamID, creatorUserID,
		now, now,
	); err != nil {
		t.Fatalf("createTriggerForTest %s: %v", trig.ID, err)
	}
}

func ptrDerefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrDerefFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// intPtr / floatPtr wrap literal values into pointers, used in test
// fixtures that build domain.EventHandler kind='trigger' rows (the
// per-kind fields are *int / *float64 because the columns are nullable
// at the schema level).
func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }
