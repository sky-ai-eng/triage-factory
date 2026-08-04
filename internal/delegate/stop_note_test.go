package delegate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// stopNotes returns the conversation's stop-note rows, oldest-first, read
// straight from the table so the assertions see exactly what was stored
// (attribution and delivery included) rather than a projection of it.
func stopNotes(t *testing.T, database *sql.DB, runID string) []domain.Message {
	t.Helper()
	rows, err := database.Query(
		`SELECT role, subtype, content, COALESCE(user_id, ''), delivered
		   FROM messages WHERE conversation_id = ? AND subtype = ? ORDER BY id`,
		runID, domain.MessageSubtypeStopNote)
	if err != nil {
		t.Fatalf("read stop notes: %v", err)
	}
	defer rows.Close()
	var out []domain.Message
	for rows.Next() {
		var m domain.Message
		var delivered bool
		if err := rows.Scan(&m.Role, &m.Subtype, &m.Content, &m.UserID, &delivered); err != nil {
			t.Fatalf("scan stop note: %v", err)
		}
		m.Delivered = &delivered
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stop notes: %v", err)
	}
	return out
}

// TestStop_DBOnlyPathWritesOneAttributedNote covers the cross-pod shape: no
// local process handle, so the stop parks the row directly. The note is what
// the conversation keeps — the summary is deliberately empty, because a stop
// reached no verdict and the run station renders one whenever a summary
// exists.
//
// The second stop is the point of the test as much as the first: re-stopping
// a parked conversation is a gesture with nothing left to stop, and a notice
// per click would be a transcript full of the user pressing a button.
func TestStop_DBOnlyPathWritesOneAttributedNote(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-note", "sess-note", "/tmp/wt-note")

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-note", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	notes := stopNotes(t, database, "r-note")
	if len(notes) != 1 {
		t.Fatalf("stop notes = %d, want 1", len(notes))
	}
	n := notes[0]
	if n.Role != "user" {
		t.Errorf("role = %q, want user — the note rides the same role the engine's own park notices do", n.Role)
	}
	if n.Content != stopNoteByUser {
		t.Errorf("content = %q, want %q", n.Content, stopNoteByUser)
	}
	if n.UserID != runmode.LocalDefaultUserID {
		t.Errorf("user_id = %q, want the stopping user %q", n.UserID, runmode.LocalDefaultUserID)
	}
	if n.Delivered == nil || !*n.Delivered {
		t.Error("the note must be delivered: it is history the model reads in place, not input awaiting a drain")
	}

	var status, summary string
	if err := database.QueryRow(
		`SELECT status, COALESCE(result_summary, '') FROM conversations WHERE id = 'r-note'`,
	).Scan(&status, &summary); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "open" {
		t.Fatalf("status = %q, want open", status)
	}
	if summary != "" {
		t.Errorf("result_summary = %q, want empty — a non-empty summary renders as a verdict the stop never reached", summary)
	}

	// Second stop, now that the row is parked.
	if err := s.Stop(runmode.LocalDefaultOrgID, "r-note", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if notes := stopNotes(t, database, "r-note"); len(notes) != 1 {
		t.Errorf("stop notes after re-stopping a parked run = %d, want 1", len(notes))
	}
}

// TestStop_LocalKillPathWritesTheNote pins the other arm. The kill returns
// early — the run's own goroutine parks the row — so a note written after the
// dispatch would exist on one path and not the other. It is written before,
// which is also the order the transcript wants: the interruption is recorded
// even if this pod dies immediately after asking for it.
func TestStop_LocalKillPathWritesTheNote(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-kill", "sess-kill", "/tmp/wt-kill")

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	killed := false
	s.mu.Lock()
	s.cancels["r-kill"] = func() { killed = true }
	s.mu.Unlock()

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-kill", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !killed {
		t.Fatal("the local handle was never cancelled; this test is no longer exercising the kill path")
	}

	notes := stopNotes(t, database, "r-kill")
	if len(notes) != 1 || notes[0].Content != stopNoteByUser {
		t.Fatalf("stop notes = %+v, want one user-stop note", notes)
	}
}

// TestStop_SystemStopSpeaksAsTheSystem: a stop with no actor says so, and
// attributes the row to nobody rather than to whoever happens to own the run.
func TestStop_SystemStopSpeaksAsTheSystem(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-sys", "sess-sys", "/tmp/wt-sys")
	if _, err := database.Exec(`UPDATE conversations SET trigger_type = 'event', creator_user_id = NULL WHERE id = 'r-sys'`); err != nil {
		t.Fatalf("make the run event-triggered: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.Stop(runmode.LocalDefaultOrgID, "r-sys", ""); err != nil {
		t.Fatalf("stop: %v", err)
	}

	notes := stopNotes(t, database, "r-sys")
	if len(notes) != 1 {
		t.Fatalf("stop notes = %d, want 1", len(notes))
	}
	if notes[0].Content != stopNoteBySystem {
		t.Errorf("content = %q, want %q", notes[0].Content, stopNoteBySystem)
	}
	if notes[0].UserID != "" {
		t.Errorf("user_id = %q, want empty — nobody asked for this stop", notes[0].UserID)
	}
}

// TestStopAndCancelBlueprint_NoteNamesTheCause: nothing resumes a conversation
// torn down by the layer above it, so its note is the only explanation its
// history will ever carry. "Stopped" alone is what the row's presence already
// says; the cause is the part worth writing.
func TestStopAndCancelBlueprint_NoteNamesTheCause(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-closed", "sess-closed", "/tmp/wt-closed")

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.StopAndCancelBlueprint(runmode.LocalDefaultOrgID, "r-closed", "", StopCauseTaskClosed); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	notes := stopNotes(t, database, "r-closed")
	if len(notes) != 1 {
		t.Fatalf("stop notes = %d, want 1", len(notes))
	}
	if notes[0].Content != StopCauseTaskClosed.note() {
		t.Errorf("content = %q, want %q", notes[0].Content, StopCauseTaskClosed.note())
	}
	if notes[0].Content == stopNoteBySystem {
		t.Error("a lifecycle teardown wrote the generic system note; the cause is what the reader needs")
	}
}

// TestStopAndCancelBlueprint_ParkedRunStillGetsItsNote is the case the plain
// verb's skip must not swallow. Every teardown caller enumerates its task's
// non-terminal runs — `open` among them — so a parked conversation is the
// ordinary thing a closed or swiped task tears down, not an edge case. That
// teardown is permanent, so the note is the only explanation the transcript
// will ever carry; skipping it as a "re-stop" would end the conversation in
// silence.
func TestStopAndCancelBlueprint_ParkedRunStillGetsItsNote(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-parked-teardown", "sess-pt", "/tmp/wt-pt")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-parked-teardown'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.StopAndCancelBlueprint(runmode.LocalDefaultOrgID, "r-parked-teardown", "", StopCauseTaskClosed); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	notes := stopNotes(t, database, "r-parked-teardown")
	if len(notes) != 1 {
		t.Fatalf("stop notes = %d, want 1 — a parked conversation torn down by its task explains nothing without it", len(notes))
	}
	if notes[0].Content != StopCauseTaskClosed.note() {
		t.Errorf("content = %q, want %q", notes[0].Content, StopCauseTaskClosed.note())
	}

	// A second teardown reaching the same row — the router closes the task,
	// then the team is archived — says nothing new, so it writes nothing.
	if err := s.StopAndCancelBlueprint(runmode.LocalDefaultOrgID, "r-parked-teardown", "", StopCauseTaskClosed); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
	if notes := stopNotes(t, database, "r-parked-teardown"); len(notes) != 1 {
		t.Errorf("stop notes after a repeated teardown = %d, want 1", len(notes))
	}
}

// TestStop_NoteRepeatsOnceAPersonHasSpoken: the dedupe walks back only to the
// last human input, so a conversation that was stopped, resumed, and stopped
// again records both stops. Suppressing the second would leave the resumed
// turn looking like it ended on its own.
func TestStop_NoteRepeatsOnceAPersonHasSpoken(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-restop", "", "/tmp/wt-restop")
	markNative(t, database, "r-restop")

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.Stop(runmode.LocalDefaultOrgID, "r-restop", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-restop", runmode.LocalDefaultUserID, "pick it back up"); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if _, err := database.Exec(`UPDATE conversations SET status = 'running' WHERE id = 'r-restop'`); err != nil {
		t.Fatalf("re-engage the conversation: %v", err)
	}
	if err := s.Stop(runmode.LocalDefaultOrgID, "r-restop", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("second stop: %v", err)
	}

	if notes := stopNotes(t, database, "r-restop"); len(notes) != 2 {
		t.Errorf("stop notes = %d, want 2 — a person spoke between the two stops, so both are news", len(notes))
	}
}

// TestStop_ResumedConversationAssemblesTheNote is the model-facing half of the
// feature. A stopped native conversation that gets a follow-up must hand the
// engine a window in which the interruption is visible in place — otherwise
// the model reads its own cut-off turn followed by a new request, with nothing
// explaining the gap, and re-reads the cut-off work as its own failure.
func TestStop_ResumedConversationAssemblesTheNote(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-resume", "", "/tmp/wt-resume")
	markNative(t, database, "r-resume")

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.Stop(runmode.LocalDefaultOrgID, "r-resume", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-resume", runmode.LocalDefaultUserID, "pick it back up"); err != nil {
		t.Fatalf("follow-up after the stop: %v", err)
	}

	rows, err := s.agentRuns.ListForAssemblySystem(context.Background(), runmode.LocalDefaultOrgID, "r-resume")
	if err != nil {
		t.Fatalf("list for assembly: %v", err)
	}
	noteIdx, followUpIdx := -1, -1
	for i, r := range rows {
		switch {
		case r.Subtype == domain.MessageSubtypeStopNote:
			noteIdx = i
		case r.Content == "pick it back up":
			followUpIdx = i
		}
	}
	if noteIdx < 0 {
		t.Fatalf("the assembled window carries no stop note: %+v", rows)
	}
	if followUpIdx < 0 {
		t.Fatalf("the assembled window carries no follow-up: %+v", rows)
	}
	if noteIdx > followUpIdx {
		t.Errorf("the stop note assembles after the follow-up (%d > %d); it explains the gap the follow-up lands in", noteIdx, followUpIdx)
	}
}
