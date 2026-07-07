package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedTaskForTest creates a task row directly via raw SQL so package
// db's CRUD tests have a task to FK-point to without reaching for the
// TaskStore impl (which lives in internal/db/sqlite and would create
// a circular import if pulled into package db). The signature matches
// the pre-D2 FindOrCreateTask just enough to land a fresh queued row;
// callers that need find-or-bump behavior should compose their own
// test against the SQLite TaskStore from a package-external test file.
//
// Lives in a *non-_test.go* file so other (non-test) build paths
// won't compile it in — guarded by build tag isn't strictly necessary
// because nothing in production calls it.
func seedTaskForTest(t *testing.T, database *sql.DB, entityID, eventType, dedupKey, primaryEventID string) *domain.Task {
	t.Helper()
	now := time.Now()
	id := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at,
		                   team_id, visibility)
		VALUES (?, ?, ?, ?, ?, 'queued', ?, 'pending', ?, ?, 'team')
	`, id, entityID, eventType, dedupKey, primaryEventID, 0.5, now, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return &domain.Task{
		ID:             id,
		EntityID:       entityID,
		EventType:      eventType,
		DedupKey:       dedupKey,
		PrimaryEventID: primaryEventID,
		Status:         "queued",
		CreatedAt:      now,
	}
}

// taskMutationsForTest exposes the in-package raw UPDATEs the
// AgentRun tests need to position their fixtures. Same justification
// as seedTaskForTest — circular imports prevent calling through the
// store interface, and these mutations are 1:1 with what the SQLite
// TaskStore methods do internally.
func taskMutationsForTest() taskMutForTest { return taskMutForTest{} }

type taskMutForTest struct{}

func (taskMutForTest) SetClaimedByAgent(t *testing.T, database *sql.DB, taskID, agentID string) {
	t.Helper()
	var a any = agentID
	if agentID == "" {
		a = nil
	}
	if _, err := database.Exec(`
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       claimed_by_user_id  = NULL
		 WHERE id = ?
	`, a, taskID); err != nil {
		t.Fatalf("test setClaimedByAgent: %v", err)
	}
}

func (taskMutForTest) SetClaimedByUser(t *testing.T, database *sql.DB, taskID, userID string) {
	t.Helper()
	var u any = userID
	if userID == "" {
		u = nil
	}
	if _, err := database.Exec(`
		UPDATE tasks
		   SET claimed_by_user_id  = ?,
		       claimed_by_agent_id = NULL
		 WHERE id = ?
	`, u, taskID); err != nil {
		t.Fatalf("test setClaimedByUser: %v", err)
	}
}

// GetTask fetches the minimal task-claim projection the agent tests
// assert against (id, status, ClaimedByAgentID, ClaimedByUserID).
// This used to scan the full taskColumnsWithEntity list via
// the package-shared scan helpers; once those moved to the per-
// backend TaskStore impls (and the legacy bridge was deleted in this
// PR), the only remaining consumer is this package's agent_test.go
// which only ever reads the two claim columns. Trimming the scan
// keeps the test helper self-contained without re-introducing the
// legacy column list.
func (taskMutForTest) GetTask(t *testing.T, database *sql.DB, taskID string) *domain.Task {
	t.Helper()
	var task domain.Task
	var claimedAgent, claimedUser sql.NullString
	if err := database.QueryRow(`
		SELECT id, status, claimed_by_agent_id, claimed_by_user_id
		FROM tasks
		WHERE id = ?
	`, taskID).Scan(&task.ID, &task.Status, &claimedAgent, &claimedUser); err != nil {
		t.Fatalf("test GetTask: %v", err)
	}
	task.ClaimedByAgentID = claimedAgent.String
	task.ClaimedByUserID = claimedUser.String
	return &task
}
