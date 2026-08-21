package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PRCoherenceTargetFactory is what a per-backend test file hands to
// RunPRCoherenceTargetsConformance. Returns:
//   - the wired ConversationStore impl,
//   - the orgID to pass to every call,
//   - the shared ConversationSeeder for the entity/event/task/conversation
//     chain every target hangs off,
//   - a PRCoherenceSeeder for the rows the three relevance arms match on,
//     which no ConversationStore method writes.
type PRCoherenceTargetFactory func(t *testing.T) (
	store db.ConversationStore,
	orgID string,
	seed ConversationSeeder,
	extra PRCoherenceSeeder,
)

// PRCoherenceSeeder stages the fixtures ListPRCoherenceTargetsSystem matches
// on but does not own. Each backend implements them against its own SQL or
// sibling stores.
type PRCoherenceSeeder struct {
	// PendingReview attaches a pending review artifact for repo#prNumber to
	// the conversation — the review arm's evidence that an agent is drafting
	// against this PR.
	PendingReview func(t *testing.T, conversationID, repo string, prNumber int)

	// Worktree records that the conversation materialized ref of the
	// "owner/repo" slug, registering the repository if it isn't known yet.
	Worktree func(t *testing.T, conversationID, slug, ref string)

	// MarkEventInjected records the task's timeline row for eventID as
	// 'injected' — the same-task additive path's mark, which is the fence
	// the coherence query subtracts.
	MarkEventInjected func(t *testing.T, taskID, eventID string)

	// ActiveClaim opens an unreleased claim on the conversation, which is
	// what the query's Active projection derives from.
	ActiveClaim func(t *testing.T, conversationID string)
}

// The coherence query asks about one PR under three independent names, and a
// backend that gets any of them wrong fails silently — an agent simply never
// hears that the PR moved. So the suite pins each arm on a conversation the
// other two arms cannot reach: the review and worktree cases deliberately sit
// on a *different* entity than the one being queried, which is the whole point
// of looking past the task's own entity.
const (
	coherenceRepo      = "acme/widgets"
	coherencePRNumber  = 7
	coherencePRRef     = "pr-7"
	coherenceBranchRef = "ref-feature~coherence"
)

// RunPRCoherenceTargetsConformance drives ListPRCoherenceTargetsSystem through
// the three relevance arms, the injected fence, and the Active projection.
func RunPRCoherenceTargetsConformance(t *testing.T, factory PRCoherenceTargetFactory) {
	t.Helper()

	// query builds the lookup the coherence subscriber issues for entityID,
	// naming the same PR every way the schema can record it.
	query := func(entityID, eventID string) db.PRCoherenceTargetQuery {
		return db.PRCoherenceTargetQuery{
			EntityID:     entityID,
			EventID:      eventID,
			BaseRepo:     coherenceRepo,
			HeadRepo:     coherenceRepo,
			ReviewTarget: domain.ReviewTarget(coherenceRepo, coherencePRNumber),
			PRRef:        coherencePRRef,
			BranchRef:    coherenceBranchRef,
		}
	}

	t.Run("EntityArm", func(t *testing.T) {
		store, orgID, seed, _ := factory(t)
		ctx := context.Background()

		entityID := seed.Entity(t, "entity-arm")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRNewCommits)
		taskID := seed.Task(t, entityID, domain.EventGitHubPRNewCommits, eventID)
		conversationID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})

		targets, err := store.ListPRCoherenceTargetsSystem(ctx, orgID, query(entityID, eventID))
		if err != nil {
			t.Fatalf("ListPRCoherenceTargetsSystem: %v", err)
		}
		target := requireOneCoherenceTarget(t, targets, conversationID)
		if target.TaskID != taskID {
			t.Errorf("TaskID = %q, want %q", target.TaskID, taskID)
		}
		if target.Status != "open" {
			t.Errorf("Status = %q, want open", target.Status)
		}
		if target.Active {
			t.Error("Active = true with no claim open")
		}
	})

	t.Run("ReviewArmReachesAnotherEntitysConversation", func(t *testing.T) {
		store, orgID, seed, extra := factory(t)
		ctx := context.Background()

		entityID := seed.Entity(t, "review-subject")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed)

		// The reviewer's task hangs off a different entity entirely, so only
		// the pending review artifact can connect it to the PR in question.
		otherEntityID := seed.Entity(t, "review-author")
		otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRCICheckFailed)
		otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRCICheckFailed, otherEventID)
		reviewerID := seed.Conversation(t, domain.Conversation{
			TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
		})
		extra.PendingReview(t, reviewerID, coherenceRepo, coherencePRNumber)

		targets, err := store.ListPRCoherenceTargetsSystem(ctx, orgID, query(entityID, eventID))
		if err != nil {
			t.Fatalf("ListPRCoherenceTargetsSystem: %v", err)
		}
		requireOneCoherenceTarget(t, targets, reviewerID)
	})

	t.Run("WorktreeArmMatchesPRRefAndHeadBranch", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			ref  string
		}{
			{"PRCheckout", coherencePRRef},
			{"HeadBranchCheckout", coherenceBranchRef},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store, orgID, seed, extra := factory(t)
				ctx := context.Background()

				entityID := seed.Entity(t, "worktree-subject")
				eventID := seed.Event(t, entityID, domain.EventGitHubPRConflicts)

				otherEntityID := seed.Entity(t, "worktree-worker")
				otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRConflicts)
				otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRConflicts, otherEventID)
				workerID := seed.Conversation(t, domain.Conversation{
					TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
					BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
				})
				extra.Worktree(t, workerID, coherenceRepo, tc.ref)

				targets, err := store.ListPRCoherenceTargetsSystem(ctx, orgID, query(entityID, eventID))
				if err != nil {
					t.Fatalf("ListPRCoherenceTargetsSystem: %v", err)
				}
				requireOneCoherenceTarget(t, targets, workerID)
			})
		}
	})

	t.Run("InjectedEventFencesTheSameTaskConversation", func(t *testing.T) {
		store, orgID, seed, extra := factory(t)
		ctx := context.Background()

		entityID := seed.Entity(t, "fenced")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRNewCommits)
		taskID := seed.Task(t, entityID, domain.EventGitHubPRNewCommits, eventID)
		conversationID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})

		// Before the mark the conversation is a target; after it, the additive
		// path has already delivered this event and the coherence feed must
		// not deliver it a second time.
		targets, err := store.ListPRCoherenceTargetsSystem(ctx, orgID, query(entityID, eventID))
		if err != nil {
			t.Fatalf("ListPRCoherenceTargetsSystem before mark: %v", err)
		}
		requireOneCoherenceTarget(t, targets, conversationID)

		extra.MarkEventInjected(t, taskID, eventID)

		targets, err = store.ListPRCoherenceTargetsSystem(ctx, orgID, query(entityID, eventID))
		if err != nil {
			t.Fatalf("ListPRCoherenceTargetsSystem after mark: %v", err)
		}
		if len(targets) != 0 {
			t.Errorf("injected event still returned %d target(s), want 0", len(targets))
		}

		// The fence is per event, not per task: a different event on the same
		// task still reaches the conversation.
		secondEventID := seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed)
		targets, err = store.ListPRCoherenceTargetsSystem(ctx, orgID, query(entityID, secondEventID))
		if err != nil {
			t.Fatalf("ListPRCoherenceTargetsSystem for second event: %v", err)
		}
		requireOneCoherenceTarget(t, targets, conversationID)
	})

	t.Run("ActiveReflectsAnOpenClaim", func(t *testing.T) {
		store, orgID, seed, extra := factory(t)
		ctx := context.Background()

		entityID := seed.Entity(t, "claimed")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRCICheckPassed)
		taskID := seed.Task(t, entityID, domain.EventGitHubPRCICheckPassed, eventID)
		conversationID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		extra.ActiveClaim(t, conversationID)

		targets, err := store.ListPRCoherenceTargetsSystem(ctx, orgID, query(entityID, eventID))
		if err != nil {
			t.Fatalf("ListPRCoherenceTargetsSystem: %v", err)
		}
		if target := requireOneCoherenceTarget(t, targets, conversationID); !target.Active {
			t.Error("Active = false with an unreleased claim open")
		}
	})

	t.Run("UnrelatedConversationIsNotATarget", func(t *testing.T) {
		store, orgID, seed, _ := factory(t)
		ctx := context.Background()

		entityID := seed.Entity(t, "subject")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRNewCommits)

		strangerEntityID := seed.Entity(t, "stranger")
		strangerEventID := seed.Event(t, strangerEntityID, domain.EventGitHubPRNewCommits)
		strangerTaskID := seed.Task(t, strangerEntityID, domain.EventGitHubPRNewCommits, strangerEventID)
		seed.Conversation(t, domain.Conversation{
			TaskID: strangerTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, strangerTaskID),
		})

		targets, err := store.ListPRCoherenceTargetsSystem(ctx, orgID, query(entityID, eventID))
		if err != nil {
			t.Fatalf("ListPRCoherenceTargetsSystem: %v", err)
		}
		if len(targets) != 0 {
			t.Errorf("unrelated conversation returned %d target(s), want 0", len(targets))
		}
	})
}

// requireOneCoherenceTarget asserts the query returned exactly the expected
// conversation and hands the row back for field assertions.
func requireOneCoherenceTarget(t *testing.T, targets []domain.PRCoherenceTarget, want string) domain.PRCoherenceTarget {
	t.Helper()
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1 (%s)", len(targets), want)
	}
	if targets[0].ConversationID != want {
		t.Fatalf("target = %q, want %q", targets[0].ConversationID, want)
	}
	return targets[0]
}
