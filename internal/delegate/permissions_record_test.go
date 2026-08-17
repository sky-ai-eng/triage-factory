// The durable half of the permission round-trip: the row a surfaced prompt
// writes, and the verdict every resolution path stamps back onto it. The
// in-memory broker's own contract is pinned next door in permissions_test.go —
// what's tested here is that a prompt is reachable by someone who never saw the
// websocket frame, and that "a human denied this" ends up distinguishable from
// "nobody was watching" without reading a word of the agent-facing prose.

package delegate

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// permRecordFixture is a spawner wired to a real store, plus a conversation
// with a live claim for its prompts to hang off.
type permRecordFixture struct {
	spawner        *Spawner
	database       *sql.DB
	conversationID string
	claimID        string
	userID         string
}

func newPermRecordFixture(t *testing.T) permRecordFixture {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}

	conversationID := uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, conversationID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	claimID := dbtest.SeedActiveClaim(t, database, conversationID, "exec-1", 0)

	userID := uuid.New().String()
	if _, err := database.Exec(`INSERT INTO users (id, display_name) VALUES (?, 'approver')`, userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	stores := sqlitestore.New(database)
	return permRecordFixture{
		spawner:        NewSpawner(database, stores, nil, nil, ""),
		database:       database,
		conversationID: conversationID,
		claimID:        claimID,
		userID:         userID,
	}
}

// permRow is the stored verdict, read straight from the table.
type permRow struct {
	state     string
	reason    string
	decidedBy string
	waitedMs  *int
}

func (f permRecordFixture) row(t *testing.T, toolCallID string) (permRow, bool) {
	t.Helper()
	var r permRow
	var reason, decidedBy sql.NullString
	var waited sql.NullInt64
	err := f.database.QueryRow(`
		SELECT state, reason, decided_by, waited_ms FROM conversation_permissions
		WHERE conversation_id = ? AND tool_call_id = ?
	`, f.conversationID, toolCallID).Scan(&r.state, &reason, &decidedBy, &waited)
	if err == sql.ErrNoRows {
		return permRow{}, false
	}
	if err != nil {
		t.Fatalf("read permission row: %v", err)
	}
	r.reason = reason.String
	r.decidedBy = decidedBy.String
	if waited.Valid {
		v := int(waited.Int64)
		r.waitedMs = &v
	}
	return r, true
}

func (f permRecordFixture) listPending(t *testing.T) []domain.ConversationPermission {
	t.Helper()
	got, err := sqlitestore.New(f.database).Permissions.ListPending(
		context.Background(), runmode.LocalDefaultOrgID, f.conversationID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	return got
}

// TestBrowserPermissionHandler_PromptIsReadableWhileParked is the ticket's
// whole point: while the agent sits parked, a caller that never saw the
// fire-once frame — a refreshed page, a second tab, a board loaded cold — can
// still find out what is being asked, and answering it still unblocks the
// agent.
func TestBrowserPermissionHandler_PromptIsReadableWhileParked(t *testing.T) {
	f := newPermRecordFixture(t)
	h := f.spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, f.conversationID, f.claimID, AbsentAutoDeny{})

	got := make(chan agentproc.PermissionDecision, 1)
	go func() {
		got <- h(agentproc.PermissionRequest{
			ToolCallID: "toolu_read",
			ToolName:   "Bash",
			Input:      map[string]any{"command": "rm -rf ./build"},
			Title:      "Claude wants to run rm -rf ./build",
		})
	}()
	waitForPending(t, f.spawner, f.conversationID, "toolu_read")

	pending := f.listPending(t)
	if len(pending) != 1 {
		t.Fatalf("expected the parked prompt to be readable, got %d rows: %+v", len(pending), pending)
	}
	p := pending[0]
	if p.ToolCallID != "toolu_read" || p.ToolName != "Bash" {
		t.Fatalf("prompt identity lost: %+v", p)
	}
	// The input is what a user actually approves — reconstructing the prompt
	// from tool name alone would be asking them to approve a category.
	if cmd, _ := p.Input["command"].(string); cmd != "rm -rf ./build" {
		t.Fatalf("tool input not reloadable: %+v", p.Input)
	}
	if p.Title != "Claude wants to run rm -rf ./build" {
		t.Fatalf("title not reloadable: %q", p.Title)
	}
	if p.ExpiresAt == nil {
		t.Fatal("expires_at must be recorded so a reconstructed prompt shows the time it actually has left")
	}

	// Answering still drives the broker — the row is a record alongside the
	// mechanism, not a replacement for it.
	if err := f.spawner.ResolvePermission(runmode.LocalDefaultOrgID, f.conversationID, "toolu_read", f.userID,
		agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("ResolvePermission: %v", err)
	}
	select {
	case d := <-got:
		if d.Behavior != "allow" {
			t.Fatalf("behavior = %q, want allow", d.Behavior)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never unparked")
	}
	if pending := f.listPending(t); len(pending) != 0 {
		t.Fatalf("an answered prompt must leave the pending set: %+v", pending)
	}
}

// TestResolvePermission_ApproveRecordsWhoAndHowLong pins the audit gap this
// closes: an approve used to leave no trace whatsoever — the tool simply ran.
func TestResolvePermission_ApproveRecordsWhoAndHowLong(t *testing.T) {
	f := newPermRecordFixture(t)
	h := f.spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, f.conversationID, f.claimID, AbsentAutoDeny{})

	go h(agentproc.PermissionRequest{ToolCallID: "toolu_allow", ToolName: "Bash"})
	waitForPending(t, f.spawner, f.conversationID, "toolu_allow")

	if err := f.spawner.ResolvePermission(runmode.LocalDefaultOrgID, f.conversationID, "toolu_allow", f.userID,
		agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("ResolvePermission: %v", err)
	}

	row, found := f.row(t, "toolu_allow")
	if !found {
		t.Fatal("no row recorded for an approved tool call")
	}
	if row.state != domain.PermissionStateAllowed || row.reason != domain.PermissionReasonUser {
		t.Fatalf("state/reason = %q/%q, want allowed/user", row.state, row.reason)
	}
	if row.decidedBy != f.userID {
		t.Fatalf("decided_by = %q, want the approving user %q", row.decidedBy, f.userID)
	}
	if row.waitedMs == nil {
		t.Fatal("waited_ms must be stamped — how long the agent sat waiting on a human is not recoverable afterwards")
	}
}

// TestBrowserPermissionHandler_DenyPathsAreDistinguishable is the reason
// `reason` is a column. All three of these hand the agent English prose that
// says roughly "you were denied"; only the enum separates a human's refusal
// from a room nobody was in.
func TestBrowserPermissionHandler_DenyPathsAreDistinguishable(t *testing.T) {
	t.Run("a human denied it", func(t *testing.T) {
		f := newPermRecordFixture(t)
		h := f.spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, f.conversationID, f.claimID, AbsentAutoDeny{})
		go h(agentproc.PermissionRequest{ToolCallID: "toolu_user", ToolName: "Bash"})
		waitForPending(t, f.spawner, f.conversationID, "toolu_user")

		if err := f.spawner.ResolvePermission(runmode.LocalDefaultOrgID, f.conversationID, "toolu_user", f.userID,
			agentproc.PermissionDecision{Behavior: "deny", Message: "not on prod"}); err != nil {
			t.Fatalf("ResolvePermission: %v", err)
		}
		row, _ := f.row(t, "toolu_user")
		if row.state != domain.PermissionStateDenied || row.reason != domain.PermissionReasonUser || row.decidedBy != f.userID {
			t.Fatalf("a human deny must be attributable: %+v", row)
		}
	})

	t.Run("watched but unanswered", func(t *testing.T) {
		f := newPermRecordFixture(t)
		// permTimeout() is idleTimeout()/2, so a tiny idle gives a tiny window.
		f.spawner.SetIdleHibernateTimeout(60 * time.Millisecond)
		h := f.spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, f.conversationID, f.claimID, AbsentAutoDeny{})

		d := h(agentproc.PermissionRequest{ToolCallID: "toolu_timeout", ToolName: "Bash"})
		if d.Behavior != "deny" {
			t.Fatalf("behavior = %q, want deny", d.Behavior)
		}
		row, found := f.row(t, "toolu_timeout")
		if !found {
			t.Fatal("no row recorded for a timed-out prompt")
		}
		if row.state != domain.PermissionStateDenied || row.reason != domain.PermissionReasonTimeout {
			t.Fatalf("state/reason = %q/%q, want denied/timeout", row.state, row.reason)
		}
		if row.decidedBy != "" {
			t.Fatalf("decided_by = %q, want empty — nobody decided this", row.decidedBy)
		}
	})

	t.Run("nobody was watching", func(t *testing.T) {
		f := newPermRecordFixture(t)
		f.spawner.SetPresencePollInterval(5 * time.Millisecond)
		h := f.spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, f.conversationID, f.claimID,
			AbsentAutoDeny{enabled: true, grace: 20 * time.Millisecond})

		d := h(agentproc.PermissionRequest{ToolCallID: "toolu_absent", ToolName: "Bash"})
		if d.Behavior != "deny" {
			t.Fatalf("behavior = %q, want deny", d.Behavior)
		}
		row, found := f.row(t, "toolu_absent")
		if !found {
			t.Fatal("no row recorded for an unattended prompt")
		}
		if row.state != domain.PermissionStateDenied || row.reason != domain.PermissionReasonAbsent {
			t.Fatalf("state/reason = %q/%q, want denied/absent", row.state, row.reason)
		}
	})
}

// TestExpirePermissionsForClaim_SettlesAndIsNotLoadBearing pins both halves of
// the release path: the tidy-up stamps a settled verdict, AND the pending read
// is already correct without it. The second half is what makes this survive a
// crash, which by definition runs no cleanup.
func TestExpirePermissionsForClaim_SettlesAndIsNotLoadBearing(t *testing.T) {
	f := newPermRecordFixture(t)
	h := f.spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, f.conversationID, f.claimID, AbsentAutoDeny{})
	go h(agentproc.PermissionRequest{ToolCallID: "toolu_orphan", ToolName: "Bash"})
	waitForPending(t, f.spawner, f.conversationID, "toolu_orphan")

	// Release the claim the way a crash would: nothing else runs.
	if _, err := f.database.Exec(
		`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'reaped' WHERE id = ?`, f.claimID,
	); err != nil {
		t.Fatalf("release claim: %v", err)
	}
	if pending := f.listPending(t); len(pending) != 0 {
		t.Fatalf("a prompt from a released engagement must not read as pending even with the row untouched: %+v", pending)
	}
	if row, _ := f.row(t, "toolu_orphan"); row.state != domain.PermissionStatePending {
		t.Fatalf("nothing should have written the row yet, got state %q", row.state)
	}

	// Now the tidy-up that normally would have run.
	f.spawner.ExpirePermissionsForClaim(runmode.LocalDefaultOrgID, f.claimID)
	row, _ := f.row(t, "toolu_orphan")
	if row.state != domain.PermissionStateExpired || row.reason != domain.PermissionReasonClaimLost {
		t.Fatalf("state/reason = %q/%q, want expired/claim_lost", row.state, row.reason)
	}
}

// TestBrowserPermissionHandler_RecordFailureStillAsksTheQuestion: a spawner
// with no permission store is the hub-less/partial-bundle shape every other
// test in this package uses, and it must behave exactly as it did before this
// table existed. Failing to RECORD a prompt can never become a reason to fail
// to ASK it.
func TestBrowserPermissionHandler_RecordFailureStillAsksTheQuestion(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-1", "", AbsentAutoDeny{})

	got := make(chan agentproc.PermissionDecision, 1)
	go func() { got <- h(agentproc.PermissionRequest{ToolCallID: "toolu_nostore", ToolName: "Bash"}) }()
	waitForPending(t, s, "run-1", "toolu_nostore")

	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-1", "toolu_nostore", "",
		agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("ResolvePermission: %v", err)
	}
	select {
	case d := <-got:
		if d.Behavior != "allow" {
			t.Fatalf("behavior = %q, want allow", d.Behavior)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never unparked without a store")
	}
}

// TestBrowserPermissionHandler_NoClaimRecordsNothingAndStillAsks pins the
// degradation for a handler built without a claim in scope. Every production
// call site threads one, but the coupling is invisible at the call site and a
// wiring slip would otherwise write a row that is invisible to ListPending AND
// untouched by ExpireForClaim — stuck at 'pending' forever with nothing able to
// surface or settle it. The store refuses instead, so the cost is one logged
// write failure and a prompt that isn't reloadable, never a broken run.
func TestBrowserPermissionHandler_NoClaimRecordsNothingAndStillAsks(t *testing.T) {
	f := newPermRecordFixture(t)
	h := f.spawner.BrowserPermissionHandler(runmode.LocalDefaultOrgID, f.conversationID, "", AbsentAutoDeny{})

	got := make(chan agentproc.PermissionDecision, 1)
	go func() { got <- h(agentproc.PermissionRequest{ToolCallID: "toolu_noclaim", ToolName: "Bash"}) }()
	waitForPending(t, f.spawner, f.conversationID, "toolu_noclaim")

	if _, found := f.row(t, "toolu_noclaim"); found {
		t.Fatal("a claimless prompt must not be recorded — the row could never be read or settled")
	}
	if pending := f.listPending(t); len(pending) != 0 {
		t.Fatalf("nothing should be pending: %+v", pending)
	}

	// The agent's question still stands, and answering it still works.
	if err := f.spawner.ResolvePermission(runmode.LocalDefaultOrgID, f.conversationID, "toolu_noclaim", f.userID,
		agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("ResolvePermission: %v", err)
	}
	select {
	case d := <-got:
		if d.Behavior != "allow" {
			t.Fatalf("behavior = %q, want allow", d.Behavior)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never unparked")
	}
}
