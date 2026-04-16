package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GetActiveTriggersForEvent returns all enabled triggers matching the given event type.
// Used by the auto-delegation hook to find which prompts to fire.
func GetActiveTriggersForEvent(db *sql.DB, eventType string) ([]domain.PromptTrigger, error) {
	rows, err := db.Query(`
		SELECT id, prompt_id, trigger_type, event_type, scope_predicate_json,
		       breaker_threshold, cooldown_seconds, enabled, created_at, updated_at
		FROM prompt_triggers
		WHERE event_type = ? AND enabled = 1
	`, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

// GetPromptTrigger returns a single trigger by ID, or nil if not found.
func GetPromptTrigger(db *sql.DB, id string) (*domain.PromptTrigger, error) {
	row := db.QueryRow(`
		SELECT id, prompt_id, trigger_type, event_type, scope_predicate_json,
		       breaker_threshold, cooldown_seconds, enabled, created_at, updated_at
		FROM prompt_triggers WHERE id = ?
	`, id)

	t, err := scanTriggerRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListPromptTriggers returns all triggers, ordered by creation time.
func ListPromptTriggers(db *sql.DB) ([]domain.PromptTrigger, error) {
	rows, err := db.Query(`
		SELECT id, prompt_id, trigger_type, event_type, scope_predicate_json,
		       breaker_threshold, cooldown_seconds, enabled, created_at, updated_at
		FROM prompt_triggers ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

// ListTriggersForPrompt returns all triggers associated with a specific prompt.
func ListTriggersForPrompt(db *sql.DB, promptID string) ([]domain.PromptTrigger, error) {
	rows, err := db.Query(`
		SELECT id, prompt_id, trigger_type, event_type, scope_predicate_json,
		       breaker_threshold, cooldown_seconds, enabled, created_at, updated_at
		FROM prompt_triggers WHERE prompt_id = ? ORDER BY created_at DESC
	`, promptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

// SavePromptTrigger inserts or updates a trigger. Validates trigger_type.
func SavePromptTrigger(db *sql.DB, t domain.PromptTrigger) error {
	if t.TriggerType != domain.TriggerTypeEvent {
		return fmt.Errorf("unsupported trigger_type %q: only %q is supported", t.TriggerType, domain.TriggerTypeEvent)
	}

	now := time.Now()
	_, err := db.Exec(`
		INSERT INTO prompt_triggers (id, prompt_id, trigger_type, event_type, scope_predicate_json,
		                             breaker_threshold, cooldown_seconds, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			prompt_id = excluded.prompt_id,
			trigger_type = excluded.trigger_type,
			event_type = excluded.event_type,
			scope_predicate_json = excluded.scope_predicate_json,
			breaker_threshold = excluded.breaker_threshold,
			cooldown_seconds = excluded.cooldown_seconds,
			enabled = excluded.enabled,
			updated_at = ?
	`, t.ID, t.PromptID, t.TriggerType, t.EventType, t.ScopePredicateJSON,
		t.BreakerThreshold, t.CooldownSeconds, t.Enabled, now, now, now)
	return err
}

// DeletePromptTrigger removes a trigger by ID.
func DeletePromptTrigger(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM prompt_triggers WHERE id = ?`, id)
	return err
}

// SetTriggerEnabled enables or disables a trigger.
func SetTriggerEnabled(db *sql.DB, id string, enabled bool) error {
	_, err := db.Exec(`
		UPDATE prompt_triggers SET enabled = ?, updated_at = ? WHERE id = ?
	`, enabled, time.Now(), id)
	return err
}

// scanTrigger scans a PromptTrigger from a sql.Rows iterator.
func scanTrigger(rows *sql.Rows) (domain.PromptTrigger, error) {
	var t domain.PromptTrigger
	err := rows.Scan(&t.ID, &t.PromptID, &t.TriggerType, &t.EventType, &t.ScopePredicateJSON,
		&t.BreakerThreshold, &t.CooldownSeconds, &t.Enabled, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

// scanTriggerRow scans a PromptTrigger from a single sql.Row.
func scanTriggerRow(row *sql.Row) (domain.PromptTrigger, error) {
	var t domain.PromptTrigger
	err := row.Scan(&t.ID, &t.PromptID, &t.TriggerType, &t.EventType, &t.ScopePredicateJSON,
		&t.BreakerThreshold, &t.CooldownSeconds, &t.Enabled, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}
