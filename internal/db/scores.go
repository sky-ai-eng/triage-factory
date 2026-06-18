package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=ScoreStore --output=./mocks --case=underscore --with-expecter

// ScoreStore owns the scoring-pipeline reads + writes against the
// tasks table's scoring_status / priority_score / autonomy_suitability
// / ai_summary / priority_reasoning columns. Logically a TaskStore
// concern, split into its own interface to keep TaskStore focused on
// task lifecycle and so the AI scorer (the sole production caller)
// can depend on a 4-method surface instead of the full task surface.
//
// All methods take orgID. Local mode passes runmode.LocalDefaultOrgID
// (asserted by the SQLite impl). Multi mode passes the scorer's
// current org context; the Postgres impl includes org_id in WHERE
// clauses as defense in depth alongside RLS.
type ScoreStore interface {
	// MarkScoring flips scoring_status to 'in_progress' for the given
	// task IDs. Called by the runner before dispatching a batch to
	// the LLM so concurrent triggers don't re-pick the same tasks.
	MarkScoring(ctx context.Context, orgID string, taskIDs []string) error

	// ResetScoringToPending flips scoring_status back to 'pending'.
	// Used when a scoring batch failed so the tasks are retried on
	// the next cycle — without this, MarkScoring would have left
	// them stuck in 'in_progress' (UnscoredTasks only picks up
	// 'pending') and they'd never be rescored.
	ResetScoringToPending(ctx context.Context, orgID string, taskIDs []string) error

	// UpdateTaskScores applies AI-generated scores and summaries to
	// tasks and sets scoring_status = 'scored'. Atomic across the
	// whole batch (single tx); a partial-application failure rolls
	// back so the runner sees an all-or-nothing outcome.
	UpdateTaskScores(ctx context.Context, orgID string, updates []domain.TaskScoreUpdate) error

	// UnscoredTasks returns queued tasks that haven't been scored
	// yet (status='queued' AND scoring_status='pending'), joined to
	// their entity. Used by the runner to discover work per cycle.
	UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error)
}
