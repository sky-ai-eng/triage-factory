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
		VALUES (?, ?, ?, 1, ?, ?, ?, 'system', ?, ?)
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
			Predicate:       "", // null — match-all
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
