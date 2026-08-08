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

	// ResetStaleScoring flips every 'in_progress' row in the org back
	// to 'pending' and reports how many it moved. The runner calls it
	// at the top of a cycle, strictly before that cycle's own
	// MarkScoring, so it can never reset a row the cycle just claimed.
	//
	// It recovers what a killed process leaves behind. Every in-process
	// failure path resets the rows it marked; a crash between
	// MarkScoring and UpdateTaskScores resets nothing, and UnscoredTasks
	// only picks 'pending', so those rows would never be scored again —
	// which in turn leaves autonomy_suitability NULL and silently
	// disables every min_autonomy_suitability-deferred trigger on them.
	//
	// Telling residue from live work needs no timestamp. The scorer is
	// single-flight per org and runs only on the background-brain lease
	// holder, so at the moment a cycle starts no other cycle for that
	// org can be running and any 'in_progress' row is by definition left
	// over from one that died. A lease handoff can at worst cost a
	// redundant re-score, and UpdateTaskScores is an idempotent
	// overwrite.
	ResetStaleScoring(ctx context.Context, orgID string) (int, error)

	// UpdateTaskScores applies AI-generated scores and summaries to
	// tasks and sets scoring_status = 'scored'. Atomic across the
	// whole batch (single tx); a partial-application failure rolls
	// back so the runner sees an all-or-nothing outcome.
	//
	// It also raises rederive_owed in the same statement. The scores and
	// the obligation they create — evaluate this task's deferred triggers
	// against the new autonomy_suitability — are one write, so no crash can
	// land the scores without the debt.
	UpdateTaskScores(ctx context.Context, orgID string, updates []domain.TaskScoreUpdate) error

	// TasksOwedReDerive returns the org's task IDs whose scores committed
	// but whose post-scoring re-derive has not been recorded as done. The
	// runner drains it at the top of a cycle, which is the crash backstop
	// behind the OnScoringCompleted fast path: the callback runs after the
	// scores commit, so a process killed in between leaves tasks 'scored'
	// (invisible to UnscoredTasks forever) with their deferred triggers
	// never evaluated.
	//
	// Empty on every crash-free cycle. Oldest first, so a backlog drains in
	// the order it accrued.
	TasksOwedReDerive(ctx context.Context, orgID string) ([]string, error)

	// ClearReDeriveOwed discharges the obligation for tasks the re-derive
	// pass has evaluated. Called by that pass and by nothing else: clearing
	// before the evaluation would recreate the very window the column
	// exists to close, and clearing on a bail (a store error mid-evaluation)
	// would drop work the next cycle should retry.
	//
	// Re-clearing an already-clear task is a no-op, which is what a drain
	// that races the callback sees.
	ClearReDeriveOwed(ctx context.Context, orgID string, taskIDs []string) error

	// UnscoredTasks returns queued tasks that haven't been scored
	// yet (status='queued' AND scoring_status='pending'), joined to
	// their entity. Used by the runner to discover work per cycle.
	UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error)
}
