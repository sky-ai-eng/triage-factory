package delegate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedMessageAt inserts a non-user run message stamped at the given time, so
// a test can place the deliverable-change ledger watermark (the run's last agent
// activity) before/after the deliverables it seeds.
func seedMessageAt(t *testing.T, s *Spawner, conversationID string, at time.Time) {
	t.Helper()
	if _, err := s.conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: conversationID, Role: "assistant", Content: "agent turn", CreatedAt: at,
	}); err != nil {
		t.Fatalf("seed agent message: %v", err)
	}
}

// seedResolvedDeliverable upserts a deliverable in a given post-resolution state for
// conversationID (updated_at = now), so the ledger derivation sees it as a human
// resolution after a past watermark.
func seedResolvedDeliverable(t *testing.T, s *Spawner, conversationID string, a domain.Deliverable) {
	t.Helper()
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	if _, err := s.deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, s.orgs, runmode.LocalDefaultOrgID, a)); err != nil {
		t.Fatalf("seed deliverable %s/%s: %v", a.Kind, a.State, err)
	}
}

// TestDeliverableLedgerForResume_DerivesResolvedSinceWatermark: a resumable run's
// ledger bundles exactly the deliverables a human resolved AFTER the agent's last
// activity, skipping unresolved drafts — derived from the deliverable rows, no
// ledger table.
func TestDeliverableLedgerForResume_DerivesResolvedSinceWatermark(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-led", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// Watermark in the past, so the deliverables seeded "now" all land after it.
	seedMessageAt(t, s, "r-led", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	// Resolved: PR approved (open) and a review dismissed → both reported.
	approvedPR := domain.NewPullRequestDeliverable("o/r", 1, "node1", "h", "main",
		"https://github.com/o/r/pull/1", "PR one", "", false) // draft=false → state=open
	seedResolvedDeliverable(t, s, "r-led", approvedPR)

	dismissedReview := domain.NewReviewDeliverable("o/r", 3, "node3", "headsha", "r-led")
	dismissedReview.State = domain.DeliverableStateReviewDismissed
	seedResolvedDeliverable(t, s, "r-led", dismissedReview)

	// Unresolved draft PR → must NOT appear (still awaiting a human).
	draftPR := domain.NewPullRequestDeliverable("o/r", 2, "node2", "h", "main",
		"https://github.com/o/r/pull/2", "PR two", "", true) // draft=true → state=draft
	seedResolvedDeliverable(t, s, "r-led", draftPR)

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, "r-led")
	if err != nil || conv == nil {
		t.Fatalf("GetSystem: err=%v conversation=%v", err, conv)
	}

	block := s.deliverableLedgerForResume(context.Background(), runmode.LocalDefaultOrgID, conv)
	if block == "" {
		t.Fatal("expected a non-empty ledger block")
	}
	if !strings.HasPrefix(block, "<system-note>") || !strings.HasSuffix(block, "</system-note>") {
		t.Errorf("ledger block not wrapped: %q", block)
	}
	if !strings.Contains(block, "o/r#1") {
		t.Errorf("ledger missing the approved PR o/r#1: %q", block)
	}
	if !strings.Contains(block, "o/r#3") {
		t.Errorf("ledger missing the dismissed review on o/r#3: %q", block)
	}
	if strings.Contains(block, "o/r#2") {
		t.Errorf("ledger should not mention the still-draft PR o/r#2: %q", block)
	}
}

// TestDeliverableLedgerForResume_ExcludesPreWatermark: a deliverable resolved BEFORE
// the agent's last activity (e.g. delivered live while the run was warm) is not
// re-bundled into the ledger — the watermark filters it out.
func TestDeliverableLedgerForResume_ExcludesPreWatermark(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-pre", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// Watermark in the FAR future, so the deliverable seeded "now" is before it.
	seedMessageAt(t, s, "r-pre", time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC))

	approvedPR := domain.NewPullRequestDeliverable("o/r", 1, "node1", "h", "main",
		"https://github.com/o/r/pull/1", "PR one", "", false)
	seedResolvedDeliverable(t, s, "r-pre", approvedPR)

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, "r-pre")
	if err != nil || conv == nil {
		t.Fatalf("GetSystem: err=%v conversation=%v", err, conv)
	}
	if block := s.deliverableLedgerForResume(context.Background(), runmode.LocalDefaultOrgID, conv); block != "" {
		t.Errorf("expected empty ledger (resolution predates the watermark), got %q", block)
	}
}

// waitStagedNotes polls until convID has at least want staged system notes,
// or fails, and returns their contents. The stage runs on a detached goroutine
// (InjectDeliverableNote is fire-and-forget), so the side effect is asserted by
// polling rather than a synchronous read.
func waitStagedNotes(t *testing.T, database *sql.DB, convID string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		notes := stagedNotes(t, database, convID)
		if len(notes) >= want {
			return notes
		}
		if time.Now().After(deadline) {
			t.Fatalf("staged notes did not reach %d in time, have %v", want, notes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stagedNotes reads the contents of convID's undelivered system notes.
func stagedNotes(t *testing.T, database *sql.DB, convID string) []string {
	t.Helper()
	rows, err := database.Query(`SELECT content FROM messages WHERE conversation_id = ? AND subtype = ? AND delivered = 0 ORDER BY id`,
		convID, domain.MessageSubtypeInjectionSystemNote)
	if err != nil {
		t.Fatalf("read staged notes: %v", err)
	}
	defer rows.Close()
	var notes []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan staged note: %v", err)
		}
		notes = append(notes, c)
	}
	return notes
}

// TestDeliverableLedgerForResume_FallsBackToStartedAt: a run with NO agent messages
// yet (LastAgentActivityAtSystem ok=false) uses run.StartedAt as the watermark,
// so a resolution after the run started still lands in the ledger.
func TestDeliverableLedgerForResume_FallsBackToStartedAt(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-nomsg", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// Pin started_at into the past so the deliverable seeded "now" is unambiguously
	// after it; insert NO messages, so the watermark must fall back to it.
	if _, err := database.Exec(`UPDATE conversations SET started_at = ? WHERE id = 'r-nomsg'`,
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("set started_at: %v", err)
	}

	approvedPR := domain.NewPullRequestDeliverable("o/r", 1, "node1", "h", "main",
		"https://github.com/o/r/pull/1", "PR one", "", false)
	seedResolvedDeliverable(t, s, "r-nomsg", approvedPR)

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, "r-nomsg")
	if err != nil || conv == nil {
		t.Fatalf("GetSystem: err=%v conversation=%v", err, conv)
	}
	// Sanity: this run genuinely has no agent message, so we exercise the fallback.
	if _, ok, err := s.conversations.LastAgentActivityAtSystem(context.Background(), runmode.LocalDefaultOrgID, "r-nomsg"); err != nil || ok {
		t.Fatalf("precondition: want no agent message (ok=false), got ok=%v err=%v", ok, err)
	}

	block := s.deliverableLedgerForResume(context.Background(), runmode.LocalDefaultOrgID, conv)
	if block == "" || !strings.Contains(block, "o/r#1") {
		t.Errorf("expected the resolution in the ledger via the StartedAt fallback, got %q", block)
	}
}

// TestInjectDeliverableNote_LiveStagesTheNoteAtTheToolBoundary: resolving a
// deliverable on a live run stages a single kind-specific note for the warm
// process (on a detached goroutine), arms its tool boundary, and reports
// dispatched=true. The next tool batch hands the note to the agent wrapped in
// <system-note>.
func TestInjectDeliverableNote_LiveStagesTheNoteAtTheToolBoundary(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-live", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	// Register a live handle so getProc (the liveness gate) sees the run as warm.
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})
	s.getProc("run-live").boundary.take() // armed from registration

	art := domain.Deliverable{Kind: domain.DeliverableKindPullRequest, State: domain.DeliverableStatePROpen, Target: "o/r#7"}
	if dispatched := s.InjectDeliverableNote(runmode.LocalDefaultOrgID, "run-live", art); !dispatched {
		t.Fatal("expected dispatched=true for a live run")
	}
	notes := waitStagedNotes(t, database, "run-live", 1)
	if len(notes) != 1 || !strings.Contains(notes[0], "o/r#7") || !strings.Contains(notes[0], "approved") {
		t.Fatalf("staged notes = %q, want one note naming the deliverable and its resolution", notes)
	}
	batch := s.notesAtToolBatch(runmode.LocalDefaultOrgID, "run-live", "").Text
	if !strings.Contains(batch, "<system-note>") || !strings.Contains(batch, "o/r#7") {
		t.Errorf("the next tool batch handed over %q, want the note wrapped in <system-note>", batch)
	}
}

// TestInjectDeliverableNote_TerminalNoOp: with no live process, InjectDeliverableNote
// stages nothing (it must not restart the agent) and reports dispatched=false
// — the resolution surfaces via the derived ledger on the next resume instead.
// dispatched=false means no goroutine was spawned, so nothing can be staged
// later.
func TestInjectDeliverableNote_TerminalNoOp(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-dead", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	art := domain.Deliverable{Kind: domain.DeliverableKindPullRequest, State: domain.DeliverableStatePRClosed, Target: "o/r#7"}
	if dispatched := s.InjectDeliverableNote(runmode.LocalDefaultOrgID, "run-dead", art); dispatched {
		t.Error("expected dispatched=false for a run with no live process")
	}
	if notes := stagedNotes(t, database, "run-dead"); len(notes) != 0 {
		t.Errorf("staged notes = %q, want none (a terminal run is told through the ledger)", notes)
	}
}

// TestInjectDeliverableNote_NonResolutionStateNoOp: a non-resolution deliverable state
// (an unresolved draft) produces no note, even on a live run.
func TestInjectDeliverableNote_NonResolutionStateNoOp(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-live", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})

	art := domain.Deliverable{Kind: domain.DeliverableKindPullRequest, State: domain.DeliverableStatePRDraft, Target: "o/r#7"}
	if dispatched := s.InjectDeliverableNote(runmode.LocalDefaultOrgID, "run-live", art); dispatched {
		t.Error("expected dispatched=false for a non-resolution state")
	}
	// Deterministic: the empty-note guard returns before the `go func`, so no
	// goroutine is spawned and nothing is staged without any wait.
	if notes := stagedNotes(t, database, "run-live"); len(notes) != 0 {
		t.Errorf("staged notes = %q, want none", notes)
	}
}
