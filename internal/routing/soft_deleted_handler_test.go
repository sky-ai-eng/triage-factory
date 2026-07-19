package routing

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// softDeleteHandler stamps deleted_at on an event_handlers row directly —
// the router reads event_handlers straight off the DB, so this is
// indistinguishable from what EventHandlerStore.Delete does for a
// system_slug row.
func softDeleteHandler(t *testing.T, database *sql.DB, id string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE event_handlers SET deleted_at = datetime('now') WHERE id = ?`, id); err != nil {
		t.Fatalf("soft-delete handler %s: %v", id, err)
	}
}

// TestRouter_SoftDeletedTrigger_NoLongerFires is the router-level proof for
// TFAC-661: GetEnabledForEventSystem — the router's query — filters
// deleted_at IS NULL, so a soft-deleted trigger stops firing immediately,
// with no need to wait for a poll cycle or re-derive pass.
func TestRouter_SoftDeletedTrigger_NoLongerFires(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	stub := &stubDelegator{db: database}
	team := runmode.LocalDefaultTeamID
	seedUserOnTeam(t, database, team, "aidan")
	enableTeamAutoDelegate(t, database, team)
	seedImmediateTrigger(t, database, team, domain.EventGitHubPRCICheckFailed, "sdel")

	router := firingRouter(database, stub)
	st := sqlitestore.New(database)
	ctx := context.Background()

	entityA, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "PR 1", "https://example.com/1")
	if err != nil {
		t.Fatalf("create entity A: %v", err)
	}
	router.HandleEvent(ciEvent(t, entityA.ID, "owner/repo"))
	if got := atomic.LoadInt64(&stub.calls); got != 1 {
		t.Fatalf("control: trigger fired %d times before delete, want 1", got)
	}

	var triggerID string
	if err := database.QueryRow(`SELECT id FROM event_handlers WHERE kind = 'trigger' AND event_type = ?`, domain.EventGitHubPRCICheckFailed).Scan(&triggerID); err != nil {
		t.Fatalf("find seeded trigger: %v", err)
	}
	softDeleteHandler(t, database, triggerID)

	entityB, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", "owner/repo#2", "pr", "PR 2", "https://example.com/2")
	if err != nil {
		t.Fatalf("create entity B: %v", err)
	}
	router.HandleEvent(ciEvent(t, entityB.ID, "owner/repo"))
	if got := atomic.LoadInt64(&stub.calls); got != 1 {
		t.Errorf("soft-deleted trigger fired again, calls = %d, want still 1", got)
	}
}
