package delegate

import (
	"database/sql"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDelegate_EventPath_StampsTriggerIDOnStepRun pins the write half of
// the usage by-rule fix: an event-fired delegation denormalizes the firing
// trigger onto the step-0 conversations row (conversations.trigger_id), and
// the JOIN-free llm_spend view therefore categorizes that run 'autonomous'
// WITH a rule to attribute it to. A NULL trigger_id here is exactly the
// "Automated cost with an empty by-rule breakdown" bug on the usage page.
func TestDelegate_EventPath_StampsTriggerIDOnStepRun(t *testing.T) {
	database := newDelegateTestDB(t)
	org := runmode.LocalDefaultOrgID
	task, bpID := delegatableFixture(t, database, "trigprov")
	stores := sqlitestore.New(database)

	// The firing trigger + the event instance it fired on (the fenced
	// blueprint_run insert requires both).
	breaker, minSuit := 3, 0.5
	trigID := "trigprov-handler"
	if _, err := stores.EventHandlers.Create(t.Context(), org, runmode.LocalDefaultTeamID, domain.EventHandler{
		ID: trigID, Kind: domain.EventHandlerKindTrigger, EventType: domain.EventGitHubPRCICheckFailed,
		BlueprintID: bpID, BreakerThreshold: &breaker, MinAutonomySuitability: &minSuit, Enabled: true,
	}); err != nil {
		t.Fatalf("EventHandlers.Create: %v", err)
	}
	eventID, err := stores.Events.Record(t.Context(), org, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	brID, err := s.Delegate(task, DelegateOpts{
		OrgID: org, ExplicitBlueprintID: bpID,
		TriggerType: "event", TriggerID: trigID, TriggeringEventID: eventID,
	})
	if err != nil {
		t.Fatalf("event Delegate: %v", err)
	}

	// The queued step-0 run carries the event shape AND the firing trigger.
	var conversationID, gotType, gotTrig string
	if err := database.QueryRow(
		`SELECT id, trigger_type, COALESCE(trigger_id, '') FROM conversations WHERE blueprint_run_id = ?`, brID,
	).Scan(&conversationID, &gotType, &gotTrig); err != nil {
		t.Fatalf("read step-0 run: %v", err)
	}
	if gotType != "event" || gotTrig != trigID {
		t.Errorf("step-0 run (trigger_type, trigger_id) = (%q, %q), want (\"event\", %q)", gotType, gotTrig, trigID)
	}

	// End-to-end into the spend layer: give the run one ledger row (the
	// view reads messages, so a run with no stream has no spend rows yet)
	// and check the row pairs category='autonomous' with the firing rule —
	// the invariant the usage page's by-category / by-rule split depends on.
	if _, err := database.Exec(`
		INSERT INTO messages (org_id, conversation_id, role, subtype, content)
		VALUES (?, ?, 'assistant', '', 'work')
	`, org, conversationID); err != nil {
		t.Fatalf("seed ledger row: %v", err)
	}
	var cat string
	var viewTrig sql.NullString
	if err := database.QueryRow(
		`SELECT category, trigger_id FROM llm_spend
		 WHERE source = 'run' AND source_id = (SELECT id FROM messages WHERE conversation_id = ?)`, conversationID,
	).Scan(&cat, &viewTrig); err != nil {
		t.Fatalf("read llm_spend row: %v", err)
	}
	if cat != domain.SpendCategoryAutonomous {
		t.Errorf("llm_spend category = %q, want %q", cat, domain.SpendCategoryAutonomous)
	}
	if !viewTrig.Valid || viewTrig.String != trigID {
		t.Errorf("llm_spend trigger_id = %q (valid=%t), want %q", viewTrig.String, viewTrig.Valid, trigID)
	}
}

// TestDelegate_ManualPath_LeavesTriggerIDNull is the manual counterpart: a
// manual delegation's step run carries no trigger_id (NULL), so it stays out
// of the autonomous by-rule breakdown (it's 'manual' spend, sliced by user).
func TestDelegate_ManualPath_LeavesTriggerIDNull(t *testing.T) {
	database := newDelegateTestDB(t)
	org := runmode.LocalDefaultOrgID
	task, bpID := delegatableFixture(t, database, "trigprov-man")

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	brID, err := s.Delegate(task, DelegateOpts{
		OrgID: org, ExplicitBlueprintID: bpID,
		TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
	})
	if err != nil {
		t.Fatalf("manual Delegate: %v", err)
	}

	var trig sql.NullString
	if err := database.QueryRow(`SELECT trigger_id FROM conversations WHERE blueprint_run_id = ?`, brID).Scan(&trig); err != nil {
		t.Fatalf("read step-0 run: %v", err)
	}
	if trig.Valid {
		t.Errorf("manual step-0 run trigger_id = %q, want NULL", trig.String)
	}
}
