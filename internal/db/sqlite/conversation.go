package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// conversationStore is the SQLite impl of db.ConversationStore over the
// conversations + messages + claims tables. SQLite is single-tenant; the
// orgID assertion at each method entry catches a confused caller passing
// anything but the local sentinel.
//
// The constructor takes a single queryer (SQLite has one connection)
// rather than the (app, admin) pair the Postgres impl uses — the
// ConversationStore was the first store to ship multi-pool, and the
// SQLite side never grew the second arg. The
// `...System` admin-pool variants are thin wrappers around their
// non-System counterparts on the SQLite side.
var conversationLog = logging.Component("db/conversation")

type conversationStore struct{ q queryer }

func newConversationStore(q queryer) db.ConversationStore { return &conversationStore{q: q} }

var _ db.ConversationStore = (*conversationStore)(nil)

// --- Lifecycle ---

// claimOutcomeForStatus maps a conversation's terminal status onto the
// claims outcome vocabulary: the engagement that produced a failed
// conversation failed, and everything else completed. The stored terminal
// vocabulary is two names, so this is a two-way map; a cancelled engagement
// releases with outcome 'cancelled' from the park path, which names the
// outcome directly rather than deriving it from a status.
func claimOutcomeForStatus(status string) string {
	if status == "failed" {
		return "failed"
	}
	return "completed"
}

// releaseActiveClaim stamps released_at + outcome on the conversation's
// active claim, if one exists. A no-op (no error) when the conversation has
// no live engagement — a terminal write may land on a row whose claim was
// already released (requeue, boot sweep).
func releaseActiveClaim(ctx context.Context, q queryer, conversationID, outcome string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = ?, outcome = ?
		WHERE conversation_id = ? AND released_at IS NULL
	`, time.Now().UTC(), outcome, conversationID)
	return err
}

func (s *conversationStore) Complete(ctx context.Context, orgID, conversationID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// The conversation carries no accounting cache: cost settles as one
	// lump on the invocation's own newest ledger row, and the reported
	// duration/turns telemetry rides the claim release. A resume's
	// Complete stamps its own invocation's lump on its own row, so
	// nothing accumulates or doubles.
	return inTx(ctx, s.q, func(q queryer) error {
		_, err := q.ExecContext(ctx, `
			UPDATE conversations
			SET status = ?,
			    completed_at = ?,
			    result_summary = ?,
			    outcome = ?,
			    outcome_reason = ?,
			    failure_kind = ?
			WHERE id = ?
		`, status, time.Now(), resultSummary,
			nullIfEmpty(outcome), nullIfEmpty(outcomeReason), nullIfEmpty(failureKind),
			conversationID)
		if err != nil {
			return err
		}
		// The active claim this terminal write releases identifies the
		// engagement's own message rows (they insert claim-stamped), so
		// the lump settles claim-keyed — the curator turn release's shape.
		// Read before the release below: a released claim is no longer
		// findable.
		var claimID string
		err = q.QueryRowContext(ctx, `
			SELECT id FROM claims WHERE conversation_id = ? AND released_at IS NULL
		`, conversationID).Scan(&claimID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		settled := false
		// A zero lump settles nothing, in either arm. Zero means the runtime
		// had nothing to report at terminal time: the native loop settles
		// spend per assistant row as it goes, and overwriting its newest
		// stamp with 0 would erase real recorded dollars. An SDK invocation
		// that reports zero leaves its rows NULL — price unknown — rather
		// than asserting the run was genuinely free.
		if claimID != "" && costUSD != 0 {
			// Overwrite, not add: the engagement's newest claim-attributed
			// row is its own fresh row, and the lump is that invocation's
			// whole total. Runtime-composed rows are skipped as targets —
			// an errored or interrupted invocation ends on one, and
			// settling there would bill the whole invocation to a model
			// that never ran. An engagement whose rows are all synthetic
			// falls through to the conversation-wide arm below.
			//
			// Among what's left, a row that names a model wins over one
			// that names none (a user or tool row) however much newer the
			// latter is: only the first keeps the lump in the per-model
			// breakdown, and this engagement did run that model. NULL is
			// not "some other model" here — the comparison has to test IS
			// NOT NULL explicitly, since every <> test against the
			// sentinel admits NULL rows into the same tier as real ones.
			res, err := q.ExecContext(ctx, `
				UPDATE messages SET cost_usd = ?
				WHERE conversation_id = ?
				  AND id = (SELECT id FROM messages
				            WHERE claim_id = ? AND (model IS NULL OR model <> ?)
				            ORDER BY (model IS NOT NULL AND model <> ?) DESC, id DESC
				            LIMIT 1)
			`, costUSD, conversationID, claimID, domain.ModelSynthetic, domain.ModelSynthetic)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			settled = n > 0
		}
		if !settled && costUSD != 0 {
			// An invocation can bill real tokens while streaming zero rows
			// of its own (system-prompt/cache overhead on an errored run),
			// and the messages ledger is the only spend record — so the
			// lump settles on the conversation's newest existing row rather
			// than being dropped. ADDITIVE, unlike the overwrite above: that
			// row may already carry an earlier invocation's lump. The caveat:
			// a rowless resume's spend lands on an older invocation's row —
			// totals stay exact, per-row time attribution smears; accepted
			// for this narrow corner.
			//
			// The ORDER BY ranks rows in three tiers, newest-first within
			// each, and takes the first: a row naming a real model, else
			// one naming no model (a user or tool row), else a
			// runtime-composed one. Both keys are needed — the first alone
			// would tie NULL with real, the second alone would tie NULL
			// with synthetic. The bottom tier keeps the dollars on the
			// ledger when a conversation is nothing but synthetic rows; the
			// model breakdowns exclude them, so the spend shows in the
			// totals without inventing a model.
			res, err := q.ExecContext(ctx, `
				UPDATE messages SET cost_usd = COALESCE(cost_usd, 0) + ?
				WHERE conversation_id = ?
				  AND id = (SELECT id FROM messages
				            WHERE conversation_id = ?
				            ORDER BY (model IS NOT NULL AND model <> ?) DESC,
				                     (model IS NULL OR model <> ?) DESC,
				                     id DESC
				            LIMIT 1)
			`, costUSD, conversationID, conversationID, domain.ModelSynthetic, domain.ModelSynthetic)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				// No message rows at all: the spend has no ledger row to
				// live on. Loud, so the dropped dollars are at least
				// observable.
				conversationLog.Warn("run cost has no message row to settle on; spend unrecorded",
					"conversation_id", conversationID, "cost_usd", costUSD)
			}
		}
		_, err = q.ExecContext(ctx, `
			UPDATE claims SET released_at = ?, outcome = ?, duration_ms = ?, num_turns = ?
			WHERE conversation_id = ? AND released_at IS NULL
		`, time.Now().UTC(), claimOutcomeForStatus(status), durationMs, numTurns, conversationID)
		return err
	})
}

func (s *conversationStore) ParkOpen(ctx context.Context, orgID, conversationID string, park db.Park) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var flipped bool
	err := inTx(ctx, s.q, func(q queryer) error {
		var err error
		flipped, err = parkOpen(ctx, q, conversationID, park)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, conversationID, park.ClaimOutcome())
	})
	return flipped, err
}

// parkOpen is the one row-write behind every park — see
// ConversationStore.ParkOpen for what `park` decides.
//
// The exclusion list is the settled set, and the guard is an exclusion — so a
// status missing from it doesn't refuse, it readmits. An `open` row with
// undelivered input is claimable, so a park that "succeeded" on a finished run
// would hand it back to the dispatcher.
//
// COALESCE on park_reason / result_summary rather than a bare assignment: a
// park that carries neither must not blank what an earlier one recorded.
func parkOpen(ctx context.Context, q queryer, conversationID string, park db.Park) (bool, error) {
	// A deliberate stop re-parks an already-parked row; an idle turn-end does
	// not. Spelled as an extra excluded status rather than two queries.
	reparkGuard := `, 'open'`
	if park.Deliberate {
		reparkGuard = ``
	}
	res, err := q.ExecContext(ctx, `
		UPDATE conversations
		SET status = 'open',
		    parked_at = COALESCE(parked_at, ?),
		    park_reason = COALESCE(NULLIF(?, ''), park_reason),
		    result_summary = COALESCE(NULLIF(?, ''), result_summary)
		WHERE id = ?
		  AND (status IS NULL
		       OR status NOT IN (`+conversationTerminalStatusesSQL+reparkGuard+`))
	`, time.Now().UTC(), string(park.Reason), park.ResultSummary, conversationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkQueuedForResume is resume-by-enqueue's un-terminal write — the ONE
// path that puts an outcome-bearing conversation back into the mid-flight
// (status NULL) state the needs-driving predicate claims from. See the
// interface doc comment / the Postgres twin.
func (s *conversationStore) MarkQueuedForResume(ctx context.Context, orgID, conversationID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var flipped bool
	err := inTx(ctx, s.q, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations SET status = NULL,
			                parked_at = NULL, park_reason = NULL,
			                preferred_executor_id = NULL
			WHERE id = ?
			  AND (status = 'open'
			       OR (status = 'completed'
			           AND NOT EXISTS (SELECT 1 FROM blueprint_runs br
			                           WHERE br.id = conversations.blueprint_run_id
			                             AND br.status = 'running')))
		`, conversationID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		flipped = n > 0
		if !flipped {
			return nil
		}
		return releaseActiveClaim(ctx, q, conversationID, "requeued")
	})
	return flipped, err
}

func (s *conversationStore) SetSession(ctx context.Context, orgID, conversationID, sessionID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE conversations SET sdk_session_id = ? WHERE id = ?
	`, sessionID, conversationID)
	return err
}

// SetActiveClaimPhaseSystem scopes the write to the ACTIVE claim only: a
// released claim's phase is inert history, and a conversation with no live
// engagement has no sub-state to record — both fall through as a no-op.
func (s *conversationStore) SetActiveClaimPhaseSystem(ctx context.Context, orgID, conversationID, phase string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE claims SET phase = NULLIF(?, '')
		WHERE conversation_id = ? AND released_at IS NULL
	`, phase, conversationID)
	return err
}

// PriorClaimExecutorSystem reads the predecessor engagement's executor. The
// caller's own claim is excluded by id — it is the newest row by construction
// (one active claim per conversation), so the predecessor is the next one back.
//
// The sort is total, for the reason the Postgres arm spells out. Ties are
// reachable here too: the mint writes a precise Go timestamp, but the column's
// own default is CURRENT_TIMESTAMP at whole-second resolution, so any row that
// takes it can tie with a neighbour. released_at breaks a tie meaningfully —
// of two claims that started together the one that finished later is the
// nearer predecessor, and an unreleased one is nearer still, which is what the
// IS NULL term buys (SQLite sorts NULLs last under DESC, where Postgres is
// told NULLS FIRST). rowid is the backstop that leaves nothing undecided.
//
// The answer is the same either way at N=1, where there is one executor to
// name, but the ordering is part of a contract both dialects are held to.
func (s *conversationStore) PriorClaimExecutorSystem(ctx context.Context, orgID, conversationID, claimID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var executorID string
	err := s.q.QueryRowContext(ctx, `
		SELECT executor_id FROM claims
		WHERE conversation_id = ? AND id <> ?
		ORDER BY claimed_at DESC, (released_at IS NULL) DESC, released_at DESC, rowid DESC
		LIMIT 1
	`, conversationID, claimID).Scan(&executorID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return executorID, err
}

// RecordClaimSandboxStatsSystem is keyed on the claim id alone, with NO
// released_at predicate — the teardown that measures these numbers runs
// after the claim is released.
//
// No production caller reaches this arm: local mode never sandboxes, so no
// local run has a cgroup, and the executor-side teardown skips the write
// entirely when there is nothing measured — a local claim's columns stay
// NULL for their whole life. It exists because the store is one
// dual-dialect contract (identical behavior, identical conformance
// assertions), and because "not measured" has to be a value the local
// schema can hold rather than a mode branch at every write.
func (s *conversationStore) RecordClaimSandboxStatsSystem(ctx context.Context, orgID, claimID string, peakMemMB *int, cpuUsec *int64) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE claims SET peak_mem_mb = ?, cpu_usec = ? WHERE id = ?
	`, sqliteNullInt(peakMemMB), sqliteNullInt64(cpuUsec), claimID)
	return err
}

func (s *conversationStore) SetWorktreePath(ctx context.Context, orgID, conversationID, path string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE conversations SET worktree_path = ? WHERE id = ?`, path, conversationID)
	return err
}

func (s *conversationStore) SetExecutorSystem(ctx context.Context, orgID, conversationID, executorID string, bootEpoch int64) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// An empty executorID keeps the legacy clear semantics: the live
	// engagement is over, so its claim releases as requeued.
	if executorID == "" {
		return releaseActiveClaim(ctx, s.q, conversationID, "requeued")
	}
	// Idempotent go-live confirmation: update the active claim's identity
	// if one exists, mint one if none does — the live process must never
	// run unattributed.
	return inTx(ctx, s.q, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET executor_id = ?, boot_epoch = ?
			WHERE conversation_id = ? AND released_at IS NULL
		`, executorID, bootEpoch, conversationID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		_, err = q.ExecContext(ctx, `
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, uuid.New().String(), orgID, conversationID, executorID, bootEpoch, time.Now().UTC())
		return err
	})
}

func (s *conversationStore) MarkFailedIfActive(ctx context.Context, orgID, conversationID, failureKind string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// 'open' is deliberately failable here — see
	// ConversationStore.MarkFailedIfActive: a warm 'open' run has no durable
	// snapshot yet, so an infra error reaching failConversation must terminate it.
	var flipped bool
	err := inTx(ctx, s.q, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations SET status = 'failed', completed_at = COALESCE(completed_at, ?),
			    failure_kind = ?
			WHERE id = ?
			  AND (status IS NULL
			       OR status NOT IN (`+conversationTerminalStatusesSQL+`))
		`, time.Now().UTC(), nullIfEmpty(failureKind), conversationID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		flipped = n > 0
		if !flipped {
			return nil
		}
		return releaseActiveClaim(ctx, q, conversationID, "failed")
	})
	return flipped, err
}

// --- Queries ---

// sqliteConversationColumns is the SELECT list scanned into a domain.Conversation
// via scanConversation. Same shape as Postgres' pgConversationColumns; the
// memory_missing derivation uses SQLite's TRIM(...) variant with the
// explicit whitespace charset (Postgres uses BTRIM with an E'...'
// escape string). The claim-derived columns (claimed_at / executor_id /
// attempts / duration_ms / num_turns) are correlated subselects over
// claims and the accounting columns (cost + tokens) subselects over the
// messages ledger; claimed_at loses its declared column type inside the
// subselect, so it scans as text and parses via parseDBDatetime. Status is
// the derived display ladder (sqliteDisplayStatusSQL) rather than the stored
// column.
//
// `attempts` here is the LIFETIME claim count — engagement history for a
// human, matching the lifetime sums beside it. It is deliberately not the
// same quantity ClaimNextConversation returns under that name, which is the retry
// budget's current-episode counter (episodeAttemptsSQL, conversation_queue.go). Two
// questions, one field; see domain.Conversation.Attempts before carrying
// either one somewhere new.
const sqliteConversationColumns = `
	r.id, COALESCE(r.task_id, ''), COALESCE(r.runtime, ''),
	` + sqliteDisplayStatusSQL + `,
	r.model, r.started_at, r.queued_at,
	(SELECT MAX(cl.claimed_at) FROM claims cl WHERE cl.conversation_id = r.id) AS claimed_at,
	r.completed_at,
	(SELECT SUM(m.cost_usd) FROM messages m WHERE m.conversation_id = r.id)     AS total_cost_usd,
	(SELECT SUM(cl.duration_ms) FROM claims cl WHERE cl.conversation_id = r.id) AS duration_ms,
	(SELECT SUM(cl.num_turns) FROM claims cl WHERE cl.conversation_id = r.id)   AS num_turns,
	r.park_reason, r.worktree_path,
	r.result_summary, r.outcome, r.outcome_reason, r.failure_kind, r.sdk_session_id, r.actor_agent_id,
	COALESCE(r.trigger_type, ''),
	r.creator_user_id,
	COALESCE(r.team_id, ''),
	(SELECT cl.executor_id FROM claims cl WHERE cl.conversation_id = r.id AND cl.released_at IS NULL) AS executor_id,
	(SELECT COUNT(*) FROM claims cl WHERE cl.conversation_id = r.id) AS attempts,
	r.blueprint_run_id, r.blueprint_step_index,
	(SELECT COALESCE(SUM(m.input_tokens), 0)          FROM messages m WHERE m.conversation_id = r.id) AS input_tokens,
	(SELECT COALESCE(SUM(m.output_tokens), 0)         FROM messages m WHERE m.conversation_id = r.id) AS output_tokens,
	(SELECT COALESCE(SUM(m.cache_read_tokens), 0)     FROM messages m WHERE m.conversation_id = r.id) AS cache_read_tokens,
	(SELECT COALESCE(SUM(m.cache_creation_tokens), 0) FROM messages m WHERE m.conversation_id = r.id) AS cache_creation_tokens,
	(NULLIF(TRIM(rm.agent_content, ' ' || char(9) || char(10) || char(13)), '') IS NULL) AS memory_missing,
	COALESCE(a.display_name, '') AS actor_agent_name
`

// sqliteDisplayStatusSQL is the wire status: the SQLite mirror of the
// Postgres pgDisplayStatusSQL ladder. The active claim's setup sub-state
// wins; then the mere existence of an active claim is 'running'; then a
// conversation matching the needs-driving predicate — mid-flight and
// unclaimed, or parked and woken by input — is 'queued'; and finally the
// stored column carries the deliberate park and the terminals. The trailing
// ” is unreachable by construction and exists so NULL can never reach the
// wire. Requires the conversation alias `r`.
const sqliteDisplayStatusSQL = `COALESCE(
		(SELECT cl_d.phase FROM claims cl_d WHERE cl_d.conversation_id = r.id AND cl_d.released_at IS NULL),
		CASE WHEN ` + activeClaimExistsSQL + ` THEN 'running' END,
		CASE WHEN r.status IS NULL
		       OR (r.status = 'open' AND ` + undeliveredInputExistsSQL + `)
		     THEN 'queued' END,
		r.status,
		'')`

func (s *conversationStore) Get(ctx context.Context, orgID, conversationID string) (*domain.Conversation, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteConversationColumns+`
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id
		LEFT JOIN agents a ON a.id = r.actor_agent_id
		WHERE r.id = ?
	`, conversationID)

	var r domain.Conversation
	if err := scanConversation(row, &r); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func (s *conversationStore) ListForTask(ctx context.Context, orgID, taskID string) ([]domain.Conversation, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteConversationColumns+`
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id
		LEFT JOIN agents a ON a.id = r.actor_agent_id
		WHERE r.task_id = ?
		ORDER BY r.started_at DESC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convs []domain.Conversation
	for rows.Next() {
		var r domain.Conversation
		if err := scanConversationRows(rows, &r); err != nil {
			return nil, err
		}
		convs = append(convs, r)
	}
	return convs, rows.Err()
}

func (s *conversationStore) ListForTasks(ctx context.Context, orgID string, taskIDs []string, opts db.ListOpts) ([]domain.Conversation, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	if len(taskIDs) == 0 {
		return nil, 0, nil
	}
	// A window is meaningless across statements, so a windowed read must be a
	// single IN list. The HTTP route caps its id set at exactly inListChunkSize,
	// so this refusal is unreachable from a real caller and exists so a future
	// one fails loudly instead of receiving a window over the first chunk only.
	if opts.Limit > 0 && len(taskIDs) > inListChunkSize {
		return nil, 0, fmt.Errorf("sqlite ListForTasks: a windowed read takes at most %d task ids, got %d", inListChunkSize, len(taskIDs))
	}

	total := 0
	for _, chunk := range chunkIDs(taskIDs) {
		placeholders, args := inListArgs(chunk)
		var n int
		if err := s.q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM conversations WHERE task_id IN (`+placeholders+`)`, args...,
		).Scan(&n); err != nil {
			return nil, 0, err
		}
		total += n
	}

	// ?-placeholder IN list (SQLite has no array bind), mirroring
	// artifactStore.ListByConversations, chunked to stay inside SQLite's variable
	// limit (chunkIDs) on the unwindowed path. Ordering by (task_id,
	// started_at DESC, id) makes a windowed read's pages partition a total
	// order; on the unwindowed chunked path the order ACROSS chunks is chunk
	// order, which is all the caller — grouping by run.TaskID — relies on.
	// Same projection as ListForTask.
	var convs []domain.Conversation
	for _, chunk := range chunkIDs(taskIDs) {
		placeholders, args := inListArgs(chunk)
		query := `
			SELECT ` + sqliteConversationColumns + `
			FROM conversations r
			LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id
			LEFT JOIN agents a ON a.id = r.actor_agent_id
			WHERE r.task_id IN (` + placeholders + `)
			ORDER BY r.task_id, r.started_at DESC, r.id`
		if opts.Limit > 0 {
			query += ` LIMIT ? OFFSET ?`
			args = append(args, opts.Limit, opts.Offset)
		}
		rows, err := s.q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var r domain.Conversation
			if err := scanConversationRows(rows, &r); err != nil {
				rows.Close()
				return nil, 0, err
			}
			convs = append(convs, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, 0, err
		}
		rows.Close()
	}
	return convs, total, nil
}

// inListArgs renders an id slice as a "?, ?, ?" placeholder list plus its
// bind args.
func inListArgs(ids []string) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ", "), args
}

// HasActiveAutoConversationForTask: any non-terminal trigger_type='event' run on the
// task. Manual delegations are excluded. Used by the router's per-task firing
// gate.
func (s *conversationStore) HasActiveAutoConversationForTask(ctx context.Context, orgID, taskID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var count int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations r
		WHERE r.task_id = ?
		  AND r.trigger_type = 'event'
		  AND (r.status IS NULL
		       OR r.status NOT IN (`+conversationTerminalStatusesSQL+`))
	`, taskID).Scan(&count)
	return count > 0, err
}

func (s *conversationStore) ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE task_id = ?
		  AND (status IS NULL
		       OR status NOT IN (`+conversationTerminalStatusesSQL+`))
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// --- Admin-pool variants ---
//
// All `...System` methods below delegate straight through to their
// non-System counterparts. SQLite has one connection so the pool
// distinction doesn't exist; the wrappers are kept for signature
// parity with Postgres. The delegate spawner consumes these from
// its goroutine paths that detach from the request context.

func (s *conversationStore) HasActiveAutoConversationForTaskSystem(ctx context.Context, orgID, taskID string) (bool, error) {
	return s.HasActiveAutoConversationForTask(ctx, orgID, taskID)
}

func (s *conversationStore) ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return s.ActiveIDsForTask(ctx, orgID, taskID)
}

func (s *conversationStore) ActiveAutoConversationIDForTaskSystem(ctx context.Context, orgID, taskID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var id string
	err := s.q.QueryRowContext(ctx, `
		SELECT r.id FROM conversations r
		WHERE r.task_id = ?
		  AND r.trigger_type = 'event'
		  AND (r.status IS NULL
		       OR r.status NOT IN (`+conversationTerminalStatusesSQL+`))
		ORDER BY r.started_at DESC
		LIMIT 1
	`, taskID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *conversationStore) ActiveIDsForTeamSystem(ctx context.Context, orgID, teamID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE team_id = ?
		  AND (status IS NULL
		       OR status NOT IN (`+conversationTerminalStatusesSQL+`))
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *conversationStore) GetSystem(ctx context.Context, orgID, conversationID string) (*domain.Conversation, error) {
	return s.Get(ctx, orgID, conversationID)
}

func (s *conversationStore) LookupOrgForConversationSystem(ctx context.Context, conversationID string) (string, error) {
	var orgID string
	err := s.q.QueryRowContext(ctx, `SELECT org_id FROM conversations WHERE id = ?`, conversationID).Scan(&orgID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return orgID, err
}

func (s *conversationStore) CompleteSystem(ctx context.Context, orgID, conversationID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) error {
	return s.Complete(ctx, orgID, conversationID, status, costUSD, durationMs, numTurns, resultSummary, outcome, outcomeReason, failureKind)
}

func (s *conversationStore) ParkOpenSystem(ctx context.Context, orgID, conversationID string, park db.Park) (bool, error) {
	return s.ParkOpen(ctx, orgID, conversationID, park)
}

func (s *conversationStore) ListReapableSnapshotKeysSystem(ctx context.Context, cutoff time.Time) ([]domain.SnapshotReapKey, error) {
	// Snapshot-bearing runs (parked `open` / any `completed` terminal) grouped by
	// their shared snapshot key (org, blueprint_run_id); a key is reapable once
	// its newest such run last parked or concluded before the cutoff. The
	// timestamp is COALESCE(parked_at, completed_at, started_at): parked_at for an
	// open run (re-stamped each park, so resumes don't age it), completed_at for a
	// terminal, started_at a legacy fallback. datetime()
	// normalizes the mixed on-disk timestamp formats (CURRENT_TIMESTAMP text vs
	// Go-bound values) so the MAX is consistent; the cutoff binds as a canonical
	// UTC string.
	//
	// The substr is what makes that normalization actually work on a Go-bound
	// value. The driver stores a time.Time as Go's own rendering —
	// "2006-01-02 15:04:05.999999999 +0000 UTC" — which datetime() cannot parse
	// at all: it returns NULL, the MAX goes NULL, the comparison goes NULL, and
	// the row silently never reaps. Every parked_at and completed_at written
	// from Go (MarkOpen, the cancel park, Complete) is in that form, so without
	// this the retention sweep only ever saw rows whose timestamps happened to
	// be written as SQL text. The first 19 characters are the seconds-precision
	// prefix of every format in play — Go's, CURRENT_TIMESTAMP's, and ISO-8601's
	// — and datetime() parses all three of those.
	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id, blueprint_run_id
		FROM conversations
		WHERE status IN ('open', 'completed')
		GROUP BY org_id, blueprint_run_id
		HAVING MAX(datetime(substr(COALESCE(parked_at, completed_at, started_at), 1, 19))) < datetime(?)
	`, cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []domain.SnapshotReapKey
	for rows.Next() {
		var k domain.SnapshotReapKey
		if err := rows.Scan(&k.OrgID, &k.BlueprintRunID); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *conversationStore) SetSessionSystem(ctx context.Context, orgID, conversationID, sessionID string) error {
	return s.SetSession(ctx, orgID, conversationID, sessionID)
}

func (s *conversationStore) SetWorktreePathSystem(ctx context.Context, orgID, conversationID, path string) error {
	return s.SetWorktreePath(ctx, orgID, conversationID, path)
}

func (s *conversationStore) MarkFailedIfActiveSystem(ctx context.Context, orgID, conversationID, failureKind string) (bool, error) {
	return s.MarkFailedIfActive(ctx, orgID, conversationID, failureKind)
}

func (s *conversationStore) InsertMessageSystem(ctx context.Context, orgID string, msg *domain.Message) (int64, error) {
	return s.InsertMessage(ctx, orgID, msg)
}

// --- Claim-fenced engagement writes ---
//
// Unfenced here, and deliberately so. The fence guards one race: a zombie
// executor writing into a conversation a successor has been handed. Local
// mode is a single process that claims its own work, with no fleet reaper to
// hand anything over and no second executor to hand it to — the losing side
// of that race has no way to exist. These wrappers therefore do exactly what
// their unfenced counterparts do.
//
// The claim id carries whichever of its two jobs the write actually has.
// Where it is attribution — the claim stamped onto a transcript row — it is
// written here exactly as Postgres writes it. Where it is only the assertion
// of ownership the fence would have tested — the session id, the worktree
// path, the terminal, the park — it is unused, because there is no rival
// owner for it to be measured against.
//
// This is not a dialect fork of shared semantics: the write set, the
// attribution, and the resulting rows are identical. What differs is a
// refusal that has nothing to refuse. Postgres carries the enforcement and
// the conformance suite asserts it there.
//
// Two are written out below rather than delegated — SetExecutorForClaimSystem
// and SetClaimPhaseSystem. Their unfenced twins resolve the conversation's
// ACTIVE claim instead of the named one, and one of them mints a claim when
// there is none. Neither of those is the fence; both are contract, and
// delegating would have quietly dropped the caller's claim id on the floor.

func (s *conversationStore) SetSessionForClaimSystem(ctx context.Context, orgID, conversationID, claimID, sessionID string) error {
	return s.SetSession(ctx, orgID, conversationID, sessionID)
}

func (s *conversationStore) SetWorktreePathForClaimSystem(ctx context.Context, orgID, conversationID, claimID, path string) error {
	return s.SetWorktreePath(ctx, orgID, conversationID, path)
}

func (s *conversationStore) InsertMessageForClaimSystem(ctx context.Context, orgID, claimID string, msg *domain.Message) (int64, error) {
	msg.ClaimID = claimID
	return s.InsertMessage(ctx, orgID, msg)
}

func (s *conversationStore) MarkDeliveredForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, ids []int, subtype string) error {
	return s.markDelivered(ctx, orgID, conversationID, ids, subtype)
}

func (s *conversationStore) CompleteForClaimSystem(ctx context.Context, orgID, conversationID, claimID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) error {
	return s.Complete(ctx, orgID, conversationID, status, costUSD, durationMs, numTurns, resultSummary, outcome, outcomeReason, failureKind)
}

func (s *conversationStore) MarkFailedIfActiveForClaimSystem(ctx context.Context, orgID, conversationID, claimID, failureKind string) (bool, error) {
	return s.MarkFailedIfActive(ctx, orgID, conversationID, failureKind)
}

func (s *conversationStore) ParkOpenForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, park db.Park) (bool, error) {
	return s.ParkOpen(ctx, orgID, conversationID, park)
}

// SetExecutorForClaimSystem is written out rather than delegated, for the
// same reason SetClaimPhaseSystem below is: its unfenced twin resolves "the
// conversation's active claim" and MINTS one when there is none, and those two
// behaviors are not the fence. They are the contract — a claim-keyed write
// that never invents ownership — and it holds on both dialects. Delegating
// would have ignored the caller's claimID and could mint a claim in exactly
// the edge case (no active claim) where the interface says to write nothing.
//
// What local mode drops is only the refusal. A row naming a released claim is
// a no-op here rather than an ErrClaimReleased, which is the standing N=1
// exemption: there is no second executor for the refusal to protect the row
// from. The released_at filter is the twin's own, kept for the same reason
// SetClaimPhaseSystem keeps it — a released claim's identity is settled
// history, and rewriting it would be a change neither dialect makes.
func (s *conversationStore) SetExecutorForClaimSystem(ctx context.Context, orgID, conversationID, claimID, executorID string, bootEpoch int64) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE claims SET executor_id = ?, boot_epoch = ?
		WHERE id = ? AND conversation_id = ? AND released_at IS NULL
	`, executorID, bootEpoch, claimID, conversationID)
	return err
}

// SetClaimPhaseSystem keeps the released_at filter its active-claim sibling
// has always had: a released claim's phase is inert history either way, so a
// call naming one stays the no-op it is today rather than rewriting it. The
// conversation binds too — it costs nothing and keeps the row this writes
// from drifting away from the one the caller named.
func (s *conversationStore) SetClaimPhaseSystem(ctx context.Context, orgID, conversationID, claimID, phase string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE claims SET phase = NULLIF(?, '')
		WHERE id = ? AND conversation_id = ? AND released_at IS NULL
	`, phase, claimID, conversationID)
	return err
}

// LastAgentActivityAtSystem returns the created_at of the run's newest non-user
// message (the artifact-change ledger watermark). Ordered by id DESC —
// the monotonic insertion order — rather than MAX(created_at), so the watermark
// is the genuinely last-inserted agent row and never trips over mixed timestamp
// text formats in a lexical MAX. ok=false when the run has no agent message yet.
//
// assertLocalOrg above is SQLite's hard single-tenant gate; org_id is also kept
// in the WHERE to mirror the Postgres twin (defense-in-depth, and resilience if a
// multi-org SQLite path ever lands).
func (s *conversationStore) LastAgentActivityAtSystem(ctx context.Context, orgID, conversationID string) (time.Time, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return time.Time{}, false, err
	}
	var at time.Time
	err := s.q.QueryRowContext(ctx, `
		SELECT created_at FROM messages
		WHERE org_id = ? AND conversation_id = ? AND role <> 'user'
		ORDER BY id DESC LIMIT 1
	`, orgID, conversationID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
}

// ListParkedWorktreePathsSystem returns the worktree dirs the startup sweep must
// keep warm — parked `open` runs whose owning blueprint_run is still 'running'. A
// parked run under an already-terminal blueprint_run is NOT resumable (every
// resume path gates on cr.Status == running), so its worktree must NOT be
// preserved: preserving it would leave a checked-out branch on disk that the
// boot reconcile then orphans by cancelling the row, reviving the "refusing to
// fetch into a branch checked out in a worktree" loop.
func (s *conversationStore) ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx,
		`SELECT r.worktree_path FROM conversations r
		 LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
		 WHERE r.status = 'open'
		   AND COALESCE(r.worktree_path, '') != ''
		   AND (br.id IS NULL OR br.status = 'running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

func (s *conversationStore) EntitiesWithOpenConversations(ctx context.Context, orgID string, entityIDs []string) (map[string]struct{}, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	out := make(map[string]struct{})
	if len(entityIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(entityIDs))
	args := make([]any, 0, len(entityIDs))
	for i, id := range entityIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `
		SELECT DISTINCT t.entity_id
		FROM conversations r
		JOIN tasks t ON t.id = r.task_id
		WHERE r.status = 'open'
		  AND t.entity_id IN (` + strings.Join(placeholders, ",") + `)
	`
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// --- Transcript / messages ---

// InsertMessage attributes the row to an engagement: an explicit
// non-empty msg.ClaimID always wins (the curator sink names its own
// claim), otherwise claim_id resolves server-side to the conversation's
// active claim. Rows written during an engagement belong to it; rows
// written outside one (pending inputs, queued turns, injections)
// correctly resolve NULL. A message racing its claim's release lands
// NULL — harmless, the terminal settle's newest-row fallback still
// reaches it.
func (s *conversationStore) InsertMessage(ctx context.Context, orgID string, msg *domain.Message) (int64, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var toolCallsJSON, metadataJSON, reasoningJSON, contentBlocksJSON sql.NullString

	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.Reasoning) > 0 {
		b, err := json.Marshal(msg.Reasoning)
		if err != nil {
			return 0, fmt.Errorf("marshal reasoning: %w", err)
		}
		reasoningJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.ContentBlocks) > 0 {
		b, err := json.Marshal(msg.ContentBlocks)
		if err != nil {
			return 0, fmt.Errorf("marshal content_blocks: %w", err)
		}
		contentBlocksJSON = sql.NullString{String: string(b), Valid: true}
	}

	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	// delivered/window_state apply the schema default in Go rather than
	// relying on the column DEFAULT — every column is named explicitly below,
	// so the DEFAULT clause never fires. nil/"" is what every caller in this
	// repo passes today, so this preserves exactly today's behavior
	// (delivered=true, window_state='active') for every existing insert.
	delivered := true
	if msg.Delivered != nil {
		delivered = *msg.Delivered
	}
	windowState := msg.WindowState
	if windowState == "" {
		windowState = domain.MessageWindowActive
	}

	// SQLite uses AUTOINCREMENT on messages.id, so LastInsertId
	// on the Result gives us the assigned row id. Postgres uses a
	// sequence + RETURNING — see postgres/conversation.go.
	result, err := s.q.ExecContext(ctx, `
		INSERT INTO messages (conversation_id, user_id, claim_id, role, content, subtype, tool_calls, tool_call_id,
		                      is_error, metadata, model,
		                      input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd, created_at,
		                      reasoning, content_blocks, delivered, window_state, seq, duration_ms, stop_reason)
		VALUES (?, ?,
		        COALESCE(?, (SELECT id FROM claims
		                     WHERE conversation_id = ? AND released_at IS NULL)),
		        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.ConversationID, sqliteNullStr(msg.UserID), sqliteNullStr(msg.ClaimID), msg.ConversationID,
		msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, sqliteNullStr(msg.ToolCallID), msg.IsError, metadataJSON,
		sqliteNullStr(msg.Model), sqliteNullInt(msg.InputTokens), sqliteNullInt(msg.OutputTokens),
		sqliteNullInt(msg.CacheReadTokens), sqliteNullInt(msg.CacheCreationTokens),
		sqliteNullFloat(msg.CostUSD), msg.CreatedAt,
		reasoningJSON, contentBlocksJSON, delivered, string(windowState), sqliteNullFloat(msg.Seq),
		sqliteNullInt(msg.DurationMs), sqliteNullStr(msg.StopReason),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

const sqliteMessageColumns = `id, conversation_id, user_id, claim_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
	model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd, created_at,
	reasoning, content_blocks, delivered, window_state, seq, duration_ms, stop_reason`

// scanMessageRows drains a messages result set selecting
// sqliteMessageColumns into domain.Message values. Shared by the
// single-run Messages and the batched MessagesForConversations.
func scanMessageRows(rows *sql.Rows) ([]domain.Message, error) {
	var messages []domain.Message
	for rows.Next() {
		var m domain.Message
		var userID, claimID sql.NullString
		var content, subtype, toolCallsStr, toolCallID, metadataStr, model sql.NullString
		var inputTok, outputTok, cacheReadTok, cacheCreateTok sql.NullInt64
		var costUSD sql.NullFloat64
		var reasoningStr, contentBlocksStr sql.NullString
		var delivered bool
		var windowState string
		var seq sql.NullFloat64
		var durationMs sql.NullInt64
		var stopReason sql.NullString

		if err := rows.Scan(
			&m.ID, &m.ConversationID, &userID, &claimID, &m.Role, &content, &subtype, &toolCallsStr,
			&toolCallID, &m.IsError, &metadataStr, &model,
			&inputTok, &outputTok, &cacheReadTok, &cacheCreateTok, &costUSD, &m.CreatedAt,
			&reasoningStr, &contentBlocksStr, &delivered, &windowState, &seq, &durationMs, &stopReason,
		); err != nil {
			return nil, err
		}

		m.UserID = userID.String
		m.StopReason = stopReason.String
		m.ClaimID = claimID.String
		m.Content = content.String
		m.Subtype = subtype.String
		m.ToolCallID = toolCallID.String
		m.Model = model.String

		if toolCallsStr.Valid {
			_ = json.Unmarshal([]byte(toolCallsStr.String), &m.ToolCalls)
		}
		if metadataStr.Valid {
			_ = json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
		}
		// Unlike tool_calls/metadata above, reasoning/content_blocks are part
		// of the canonical replay context a native loop reconstructs via
		// ListForAssembly — a decode failure here must surface, not silently
		// yield an empty Reasoning/ContentBlocks that looks like "no
		// reasoning on this message" when the row actually has some.
		if reasoningStr.Valid {
			if err := json.Unmarshal([]byte(reasoningStr.String), &m.Reasoning); err != nil {
				return nil, fmt.Errorf("unmarshal reasoning (message %d): %w", m.ID, err)
			}
		}
		if contentBlocksStr.Valid {
			if err := json.Unmarshal([]byte(contentBlocksStr.String), &m.ContentBlocks); err != nil {
				return nil, fmt.Errorf("unmarshal content_blocks (message %d): %w", m.ID, err)
			}
		}
		if inputTok.Valid {
			v := int(inputTok.Int64)
			m.InputTokens = &v
		}
		if outputTok.Valid {
			v := int(outputTok.Int64)
			m.OutputTokens = &v
		}
		if cacheReadTok.Valid {
			v := int(cacheReadTok.Int64)
			m.CacheReadTokens = &v
		}
		if cacheCreateTok.Valid {
			v := int(cacheCreateTok.Int64)
			m.CacheCreationTokens = &v
		}
		if costUSD.Valid {
			v := costUSD.Float64
			m.CostUSD = &v
		}
		// Read back the concrete stored value (the column is NOT NULL) —
		// unlike on insert, nil here would just mean "unknown", which is
		// never the case for a persisted row.
		deliveredVal := delivered
		m.Delivered = &deliveredVal
		m.WindowState = domain.MessageWindowState(windowState)
		if seq.Valid {
			v := seq.Float64
			m.Seq = &v
		}
		if durationMs.Valid {
			v := int(durationMs.Int64)
			m.DurationMs = &v
		}

		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// Messages is the whole-transcript display read — MessagesSince from the
// bottom. id is AUTOINCREMENT, so no row can be at or below 0 and the
// watermark drops out: routing both through one query is what makes the
// two reads' visibility filter identical rather than merely matching.
func (s *conversationStore) Messages(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	return s.MessagesSince(ctx, orgID, conversationID, 0)
}

func (s *conversationStore) MessagesSince(ctx context.Context, orgID, conversationID string, sinceID int) ([]domain.Message, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Withdrawn-pending rows (undelivered + inactive — a staged injection
	// that was withdrawn before any flush) never happened, so the display
	// read hides them. delivered + inactive stays visible: that is compacted
	// history, still part of the rendered transcript.
	//
	// Ordered by the same effective assembly key ListForAssembly uses, so a
	// row placed out of insertion order renders where the model read it. The
	// watermark stays on id: it answers "which rows has this client not seen
	// yet", which is an insertion question, not a placement one.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteMessageColumns+`
		FROM messages
		WHERE conversation_id = ?
		  AND id > ?
		  AND NOT (delivered = 0 AND window_state = 'inactive')
		ORDER BY COALESCE(seq, id) ASC
	`, conversationID, sinceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

func (s *conversationStore) MessagesWindow(ctx context.Context, orgID, conversationID string, w db.MessageWindow) ([]domain.Message, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Same visibility filter and same placement ordering as MessagesSince —
	// see its comment. What differs is the window: a backward page selects
	// the NEWEST rows below a ceiling, which means ordering DESC to pick them
	// and reversing afterwards so the caller always receives oldest-first.
	where := `conversation_id = ? AND NOT (delivered = 0 AND window_state = 'inactive')`
	args := []any{conversationID}
	order := `ORDER BY COALESCE(seq, id) ASC`
	reverse := false
	switch {
	case w.SinceID > 0:
		where += ` AND id > ?`
		args = append(args, w.SinceID)
	case w.BeforeID > 0:
		where += ` AND id < ?`
		args = append(args, w.BeforeID)
		order = `ORDER BY COALESCE(seq, id) DESC`
		reverse = true
	default:
		if w.Limit > 0 {
			order = `ORDER BY COALESCE(seq, id) DESC`
			reverse = true
		}
	}
	query := `SELECT ` + sqliteMessageColumns + ` FROM messages WHERE ` + where + ` ` + order
	if w.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, w.Limit)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanMessageRows(rows)
	if err != nil {
		return nil, err
	}
	if reverse {
		slices.Reverse(out)
	}
	return out, nil
}

func (s *conversationStore) MessagesForConversations(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Message, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	// ?-placeholder IN list (SQLite has no array bind), mirroring
	// artifactStore.ListByConversations, chunked to stay inside SQLite's variable
	// limit (chunkIDs). A conversation_id falls in exactly one chunk, so
	// ordering on (conversation_id, the effective assembly key) keeps each
	// run's messages contiguous within the merged slice and in the same
	// order the single-run display read gives them; the caller groups by
	// ConversationID.
	var messages []domain.Message
	for _, chunk := range chunkIDs(conversationIDs) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		// Withdrawn-pending rows are hidden, same as Messages.
		rows, err := s.q.QueryContext(ctx, `
			SELECT `+sqliteMessageColumns+`
			FROM messages
			WHERE conversation_id IN (`+strings.Join(placeholders, ", ")+`)
			  AND NOT (delivered = 0 AND window_state = 'inactive')
			ORDER BY conversation_id ASC, COALESCE(seq, id) ASC
		`, args...)
		if err != nil {
			return nil, err
		}
		batch, err := scanMessageRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		messages = append(messages, batch...)
	}
	return messages, nil
}

// ListForAssemblySystem returns every row a native loop needs to rebuild this run's
// exact LLM context, ordered by the effective assembly key COALESCE(seq, id).
// window_state='inactive' rows are excluded (superseded by compaction);
// 'elided' and undelivered rows are included — see the interface doc for the
// full contract. Pure read over messages; no other table or in-process
// state feeds in.
func (s *conversationStore) ListForAssemblySystem(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteMessageColumns+`
		FROM messages
		WHERE conversation_id = ? AND window_state <> 'inactive'
		ORDER BY COALESCE(seq, id) ASC
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

// markDelivered flips delivered=true on the given message ids, scoped to
// conversationID, stamping subtype in the same statement when non-empty. ids outside
// the run or already delivered are silently unaffected. Reached through
// MarkDeliveredForClaimSystem — the flush is an engagement write, unfenced
// on this dialect (see the claim-fence block above).
func (s *conversationStore) markDelivered(ctx context.Context, orgID, conversationID string, ids []int, subtype string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	// NULLIF keeps the stamp optional without forking the statement: an
	// empty subtype leaves each row's own value in place.
	for start := 0; start < len(ids); start += inListChunkSize {
		end := start + inListChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+2)
		args = append(args, subtype, conversationID)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		if _, err := s.q.ExecContext(ctx, `
			UPDATE messages SET delivered = 1, subtype = COALESCE(NULLIF(?, ''), subtype)
			WHERE conversation_id = ? AND id IN (`+strings.Join(placeholders, ",")+`)
		`, args...); err != nil {
			return err
		}
	}
	return nil
}

// CompactForClaimSystem commits one compaction atomically — see the interface
// doc for the full contract. Unfenced on this dialect like every ForClaim
// write (see the claim-fence block above); the claim id is attribution.
func (s *conversationStore) CompactForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(q queryer) error {
		txStore := &conversationStore{q: q}
		if replyRow != nil {
			replyRow.ConversationID = conversationID
			replyRow.ClaimID = claimID
			replyRow.WindowState = domain.MessageWindowInactive
			id, err := txStore.InsertMessage(ctx, orgID, replyRow)
			if err != nil {
				return fmt.Errorf("insert compaction reply row: %w", err)
			}
			replyRow.ID = int(id)
		}
		resultRow.ConversationID = conversationID
		resultRow.ClaimID = claimID
		resultID, err := txStore.InsertMessage(ctx, orgID, resultRow)
		if err != nil {
			return fmt.Errorf("insert compaction result row: %w", err)
		}
		resultRow.ID = int(resultID)

		for _, chunk := range chunkIDs(intIDsToStrings(inactiveIDs)) {
			placeholders := make([]string, len(chunk))
			args := make([]any, 0, len(chunk)+1)
			args = append(args, conversationID)
			for i, id := range chunk {
				placeholders[i] = "?"
				args = append(args, id)
			}
			if _, err := q.ExecContext(ctx, `
				UPDATE messages SET window_state = 'inactive'
				WHERE conversation_id = ? AND id IN (`+strings.Join(placeholders, ",")+`)
			`, args...); err != nil {
				return fmt.Errorf("flip compacted span: %w", err)
			}
		}

		rows, err := q.QueryContext(ctx, `
			SELECT id FROM messages
			WHERE conversation_id = ? AND delivered = 0 AND COALESCE(seq, id) < ?
			ORDER BY COALESCE(seq, id) ASC
		`, conversationID, float64(resultID))
		if err != nil {
			return fmt.Errorf("read queued rows for re-seq: %w", err)
		}
		queued, err := scanIntIDs(rows)
		if err != nil {
			return err
		}
		for i, id := range queued {
			seq := float64(resultID) + float64(i+1)/float64(len(queued)+1)
			if _, err := q.ExecContext(ctx, `
				UPDATE messages SET seq = ? WHERE conversation_id = ? AND id = ?
			`, seq, conversationID, id); err != nil {
				return fmt.Errorf("re-seq queued row %d: %w", id, err)
			}
		}
		return nil
	})
}

func intIDsToStrings(ids []int) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.Itoa(id)
	}
	return out
}

func scanIntIDs(rows *sql.Rows) ([]int, error) {
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SettleCompactionRequestForClaimSystem records a discarded warm attempt on
// the request row — see the interface doc. Unfenced on this dialect.
func (s *conversationStore) SettleCompactionRequestForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE messages
		SET input_tokens = ?, output_tokens = ?,
		    cache_read_tokens = ?, cache_creation_tokens = ?,
		    cost_usd = ?,
		    metadata = json_set(COALESCE(metadata, '{}'), '$.compaction_failure', ?)
		WHERE conversation_id = ? AND id = ?
	`, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens,
		sqliteNullFloat(costUSD), reason, conversationID, requestID)
	return err
}

// SetWindowStateSystem is the elision/compaction primitive: a batched range flip of
// window_state from `from` to `to`, restricted to rows currently in state
// `from` whose effective assembly key (COALESCE(seq, id)) is strictly less
// than beforeSeq. Returns the number of rows flipped.
func (s *conversationStore) SetWindowStateSystem(ctx context.Context, orgID, conversationID string, beforeSeq float64, from, to domain.MessageWindowState) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE messages
		SET window_state = ?
		WHERE conversation_id = ? AND window_state = ? AND COALESCE(seq, id) < ?
	`, string(to), conversationID, string(from), beforeSeq)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

func (s *conversationStore) TokenTotalsSystem(ctx context.Context, orgID, conversationID string) (*domain.TokenTotals, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(model), ''),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COUNT(*)
		FROM messages
		WHERE conversation_id = ? AND role = 'assistant'
	`, conversationID)

	var t domain.TokenTotals
	if err := row.Scan(&t.Model, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheCreationTokens, &t.NumTurns); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *conversationStore) BlueprintSiblingCostUSDSystem(ctx context.Context, orgID, blueprintRunID, excludeConversationID string) (float64, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var cost sql.NullFloat64
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(m.cost_usd), 0)
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE c.blueprint_run_id = ? AND c.id <> ?
	`, blueprintRunID, excludeConversationID).Scan(&cost)
	if err != nil {
		return 0, err
	}
	return cost.Float64, nil
}

func (s *conversationStore) BlueprintSiblingDurationMsSystem(ctx context.Context, orgID, blueprintRunID, excludeConversationID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var ms sql.NullInt64
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cl.duration_ms), 0)
		FROM claims cl
		JOIN conversations c ON c.id = cl.conversation_id
		WHERE c.blueprint_run_id = ? AND c.id <> ?
	`, blueprintRunID, excludeConversationID).Scan(&ms)
	if err != nil {
		return 0, err
	}
	return int(ms.Int64), nil
}

// --- Helpers ---

func scanConversation(row *sql.Row, r *domain.Conversation) error {
	var queuedAt, completedAt sql.NullTime
	var claimedAt sql.NullString
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var parkReason, worktreePath, model, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, creatorUserID, executorID, blueprintRunID sql.NullString

	if err := row.Scan(
		&r.ID, &r.TaskID, &r.Runtime, &r.Status, &model, &r.StartedAt, &queuedAt, &claimedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &parkReason, &worktreePath,
		&resultSummary, &outcome, &outcomeReason, &failureKind, &sessionID, &actorAgentID, &r.TriggerType, &creatorUserID, &r.TeamID, &executorID, &r.Attempts, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	); err != nil {
		return err
	}
	return finalizeConversation(r, queuedAt, claimedAt, completedAt, costUSD, durationMs, numTurns, blueprintStep,
		model, parkReason, worktreePath, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID)
}

func scanConversationRows(rows *sql.Rows, r *domain.Conversation) error {
	var queuedAt, completedAt sql.NullTime
	var claimedAt sql.NullString
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var parkReason, worktreePath, model, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, creatorUserID, executorID, blueprintRunID sql.NullString

	if err := rows.Scan(
		&r.ID, &r.TaskID, &r.Runtime, &r.Status, &model, &r.StartedAt, &queuedAt, &claimedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &parkReason, &worktreePath,
		&resultSummary, &outcome, &outcomeReason, &failureKind, &sessionID, &actorAgentID, &r.TriggerType, &creatorUserID, &r.TeamID, &executorID, &r.Attempts, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	); err != nil {
		return err
	}
	return finalizeConversation(r, queuedAt, claimedAt, completedAt, costUSD, durationMs, numTurns, blueprintStep,
		model, parkReason, worktreePath, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID)
}

func finalizeConversation(r *domain.Conversation, queuedAt sql.NullTime, claimedAt sql.NullString, completedAt sql.NullTime, costUSD sql.NullFloat64,
	durationMs, numTurns, blueprintStep sql.NullInt64,
	model, parkReason, worktreePath, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID sql.NullString) error {
	r.Model = model.String
	r.ParkReason = domain.ParkReason(parkReason.String)
	r.WorktreePath = worktreePath.String
	r.ResultSummary = resultSummary.String
	r.Outcome = outcome.String
	r.OutcomeReason = outcomeReason.String
	r.FailureKind = domain.ConversationFailureKind(failureKind.String)
	r.SessionID = sessionID.String
	r.ActorAgentID = actorAgentID.String
	r.CreatorUserID = creatorUserID.String
	r.ExecutorID = executorID.String
	if blueprintRunID.Valid {
		r.BlueprintRunID = blueprintRunID.String
	}
	if blueprintStep.Valid {
		v := int(blueprintStep.Int64)
		r.BlueprintStepIndex = &v
	}
	if queuedAt.Valid {
		r.QueuedAt = &queuedAt.Time
	}
	// ClaimedAt derives from a claims subselect, which loses the declared
	// column type, so the driver hands back raw text — parse the mixed
	// on-disk formats the same way the factory projection does.
	if claimedAt.Valid && claimedAt.String != "" {
		at, err := parseDBDatetime(claimedAt.String)
		if err != nil {
			return fmt.Errorf("parse claimed_at %q: %w", claimedAt.String, err)
		}
		r.ClaimedAt = &at
	}
	if completedAt.Valid {
		r.CompletedAt = &completedAt.Time
	}
	if costUSD.Valid {
		r.TotalCostUSD = &costUSD.Float64
	}
	if durationMs.Valid {
		v := int(durationMs.Int64)
		r.DurationMs = &v
	}
	if numTurns.Valid {
		v := int(numTurns.Int64)
		r.NumTurns = &v
	}
	return nil
}

// nullIfEmpty maps "" to a SQL NULL bind. Local mirror of the
// helper that lives in postgres/conversation.go; the two impls are
// independent so neither imports the other's private helpers.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// sqliteNullStr / sqliteNullInt produce typed NullX values for the
// messages INSERT — pre-D2 these lived as nullStr / nullInt on
// internal/db/agent.go. Renamed with the sqlite prefix here to make
// it obvious this is the SQLite path's flavor (the *sql.NullX
// concrete types are SQLite-idiomatic; pgx accepts the same types
// but the Postgres impl uses raw any for clarity).
func sqliteNullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func sqliteNullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

func sqliteNullInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}
