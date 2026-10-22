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
	// PendingReview attaches a pending review artifact for repo#prNumber,
	// keyed on the pull request's node id prNodeID, on the GitHub host (the
	// artifact's scope) to the conversation, linked to the entity carrying
	// prNodeID on the host as the exec funnel links it (none, when no entity
	// carries it) — the review arm's evidence that an agent is drafting
	// against this PR.
	PendingReview func(t *testing.T, conversationID, host, repo string, prNumber int, prNodeID string)

	// PullRequestNodeID records nodeID as the external id of the PR entity
	// entityID: the node id the poller learns from the pull request.
	PullRequestNodeID func(t *testing.T, entityID, nodeID string)

	// Worktree records that the conversation materialized ref of the
	// "owner/repo" slug on a GitHub host, registering the repository on that
	// host if it isn't known yet.
	Worktree func(t *testing.T, conversationID, host, slug, ref string)

	// Repository returns the id of the registry row the "owner/repo" slug
	// resolves to on a GitHub host, registering it if nothing answers to the
	// name (RepositoryStore.FindOrCreateByRefSystem): the row the subscriber
	// names a PR's repository by.
	Repository func(t *testing.T, host, slug string) string

	// RetireRepository retires the registry row of the "owner/repo" slug on
	// a GitHub host (RetireRepo).
	RetireRepository func(t *testing.T, host, slug string)

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
	coherencePRNodeID  = "PR_kwDOcoherence7"
	coherencePRRef     = "pr-7"
	coherenceBranchRef = "ref-feature~coherence"
)

// RunPRCoherenceTargetsConformance drives ListPRCoherenceTargetsSystem through
// the three relevance arms, the injected fence, and the Active projection.
func RunPRCoherenceTargetsConformance(t *testing.T, factory PRCoherenceTargetFactory) {
	t.Helper()

	// query builds the lookup the coherence subscriber issues for entityID,
	// naming the same PR every way the schema can record it: repositoryID is
	// the registry row of the PR's repository, its base and its head branch's
	// alike, and "" for a case that stages no checkout.
	lookup := func(t *testing.T, store db.ConversationStore, orgID string, q db.PRCoherenceTargetQuery) []domain.PRCoherenceTarget {
		t.Helper()
		got, err := store.ListPRCoherenceTargetsSystem(context.Background(), orgID, q)
		if err != nil {
			t.Fatalf("ListPRCoherenceTargetsSystem(%+v): %v", q, err)
		}
		return got
	}
	query := func(entityID, eventID, repositoryID string) db.PRCoherenceTargetQuery {
		return db.PRCoherenceTargetQuery{
			EntityID:         entityID,
			EventID:          eventID,
			BaseRepositoryID: repositoryID,
			HeadRepositoryID: repositoryID,
			PRRef:            coherencePRRef,
			BranchRef:        coherenceBranchRef,
		}
	}

	t.Run("EntityArm", func(t *testing.T) {
		store, orgID, seed, _ := factory(t)

		entityID := seed.Entity(t, "entity-arm")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRNewCommits)
		taskID := seed.Task(t, entityID, domain.EventGitHubPRNewCommits, eventID)
		conversationID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})

		targets := lookup(t, store, orgID, query(entityID, eventID, ""))
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

		entityID := seed.Entity(t, "review-subject")
		extra.PullRequestNodeID(t, entityID, coherencePRNodeID)
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
		extra.PendingReview(t, reviewerID, TestGitHubHost, coherenceRepo, coherencePRNumber, coherencePRNodeID)

		requireOneCoherenceTarget(t, lookup(t, store, orgID, query(entityID, eventID, "")), reviewerID)
	})

	t.Run("ReviewArmMatchesTheEntityWhateverNameTheTargetSpells", func(t *testing.T) {
		// A draft written under a name the repository has since given up, or
		// in another case, is still a draft of this pull request: the arm
		// matches the entity the draft is about, not its target.
		store, orgID, seed, extra := factory(t)

		entityID := seed.Entity(t, "review-name-subject")
		extra.PullRequestNodeID(t, entityID, coherencePRNodeID)
		eventID := seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed)

		otherEntityID := seed.Entity(t, "review-name-author")
		otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRCICheckFailed)
		otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRCICheckFailed, otherEventID)
		reviewerID := seed.Conversation(t, domain.Conversation{
			TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
		})
		extra.PendingReview(t, reviewerID, TestGitHubHost, "Acme/old-widgets", coherencePRNumber, coherencePRNodeID)

		requireOneCoherenceTarget(t, lookup(t, store, orgID, query(entityID, eventID, "")), reviewerID)
	})

	t.Run("ReviewArmSkipsAnotherPullRequestUnderTheSameName", func(t *testing.T) {
		// A repository deleted and recreated under the same name has its own
		// pull request #7. A draft of the old one shares the target but not
		// the entity, so its conversation is not told the new one moved.
		store, orgID, seed, extra := factory(t)

		entityID := seed.Entity(t, "review-reuse-subject")
		extra.PullRequestNodeID(t, entityID, coherencePRNodeID)
		eventID := seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed)

		otherEntityID := seed.Entity(t, "review-reuse-author")
		otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRCICheckFailed)
		otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRCICheckFailed, otherEventID)
		reviewerID := seed.Conversation(t, domain.Conversation{
			TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
		})
		extra.PendingReview(t, reviewerID, TestGitHubHost, coherenceRepo, coherencePRNumber, "PR_kwDOdeleted7")

		if targets := lookup(t, store, orgID, query(entityID, eventID, "")); len(targets) != 0 {
			t.Fatalf("a draft of another pull request under %s#%d reached this one: %+v", coherenceRepo, coherencePRNumber, targets)
		}
	})

	t.Run("ReviewArmMatchesOnlyTheEntitysHost", func(t *testing.T) {
		// A node id names a pull request only on one host. A pending review
		// recorded on another host is a review of a different pull request,
		// so its conversation is not told this one moved.
		store, orgID, seed, extra := factory(t)

		entityID := seed.Entity(t, "review-host-subject")
		extra.PullRequestNodeID(t, entityID, coherencePRNodeID)
		eventID := seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed)

		otherEntityID := seed.Entity(t, "review-host-author")
		otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRCICheckFailed)
		otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRCICheckFailed, otherEventID)
		reviewerID := seed.Conversation(t, domain.Conversation{
			TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
		})
		extra.PendingReview(t, reviewerID, TestOtherGitHubHost, coherenceRepo, coherencePRNumber, coherencePRNodeID)

		if targets := lookup(t, store, orgID, query(entityID, eventID, "")); len(targets) != 0 {
			t.Fatalf("a pending review of %s#%d on %s reached a PR on %s: %+v", coherenceRepo, coherencePRNumber, TestOtherGitHubHost, TestGitHubHost, targets)
		}
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

				entityID := seed.Entity(t, "worktree-subject")
				eventID := seed.Event(t, entityID, domain.EventGitHubPRConflicts)

				otherEntityID := seed.Entity(t, "worktree-worker")
				otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRConflicts)
				otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRConflicts, otherEventID)
				workerID := seed.Conversation(t, domain.Conversation{
					TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
					BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
				})
				extra.Worktree(t, workerID, TestGitHubHost, coherenceRepo, tc.ref)

				repositoryID := extra.Repository(t, TestGitHubHost, coherenceRepo)
				requireOneCoherenceTarget(t, lookup(t, store, orgID, query(entityID, eventID, repositoryID)), workerID)
			})
		}
	})

	t.Run("WorktreeArmMatchesOnlyTheRepositorysRow", func(t *testing.T) {
		// A checkout matches by the registry row its ledger row references.
		// The same owner/repo on another GitHub host is another row, so a
		// conversation holding only that checkout is not told this PR moved.
		store, orgID, seed, extra := factory(t)

		entityID := seed.Entity(t, "host-subject")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRConflicts)

		otherEntityID := seed.Entity(t, "host-worker")
		otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRConflicts)
		otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRConflicts, otherEventID)
		elsewhereID := seed.Conversation(t, domain.Conversation{
			TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
		})
		extra.Worktree(t, elsewhereID, TestOtherGitHubHost, coherenceRepo, coherencePRRef)

		repositoryID := extra.Repository(t, TestGitHubHost, coherenceRepo)
		if targets := lookup(t, store, orgID, query(entityID, eventID, repositoryID)); len(targets) != 0 {
			t.Fatalf("a checkout of %s on %s reached a PR on %s: %+v", coherenceRepo, TestOtherGitHubHost, TestGitHubHost, targets)
		}

		// The same slug checked out on the PR's own host is the PR's
		// repository, and does reach it.
		extra.Worktree(t, elsewhereID, TestGitHubHost, coherenceRepo, coherencePRRef)
		requireOneCoherenceTarget(t, lookup(t, store, orgID, query(entityID, eventID, repositoryID)), elsewhereID)
	})

	t.Run("WorktreeArmMatchesTheRowNotTheName", func(t *testing.T) {
		// A retired row keeps its name, and GitHub serves that name as another
		// repository now. A pull request in that repository references the
		// row the name resolves to today, and a checkout of the retired row is
		// not a checkout of it, so its conversation is not told the PR moved.
		store, orgID, seed, extra := factory(t)

		entityID := seed.Entity(t, "retired-subject")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRConflicts)

		otherEntityID := seed.Entity(t, "retired-worker")
		otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRConflicts)
		otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRConflicts, otherEventID)
		workerID := seed.Conversation(t, domain.Conversation{
			TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
		})
		extra.Worktree(t, workerID, TestGitHubHost, coherenceRepo, coherencePRRef)
		retiredID := extra.Repository(t, TestGitHubHost, coherenceRepo)
		requireOneCoherenceTarget(t, lookup(t, store, orgID, query(entityID, eventID, retiredID)), workerID)

		extra.RetireRepository(t, TestGitHubHost, coherenceRepo)
		namesakeID := extra.Repository(t, TestGitHubHost, coherenceRepo)
		if namesakeID == retiredID {
			t.Fatalf("the name resolved to the retired row %s; want a row of its own", retiredID)
		}
		if targets := lookup(t, store, orgID, query(entityID, eventID, namesakeID)); len(targets) != 0 {
			t.Fatalf("a checkout of the retired %s reached a PR of the repository now under its name: %+v", coherenceRepo, targets)
		}
	})

	t.Run("WorktreeArmMatchesAForksHeadBranchByItsRow", func(t *testing.T) {
		// A head branch can live in a fork, which is its own registry row.
		// The head branch's checkout of the fork reaches the PR when the
		// query names the fork's row as the head's; the PR's own ref checked
		// out of the fork does not, since a PR ref is the base repository's.
		store, orgID, seed, extra := factory(t)

		entityID := seed.Entity(t, "fork-subject")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRConflicts)

		otherEntityID := seed.Entity(t, "fork-worker")
		otherEventID := seed.Event(t, otherEntityID, domain.EventGitHubPRConflicts)
		otherTaskID := seed.Task(t, otherEntityID, domain.EventGitHubPRConflicts, otherEventID)
		workerID := seed.Conversation(t, domain.Conversation{
			TaskID: otherTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, otherTaskID),
		})
		const fork = "contributor/widgets"
		extra.Worktree(t, workerID, TestGitHubHost, fork, coherencePRRef)
		baseID := extra.Repository(t, TestGitHubHost, coherenceRepo)
		forkID := extra.Repository(t, TestGitHubHost, fork)

		withHead := func(headID string) db.PRCoherenceTargetQuery {
			q := query(entityID, eventID, baseID)
			q.HeadRepositoryID = headID
			return q
		}
		if targets := lookup(t, store, orgID, withHead(forkID)); len(targets) != 0 {
			t.Fatalf("the PR's ref checked out of the fork reached the PR: %+v", targets)
		}

		extra.Worktree(t, workerID, TestGitHubHost, fork, coherenceBranchRef)
		if targets := lookup(t, store, orgID, withHead("")); len(targets) != 0 {
			t.Fatalf("with no head row, the fork's head branch checkout = %+v; want no target", targets)
		}
		requireOneCoherenceTarget(t, lookup(t, store, orgID, withHead(forkID)), workerID)
	})

	t.Run("InjectedEventFencesTheSameTaskConversation", func(t *testing.T) {
		store, orgID, seed, extra := factory(t)

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
		requireOneCoherenceTarget(t, lookup(t, store, orgID, query(entityID, eventID, "")), conversationID)

		extra.MarkEventInjected(t, taskID, eventID)

		if targets := lookup(t, store, orgID, query(entityID, eventID, "")); len(targets) != 0 {
			t.Errorf("injected event still returned %d target(s), want 0", len(targets))
		}

		// The fence is per event, not per task: a different event on the same
		// task still reaches the conversation.
		secondEventID := seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed)
		requireOneCoherenceTarget(t, lookup(t, store, orgID, query(entityID, secondEventID, "")), conversationID)
	})

	t.Run("ActiveReflectsAnOpenClaim", func(t *testing.T) {
		store, orgID, seed, extra := factory(t)

		entityID := seed.Entity(t, "claimed")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRCICheckPassed)
		taskID := seed.Task(t, entityID, domain.EventGitHubPRCICheckPassed, eventID)
		conversationID := seed.Conversation(t, domain.Conversation{
			TaskID: taskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, taskID),
		})
		extra.ActiveClaim(t, conversationID)

		targets := lookup(t, store, orgID, query(entityID, eventID, ""))
		if target := requireOneCoherenceTarget(t, targets, conversationID); !target.Active {
			t.Error("Active = false with an unreleased claim open")
		}
	})

	t.Run("UnrelatedConversationIsNotATarget", func(t *testing.T) {
		store, orgID, seed, _ := factory(t)

		entityID := seed.Entity(t, "subject")
		eventID := seed.Event(t, entityID, domain.EventGitHubPRNewCommits)

		strangerEntityID := seed.Entity(t, "stranger")
		strangerEventID := seed.Event(t, strangerEntityID, domain.EventGitHubPRNewCommits)
		strangerTaskID := seed.Task(t, strangerEntityID, domain.EventGitHubPRNewCommits, strangerEventID)
		seed.Conversation(t, domain.Conversation{
			TaskID: strangerTaskID, PromptID: conversationTestPromptID, Status: "open",
			BlueprintRunID: seed.BlueprintRun(t, strangerTaskID),
		})

		if targets := lookup(t, store, orgID, query(entityID, eventID, "")); len(targets) != 0 {
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
