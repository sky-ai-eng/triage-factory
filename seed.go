package main

import (
	"database/sql"
	"log"

	"github.com/sky-ai-eng/todo-tinder/internal/ai"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
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
}
