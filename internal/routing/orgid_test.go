package routing

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestHandleEvent_EmptyOrgID_Dropped pins the defensive early-return
// at the top of HandleEvent. An emitter that fails to tag evt.OrgID is
// a bug — silently routing the event to the local sentinel would
// produce tenant-mixed writes in multi mode. The check exists at the
// single entry point so downstream helpers can trust their typed
// orgID parameter.
//
// We swap log.Default's writer to a buffer for the duration of the
// test so the assertion can pin the diagnostic without scraping
// stdout. No side effects expected: events.RecordSystem must NOT
// fire, entities.GetSystem must NOT fire — every downstream path is
// gated behind the early return.
func TestHandleEvent_EmptyOrgID_Dropped(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	// Capture the warning log line. log.Default goes to stderr by
	// default; redirect to a buffer for the duration of the test.
	var logBuf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origWriter) })

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#empty-org", "pr", "PR", "https://example.com/empty")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
		// OrgID intentionally omitted.
	})

	// No event row recorded.
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Errorf("events table = %d rows, want 0 (dropped event should never persist)", n)
	}

	// No task created for the orphaned entity.
	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("expected no tasks, got %d (early-return must skip task creation)", len(active))
	}

	// Diagnostic log line emitted so operators can spot the emitter
	// bug. Substring match keeps the assertion robust to formatting
	// tweaks.
	if !strings.Contains(logBuf.String(), "dropping event") {
		t.Errorf("expected 'dropping event' log line, got: %q", logBuf.String())
	}
}

// TestHandleEvent_OrgIDThreaded pins that evt.OrgID flows through
// HandleEvent's helpers into the persisted event row. The SQLite
// store enforces the local-sentinel-only invariant (assertLocalOrg)
// so a true multi-org smoke test belongs in the Postgres pgtest
// matrix (deferred follow-up under SKY-253). Within SQLite the
// smallest non-vacuous assertion is that the recorded org_id matches
// the event's OrgID — a regression that resurrected the hardcoded
// LocalDefaultOrgID sentinel inside the events.RecordSystem call
// would still pass this test trivially, but a regression that
// dropped the orgID parameter entirely (zero-valued string) would
// fail assertLocalOrg with a clear error surfaced here.
func TestHandleEvent_OrgIDThreaded(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#thread", "pr", "PR", "https://example.com/thread")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	router := NewRouter(testPromptStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())

	router.HandleEvent(domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrg,
	})

	// The recorded event row must carry the same org_id the caller
	// passed. The events table has an org_id column; a regression
	// that dropped the parameter would land NULL or empty here.
	var recordedOrgID string
	if err := database.QueryRow(`SELECT org_id FROM events WHERE entity_id = ?`, entity.ID).Scan(&recordedOrgID); err != nil {
		t.Fatalf("read recorded event: %v", err)
	}
	if recordedOrgID != runmode.LocalDefaultOrg {
		t.Errorf("recorded org_id = %q, want %q", recordedOrgID, runmode.LocalDefaultOrg)
	}

	// And the downstream task should be attributable to the same
	// tenant via the store's orgID-scoped read.
	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrg, entity.ID)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) == 0 {
		t.Fatalf("expected at least one task created, got 0")
	}
}
