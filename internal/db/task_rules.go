package db

import (
	"database/sql"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SeedTaskRules inserts the v1 seeded `task_rules` defaults if they aren't
// already present. See "Seeded defaults" in docs/data-model-target.md.
//
// Uses INSERT OR IGNORE so user customizations on existing rows (renames,
// disables, predicate edits) are preserved across restarts. Sub-ticket
// SKY-180 will add CRUD endpoints; this file is just the seed.
func SeedTaskRules(db *sql.DB) error {
	stmt, err := db.Prepare(`
		INSERT OR IGNORE INTO task_rules
			(id, event_type, scope_predicate_json, enabled, name, default_priority, sort_order, source, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?, ?, 'system', ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now()
	authorIsSelf := `{"author_is_self":true}`

	rules := []seededRule{
		{
			ID:              "system-rule-ci-check-failed",
			EventType:       domain.EventGitHubPRCICheckFailed,
			Predicate:       authorIsSelf,
			Name:            "CI check failed on my PR",
			DefaultPriority: 0.75,
			SortOrder:       0,
		},
		{
			ID:              "system-rule-review-changes-requested",
			EventType:       domain.EventGitHubPRReviewChangesRequested,
			Predicate:       authorIsSelf,
			Name:            "Changes requested on my PR",
			DefaultPriority: 0.85,
			SortOrder:       1,
		},
		{
			ID:              "system-rule-review-commented",
			EventType:       domain.EventGitHubPRReviewCommented,
			Predicate:       authorIsSelf,
			Name:            "Reviewer commented on my PR",
			DefaultPriority: 0.65,
			SortOrder:       2,
		},
		{
			ID:              "system-rule-review-requested",
			EventType:       domain.EventGitHubPRReviewRequested,
			Predicate:       "", // null — match-all
			Name:            "Someone requested my review",
			DefaultPriority: 0.80,
			SortOrder:       3,
		},
		{
			ID:              "system-rule-jira-assigned",
			EventType:       domain.EventJiraIssueAssigned,
			Predicate:       `{"assignee_is_self":true}`,
			Name:            "Jira issue assigned to me",
			DefaultPriority: 0.60,
			SortOrder:       4,
		},
	}

	var inserted int64
	for _, r := range rules {
		var pred any
		if r.Predicate == "" {
			pred = nil
		} else {
			pred = r.Predicate
		}
		res, err := stmt.Exec(r.ID, r.EventType, pred, r.Name, r.DefaultPriority, r.SortOrder, now, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted += n
	}
	log.Printf("[db] seeded %d new task_rules (%d already existed)", inserted, int64(len(rules))-inserted)
	return nil
}

// seededRule is an internal helper for tabular seeding above. Not exported.
type seededRule struct {
	ID              string
	EventType       string
	Predicate       string // empty = NULL (match-all)
	Name            string
	DefaultPriority float64
	SortOrder       int
}

// --- CRUD for task_rules --------------------------------------------------

const taskRuleColumns = `id, event_type, scope_predicate_json, enabled, name,
       default_priority, sort_order, source, created_at, updated_at`

// ListTaskRules returns every task_rule, ordered by sort_order then name.
// Both system-seeded and user-created rules are returned; callers distinguish
// by `source`.
func ListTaskRules(db *sql.DB) ([]domain.TaskRule, error) {
	rows, err := db.Query(`SELECT ` + taskRuleColumns + ` FROM task_rules ORDER BY sort_order ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []domain.TaskRule
	for rows.Next() {
		r, err := scanTaskRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// GetTaskRule returns a single rule by ID, or nil if not found.
func GetTaskRule(db *sql.DB, id string) (*domain.TaskRule, error) {
	row := db.QueryRow(`SELECT `+taskRuleColumns+` FROM task_rules WHERE id = ?`, id)
	r, err := scanTaskRuleRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateTaskRule inserts a new user-created rule. The caller supplies the
// ID, event_type, predicate (canonical JSON or empty), name, priority, and
// sort_order; source is forced to "user" and timestamps are set server-side.
func CreateTaskRule(db *sql.DB, r domain.TaskRule) error {
	now := time.Now()
	_, err := db.Exec(`
		INSERT INTO task_rules (id, event_type, scope_predicate_json, enabled, name,
		                        default_priority, sort_order, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'user', ?, ?)
	`, r.ID, r.EventType, r.ScopePredicateJSON, r.Enabled, r.Name,
		r.DefaultPriority, r.SortOrder, now, now)
	return err
}

// UpdateTaskRule updates a rule's mutable fields. ID/source/created_at are
// immutable; event_type is intentionally immutable too (changing it would
// invalidate the predicate schema). updated_at is refreshed server-side.
func UpdateTaskRule(db *sql.DB, r domain.TaskRule) error {
	_, err := db.Exec(`
		UPDATE task_rules
		SET scope_predicate_json = ?, enabled = ?, name = ?,
		    default_priority = ?, sort_order = ?, updated_at = ?
		WHERE id = ?
	`, r.ScopePredicateJSON, r.Enabled, r.Name,
		r.DefaultPriority, r.SortOrder, time.Now(), r.ID)
	return err
}

// SetTaskRuleEnabled toggles just the enabled bit — useful for the
// "disable instead of delete" path on system rules.
func SetTaskRuleEnabled(db *sql.DB, id string, enabled bool) error {
	_, err := db.Exec(`
		UPDATE task_rules SET enabled = ?, updated_at = ? WHERE id = ?
	`, enabled, time.Now(), id)
	return err
}

// DeleteTaskRule hard-deletes a rule. The server handler gates this on
// source='user' — system rules go through SetTaskRuleEnabled(false) instead
// so that SeedTaskRules on next boot doesn't resurrect them as enabled.
func DeleteTaskRule(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM task_rules WHERE id = ?`, id)
	return err
}

// ReorderTaskRules updates sort_order for each rule based on its position in
// the given ID list. IDs not in the list keep their current sort_order.
// Non-existent IDs are silently skipped (UPDATE affects 0 rows) — the only
// caller is the frontend which sends IDs it just read; a stale ID means a
// concurrent delete, and the next re-fetch corrects the list.
func ReorderTaskRules(db *sql.DB, ids []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`UPDATE task_rules SET sort_order = ?, updated_at = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now()
	for i, id := range ids {
		if _, err := stmt.Exec(i, now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanTaskRule(rows *sql.Rows) (domain.TaskRule, error) {
	var r domain.TaskRule
	err := rows.Scan(&r.ID, &r.EventType, &r.ScopePredicateJSON, &r.Enabled, &r.Name,
		&r.DefaultPriority, &r.SortOrder, &r.Source, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func scanTaskRuleRow(row *sql.Row) (domain.TaskRule, error) {
	var r domain.TaskRule
	err := row.Scan(&r.ID, &r.EventType, &r.ScopePredicateJSON, &r.Enabled, &r.Name,
		&r.DefaultPriority, &r.SortOrder, &r.Source, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}
