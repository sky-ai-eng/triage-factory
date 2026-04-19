package main

import (
	"database/sql"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func seedDefaultPrompts(database *sql.DB) {
	// Default PR review prompt
	err := db.SeedPrompt(database, domain.Prompt{
		ID:     "system-pr-review",
		Name:   "PR Code Review",
		Body:   ai.PRReviewPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed PR review prompt: %v", err)
	}

	// Merge conflict resolution prompt
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-conflict-resolution",
		Name:   "Merge Conflict Resolution",
		Body:   ai.ConflictResolutionPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed conflict resolution prompt: %v", err)
	}

	// CI fix prompt — auto-fired on CI failures via prompt_trigger
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-ci-fix",
		Name:   "CI Fix",
		Body:   ai.CIFixPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed CI fix prompt: %v", err)
	}

	// Default Jira implementation prompt
	err = db.SeedPrompt(database, domain.Prompt{
		ID:     "system-jira-implement",
		Name:   "Jira Issue Implementation",
		Body:   ai.JiraImplementPromptTemplate,
		Source: "system",
	})
	if err != nil {
		log.Printf("[seed] warning: failed to seed Jira implement prompt: %v", err)
	}

	// Default trigger: auto-fire CI fix on per-check CI failures on the
	// user's own PRs. Shipped disabled — users opt in by enabling it.
	// System triggers are static reference examples; users who want
	// different behavior (e.g. all PRs) disable this and create their own.
	authorIsSelf := `{"author_is_self":true}`
	if err := db.SeedPromptTrigger(database, domain.PromptTrigger{
		ID:                 "system-trigger-ci-fix",
		PromptID:           "system-ci-fix",
		TriggerType:        domain.TriggerTypeEvent,
		EventType:          domain.EventGitHubPRCICheckFailed,
		ScopePredicateJSON: &authorIsSelf,
		BreakerThreshold:   3,
		CooldownSeconds:    60,
		Enabled:            false,
	}); err != nil {
		log.Printf("[seed] warning: failed to seed CI fix trigger: %v", err)
	}
}
