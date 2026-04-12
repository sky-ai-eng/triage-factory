package main

import (
	"database/sql"
	"log"

	"github.com/sky-ai-eng/todo-triage/internal/ai"
	"github.com/sky-ai-eng/todo-triage/internal/db"
	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

func seedDefaultPrompts(database *sql.DB) {
	// Default PR review prompt — bound to all github:pr events
	err := db.SeedPrompt(database, domain.Prompt{
		ID:     "system-pr-review",
		Name:   "PR Code Review",
		Body:   ai.PRReviewPromptTemplate,
		Source: "system",
	}, []domain.PromptBinding{
		{PromptID: "system-pr-review", EventType: "github:pr:review_requested", IsDefault: true},
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed PR review prompt: %v", err)
	}

	// Merge conflict resolution prompt — bound to conflicts event (not yet emitted by poller)
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-conflict-resolution",
		Name:   "Merge Conflict Resolution",
		Body:   ai.ConflictResolutionPromptTemplate,
		Source: "system",
	}, []domain.PromptBinding{
		{PromptID: "system-conflict-resolution", EventType: "github:pr:conflicts", IsDefault: true},
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed conflict resolution prompt: %v", err)
	}

	// CI fix prompt — auto-fired on CI failures
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-ci-fix",
		Name:   "CI Fix",
		Body:   ai.CIFixPromptTemplate,
		Source: "system",
	}, []domain.PromptBinding{
		{PromptID: "system-ci-fix", EventType: "github:pr:ci_failed", IsDefault: false},
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed CI fix prompt: %v", err)
	}

	// Default Jira implementation prompt — bound to assigned issues
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-jira-implement",
		Name:   "Jira Issue Implementation",
		Body:   ai.JiraImplementPromptTemplate,
		Source: "system",
	}, []domain.PromptBinding{
		{PromptID: "system-jira-implement", EventType: "jira:issue:assigned", IsDefault: true},
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed Jira implement prompt: %v", err)
	}

	// Default trigger: auto-fire CI fix on CI failures
	if err := db.SavePromptTrigger(database, domain.PromptTrigger{
		ID:              "system-trigger-ci-fix",
		PromptID:        "system-ci-fix",
		TriggerType:     domain.TriggerTypeEvent,
		EventType:       domain.EventGitHubPRCIFailed,
		MaxIterations:   3,
		CooldownSeconds: 60,
		Enabled:         true,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed CI fix trigger: %v", err)
	}
}
