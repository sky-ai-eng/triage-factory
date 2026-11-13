package delegate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// twoConversationTask is a GitHub task with two conversations, run-1 and
// run-2, over the real stores, and a spawner holding the reads the turn-end
// nudge and the task context make.
type twoConversationTask struct {
	database *sql.DB
	stores   db.Stores
	spawner  *Spawner
	taskID   string
}

func newTwoConversationTask(t *testing.T) twoConversationTask {
	t.Helper()
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-1", "", "/tmp/wt-run-1")
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id = 'run-1'`).Scan(&taskID); err != nil {
		t.Fatalf("read run-1's task: %v", err)
	}
	stepIdx := 0
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID:                 "run-2",
		TaskID:             taskID,
		PromptID:           "test-prompt",
		Status:             "running",
		Model:              "claude-sonnet-4-6",
		WorktreePath:       "/tmp/wt-run-2",
		BlueprintRunID:     seedConversationBlueprint(t, database, "run-2", taskID),
		BlueprintStepIndex: &stepIdx,
	})
	stores := sqlitestore.New(database)
	registerOctoRepo(t, stores)
	return twoConversationTask{
		database: database,
		stores:   stores,
		spawner: &Spawner{
			conversations: stores.Conversations,
			artifacts:     stores.Artifacts,
			deliverables:  stores.Deliverables,
			tasks:         stores.Tasks,
		},
		taskID: taskID,
	}
}

// push records an accepted push of ref to octo/repo by conversationID, through
// the git proxy's recorder: its branch_pushed ledger row.
func (f twoConversationTask) push(t *testing.T, conversationID, ref, sha string) {
	t.Helper()
	info := recorderInfo(conversationID)
	gitPushRecorder(agenthost.NewLocal(f.stores, info))(context.Background(), gitproxy.PushedRef{
		Repo:         "octo/repo",
		RepositoryID: "1",
		Ref:          ref,
		NewSHA:       sha,
		Status:       200,
	})
}

// stageReview records a staged review of octo/repo#7 by conversationID: a
// pending review deliverable, with nothing on GitHub and so no ledger row.
func (f twoConversationTask) stageReview(t *testing.T, conversationID string) {
	t.Helper()
	d := domain.NewReviewDeliverable("octo/repo", 7, "PR_kwDOnudge7", "head", conversationID)
	d.ConversationID = conversationID
	d.OrgID = runmode.LocalDefaultOrgID
	d.TeamID = runmode.LocalDefaultTeamID
	d = dbtest.InOrgScope(t, f.stores.Orgs, runmode.LocalDefaultOrgID, d)
	if _, err := f.stores.Deliverables.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, d); err != nil {
		t.Fatalf("stage review: %v", err)
	}
}

// nudge asks run-2's turn-end check whether it would nudge a GitHub task's
// terminal step.
func (f twoConversationTask) nudge(t *testing.T) string {
	t.Helper()
	check := f.spawner.artifactContractNudge(runmode.LocalDefaultOrgID, "run-2", domain.Task{ID: f.taskID, EntitySource: "github"}, true)
	if check == nil {
		t.Fatal("a GitHub task's terminal step expects an artifact, so the check must be armed")
	}
	return check(context.Background(), 1, "done")
}

// TestArtifactContractNudge_APushToARecordedBranchIsOutput is the case the
// nudge got wrong: a CI-fix run pushes to a branch an earlier run already
// pushed. The branch's deliverable stays the earlier run's, so the later run
// has none, but the ledger records its push as its own artifact.
func TestArtifactContractNudge_APushToARecordedBranchIsOutput(t *testing.T) {
	f := newTwoConversationTask(t)
	f.push(t, "run-1", "refs/heads/fix", "sha-1")
	f.push(t, "run-2", "refs/heads/fix", "sha-2")

	deliverables, err := f.stores.Deliverables.ListByConversationSystem(context.Background(), runmode.LocalDefaultOrgID, "run-2")
	if err != nil {
		t.Fatalf("read run-2's deliverables: %v", err)
	}
	if len(deliverables) != 0 {
		t.Fatalf("run-2 has %d deliverables; the fixture needs the branch pinned to run-1", len(deliverables))
	}

	if got := f.nudge(t); got != "" {
		t.Errorf("a run that pushed was nudged as having produced nothing:\n%s", got)
	}
}

// TestArtifactContractNudge_AStagedReviewIsOutput: a staged review has no
// artifact until a person approves it and it is posted, and it is still the
// run's output.
func TestArtifactContractNudge_AStagedReviewIsOutput(t *testing.T) {
	f := newTwoConversationTask(t)
	f.stageReview(t, "run-2")

	if got := f.nudge(t); got != "" {
		t.Errorf("a run that staged a review was nudged as having produced nothing:\n%s", got)
	}
}

// TestArtifactContractNudge_NoOutputIsNudgedOnce: a run with neither an
// artifact nor a deliverable is asked once, and the nudge in its transcript
// answers the question until a person speaks again.
func TestArtifactContractNudge_NoOutputIsNudgedOnce(t *testing.T) {
	f := newTwoConversationTask(t)
	// Another conversation's output is not this one's.
	f.push(t, "run-1", "refs/heads/fix", "sha-1")
	f.stageReview(t, "run-1")

	insert := func(role, subtype, content string) {
		t.Helper()
		msg := &domain.Message{ConversationID: "run-2", Role: role, Subtype: subtype, Content: content}
		if _, err := f.stores.Conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, msg); err != nil {
			t.Fatalf("insert %s message: %v", role, err)
		}
	}
	insert("user", "", "fix the build")
	insert("assistant", "", "nothing needed changing")

	if got := f.nudge(t); got != artifactNudgeNote {
		t.Fatalf("a run that produced nothing was not nudged; got %q", got)
	}

	insert("user", domain.MessageSubtypeInjectionNudge, artifactNudgeNote)
	insert("assistant", "", "there was genuinely nothing to publish")
	if got := f.nudge(t); got != "" {
		t.Errorf("the nudge was put twice about the same silence:\n%s", got)
	}
}

// TestArtifactContractNudge_AClosedTaskIsNotNudged: a run that produced
// nothing is asked about it on an open task, and not once the task has
// closed under it, however it closed. The check reads the task at the turn's
// end, so a close during the engagement counts.
func TestArtifactContractNudge_AClosedTaskIsNotNudged(t *testing.T) {
	for _, status := range []string{"done", "dismissed"} {
		t.Run(status, func(t *testing.T) {
			f := newTwoConversationTask(t)
			ctx := context.Background()
			msg := &domain.Message{ConversationID: "run-2", Role: "user", Content: "fix the build"}
			if _, err := f.stores.Conversations.InsertMessageSystem(ctx, runmode.LocalDefaultOrgID, msg); err != nil {
				t.Fatalf("insert the opening message: %v", err)
			}
			if got := f.nudge(t); got != artifactNudgeNote {
				t.Fatalf("setup: the open task's run was not nudged; got %q", got)
			}
			version := dbtest.TaskVersion(t, f.stores.Tasks, runmode.LocalDefaultOrgID, f.taskID)
			if _, err := f.stores.Tasks.CloseByUser(ctx, runmode.LocalDefaultOrgID, f.taskID, status, version); err != nil {
				t.Fatalf("close the task: %v", err)
			}
			if got := f.nudge(t); got != "" {
				t.Errorf("the run on a closed task was nudged:\n%s", got)
			}
		})
	}
}

// TestArtifactContractNudge_ATaskWhoseCloseIsHeldIsNotNudged: a close held
// for a person leaves the task open with its situation resolved, and the run
// was told so. A run that then stops without an artifact is not asked for
// one.
func TestArtifactContractNudge_ATaskWhoseCloseIsHeldIsNotNudged(t *testing.T) {
	f := newTwoConversationTask(t)
	ctx := context.Background()
	msg := &domain.Message{ConversationID: "run-2", Role: "user", Content: "fix the build"}
	if _, err := f.stores.Conversations.InsertMessageSystem(ctx, runmode.LocalDefaultOrgID, msg); err != nil {
		t.Fatalf("insert the opening message: %v", err)
	}
	if got := f.nudge(t); got != artifactNudgeNote {
		t.Fatalf("setup: the open task's run was not nudged; got %q", got)
	}
	if _, err := f.database.Exec(`
		UPDATE tasks SET close_deferred_event_id = primary_event_id, close_deferred_at = CURRENT_TIMESTAMP
		 WHERE id = ?`, f.taskID); err != nil {
		t.Fatalf("hold the task's close: %v", err)
	}
	if hold, err := f.stores.Tasks.CloseDeferralForTaskSystem(ctx, runmode.LocalDefaultOrgID, f.taskID); err != nil || hold == nil {
		t.Fatalf("setup: the task holds no close: (%+v, %v)", hold, err)
	}
	if got := f.nudge(t); got != "" {
		t.Errorf("the run on a task whose close is held was nudged:\n%s", got)
	}
}
