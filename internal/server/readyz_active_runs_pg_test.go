package server

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestCountActiveRuns_Postgres runs the active_runs COUNT against real
// Postgres. Two things need proving there and cannot be proven by the
// SQLite-backed handler tests:
//
//   - The query is dialect-neutral. /readyz holds one *sql.DB and does not
//     know which backend it is, so `delivered = false` and the correlated
//     EXISTS clauses have to parse and run identically on both.
//   - The waiting arm is the full needs-driving predicate. A delegation
//     conversation parked at `open` and woken by a follow-up is claimable
//     and displays as queued, so it is waiting work; its twin with nothing
//     queued is not. Counting only the mid-flight arm would silently
//     undercount exactly the runs an operator watches this signal for.
func TestCountActiveRuns_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "readyz-active")

	// origin='interactive' sidesteps the origin-requires-parents CHECK; this
	// counter only cares about type, status, claims and messages.
	seed := func(status any) string {
		t.Helper()
		id := uuid.New().String()
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility,
			                           type, origin, trigger_type, status)
			VALUES ($1, $2, $3,
			        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
			        'team', 'delegation', 'interactive', 'manual', $4)
		`, id, orgID, userID, status)
		return id
	}

	seed(nil) // mid-flight, unclaimed — waiting
	working := seed(nil)
	woken := seed("open")
	seed("open") // parked with nothing queued — not waiting
	done := seed("completed")

	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch)
		VALUES ($1, $2, 'readyz-executor', 1)
	`, orgID, working)
	// The one row that separates woken from parked.
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO messages (org_id, conversation_id, role, content, subtype, delivered, window_state)
		VALUES ($1, $2, 'user', 'keep going', '', false, 'active')
	`, orgID, woken)
	// A terminal conversation is never waiting, whatever rows it holds.
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO messages (org_id, conversation_id, role, content, subtype, delivered, window_state)
		VALUES ($1, $2, 'user', 'too late', '', false, 'active')
	`, orgID, done)

	s := &Server{db: h.AdminDB}
	n, err := s.countActiveRuns(ctx)
	if err != nil {
		t.Fatalf("countActiveRuns: %v", err)
	}
	if n != 3 {
		t.Errorf("active_runs = %d, want 3 (mid-flight + engaged + parked-and-woken; not the idle parked one, not the terminal one)", n)
	}
}
