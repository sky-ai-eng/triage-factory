package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// SeedPrompt inserts a prompt if it doesn't exist. Returns true if it was inserted.
func SeedPrompt(db *sql.DB, p domain.Prompt, bindings []domain.PromptBinding) error {
	// Skip if already seeded
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prompts WHERE id = ?`, p.ID).Scan(&exists); err != nil {
		return fmt.Errorf("check prompt existence: %w", err)
	}
	if exists > 0 {
		return nil
	}

	now := time.Now()
	_, err := db.Exec(`
		INSERT INTO prompts (id, name, body, source, usage_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)
	`, p.ID, p.Name, p.Body, p.Source, now, now)
	if err != nil {
		return err
	}

	for _, b := range bindings {
		if _, err := db.Exec(`
			INSERT OR IGNORE INTO prompt_bindings (prompt_id, event_type, is_default)
			VALUES (?, ?, ?)
		`, b.PromptID, b.EventType, b.IsDefault); err != nil {
			log.Printf("[db] warning: failed to insert binding %s -> %s: %v", b.PromptID, b.EventType, err)
		}
	}
	return nil
}

// ListPrompts returns all non-hidden prompts.
func ListPrompts(db *sql.DB) ([]domain.Prompt, error) {
	rows, err := db.Query(`
		SELECT id, name, body, source, usage_count, created_at, updated_at
		FROM prompts WHERE hidden = 0 ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prompts []domain.Prompt
	for rows.Next() {
		var p domain.Prompt
		if err := rows.Scan(&p.ID, &p.Name, &p.Body, &p.Source, &p.UsageCount, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// GetPrompt returns a single prompt by ID.
func GetPrompt(db *sql.DB, id string) (*domain.Prompt, error) {
	var p domain.Prompt
	err := db.QueryRow(`
		SELECT id, name, body, source, usage_count, created_at, updated_at
		FROM prompts WHERE id = ?
	`, id).Scan(&p.ID, &p.Name, &p.Body, &p.Source, &p.UsageCount, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePrompt inserts a new prompt.
func CreatePrompt(db *sql.DB, p domain.Prompt) error {
	now := time.Now()
	_, err := db.Exec(`
		INSERT INTO prompts (id, name, body, source, usage_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)
	`, p.ID, p.Name, p.Body, p.Source, now, now)
	return err
}

// UpdatePrompt updates a prompt's name and body.
func UpdatePrompt(db *sql.DB, id, name, body string) error {
	_, err := db.Exec(`
		UPDATE prompts SET name = ?, body = ?, updated_at = ? WHERE id = ?
	`, name, body, time.Now(), id)
	return err
}

// DeletePrompt removes a prompt and its bindings (CASCADE).
func DeletePrompt(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM prompts WHERE id = ?`, id)
	return err
}

// GetBindingsForPrompt returns all event type bindings for a prompt.
func GetBindingsForPrompt(db *sql.DB, promptID string) ([]domain.PromptBinding, error) {
	rows, err := db.Query(`
		SELECT prompt_id, event_type, is_default
		FROM prompt_bindings WHERE prompt_id = ?
	`, promptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []domain.PromptBinding
	for rows.Next() {
		var b domain.PromptBinding
		if err := rows.Scan(&b.PromptID, &b.EventType, &b.IsDefault); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// SetBindingsForPrompt replaces all bindings for a prompt.
func SetBindingsForPrompt(db *sql.DB, promptID string, bindings []domain.PromptBinding) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM prompt_bindings WHERE prompt_id = ?`, promptID); err != nil {
		return err
	}

	for _, b := range bindings {
		// If marking as default, clear any existing default for that event_type
		if b.IsDefault {
			if _, err := tx.Exec(`UPDATE prompt_bindings SET is_default = 0 WHERE event_type = ? AND is_default = 1`, b.EventType); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO prompt_bindings (prompt_id, event_type, is_default)
			VALUES (?, ?, ?)
		`, promptID, b.EventType, b.IsDefault); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// FindDefaultPrompt returns the default prompt for a given event type.
// Tries exact match first, then prefix match (e.g. "github:" matches "github:pr:ci_failed").
func FindDefaultPrompt(db *sql.DB, eventType string) (*domain.Prompt, error) {
	// Exact match
	var promptID string
	err := db.QueryRow(`
		SELECT prompt_id FROM prompt_bindings
		WHERE event_type = ? AND is_default = 1
	`, eventType).Scan(&promptID)
	if err == nil {
		return GetPrompt(db, promptID)
	}

	// Prefix match: find bindings where the binding's event_type is a prefix of the actual event
	rows, err := db.Query(`
		SELECT prompt_id, event_type FROM prompt_bindings
		WHERE is_default = 1 ORDER BY LENGTH(event_type) DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var pid, et string
		if err := rows.Scan(&pid, &et); err != nil {
			continue
		}
		if len(eventType) >= len(et) && eventType[:len(et)] == et {
			return GetPrompt(db, pid)
		}
	}

	return nil, nil
}

// CreateBinding adds a single binding.
func CreateBinding(db *sql.DB, b domain.PromptBinding) error {
	_, err := db.Exec(`
		INSERT OR IGNORE INTO prompt_bindings (prompt_id, event_type, is_default)
		VALUES (?, ?, ?)
	`, b.PromptID, b.EventType, b.IsDefault)
	return err
}

// DeleteBinding removes a single binding.
func DeleteBinding(db *sql.DB, promptID, eventType string) error {
	_, err := db.Exec(`DELETE FROM prompt_bindings WHERE prompt_id = ? AND event_type = ?`, promptID, eventType)
	return err
}

// SetBindingDefault toggles the is_default flag. Clears other defaults for the same event_type first.
func SetBindingDefault(db *sql.DB, promptID, eventType string, isDefault bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if isDefault {
		if _, err := tx.Exec(`UPDATE prompt_bindings SET is_default = 0 WHERE event_type = ? AND is_default = 1`, eventType); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE prompt_bindings SET is_default = ? WHERE prompt_id = ? AND event_type = ?`, isDefault, promptID, eventType); err != nil {
		return err
	}
	return tx.Commit()
}

// HidePrompt soft-deletes a prompt by setting hidden = 1.
func HidePrompt(db *sql.DB, id string) error {
	_, err := db.Exec(`UPDATE prompts SET hidden = 1 WHERE id = ?`, id)
	return err
}

// UnhidePrompt restores a hidden prompt.
func UnhidePrompt(db *sql.DB, id string) error {
	_, err := db.Exec(`UPDATE prompts SET hidden = 0 WHERE id = ?`, id)
	return err
}

// IncrementPromptUsage bumps the usage_count for a prompt.
func IncrementPromptUsage(db *sql.DB, promptID string) error {
	_, err := db.Exec(`UPDATE prompts SET usage_count = usage_count + 1 WHERE id = ?`, promptID)
	return err
}
