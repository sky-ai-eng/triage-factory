package db

import (
	"database/sql"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// MarkScoring sets scoring_status = 'scoring' for the given task IDs.
func MarkScoring(database *sql.DB, taskIDs []string) error {
	for _, id := range taskIDs {
		if _, err := database.Exec(`UPDATE tasks SET scoring_status = 'scoring' WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// UpdateTaskScores applies AI-generated scores and summaries to tasks,
// and sets scoring_status = 'scored'.
func UpdateTaskScores(database *sql.DB, updates []domain.TaskScoreUpdate) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		UPDATE tasks
		SET priority_score = ?, agent_confidence = ?, ai_summary = ?, priority_reasoning = ?, scoring_status = 'scored'
		WHERE id = ?
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, u := range updates {
		_, err := stmt.Exec(u.PriorityScore, u.AgentConfidence, u.Summary, u.PriorityReasoning, u.ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// UnscoredTasks returns queued tasks that haven't been scored yet.
func UnscoredTasks(database *sql.DB) ([]domain.Task, error) {
	return queryTasks(database, `SELECT `+taskColumns+` FROM tasks
		WHERE status = 'queued' AND scoring_status = 'unscored'
		ORDER BY created_at DESC`)
}
