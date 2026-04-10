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
}
