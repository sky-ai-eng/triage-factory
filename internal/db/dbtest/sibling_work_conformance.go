package dbtest

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SiblingWorkSeeder stages what the overlapping-work reads span, on the
// held-close suite's seeder: tasks and their conversations, plus the two
// things the relation reads that no other suite stages.
type SiblingWorkSeeder struct {
	TaskCloseDeferralSeeder

	// Touch records that the conversation touched the entity: a
	// conversation_memory_entities row.
	Touch func(t *testing.T, conversationID, entityID string)
	// MoveToOtherTeam moves the task to a team other than the one every
	// seeded conversation is on, and leaves the team it had in its
	// visibility set, as a claim that moves a task between teams does.
	MoveToOtherTeam func(t *testing.T, taskID string)
}

// SiblingWorkFactory hands the suite a fresh database per case, with the
// held-close suite's stores over it.
type SiblingWorkFactory func(t *testing.T) (stores TaskCloseDeferralStores, orgID string, seed SiblingWorkSeeder)

// RunSiblingWorkConformance is the shared suite for the work that overlaps a
// conversation: the relation between a task and a conversation, read from
// the task's side (TaskStore.SiblingWorkRecipientsSystem) and from the
// conversation's (TaskStore.SiblingWorkSystem), the assignment the note is
// composed from (TaskStore.AssignmentSystem), and the retire of assignment
// notes that stopped being true
// (StagedInjectionStore.RetireStaleAssignmentNotesSystem).
func RunSiblingWorkConformance(t *testing.T, mk SiblingWorkFactory) {
	t.Helper()
	ctx := context.Background()
	const review = domain.EventGitHubPRReviewChangesRequested

	claim := func(t *testing.T, stores TaskCloseDeferralStores, orgID, taskID string) {
		t.Helper()
		if ok, err := stores.Tasks.ClaimQueuedForUser(ctx, orgID, taskID, stores.UserID, TaskVersion(t, stores.Tasks, orgID, taskID)); err != nil || !ok {
			t.Fatalf("ClaimQueuedForUser(%s) = %v (err %v)", taskID, ok, err)
		}
	}
	closeTask := func(t *testing.T, stores TaskCloseDeferralStores, orgID, taskID string) {
		t.Helper()
		if _, err := stores.Tasks.CloseSystem(ctx, orgID, taskID, db.RunCompletedCloseReason, ""); err != nil {
			t.Fatalf("CloseSystem(%s): %v", taskID, err)
		}
	}

	t.Run("Recipients_are_the_live_conversations_the_task_relates_to", func(t *testing.T) {
		// Each case stages the assigned task, then a conversation on another
		// task, and says whether that conversation is told and whether its
		// listing names the task. The two agree for every live conversation.
		cases := []struct {
			name   string
			told   bool
			listed bool
			// stage returns the conversation under test and its task.
			stage func(t *testing.T, seed SiblingWorkSeeder, assigned string) (conversationID, taskID string)
		}{
			{name: "on the same entity", told: true, listed: true,
				stage: func(t *testing.T, seed SiblingWorkSeeder, assigned string) (string, string) {
					taskID := seed.TaskOnEntity(t, seed.EntityForTask(t, assigned), review)
					_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
					return convID, taskID
				}},
			{name: "touched the task's entity", told: true, listed: true,
				stage: func(t *testing.T, seed SiblingWorkSeeder, assigned string) (string, string) {
					taskID := seed.Task(t)
					_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
					seed.Touch(t, convID, seed.EntityForTask(t, assigned))
					return convID, taskID
				}},
			{name: "a live conversation on the task touched its task's entity", told: true, listed: true,
				stage: func(t *testing.T, seed SiblingWorkSeeder, assigned string) (string, string) {
					taskID := seed.Task(t)
					_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
					_, onAssigned := seed.BlueprintAndConversation(t, assigned, "running", "")
					seed.Touch(t, onAssigned, seed.EntityForTask(t, taskID))
					return convID, taskID
				}},
			{name: "a settled conversation on the task touched its task's entity",
				stage: func(t *testing.T, seed SiblingWorkSeeder, assigned string) (string, string) {
					taskID := seed.Task(t)
					_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
					_, onAssigned := seed.BlueprintAndConversation(t, assigned, "completed", SeedConcluded)
					seed.Touch(t, onAssigned, seed.EntityForTask(t, taskID))
					return convID, taskID
				}},
			{name: "one of the task's own conversations",
				stage: func(t *testing.T, seed SiblingWorkSeeder, assigned string) (string, string) {
					_, convID := seed.BlueprintAndConversation(t, assigned, "running", "")
					return convID, assigned
				}},
			{name: "settled", listed: true,
				stage: func(t *testing.T, seed SiblingWorkSeeder, assigned string) (string, string) {
					taskID := seed.TaskOnEntity(t, seed.EntityForTask(t, assigned), review)
					_, convID := seed.BlueprintAndConversation(t, taskID, "completed", SeedConcluded)
					return convID, taskID
				}},
			{name: "on a team the task's claim moved it away from",
				stage: func(t *testing.T, seed SiblingWorkSeeder, assigned string) (string, string) {
					taskID := seed.TaskOnEntity(t, seed.EntityForTask(t, assigned), review)
					_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
					seed.MoveToOtherTeam(t, assigned)
					return convID, taskID
				}},
			{name: "on an unrelated entity",
				stage: func(t *testing.T, seed SiblingWorkSeeder, assigned string) (string, string) {
					taskID := seed.Task(t)
					_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
					return convID, taskID
				}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stores, orgID, seed := mk(t)
				// The task has an assignee before anything is staged, so
				// nothing but the relation can leave it out of a listing,
				// and a move to another team is a held task's.
				assigned := seed.Task(t)
				claim(t, stores, orgID, assigned)
				convID, taskID := tc.stage(t, seed, assigned)

				got, err := stores.Tasks.SiblingWorkRecipientsSystem(ctx, orgID, assigned)
				if err != nil {
					t.Fatalf("SiblingWorkRecipientsSystem: %v", err)
				}
				i := slices.IndexFunc(got, func(r domain.SiblingWorkRecipient) bool { return r.ConversationID == convID })
				if told := i >= 0; told != tc.told {
					t.Fatalf("conversation told = %v, want %v (recipients %+v)", told, tc.told, got)
				}
				// The listing reads the relation from the conversation's
				// side. A settled conversation is no recipient and its
				// listing still names the task, which is what a wake reads.
				listed, err := stores.Tasks.SiblingWorkSystem(ctx, orgID, convID)
				if err != nil {
					t.Fatalf("SiblingWorkSystem: %v", err)
				}
				if has := slices.ContainsFunc(listed, func(s domain.SiblingTask) bool { return s.TaskID == assigned }); has != tc.listed {
					t.Errorf("listing names the task = %v, want %v (listing %+v)", has, tc.listed, listed)
				}
				if !tc.told {
					return
				}
				own, err := stores.Tasks.GetSystem(ctx, orgID, taskID)
				if err != nil || own == nil {
					t.Fatalf("GetSystem(%s) = %v (err %v)", taskID, own, err)
				}
				want := domain.SiblingWorkRecipient{ConversationID: convID, TaskID: own.ID, EntityID: own.EntityID, EventType: own.EventType, DedupKey: own.DedupKey}
				if got[i] != want {
					t.Errorf("recipient = %+v, want %+v", got[i], want)
				}
			})
		}
	})

	t.Run("Listing_is_the_related_tasks_someone_is_on", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		own := seed.Task(t)
		_, convID := seed.BlueprintAndConversation(t, own, "running", "")
		// related stages a task the conversation's work overlaps, by a touch
		// of its entity.
		related := func(t *testing.T) string {
			t.Helper()
			taskID := seed.Task(t)
			seed.Touch(t, convID, seed.EntityForTask(t, taskID))
			return taskID
		}

		openAssigned := related(t)
		claim(t, stores, orgID, openAssigned)

		closedWorking := related(t)
		seed.BlueprintAndConversation(t, closedWorking, "running", "")
		closeTask(t, stores, orgID, closedWorking)

		openUnassigned := related(t)

		closedIdle := related(t)
		seed.BlueprintAndConversation(t, closedIdle, "completed", SeedConcluded)
		closeTask(t, stores, orgID, closedIdle)

		// On the conversation's own entity, held by nobody, with work going.
		sameEntityWorking := seed.TaskOnEntity(t, seed.EntityForTask(t, own), review)
		seed.BlueprintAndConversation(t, sameEntityWorking, "running", "")

		unrelated := seed.Task(t)
		claim(t, stores, orgID, unrelated)

		got, err := stores.Tasks.SiblingWorkSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("SiblingWorkSystem: %v", err)
		}
		byID := map[string]domain.SiblingTask{}
		for _, task := range got {
			byID[task.TaskID] = task
		}
		for name, omitted := range map[string]string{
			"its own task": own, "an open task with no assignee": openUnassigned,
			"a closed task whose only conversation is idle": closedIdle, "an unrelated task": unrelated,
		} {
			if _, ok := byID[omitted]; ok {
				t.Errorf("the listing names %s", name)
			}
		}
		for _, want := range []struct {
			name string
			id   string
			task domain.SiblingTask
		}{
			{"an open task with an assignee", openAssigned, domain.SiblingTask{Holder: domain.SiblingTaskHeldByPerson}},
			{"a closed task with a working conversation", closedWorking, domain.SiblingTask{Closed: true, Working: true}},
			{"a task on the same entity", sameEntityWorking, domain.SiblingTask{Working: true}},
		} {
			task, ok := byID[want.id]
			if !ok {
				t.Errorf("the listing omits %s", want.name)
				continue
			}
			if task.Closed != want.task.Closed || task.Holder != want.task.Holder || task.Working != want.task.Working {
				t.Errorf("%s listed as closed=%v holder=%v working=%v, want %v %v %v", want.name,
					task.Closed, task.Holder, task.Working, want.task.Closed, want.task.Holder, want.task.Working)
			}
			stored, err := stores.Tasks.GetSystem(ctx, orgID, want.id)
			if err != nil || stored == nil {
				t.Fatalf("GetSystem(%s) = %v (err %v)", want.id, stored, err)
			}
			if task.EventType != stored.EventType || task.EntitySourceID != stored.EntitySourceID {
				t.Errorf("%s listed as %q on %q, want %q on %q", want.name, task.EventType, task.EntitySourceID, stored.EventType, stored.EntitySourceID)
			}
			if (task.TeamName == "") != (stored.TeamID == nil) {
				t.Errorf("%s listed with team %q, stored team %v", want.name, task.TeamName, stored.TeamID)
			}
		}
		if len(got) != 3 {
			t.Errorf("the listing has %d tasks, want 3: %+v", len(got), got)
		}
	})

	t.Run("Assignment_is_an_open_task_and_its_holder", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		if a, err := stores.Tasks.AssignmentSystem(ctx, orgID, taskID); err != nil || a != nil {
			t.Fatalf("AssignmentSystem of an unheld task = %+v (err %v), want none", a, err)
		}
		claim(t, stores, orgID, taskID)
		stored, err := stores.Tasks.GetSystem(ctx, orgID, taskID)
		if err != nil || stored == nil {
			t.Fatalf("GetSystem = %v (err %v)", stored, err)
		}
		a, err := stores.Tasks.AssignmentSystem(ctx, orgID, taskID)
		if err != nil || a == nil {
			t.Fatalf("AssignmentSystem = %v (err %v), want the claim", a, err)
		}
		if a.TaskID != taskID || a.EntityID != stored.EntityID || a.EventType != stored.EventType || a.DedupKey != stored.DedupKey ||
			a.EntitySourceID != stored.EntitySourceID || a.UserID != stores.UserID || a.AgentID != "" || a.TeamName == "" {
			t.Errorf("AssignmentSystem = %+v, want task %+v held by user %s", *a, *stored, stores.UserID)
		}
		if a.Assignee() != domain.UserAssignee(stores.UserID) {
			t.Errorf("Assignee() = %q, want the claiming user", a.Assignee())
		}
		if a.ConversationID != "" || a.ConversationHasRun {
			t.Errorf("AssignmentSystem of a task with no conversation names %q, has run %v", a.ConversationID, a.ConversationHasRun)
		}
		// The task's own conversation rides the assignment, with whether it
		// has run: a note to it is staged only once it has.
		_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
		if a, err = stores.Tasks.AssignmentSystem(ctx, orgID, taskID); err != nil || a == nil {
			t.Fatalf("AssignmentSystem = %v (err %v), want the claim", a, err)
		}
		if a.ConversationID != convID || a.ConversationHasRun {
			t.Errorf("AssignmentSystem names conversation %q, has run %v; want %q, not yet run", a.ConversationID, a.ConversationHasRun, convID)
		}
		if _, err := stores.Conversations.InsertMessageSystem(ctx, orgID, &domain.Message{
			ConversationID: convID, Role: "assistant", Content: "a turn",
		}); err != nil {
			t.Fatalf("seed an assistant turn: %v", err)
		}
		if a, err = stores.Tasks.AssignmentSystem(ctx, orgID, taskID); err != nil || a == nil {
			t.Fatalf("AssignmentSystem = %v (err %v), want the claim", a, err)
		}
		if a.ConversationID != convID || !a.ConversationHasRun {
			t.Errorf("AssignmentSystem names conversation %q, has run %v; want %q, run", a.ConversationID, a.ConversationHasRun, convID)
		}
		closeTask(t, stores, orgID, taskID)
		if a, err := stores.Tasks.AssignmentSystem(ctx, orgID, taskID); err != nil || a != nil {
			t.Errorf("AssignmentSystem of a closed task = %+v (err %v), want none", a, err)
		}
	})

	t.Run("Retire_withdraws_the_assignment_notes_that_stopped_being_true", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		_, convID := seed.BlueprintAndConversation(t, seed.Task(t), "running", "")
		holder := domain.UserAssignee(stores.UserID)
		// note stages one waiting assignment note and returns its id.
		note := func(t *testing.T, taskID, assignee string) string {
			t.Helper()
			stored, err := stores.StagedInjections.AppendSystem(ctx, orgID, domain.StagedInjection{
				ConversationID: convID, Subtype: domain.MessageSubtypeInjectionSystemNote,
				Producer: domain.StagedInjectionProducerTaskAssigned, Body: "assigned",
				Provenance: domain.NoteProvenance{AssignedTask: taskID, Assignee: assignee},
			})
			if err != nil {
				t.Fatalf("AppendSystem: %v", err)
			}
			if stored.Provenance.AssignedTask != taskID || stored.Provenance.Assignee != assignee {
				t.Fatalf("stored provenance = %+v, want task %s assignee %s", stored.Provenance, taskID, assignee)
			}
			return stored.ID
		}
		held := func(t *testing.T) string {
			t.Helper()
			taskID := seed.Task(t)
			claim(t, stores, orgID, taskID)
			return taskID
		}

		closed := held(t)
		closedNote := note(t, closed, holder)
		closeTask(t, stores, orgID, closed)

		// A close stops no conversation, so the work this note warns of
		// goes on.
		closedWorking := held(t)
		closedWorkingNote := note(t, closedWorking, holder)
		seed.BlueprintAndConversation(t, closedWorking, "running", "")
		closeTask(t, stores, orgID, closedWorking)

		changedNote := note(t, held(t), domain.AgentAssignee(uuid.New().String()))

		twice := held(t)
		olderNote := note(t, twice, holder)
		newerNote := note(t, twice, holder)

		goneNote := note(t, uuid.New().String(), holder)

		stillTrue := note(t, held(t), holder)

		// A note of another producer is no assignment note, whatever it
		// carries.
		other, err := stores.StagedInjections.AppendSystem(ctx, orgID, domain.StagedInjection{
			ConversationID: convID, Subtype: domain.MessageSubtypeInjectionSystemNote,
			Producer: domain.StagedInjectionProducerPRCoherence, Body: "coherence",
		})
		if err != nil {
			t.Fatalf("AppendSystem: %v", err)
		}

		retired, err := stores.StagedInjections.RetireStaleAssignmentNotesSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("RetireStaleAssignmentNotesSystem: %v", err)
		}
		if retired != 4 {
			t.Errorf("retired %d notes, want 4", retired)
		}
		if again, err := stores.StagedInjections.RetireStaleAssignmentNotesSystem(ctx, orgID, convID); err != nil || again != 0 {
			t.Errorf("a second retire = %d (err %v), want nothing left to retire", again, err)
		}
		left, err := stores.StagedInjections.FlushPendingSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("FlushPendingSystem: %v", err)
		}
		var ids []string
		for _, n := range left {
			ids = append(ids, n.ID)
		}
		if want := []string{closedWorkingNote, newerNote, stillTrue, other.ID}; !slices.Equal(ids, want) {
			t.Errorf("still waiting = %v, want %v (retired: closed %s, changed %s, older %s, gone %s)",
				ids, want, closedNote, changedNote, olderNote, goneNote)
		}
	})
}
