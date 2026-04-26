package main

import (
	"database/sql"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func seedDefaultPrompts(database *sql.DB) {
	// Default PR review prompt — manual only. The user picks when to
	// review a PR; no automation makes sense for reviewing (including
	// reviewing one's own draft — that's just running this prompt by hand).
	err := db.SeedPrompt(database, domain.Prompt{
		ID:     "system-pr-review",
		Name:   "PR Code Review",
		Body:   ai.PRReviewPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed PR review prompt: %v", err)
	}

	// Merge conflict resolution prompt — auto-fired on merge conflicts on
	// the user's own PRs via the matching trigger below.
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-conflict-resolution",
		Name:   "Merge Conflict Resolution",
		Body:   ai.ConflictResolutionPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed conflict resolution prompt: %v", err)
	}

	// CI fix prompt — auto-fired on CI failures via prompt_trigger.
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-ci-fix",
		Name:   "CI Fix",
		Body:   ai.CIFixPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed CI fix prompt: %v", err)
	}

	// Jira implementation prompt — auto-fired on issues assigned to the
	// user via the matching trigger below.
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-jira-implement",
		Name:   "Jira Issue Implementation",
		Body:   ai.JiraImplementPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed Jira implement prompt: %v", err)
	}

	// Fix review feedback — fires on reviews landed on the user's PRs.
	// Same action regardless of whether the reviewer is the user
	// (self-review loop) or someone else (normal code review): read the
	// review, fix what's right, push back on what isn't, push to branch.
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-fix-review-feedback",
		Name:   "Fix Review Feedback",
		Body:   ai.FixReviewFeedbackPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed fix-review-feedback prompt: %v", err)
	}

	// --- Default triggers --------------------------------------------------
	// All shipped disabled. System triggers are reference examples users
	// opt into (or disable and replace with their own variations). Predicates
	// kept conservative.

	authorIsSelf := `{"author_is_self":true}`
	assigneeIsSelf := `{"assignee_is_self":true}`

	// Trigger: CI fix on own PRs.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-ci-fix",
		PromptID:           "system-ci-fix",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRCICheckFailed,
		ScopePredicateJSON: &authorIsSelf,
		BreakerThreshold:   3,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed CI fix trigger: %v", err)
	}

	// Trigger: merge conflict resolution on own PRs.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-conflict-resolution",
		PromptID:           "system-conflict-resolution",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRConflicts,
		ScopePredicateJSON: &authorIsSelf,
		BreakerThreshold:   2,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed conflict resolution trigger: %v", err)
	}

	// Trigger: Jira issue implementation on tickets assigned to the user.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-jira-implement",
		PromptID:           "system-jira-implement",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventJiraIssueAssigned,
		ScopePredicateJSON: &assigneeIsSelf,
		BreakerThreshold:   2,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed Jira implement trigger: %v", err)
	}

	// Companion trigger for the belated-discovery path (SKY-173): a ticket
	// that had open subtasks on first poll suppresses assigned/available
	// and only emits became_atomic when the decomposition collapses. Users
	// who enable auto-implementation on assignment almost certainly want
	// the same behavior for this belated signal — ship a parallel trigger
	// rather than quietly dropping post-decomposition tickets on the floor.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-jira-implement-atomic",
		PromptID:           "system-jira-implement",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventJiraIssueBecameAtomic,
		ScopePredicateJSON: &assigneeIsSelf,
		BreakerThreshold:   2,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed Jira implement atomic trigger: %v", err)
	}

	// Trigger: auto-review PRs when someone requests your review. No
	// predicate needed — review_requested only fires when the session user
	// is added to the PR's review request list (diff.go scopes this at emit
	// time), so the event itself is already user-scoped. 5-minute cooldown
	// because a second review on the same PR right after the first is
	// pointless unless the PR changed, and new commits on the same PR route
	// through a different event type entirely.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:               "system-trigger-pr-review",
		PromptID:         "system-pr-review",
		TriggerType:      domain.TriggerTypeEvent,
		EventType:        domain.EventGitHubPRReviewRequested,
		BreakerThreshold: 3,
		Enabled:          false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed PR review trigger: %v", err)
	}

	// Trigger: fix review feedback when changes are requested on the user's
	// own PR. Fires regardless of reviewer identity — self-review loop and
	// external reviewer response route through the same prompt since the
	// action is the same. Users who only want automation for one or the
	// other can narrow the predicate.
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-fix-review-feedback",
		PromptID:           "system-fix-review-feedback",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRReviewChangesRequested,
		ScopePredicateJSON: &authorIsSelf,
		BreakerThreshold:   3,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed fix-review-feedback trigger: %v", err)
	}
}
