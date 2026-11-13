package delegate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// waitStagedNotes polls until convID has at least want staged system notes,
// or fails, and returns their contents. A request handler's stage runs on a
// detached goroutine (InjectDeliverableNote), so the side effect is asserted
// by polling rather than a synchronous read.
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

// resolution is one way a deliverable is resolved: the row as the resolution
// stored it, and the producer that tells the conversation.
type resolution struct {
	name string
	row  func(conversationID string) domain.Deliverable
	tell func(s *Spawner, d domain.Deliverable)
}

// resolutions are the four ways a conversation's deliverable is resolved, each
// told through the producer that resolution really runs: a person's approve
// or dismiss through the request handlers' detached stage, and a review a
// pull request's end dismissed through the close's follow-up.
func resolutions() []resolution {
	pullRequest := func(state, res string) func(string) domain.Deliverable {
		return func(conversationID string) domain.Deliverable {
			return domain.Deliverable{
				ID: "pr-" + conversationID, ConversationID: conversationID,
				Kind: domain.DeliverableKindPullRequest, State: state, Target: "o/r#7",
				DetailsJSON: domain.MarshalPRDeliverableDetails(domain.PRDeliverableDetails{HeadBranch: "tf/fix", Base: "main", Resolution: res}),
			}
		}
	}
	review := func(res string) func(string) domain.Deliverable {
		return func(conversationID string) domain.Deliverable {
			return domain.Deliverable{
				ID: "review-" + conversationID, ConversationID: conversationID,
				Kind: domain.DeliverableKindReview, State: domain.DeliverableStateReviewDismissed, Target: "o/r#7",
				DetailsJSON: domain.MarshalReviewDeliverableDetails(domain.ReviewDeliverableDetails{ReviewEvent: "COMMENT", Resolution: res}),
			}
		}
	}
	person := func(s *Spawner, d domain.Deliverable) { s.InjectDeliverableNote(runmode.LocalDefaultOrgID, d) }
	closed := func(s *Spawner, d domain.Deliverable) {
		s.ReviewsDismissedSystem(context.Background(), runmode.LocalDefaultOrgID, []domain.Deliverable{d})
	}
	return []resolution{
		{"a person's approve", pullRequest(domain.DeliverableStatePROpen, domain.PRResolutionApproved), person},
		{"a person's dismiss", pullRequest(domain.DeliverableStatePRClosed, domain.PRResolutionDismissed), person},
		{"a review dismissed by a merge", review(domain.ReviewResolutionPullRequestMerged), closed},
		{"a review dismissed by a close", review(domain.ReviewResolutionPullRequestClosed), closed},
	}
}

// reader is one way a conversation comes to read its staged notes: the
// runtime and state it is in when the note is staged, and the read that
// follows, which returns what the agent was handed.
type reader struct {
	name  string
	setup func(t *testing.T, s *Spawner, database *sql.DB, conversationID string)
	read  func(t *testing.T, s *Spawner, database *sql.DB, conversationID string) string
}

// nativeRead is a native loop's drain: every pending row in the assembly
// window is marked delivered, and what it read goes into the next request.
func nativeRead(t *testing.T, s *Spawner, database *sql.DB, conversationID string) string {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	var claimID string
	if err := database.QueryRow(`SELECT id FROM claims WHERE conversation_id = ? AND released_at IS NULL`, conversationID).Scan(&claimID); err != nil {
		claimID = markEngaged(t, database, conversationID)
	}
	tr := newNativeTranscript(s, org, conversationID, claimID)
	rows, err := tr.ListForAssembly(ctx, org, conversationID)
	if err != nil {
		t.Fatalf("list for assembly: %v", err)
	}
	var ids []int
	var read []string
	for _, r := range rows {
		if pendingRow(r) {
			ids = append(ids, r.ID)
			read = append(read, r.Content)
		}
	}
	if len(ids) > 0 {
		if err := tr.MarkDelivered(ctx, org, conversationID, ids, ""); err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
	}
	return strings.Join(read, "\n")
}

func readers() []reader {
	return []reader{
		{
			name: "a working native conversation, at its next model call",
			setup: func(t *testing.T, _ *Spawner, database *sql.DB, conversationID string) {
				markEngaged(t, database, conversationID)
				markNative(t, database, conversationID)
			},
			read: nativeRead,
		},
		{
			name: "a parked native conversation, at its next engagement",
			setup: func(t *testing.T, _ *Spawner, database *sql.DB, conversationID string) {
				setConversationStatus(t, database, conversationID, domain.StatusOpen)
				markNative(t, database, conversationID)
			},
			read: nativeRead,
		},
		{
			name: "a live SDK process, at its next tool batch",
			setup: func(_ *testing.T, s *Spawner, _ *sql.DB, conversationID string) {
				s.registerProc(runmode.LocalDefaultOrgID, conversationID, &agentproc.LiveRun{})
				s.getProc(conversationID).boundary.take() // armed from registration
			},
			read: func(_ *testing.T, s *Spawner, _ *sql.DB, conversationID string) string {
				// The boundary is armed after the row commits, on the stager's
				// goroutine, so the first batch may come a moment early.
				deadline := time.Now().Add(2 * time.Second)
				for {
					if text := s.notesAtToolBatch(runmode.LocalDefaultOrgID, conversationID, "").Text; text != "" || time.Now().After(deadline) {
						return text
					}
					time.Sleep(5 * time.Millisecond)
				}
			},
		},
		{
			name: "a parked SDK conversation, at its next resume",
			setup: func(t *testing.T, _ *Spawner, database *sql.DB, conversationID string) {
				setConversationStatus(t, database, conversationID, domain.StatusOpen)
			},
			read: func(_ *testing.T, s *Spawner, _ *sql.DB, conversationID string) string {
				block, _ := s.resumeSystemPrepends(context.Background(), runmode.LocalDefaultOrgID, conversationID)
				return block
			},
		},
	}
}

// TestDeliverableResolved_EveryConversationIsToldOnce: however a deliverable
// is resolved, and whatever runtime and state its conversation is in, exactly
// one note is staged, the conversation's next read hands it to the agent
// once, and the read after that hands it nothing.
func TestDeliverableResolved_EveryConversationIsToldOnce(t *testing.T) {
	for _, res := range resolutions() {
		for _, rd := range readers() {
			t.Run(res.name+" reaches "+rd.name, func(t *testing.T) {
				const conversationID = "r-resolved"
				database := newDelegateTestDB(t)
				seedConversation(t, database, conversationID, "sess", "/tmp/wt")
				s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
				rd.setup(t, s, database, conversationID)

				d := res.row(conversationID)
				want := domain.DeliverableResolutionNote(d)
				if want == "" {
					t.Fatal("the fixture is not a resolution a note reports")
				}
				res.tell(s, d)

				if staged := waitStagedNotes(t, database, conversationID, 1); len(staged) != 1 || staged[0] != want {
					t.Fatalf("staged notes = %q, want exactly the one resolution note", staged)
				}
				if got := rd.read(t, s, database, conversationID); strings.Count(got, want) != 1 {
					t.Errorf("the read handed the agent %q, want the resolution note once", got)
				}
				if left := stagedNotes(t, database, conversationID); len(left) != 0 {
					t.Errorf("still staged after the read: %q", left)
				}
				if again := rd.read(t, s, database, conversationID); strings.Contains(again, want) {
					t.Errorf("a second read handed the agent the note again: %q", again)
				}
			})
		}
	}
}

// TestDeliverableResolved_AFinishedConversationKeepsItsNoteForAResume: a note
// is staged on a conversation that has finished too, where nothing reads it
// until a person resumes the conversation, and the row does not make the
// conversation look like it has input to run on.
func TestDeliverableResolved_AFinishedConversationKeepsItsNoteForAResume(t *testing.T) {
	const conversationID = "r-finished"
	database := newDelegateTestDB(t)
	seedConversation(t, database, conversationID, "sess", "/tmp/wt")
	setConversationStatus(t, database, conversationID, "completed")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	d := resolutions()[0].row(conversationID)
	s.TellDeliverableResolved(context.Background(), runmode.LocalDefaultOrgID, d)

	if staged := stagedNotes(t, database, conversationID); len(staged) != 1 {
		t.Fatalf("staged notes = %q, want the one note kept for a resume", staged)
	}
	rows := allRows(t, s, conversationID)
	for _, r := range rows {
		if waitingMessage(r) {
			t.Errorf("the note reads as a person's waiting input, which would wake the conversation: %+v", r)
		}
	}
}

// TestDeliverableResolved_NothingToReportStagesNothing: a deliverable that is
// not in a reported resolution state, or whose conversation was purged, gets
// no note.
func TestDeliverableResolved_NothingToReportStagesNothing(t *testing.T) {
	const conversationID = "r-unresolved"
	database := newDelegateTestDB(t)
	seedConversation(t, database, conversationID, "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	s.registerProc(runmode.LocalDefaultOrgID, conversationID, &agentproc.LiveRun{})

	draft := domain.Deliverable{ConversationID: conversationID, Kind: domain.DeliverableKindPullRequest, State: domain.DeliverableStatePRDraft, Target: "o/r#7"}
	purged := resolutions()[0].row("")
	for _, d := range []domain.Deliverable{draft, purged} {
		// Neither starts a goroutine, so there is nothing to wait for.
		s.InjectDeliverableNote(runmode.LocalDefaultOrgID, d)
		s.TellDeliverableResolved(context.Background(), runmode.LocalDefaultOrgID, d)
	}
	if notes := stagedNotes(t, database, conversationID); len(notes) != 0 {
		t.Errorf("staged notes = %q, want none", notes)
	}
}

// TestHandlePRCoherence_LiveSDKProcessReadsTheNoteAtItsNextToolBatch: a PR
// status note staged for a conversation with a live SDK process is handed to
// the agent at the process's next tool batch, once.
func TestHandlePRCoherence_LiveSDKProcessReadsTheNoteAtItsNextToolBatch(t *testing.T) {
	s, conversationID, _, source := coherenceFixture(t, domain.EventGitHubPRCICheckFailed, events.GitHubPRCICheckFailedMetadata{
		Repo: "o/r", PRNumber: 7, HeadSHA: "new-head", CheckName: "test",
	})
	org := runmode.LocalDefaultOrgID
	s.registerProc(org, conversationID, &agentproc.LiveRun{})
	s.getProc(conversationID).boundary.take() // armed from registration

	s.HandlePRCoherence(coherenceDisposition(t, source, ""))

	staged := stagedNotes(t, s.database, conversationID)
	if len(staged) != 1 {
		t.Fatalf("staged notes = %q, want the one PR status note", staged)
	}
	if got := s.notesAtToolBatch(org, conversationID, "").Text; strings.Count(got, staged[0]) != 1 {
		t.Errorf("the next tool batch handed over %q, want the PR status note once", got)
	}
	if again := s.notesAtToolBatch(org, conversationID, "").Text; again != "" {
		t.Errorf("the batch after it handed over %q, want nothing", again)
	}
}
