package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

func coherenceFixture(t *testing.T, eventType string, metadata any) (*Spawner, string, string, domain.Event) {
	t.Helper()
	database := newDelegateTestDB(t)
	conversationID := "coherence-source"
	seedConversation(t, database, conversationID, "sess", "/tmp/coherence")
	setConversationStatus(t, database, conversationID, "open")
	var taskID, entityID string
	if err := database.QueryRow(`
		SELECT r.task_id, t.entity_id FROM conversations r JOIN tasks t ON t.id = r.task_id WHERE r.id = ?
	`, conversationID).Scan(&taskID, &entityID); err != nil {
		t.Fatalf("load fixture task/entity: %v", err)
	}
	snapshot, _ := json.Marshal(domain.PRSnapshot{Repo: "o/r", Number: 7, HeadRepo: "o/r", HeadRef: "feature/coherence", HeadSHA: "new-head"})
	if _, err := database.Exec(`UPDATE entities SET source_id = 'o/r#7', snapshot_json = ? WHERE id = ?`, string(snapshot), entityID); err != nil {
		t.Fatalf("update PR snapshot: %v", err)
	}
	raw, _ := json.Marshal(metadata)
	stores := testSpawnerStores(database)
	eventID, err := stores.Events.RecordSystem(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: eventType, EntityID: &entityID, MetadataJSON: string(raw),
	})
	if err != nil {
		t.Fatalf("record source event: %v", err)
	}
	s := NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")
	return s, conversationID, taskID, domain.Event{ID: eventID, OrgID: runmode.LocalDefaultOrgID, EventType: eventType, EntityID: &entityID, MetadataJSON: string(raw)}
}

// seedPendingReview upserts a pending review artifact anchored to repo#pr for
// conversationID, with the given start-review head and (optional) reconciled
// comment SHAs.
func seedPendingReview(t *testing.T, s *Spawner, conversationID, repo string, pr int, headSHA string, commentSHAs ...string) {
	t.Helper()
	a := domain.NewReviewArtifact(repo, pr, headSHA, conversationID)
	if len(commentSHAs) > 0 {
		d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
		for _, sha := range commentSHAs {
			d.StagedComments = append(d.StagedComments, domain.ReviewArtifactComment{Path: "a.go", Body: "x", CommitSHA: sha})
		}
		a.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	}
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	if _, err := s.artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a); err != nil {
		t.Fatalf("seed pending review: %v", err)
	}
}

func flushOne(t *testing.T, s *Spawner, conversationID string) []domain.StagedInjection {
	t.Helper()
	injections, err := s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	return injections
}

func coherenceDisposition(t *testing.T, source domain.Event, injectedConversationID string) domain.Event {
	t.Helper()
	raw, err := json.Marshal(events.SystemRoutingDispositionMetadata{
		EventID: source.ID, EventType: source.EventType, EntityID: *source.EntityID,
		Disposition: events.DispositionTaskBumped, InjectedConversationID: injectedConversationID,
	})
	if err != nil {
		t.Fatalf("marshal disposition: %v", err)
	}
	return domain.Event{OrgID: source.OrgID, EventType: domain.EventSystemRoutingDisposition, MetadataJSON: string(raw)}
}

func TestHandlePRCoherence_ParkedEntityConversationStagesAllEventKinds(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		metadata  any
		want      string
	}{
		{"new commits", domain.EventGitHubPRNewCommits, events.GitHubPRNewCommitsMetadata{Repo: "o/r", PRNumber: 7, PrevHeadSHA: "old-head", HeadSHA: "new-head"}, "advanced"},
		{"CI failed", domain.EventGitHubPRCICheckFailed, events.GitHubPRCICheckFailedMetadata{Repo: "o/r", PRNumber: 7, HeadSHA: "new-head", CheckName: "test", CheckURL: "https://ci.example/test"}, "failed"},
		{"CI passed", domain.EventGitHubPRCICheckPassed, events.GitHubPRCICheckPassedMetadata{Repo: "o/r", PRNumber: 7, HeadSHA: "new-head", CheckName: "test", Conclusion: "success"}, "success"},
		{"conflicts", domain.EventGitHubPRConflicts, events.GitHubPRConflictsMetadata{Repo: "o/r", PRNumber: 7, HeadSHA: "new-head"}, "CONFLICTING"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, conversationID, _, source := coherenceFixture(t, tt.eventType, tt.metadata)
			s.HandlePRCoherence(coherenceDisposition(t, source, ""))
			injections := flushOne(t, s, conversationID)
			if len(injections) != 1 {
				t.Fatalf("want one staged injection, got %d", len(injections))
			}
			if injections[0].Producer != domain.StagedInjectionProducerPRCoherence || !strings.Contains(injections[0].Body, tt.want) {
				t.Errorf("unexpected coherence note: %+v", injections[0])
			}
		})
	}
}

func TestHandlePRCoherence_LiveCIFixReceivesNewCommits(t *testing.T) {
	s, conversationID, taskID, source := coherenceFixture(t, domain.EventGitHubPRNewCommits, events.GitHubPRNewCommitsMetadata{
		Repo: "o/r", PRNumber: 7, PrevHeadSHA: "old-head", HeadSHA: "new-head",
	})
	controller := &fakeController{}
	s.controller = controller
	s.registerProc(runmode.LocalDefaultOrgID, conversationID, &agentproc.LiveRun{})

	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	waitSteerCalls(t, controller, 1)
	controller.mu.Lock()
	steer := controller.steerText
	controller.mu.Unlock()
	if !strings.Contains(steer, "advanced") || !strings.Contains(steer, "Re-pull") {
		t.Errorf("live coherence note is not actionable: %q", steer)
	}
	if got := flushOne(t, s, conversationID); len(got) != 0 {
		t.Fatalf("live delivery also staged a note: %+v", got)
	}
	var n int
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND event_id = ? AND kind = 'injected'`, taskID, source.ID).Scan(&n); err != nil {
		t.Fatalf("count task_events: %v", err)
	}
	if n != 0 {
		t.Fatalf("informational live delivery marked task_events injected: %d", n)
	}
}

func TestHandlePRCoherence_MaterializedHeadBranchTargetsOtherEntity(t *testing.T) {
	s, _, _, source := coherenceFixture(t, domain.EventGitHubPRCICheckFailed, events.GitHubPRCICheckFailedMetadata{
		Repo: "o/r", PRNumber: 7, HeadSHA: "new-head", CheckName: "test",
	})
	database := s.database
	snapshot, _ := json.Marshal(domain.PRSnapshot{Repo: "o/r", Number: 7, HeadRepo: "fork/r", HeadRef: "feature/coherence", HeadSHA: "new-head"})
	if _, err := database.Exec(`UPDATE entities SET snapshot_json = ? WHERE id = ?`, string(snapshot), *source.EntityID); err != nil {
		t.Fatalf("update fork PR snapshot: %v", err)
	}
	seedConversation(t, database, "branch-worker", "sess", "/tmp/branch-worker")
	setConversationStatus(t, database, "branch-worker", "open")
	stores := testSpawnerStores(database)
	if _, err := stores.Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.Repository{Owner: "fork", Repo: "r", CloneURL: "https://example.com/fork/r.git"}); err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	if _, _, err := stores.ConversationWorktrees.InsertSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ConversationWorktree{
		ConversationID: "branch-worker", RepoID: "fork/r", Path: "/tmp/branch-worker/fork/r", Ref: worktree.CheckoutRefSlug("feature/coherence"),
	}); err != nil {
		t.Fatalf("seed branch worktree: %v", err)
	}

	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	if got := flushOne(t, s, "branch-worker"); len(got) != 1 || !strings.Contains(got[0].Body, "CI check") {
		t.Fatalf("materialized branch did not receive coherence note: %+v", got)
	}
}

func TestHandlePRCoherence_PendingReviewAndPRCheckoutTargetOtherEntities(t *testing.T) {
	s, _, _, source := coherenceFixture(t, domain.EventGitHubPRConflicts, events.GitHubPRConflictsMetadata{
		Repo: "o/r", PRNumber: 7, HeadSHA: "new-head",
	})
	database := s.database
	stores := testSpawnerStores(database)

	seedConversation(t, database, "review-worker", "sess", "/tmp/review-worker")
	setConversationStatus(t, database, "review-worker", "open")
	seedPendingReview(t, s, "review-worker", "o/r", 7, "old-head")

	seedConversation(t, database, "pr-worker", "sess", "/tmp/pr-worker")
	setConversationStatus(t, database, "pr-worker", "open")
	if _, err := stores.Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.Repository{Owner: "o", Repo: "r", CloneURL: "https://example.com/o/r.git"}); err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	if _, _, err := stores.ConversationWorktrees.InsertSystem(context.Background(), runmode.LocalDefaultOrgID, domain.ConversationWorktree{
		ConversationID: "pr-worker", RepoID: "o/r", Path: "/tmp/pr-worker/o/r", Ref: worktree.PRRefSlug(7),
	}); err != nil {
		t.Fatalf("seed PR worktree: %v", err)
	}

	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	for _, conversationID := range []string{"review-worker", "pr-worker"} {
		if got := flushOne(t, s, conversationID); len(got) != 1 || !strings.Contains(got[0].Body, "CONFLICTING") {
			t.Errorf("%s did not receive coherence note: %+v", conversationID, got)
		}
	}
}

func TestHandlePRCoherence_SkipsTerminalAndSameTaskDuplicate(t *testing.T) {
	s, conversationID, taskID, source := coherenceFixture(t, domain.EventGitHubPRNewCommits, events.GitHubPRNewCommitsMetadata{
		Repo: "o/r", PRNumber: 7, PrevHeadSHA: "old-head", HeadSHA: "new-head",
	})

	// The routing disposition names the exact same-task additive recipient.
	s.HandlePRCoherence(coherenceDisposition(t, source, conversationID))
	if got := flushOne(t, s, conversationID); len(got) != 0 {
		t.Fatalf("same-task additive recipient received a duplicate: %+v", got)
	}

	// The durable task_events guard covers a settled additive injection even
	// when a replayed disposition no longer carries its in-memory identity.
	if _, err := s.database.Exec(`INSERT OR REPLACE INTO task_events (task_id, event_id, kind) VALUES (?, ?, 'injected')`, taskID, source.ID); err != nil {
		t.Fatalf("mark same-task event injected: %v", err)
	}
	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	if got := flushOne(t, s, conversationID); len(got) != 0 {
		t.Fatalf("task_events injected guard missed duplicate: %+v", got)
	}

	if _, err := s.database.Exec(`DELETE FROM task_events WHERE task_id = ? AND event_id = ?`, taskID, source.ID); err != nil {
		t.Fatalf("clear injected marker: %v", err)
	}
	if _, err := s.conversations.CompleteSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID, "completed", 0, 0, 0, "", string(domain.ConversationOutcomeFinish), "", ""); err != nil {
		t.Fatalf("complete conversation: %v", err)
	}
	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	if got := flushOne(t, s, conversationID); len(got) != 0 {
		t.Fatalf("terminal non-resumable conversation accumulated a note: %+v", got)
	}
}

func TestHandlePRCoherence_DoesNotWriteTaskEvents(t *testing.T) {
	s, conversationID, taskID, source := coherenceFixture(t, domain.EventGitHubPRConflicts, events.GitHubPRConflictsMetadata{
		Repo: "o/r", PRNumber: 7, HeadSHA: "new-head",
	})
	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	if got := flushOne(t, s, conversationID); len(got) != 1 {
		t.Fatalf("expected coherence note, got %+v", got)
	}
	var n int
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND event_id = ? AND kind = 'injected'`, taskID, source.ID).Scan(&n); err != nil {
		t.Fatalf("count task_events: %v", err)
	}
	if n != 0 {
		t.Fatalf("coherence subscriber marked task_events injected: %d", n)
	}
}

// TestHandlePRCoherence_AnchoredReviewSkipsNewCommits: a review draft that
// already recorded the new head is not behind it, so the advance is not news.
func TestHandlePRCoherence_AnchoredReviewSkipsNewCommits(t *testing.T) {
	s, _, _, source := coherenceFixture(t, domain.EventGitHubPRNewCommits, events.GitHubPRNewCommitsMetadata{
		Repo: "o/r", PRNumber: 7, PrevHeadSHA: "old-head", HeadSHA: "new-head",
	})
	database := s.database
	seedConversation(t, database, "anchored-review", "sess", "/tmp/anchored-review")
	setConversationStatus(t, database, "anchored-review", "open")
	seedPendingReview(t, s, "anchored-review", "o/r", 7, "new-head")

	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	if got := flushOne(t, s, "anchored-review"); len(got) != 0 {
		t.Fatalf("review already at the new head still received a note: %+v", got)
	}
}

// TestHandlePRCoherence_BehindReviewReceivesNewCommits is the same shape with
// the review anchored at the old head — the case the gate must not swallow.
func TestHandlePRCoherence_BehindReviewReceivesNewCommits(t *testing.T) {
	s, _, _, source := coherenceFixture(t, domain.EventGitHubPRNewCommits, events.GitHubPRNewCommitsMetadata{
		Repo: "o/r", PRNumber: 7, PrevHeadSHA: "old-head", HeadSHA: "new-head",
	})
	database := s.database
	seedConversation(t, database, "behind-review", "sess", "/tmp/behind-review")
	setConversationStatus(t, database, "behind-review", "open")
	seedPendingReview(t, s, "behind-review", "o/r", 7, "old-head")

	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	if got := flushOne(t, s, "behind-review"); len(got) != 1 || !strings.Contains(got[0].Body, "advanced") {
		t.Fatalf("review behind the new head did not receive a note: %+v", got)
	}
}

// TestHandlePRCoherence_AnchoredReviewStillReceivesCIFailure: the freshness
// gate is specific to a head advance. A CI result is news whatever the review
// is anchored at.
func TestHandlePRCoherence_AnchoredReviewStillReceivesCIFailure(t *testing.T) {
	s, _, _, source := coherenceFixture(t, domain.EventGitHubPRCICheckFailed, events.GitHubPRCICheckFailedMetadata{
		Repo: "o/r", PRNumber: 7, HeadSHA: "new-head", CheckName: "test",
	})
	database := s.database
	seedConversation(t, database, "anchored-ci", "sess", "/tmp/anchored-ci")
	setConversationStatus(t, database, "anchored-ci", "open")
	seedPendingReview(t, s, "anchored-ci", "o/r", 7, "new-head")

	s.HandlePRCoherence(coherenceDisposition(t, source, ""))
	if got := flushOne(t, s, "anchored-ci"); len(got) != 1 || !strings.Contains(got[0].Body, "CI check") {
		t.Fatalf("anchored review did not receive the CI note: %+v", got)
	}
}

// TestHandlePRCoherence_IncompleteMetadataDeliversNothing: stored event
// metadata missing the field the note is built from produces no note at all,
// rather than one whose subject renders blank.
func TestHandlePRCoherence_IncompleteMetadataDeliversNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		eventType string
		metadata  any
	}{
		{"NewCommitsWithoutHead", domain.EventGitHubPRNewCommits, events.GitHubPRNewCommitsMetadata{
			Repo: "o/r", PRNumber: 7, PrevHeadSHA: "old-head",
		}},
		{"CIFailedWithoutCheckName", domain.EventGitHubPRCICheckFailed, events.GitHubPRCICheckFailedMetadata{
			Repo: "o/r", PRNumber: 7, HeadSHA: "new-head",
		}},
		{"CIPassedWithoutConclusion", domain.EventGitHubPRCICheckPassed, events.GitHubPRCICheckPassedMetadata{
			Repo: "o/r", PRNumber: 7, HeadSHA: "new-head", CheckName: "test",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, conversationID, _, source := coherenceFixture(t, tc.eventType, tc.metadata)
			s.HandlePRCoherence(coherenceDisposition(t, source, ""))
			if got := flushOne(t, s, conversationID); len(got) != 0 {
				t.Fatalf("incomplete metadata still produced a note: %+v", got)
			}
		})
	}
}

// TestHandlePRCoherence_MissingQualifiersDropTheirClause: a field that only
// qualifies the note is omitted rather than rendered blank, because the news
// itself is still real.
func TestHandlePRCoherence_MissingQualifiersDropTheirClause(t *testing.T) {
	t.Run("ConflictsWithoutHead", func(t *testing.T) {
		s, conversationID, _, source := coherenceFixture(t, domain.EventGitHubPRConflicts, events.GitHubPRConflictsMetadata{
			Repo: "o/r", PRNumber: 7,
		})
		s.HandlePRCoherence(coherenceDisposition(t, source, ""))
		got := flushOne(t, s, conversationID)
		if len(got) != 1 {
			t.Fatalf("conflicts note not delivered: %+v", got)
		}
		if !strings.Contains(got[0].Body, "CONFLICTING merge state.") || strings.Contains(got[0].Body, " at ") {
			t.Errorf("conflicts note rendered a blank head clause: %q", got[0].Body)
		}
	})

	t.Run("NewCommitsWithoutPrevHead", func(t *testing.T) {
		s, conversationID, _, source := coherenceFixture(t, domain.EventGitHubPRNewCommits, events.GitHubPRNewCommitsMetadata{
			Repo: "o/r", PRNumber: 7, HeadSHA: "new-head",
		})
		s.HandlePRCoherence(coherenceDisposition(t, source, ""))
		got := flushOne(t, s, conversationID)
		if len(got) != 1 {
			t.Fatalf("new-commits note not delivered: %+v", got)
		}
		if !strings.Contains(got[0].Body, "advanced to new-head") || strings.Contains(got[0].Body, "from  to") {
			t.Errorf("new-commits note rendered a blank from clause: %q", got[0].Body)
		}
	})
}

// TestHandlePRCoherence_RecordsProvenance: the note's message row names the
// event it was composed from, on both the staged and the live path, so a
// later reader can resolve a note back to its cause without parsing the body.
func TestHandlePRCoherence_RecordsProvenance(t *testing.T) {
	t.Run("Staged", func(t *testing.T) {
		s, conversationID, _, source := coherenceFixture(t, domain.EventGitHubPRConflicts, events.GitHubPRConflictsMetadata{
			Repo: "o/r", PRNumber: 7, HeadSHA: "new-head",
		})
		s.HandlePRCoherence(coherenceDisposition(t, source, ""))
		got := flushOne(t, s, conversationID)
		if len(got) != 1 {
			t.Fatalf("note not staged: %+v", got)
		}
		if got[0].Provenance.EventID != source.ID || got[0].Provenance.EventType != domain.EventGitHubPRConflicts {
			t.Errorf("staged provenance = %+v, want event %s / %s",
				got[0].Provenance, source.ID, domain.EventGitHubPRConflicts)
		}
		if got[0].Producer != domain.StagedInjectionProducerPRCoherence {
			t.Errorf("staged producer = %q", got[0].Producer)
		}
	})

	t.Run("Live", func(t *testing.T) {
		s, conversationID, _, source := coherenceFixture(t, domain.EventGitHubPRConflicts, events.GitHubPRConflictsMetadata{
			Repo: "o/r", PRNumber: 7, HeadSHA: "new-head",
		})
		s.controller = &fakeController{}
		s.registerProc(runmode.LocalDefaultOrgID, conversationID, &agentproc.LiveRun{})

		s.HandlePRCoherence(coherenceDisposition(t, source, ""))
		waitSteerCalls(t, s.controller.(*fakeController), 1)

		metadata := waitForNoteMetadata(t, s, conversationID)
		if metadata["event_id"] != source.ID || metadata["event_type"] != domain.EventGitHubPRConflicts {
			t.Errorf("live provenance = %+v, want event %s / %s", metadata, source.ID, domain.EventGitHubPRConflicts)
		}
		if metadata["producer"] != domain.StagedInjectionProducerPRCoherence {
			t.Errorf("live producer = %v", metadata["producer"])
		}
	})
}

// waitForNoteMetadata reads back the metadata of the live-injected system_note
// row. The live path records it from a goroutine, so this polls rather than
// assuming the write has landed.
func waitForNoteMetadata(t *testing.T, s *Spawner, conversationID string) map[string]any {
	t.Helper()
	for range 100 {
		var raw sql.NullString
		err := s.database.QueryRow(`
			SELECT metadata FROM messages
			WHERE conversation_id = ? AND subtype = 'system_note'
			ORDER BY id DESC LIMIT 1
		`, conversationID).Scan(&raw)
		if err == nil && raw.String != "" {
			var out map[string]any
			if json.Unmarshal([]byte(raw.String), &out) == nil {
				return out
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("live system_note metadata never landed")
	return nil
}
