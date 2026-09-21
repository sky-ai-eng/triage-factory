package db

import (
	"context"
	"sort"

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
	//
	// Exempt from the returned-row rule: it writes a batch. The runner claims
	// a whole cycle's tasks in one statement, so there is no single row a
	// return value could name; the batch it claimed is the argument.
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

	// UpdateTaskScores applies AI-generated scores and summaries to tasks
	// and sets scoring_status = 'scored'. Atomic across the whole batch (one
	// transaction); a partial-application failure rolls back so the runner
	// sees an all-or-nothing outcome.
	//
	// In the same transaction, and before it writes any tasks row, it admits
	// or raises each task's task_rederive_queue row (workkinds.TaskReDerive,
	// keyed on the task id) and sets the row's requested_revision to the
	// revision the tasks statement then stamps as score_revision. The scores
	// and the obligation they create — evaluate this task's deferred
	// triggers against the new autonomy_suitability — are one commit, so no
	// crash can land the scores without the obligation, and a re-evaluation
	// claimed against an older revision cannot complete against the newer
	// one. It is the only writer of priority_score, autonomy_suitability,
	// ai_summary, priority_reasoning and score_revision, and the only writer
	// of requested_revision; a future path that persists scoring results
	// goes through it.
	//
	// Lock order: task_rederive_queue before tasks, every task in ascending
	// id order. A completion holding a queue row reaches tasks only through
	// its firings admission, so a score writer that took the queue row first
	// waits behind it rather than deadlocking with it.
	//
	// Exempt from the returned-row rule: it writes a batch atomically. The
	// scorer applies a cycle's worth of updates in one transaction, so there
	// is no single row to hand back.
	UpdateTaskScores(ctx context.Context, orgID string, updates []domain.TaskScoreUpdate) error

	// UnscoredTasks returns queued tasks that haven't been scored
	// yet (status='queued' AND scoring_status='pending'), joined to
	// their entity. Used by the runner to discover work per cycle.
	UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error)
}

// OrderedScoreUpdates is the batch as UpdateTaskScores applies it on both
// dialects: one update per task, the last one for a repeated id winning, in
// ascending task id order. One per task is what keeps the queue row's
// requested_revision equal to the task's score_revision — a repeated id
// would raise the revision once per repetition on one side and once per
// row on the other — and the fixed order is what keeps two concurrent
// writers from deadlocking on each other's queue rows.
func OrderedScoreUpdates(updates []domain.TaskScoreUpdate) []domain.TaskScoreUpdate {
	last := make(map[string]domain.TaskScoreUpdate, len(updates))
	for _, u := range updates {
		last[u.ID] = u
	}
	ordered := make([]domain.TaskScoreUpdate, 0, len(last))
	for _, u := range last {
		ordered = append(ordered, u)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	return ordered
}
