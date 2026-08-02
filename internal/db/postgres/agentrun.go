package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/wakebus"
)

// agentRunStore is the Postgres impl of db.ConversationStore over the
// conversations + messages + claims tables. Holds two pools (see
// postgres.New): `q` is the app pool (or a *sql.Tx composed from it via
// WithTx) — every request-equivalent path runs here, RLS-active under
// tf_app. `admin` is the supabase_admin BYPASSRLS pool. Claims are a
// system-written table (tf_app holds SELECT only), so every claim release /
// stamp routes through `admin` regardless of which pool flipped the
// conversation; on the `...System` lifecycle variants both writes share one
// admin-pool transaction, while the app-pool variants pair an RLS-scoped
// conversation flip with an adjacent admin-side release.
//
// SQL is written against the conversations-era schema: org_id in every
// WHERE clause as defense in depth alongside RLS, $N placeholders, JSONB
// extraction for tool_calls / metadata, RETURNING id for the
// messages auto-increment (Postgres has a sequence, not AUTOINCREMENT).
var agentRunLog = logging.Component("db/agentrun")

type agentRunStore struct {
	q     queryer
	admin queryer
}

func newConversationStore(q, admin queryer) db.ConversationStore {
	return &agentRunStore{q: q, admin: admin}
}

var _ db.ConversationStore = (*agentRunStore)(nil)

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
// already released (requeue, boot sweep). q must be admin-backed: tf_app
// has no UPDATE grant on claims.
//
// On the non-System paths this release is deliberately ADJACENT to the
// app-pool conversation flip rather than atomic with it: tf_app's
// SELECT-only posture on claims is the structural guarantee that no
// request-path tx can write claims, and it is worth more than
// single-statement atomicity here. The two commits can therefore land
// without each other — a rolled-back outer tx leaves an in-flight
// conversation with no active claim, a crash between the commits leaves a
// terminal conversation with a dangling one — and both shapes are healed by
// the claim-desync janitor arms (RunQueueStore.ReconcileOrphanedRuns at
// boot in both modes; the leader reaper's HealClaimDesyncs every tick), so
// neither can strand a row past a sweep.
func releaseActiveClaim(ctx context.Context, q queryer, orgID, runID, outcome string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = now(), outcome = $1
		WHERE org_id = $2 AND conversation_id = $3 AND released_at IS NULL
	`, outcome, orgID, runID)
	return err
}

// Complete pairs the app-pool status flip + cost-lump stamp with an
// adjacent admin-pool claim release — non-atomic by design; see
// releaseActiveClaim for why the split is acceptable (the janitor arms make
// both crash shapes self-healing). The cost stamp rides the app pool: the
// messages ledger is app-writable (tf_app holds UPDATE) and the row is the
// caller's own conversation.
func (s *agentRunStore) Complete(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	if err := completeRun(ctx, s.q, orgID, runID, status, costUSD, stopReason, resultSummary, outcome, outcomeReason, failureKind); err != nil {
		return err
	}
	return releaseActiveClaimWithTelemetry(ctx, s.admin, orgID, runID, claimOutcomeForStatus(status), durationMs, numTurns)
}

func (s *agentRunStore) CompleteSystem(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	return inTx(ctx, s.admin, func(q queryer) error {
		if err := completeRun(ctx, q, orgID, runID, status, costUSD, stopReason, resultSummary, outcome, outcomeReason, failureKind); err != nil {
			return err
		}
		return releaseActiveClaimWithTelemetry(ctx, q, orgID, runID, claimOutcomeForStatus(status), durationMs, numTurns)
	})
}

// CompleteForClaimSystem is CompleteSystem with the fence in front of it, in
// the same transaction as both the status flip and the release. The claim it
// releases is resolved the same way CompleteSystem resolves it (the
// conversation's active claim) — which the fence has just proven to be
// claimID, since only one claim per conversation can be unreleased at a time.
func (s *agentRunStore) CompleteForClaimSystem(ctx context.Context, orgID, runID, claimID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	return inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, runID, claimID); err != nil {
			return err
		}
		if err := completeRun(ctx, q, orgID, runID, status, costUSD, stopReason, resultSummary, outcome, outcomeReason, failureKind); err != nil {
			return err
		}
		return releaseActiveClaimWithTelemetry(ctx, q, orgID, runID, claimOutcomeForStatus(status), durationMs, numTurns)
	})
}

func completeRun(ctx context.Context, q queryer, orgID, runID, status string, costUSD float64, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	// The conversation carries no accounting cache — cost settles as one
	// lump on the invocation's last message row (the ledger) below. A
	// resume's Complete stamps its own invocation's lump on its own last
	// row, so nothing accumulates or doubles.
	_, err := q.ExecContext(ctx, `
		UPDATE conversations
		SET status = $1,
		    completed_at = $2,
		    stop_reason = $3,
		    result_summary = $4,
		    outcome = NULLIF($5, ''),
		    outcome_reason = NULLIF($6, ''),
		    failure_kind = NULLIF($7, '')
		WHERE org_id = $8 AND id = $9
	`, status, time.Now(), stopReason, resultSummary, outcome, outcomeReason, failureKind, orgID, runID)
	if err != nil {
		return err
	}
	// The active claim this terminal write releases identifies the
	// engagement's own message rows (they insert claim-stamped), so the
	// lump settles claim-keyed — the curator turn release's shape. Read
	// before the release below: a released claim is no longer findable.
	var claimID string
	err = q.QueryRowContext(ctx, `
		SELECT id FROM claims
		WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
	`, orgID, runID).Scan(&claimID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// A zero lump settles nothing, in either arm. Zero means the runtime had
	// nothing to report at terminal time: the native loop settles spend per
	// assistant row as it goes, and overwriting its newest stamp with 0 would
	// erase real recorded dollars. An SDK invocation that reports zero leaves
	// its rows NULL — price unknown — rather than asserting the run was
	// genuinely free.
	if claimID != "" && costUSD != 0 {
		// Overwrite, not add: the engagement's newest claim-attributed row
		// is its own fresh row, and the lump is that invocation's whole
		// total. Runtime-composed rows are skipped as targets — an errored
		// or interrupted invocation ends on one, and settling there would
		// bill the whole invocation to a model that never ran. An
		// engagement whose rows are all synthetic falls through to the
		// conversation-wide arm below.
		//
		// Among what's left, a row that names a model wins over one that
		// names none (a user or tool row) however much newer the latter is:
		// only the first keeps the lump in the per-model breakdown, and
		// this engagement did run that model. NULL is not "some other
		// model" here — the comparison has to test IS NOT NULL explicitly,
		// since every <>/IS DISTINCT FROM test against the sentinel admits
		// NULL rows into the same tier as real ones.
		res, err := q.ExecContext(ctx, `
			UPDATE messages SET cost_usd = $1
			WHERE org_id = $2 AND conversation_id = $3
			  AND id = (SELECT id FROM messages
			            WHERE claim_id = $4 AND model IS DISTINCT FROM $5
			            ORDER BY (model IS NOT NULL AND model <> $5) DESC, id DESC
			            LIMIT 1)
		`, costUSD, orgID, runID, claimID, domain.ModelSynthetic)
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
	}
	if costUSD == 0 {
		return nil
	}
	// An invocation can bill real tokens while streaming zero rows of its
	// own (system-prompt/cache overhead on an errored run), and the
	// messages ledger is the only spend record — so the lump settles on the
	// conversation's newest existing row rather than being dropped. ADDITIVE,
	// unlike the overwrite above: that row may already carry an earlier
	// invocation's lump. The caveat: a rowless resume's spend lands on an
	// older invocation's row — totals stay exact, per-row time attribution
	// smears; accepted for this narrow corner.
	//
	// The ORDER BY ranks rows in three tiers, newest-first within each, and
	// takes the first: a row naming a real model, else one naming no model
	// (a user or tool row), else a runtime-composed one. Both keys are
	// needed — the first alone would tie NULL with real, the second alone
	// would tie NULL with synthetic — and neither may be written as a bare
	// <> against the sentinel, which evaluates to NULL (falsy, so the row
	// sinks a tier) for exactly the NULL rows the middle tier is about.
	// The bottom tier keeps the dollars on the ledger when a conversation
	// is nothing but synthetic rows; the model breakdowns exclude them, so
	// the spend shows in the totals without inventing a model.
	res, err := q.ExecContext(ctx, `
		UPDATE messages SET cost_usd = COALESCE(cost_usd, 0) + $1
		WHERE org_id = $2 AND conversation_id = $3
		  AND id = (SELECT id FROM messages
		            WHERE org_id = $2 AND conversation_id = $3
		            ORDER BY (model IS NOT NULL AND model <> $4) DESC,
		                     (model IS DISTINCT FROM $4) DESC,
		                     id DESC
		            LIMIT 1)
	`, costUSD, orgID, runID, domain.ModelSynthetic)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// No message rows at all: the spend has no ledger row to live on.
		// Loud, so the dropped dollars are at least observable.
		agentRunLog.Warn("run cost has no message row to settle on; spend unrecorded",
			"conversation_id", runID, "org_id", orgID, "cost_usd", costUSD)
	}
	return nil
}

// releaseActiveClaimWithTelemetry is releaseActiveClaim plus the terminal
// duration/turns stamp — the engagement telemetry the runtime reports per
// invocation, which lives on the claim because messages can't derive it.
func releaseActiveClaimWithTelemetry(ctx context.Context, q queryer, orgID, runID, outcome string, durationMs, numTurns int) error {
	_, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = now(), outcome = $1, duration_ms = $2, num_turns = $3
		WHERE org_id = $4 AND conversation_id = $5 AND released_at IS NULL
	`, outcome, durationMs, numTurns, orgID, runID)
	return err
}

// ParkOpen's release is adjacent, not atomic — see releaseActiveClaim. An
// 'open' row with a dangling claim needs no janitor arm of its own: the resume
// flip (MarkQueuedForResume) releases any active claim on its way back to the
// queue.
func (s *agentRunStore) ParkOpen(ctx context.Context, orgID, runID string, park db.Park) (bool, error) {
	flipped, err := parkOpen(ctx, s.q, orgID, runID, park)
	if err != nil || !flipped {
		return flipped, err
	}
	return true, releaseActiveClaim(ctx, s.admin, orgID, runID, park.ClaimOutcome())
}

func (s *agentRunStore) ParkOpenSystem(ctx context.Context, orgID, runID string, park db.Park) (bool, error) {
	var flipped bool
	err := inTx(ctx, s.admin, func(q queryer) error {
		var err error
		flipped, err = parkOpen(ctx, q, orgID, runID, park)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, orgID, runID, park.ClaimOutcome())
	})
	return flipped, err
}

// ParkOpenForClaimSystem is ParkOpenSystem behind the fence — the self-park an
// executor writes when its own run's ctx is killed. Its unfenced twin serves
// the user-initiated cancel, which is deliberately not gated on ownership.
func (s *agentRunStore) ParkOpenForClaimSystem(ctx context.Context, orgID, runID, claimID string, park db.Park) (bool, error) {
	var flipped bool
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, runID, claimID); err != nil {
			return err
		}
		var err error
		flipped, err = parkOpen(ctx, q, orgID, runID, park)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, orgID, runID, park.ClaimOutcome())
	})
	if err != nil {
		return false, err
	}
	return flipped, nil
}

// parkOpen is the one row-write behind every park — see
// ConversationStore.ParkOpen for what `park` decides.
//
// The exclusion list is the settled set, and the guard is an exclusion — so a
// status missing from it doesn't refuse, it readmits. An `open` row with
// undelivered input is claimable, so a park that "succeeded" on a finished run
// would hand it back to the dispatcher.
//
// COALESCE on stop_reason / result_summary rather than a bare assignment: an
// idle park carries neither and must not blank what an earlier deliberate stop
// recorded.
func parkOpen(ctx context.Context, q queryer, orgID, runID string, park db.Park) (bool, error) {
	// A deliberate stop re-parks an already-parked row; an idle turn-end does
	// not. Spelled as an extra excluded status rather than two queries.
	reparkGuard := `, 'open'`
	if park.Deliberate() {
		reparkGuard = ``
	}
	res, err := q.ExecContext(ctx, `
		UPDATE conversations
		SET status = 'open',
		    parked_at = COALESCE(parked_at, $1),
		    stop_reason = COALESCE(NULLIF($2, ''), stop_reason),
		    result_summary = COALESCE(NULLIF($3, ''), result_summary)
		WHERE org_id = $4 AND id = $5
		  AND (status IS NULL
		       OR status NOT IN (`+runTerminalStatusesSQL+reparkGuard+`))
	`, time.Now().UTC(), park.StopReason, park.ResultSummary, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkQueuedForResume is resume-by-enqueue's un-terminal write: the ONE
// path that puts an outcome-bearing conversation back into the mid-flight
// (status NULL) state the needs-driving predicate claims from. Its guard is
// what keeps a terminal conversation un-claimable by anything else, whatever
// rows it holds. Always claims-scoped — resume is always user-initiated, so
// there is no admin-pool "...System" variant. The active claim releases as
// 'requeued' (ownership is re-established when ClaimNextRun mints a fresh
// claim, exactly like a fresh EnqueueRun'd row). preferred_executor_id is
// cleared: a long-parked run's old stamp is exactly the outlives-a-dwell
// case placement calls out — NULL re-queues it as unowned/
// immediately-claimable, and the claiming executor re-warms and re-earns
// affinity on the next enqueue. queued_at is NOT re-stamped: it records when
// the conversation first entered the queue and is display-only (the
// scheduler orders by started_at).
func (s *agentRunStore) MarkQueuedForResume(ctx context.Context, orgID, runID string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE conversations SET status = NULL,
		                parked_at = NULL, preferred_executor_id = NULL
		WHERE org_id = $1 AND id = $2
		  AND (status = 'open'
		       OR (status = 'completed' AND outcome = 'abort'))
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	flipped := n > 0
	if flipped {
		if err := releaseActiveClaim(ctx, s.admin, orgID, runID, "requeued"); err != nil {
			return false, err
		}
		// tf_wake doorbell: resume-by-enqueue re-queues an
		// EXISTING row rather than inserting one, so it needs its own
		// notify — RunQueueStore.EnqueueRun's doesn't fire for this path.
		// Best-effort, same "never the only path" contract as there.
		_ = wakebus.Publish(ctx, s.q, wakebus.KindEvent, orgID)
	}
	return flipped, nil
}

func (s *agentRunStore) ListReapableSnapshotKeysSystem(ctx context.Context, cutoff time.Time) ([]domain.SnapshotReapKey, error) {
	// Snapshot-bearing runs (parked `open` / any `completed` terminal) grouped by
	// their shared snapshot key (org, blueprint_run_id); a key is reapable once
	// its newest such run last parked or concluded before the cutoff. The
	// timestamp is COALESCE(parked_at, completed_at, started_at): parked_at for an
	// open run (re-stamped each park, so resumes don't age it), completed_at for a
	// terminal, started_at a legacy fallback. Admin pool — the retention sweep is
	// a tenant-spanning system job with no JWT claims.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT org_id, blueprint_run_id
		FROM conversations
		WHERE status IN ('open', 'completed')
		GROUP BY org_id, blueprint_run_id
		HAVING MAX(COALESCE(parked_at, completed_at, started_at)) < $1
	`, cutoff)
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

func (s *agentRunStore) SetSession(ctx context.Context, orgID, runID, sessionID string) error {
	return setRunSession(ctx, s.q, orgID, runID, sessionID)
}

func (s *agentRunStore) SetSessionSystem(ctx context.Context, orgID, runID, sessionID string) error {
	return setRunSession(ctx, s.admin, orgID, runID, sessionID)
}

func setRunSession(ctx context.Context, q queryer, orgID, runID, sessionID string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE conversations SET sdk_session_id = $1 WHERE org_id = $2 AND id = $3
	`, sessionID, orgID, runID)
	return err
}

func (s *agentRunStore) SetExecutorSystem(ctx context.Context, orgID, runID, executorID string, bootEpoch int64) error {
	// An empty executorID keeps the legacy clear semantics: the live
	// engagement is over, so its claim releases as requeued.
	if executorID == "" {
		return releaseActiveClaim(ctx, s.admin, orgID, runID, "requeued")
	}
	// Idempotent go-live confirmation: update the active claim's identity
	// if one exists, mint one if none does — the live process must never
	// run unattributed.
	return inTx(ctx, s.admin, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET executor_id = $1, boot_epoch = $2
			WHERE org_id = $3 AND conversation_id = $4 AND released_at IS NULL
		`, executorID, bootEpoch, orgID, runID)
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
			INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch, claimed_at)
			VALUES ($1, $2, $3, $4, now())
		`, orgID, runID, executorID, bootEpoch)
		return err
	})
}

// SetActiveClaimPhaseSystem scopes the write to the ACTIVE claim only: a
// released claim's phase is inert history, and a conversation with no live
// engagement has no sub-state to record — both fall through as a no-op.
func (s *agentRunStore) SetActiveClaimPhaseSystem(ctx context.Context, orgID, runID, phase string) error {
	_, err := s.admin.ExecContext(ctx, `
		UPDATE claims SET phase = NULLIF($1, '')
		WHERE org_id = $2 AND conversation_id = $3 AND released_at IS NULL
	`, phase, orgID, runID)
	return err
}

// SetClaimPhaseSystem is the claim-keyed phase write, fenced: an engagement
// reporting its own setup progress must still own the conversation. Without
// the fence a zombie's stale phase would land on whatever claim happened to
// be active — the successor's — and surface as its setup sub-state on every
// display read.
func (s *agentRunStore) SetClaimPhaseSystem(ctx context.Context, orgID, conversationID, claimID, phase string) error {
	return inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `
			UPDATE claims SET phase = NULLIF($1, '')
			WHERE org_id = $2 AND id = $3 AND conversation_id = $4
		`, phase, orgID, claimID, conversationID)
		return err
	})
}

// RecordClaimSandboxStatsSystem is keyed on the claim id alone (org bound as
// defense in depth) with NO released_at predicate — the teardown that
// measures these numbers runs after the claim is released.
func (s *agentRunStore) RecordClaimSandboxStatsSystem(ctx context.Context, orgID, claimID string, peakMemMB *int, cpuUsec *int64) error {
	_, err := s.admin.ExecContext(ctx, `
		UPDATE claims SET peak_mem_mb = $1, cpu_usec = $2
		WHERE org_id = $3 AND id = $4
	`, nullIntPtr(peakMemMB), nullInt64Ptr(cpuUsec), orgID, claimID)
	return err
}

func (s *agentRunStore) SetWorktreePath(ctx context.Context, orgID, runID, path string) error {
	return setRunWorktreePath(ctx, s.q, orgID, runID, path)
}

func (s *agentRunStore) SetWorktreePathSystem(ctx context.Context, orgID, runID, path string) error {
	return setRunWorktreePath(ctx, s.admin, orgID, runID, path)
}

func setRunWorktreePath(ctx context.Context, q queryer, orgID, runID, path string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE conversations SET worktree_path = $1 WHERE org_id = $2 AND id = $3
	`, path, orgID, runID)
	return err
}

// MarkFailedIfActive's release is adjacent, not atomic — see
// releaseActiveClaim for the crash shapes and the janitor arms that heal
// them.
func (s *agentRunStore) MarkFailedIfActive(ctx context.Context, orgID, runID, failureKind string) (bool, error) {
	flipped, err := markFailedIfActive(ctx, s.q, orgID, runID, failureKind)
	if err != nil || !flipped {
		return flipped, err
	}
	return true, releaseActiveClaim(ctx, s.admin, orgID, runID, "failed")
}

func (s *agentRunStore) MarkFailedIfActiveSystem(ctx context.Context, orgID, runID, failureKind string) (bool, error) {
	var flipped bool
	err := inTx(ctx, s.admin, func(q queryer) error {
		var err error
		flipped, err = markFailedIfActive(ctx, q, orgID, runID, failureKind)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, orgID, runID, "failed")
	})
	return flipped, err
}

// MarkFailedIfActiveForClaimSystem is MarkFailedIfActiveSystem behind the
// fence. The two negative answers stay distinct: ok=false is the guarded
// flip's own "somebody else reached the terminal first", ErrClaimReleased is
// "you are not the one who gets to decide".
func (s *agentRunStore) MarkFailedIfActiveForClaimSystem(ctx context.Context, orgID, runID, claimID, failureKind string) (bool, error) {
	var flipped bool
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, runID, claimID); err != nil {
			return err
		}
		var err error
		flipped, err = markFailedIfActive(ctx, q, orgID, runID, failureKind)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, orgID, runID, "failed")
	})
	if err != nil {
		return false, err
	}
	return flipped, nil
}

func markFailedIfActive(ctx context.Context, q queryer, orgID, runID, failureKind string) (bool, error) {
	// 'open' is deliberately failable here — see
	// ConversationStore.MarkFailedIfActive: a warm 'open' run has no durable
	// snapshot yet, so an infra error reaching failRun must terminate it.
	res, err := q.ExecContext(ctx, `
		UPDATE conversations SET status = 'failed', completed_at = COALESCE(completed_at, $1),
		    failure_kind = NULLIF($2, '')
		WHERE org_id = $3 AND id = $4
		  AND (status IS NULL
		       OR status NOT IN (`+runTerminalStatusesSQL+`))
	`, time.Now().UTC(), failureKind, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- Queries ---

// pgRunColumns is the SELECT list scanned into a domain.Conversation
// via scanConversation. Owned here on ConversationStore; sibling Postgres
// stores that need to project a run (e.g. factoryReadStore.ActiveRuns)
// already use their own copy because they also project task+entity
// JOINs. Keeping this here keeps the simple "just the run" projection
// uncoupled from those. task_id/status/team_id COALESCE to ” because a
// non-delegation conversation may carry NULLs there; the claim-derived
// fields (incl. duration/turns telemetry) come from the `cl` lateral and
// the ledger-derived accounting (cost + tokens) from the `msum` lateral
// (see runClaimLateral / runLedgerLateral). Status is the derived display
// ladder (pgDisplayStatusSQL) rather than the stored column.
const pgRunColumns = `
	r.id, COALESCE(r.task_id::text, ''), COALESCE(r.runtime, ''), ` + pgDisplayStatusSQL + `, COALESCE(r.model, ''), r.started_at, r.queued_at, cl.claimed_at, r.completed_at,
	msum.total_cost_usd, cl.duration_ms, cl.num_turns,
	COALESCE(r.stop_reason, ''), COALESCE(r.worktree_path, ''),
	COALESCE(r.result_summary, ''),
	COALESCE(r.outcome, ''), COALESCE(r.outcome_reason, ''),
	COALESCE(r.failure_kind, ''),
	COALESCE(r.sdk_session_id, ''),
	COALESCE(r.actor_agent_id::text, ''),
	COALESCE(r.trigger_type, ''),
	COALESCE(r.creator_user_id::text, ''),
	COALESCE(r.team_id::text, ''),
	COALESCE(cl.executor_id, ''),
	cl.attempts,
	r.blueprint_run_id, r.blueprint_step_index,
	msum.input_tokens, msum.output_tokens, msum.cache_read_tokens, msum.cache_creation_tokens,
	(NULLIF(BTRIM(rm.agent_content, E' \t\n\r'), '') IS NULL) AS memory_missing,
	COALESCE(a.display_name, '') AS actor_agent_name
`

// pgDisplayStatusSQL is the wire status: a four-rung ladder over state that
// is no longer stored. The active claim's setup sub-state wins
// (fetching/cloning/agent_starting/awaiting_credentials); then the mere
// existence of an active claim is 'running'; then a conversation matching
// the needs-driving predicate — mid-flight and unclaimed, or parked and
// woken by input — is 'queued'; and finally the stored column carries the
// deliberate park and the terminals. The trailing ” is unreachable by
// construction (a conversation is always in exactly one of those states) and
// exists so NULL can never reach the wire. Every state renders exactly the
// vocabulary the frozen wire contract already carried.
//
// Self-contained (correlated subqueries on the tiny active-claim and
// undelivered-message partial indexes) rather than reading a join alias, so
// every projection that needs a display status spells it the same way and
// they cannot drift. Requires only the conversation alias `r`.
const pgDisplayStatusSQL = `COALESCE(
		(SELECT cl_d.phase FROM claims cl_d
		 WHERE cl_d.conversation_id = r.id AND cl_d.released_at IS NULL),
		CASE WHEN ` + activeClaimExistsSQL + ` THEN 'running' END,
		CASE WHEN r.status IS NULL
		       OR (r.status = 'open' AND ` + undeliveredInputExistsSQL + `)
		     THEN 'queued' END,
		r.status,
		'')`

// runClaimLateral derives the claim-facing Conversation fields from claims:
// ClaimedAt is the latest claim's claimed_at, Attempts the count of
// engagements, ExecutorID and phase the active (unreleased) claim's, and
// duration_ms/num_turns the SUM of the per-engagement telemetry. The
// aggregate lateral always yields exactly one row, so a never-claimed
// conversation reads (NULL, 0, NULL, NULL, NULL, NULL).
const runClaimLateral = `
	LEFT JOIN LATERAL (
		SELECT MAX(c2.claimed_at) AS claimed_at,
		       COUNT(*)::int      AS attempts,
		       MAX(c2.executor_id) FILTER (WHERE c2.released_at IS NULL) AS executor_id,
		       MAX(c2.phase)       FILTER (WHERE c2.released_at IS NULL) AS phase,
		       SUM(c2.duration_ms)::bigint AS duration_ms,
		       SUM(c2.num_turns)::bigint   AS num_turns
		FROM claims c2
		WHERE c2.conversation_id = r.id
	) cl ON true
`

// runLedgerLateral derives the money/token accounting from the messages
// ledger: total_cost_usd is the SUM of the settlement stamps (NULL until
// anything settles — the frozen wire semantics of the former stored
// column), the token columns the SUM over every row. Always exactly one
// row, so a message-less conversation reads (NULL, 0, 0, 0, 0).
const runLedgerLateral = `
	LEFT JOIN LATERAL (
		SELECT SUM(m2.cost_usd)                            AS total_cost_usd,
		       COALESCE(SUM(m2.input_tokens), 0)::bigint          AS input_tokens,
		       COALESCE(SUM(m2.output_tokens), 0)::bigint         AS output_tokens,
		       COALESCE(SUM(m2.cache_read_tokens), 0)::bigint     AS cache_read_tokens,
		       COALESCE(SUM(m2.cache_creation_tokens), 0)::bigint AS cache_creation_tokens
		FROM messages m2
		WHERE m2.conversation_id = r.id AND m2.org_id = r.org_id
	) msum ON true
`

func (s *agentRunStore) Get(ctx context.Context, orgID, runID string) (*domain.Conversation, error) {
	return getRun(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) GetSystem(ctx context.Context, orgID, runID string) (*domain.Conversation, error) {
	return getRun(ctx, s.admin, orgID, runID)
}

func (s *agentRunStore) LookupOrgForRunSystem(ctx context.Context, runID string) (string, error) {
	var orgID string
	err := s.admin.QueryRowContext(ctx, `SELECT org_id::text FROM conversations WHERE id = $1`, runID).Scan(&orgID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return orgID, err
}

func getRun(ctx context.Context, q queryer, orgID, runID string) (*domain.Conversation, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+pgRunColumns+`
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		`+runClaimLateral+`
		`+runLedgerLateral+`
		WHERE r.org_id = $1 AND r.id = $2
	`, orgID, runID)

	var r domain.Conversation
	if err := scanConversation(row, &r); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func (s *agentRunStore) ListForTask(ctx context.Context, orgID, taskID string) ([]domain.Conversation, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgRunColumns+`
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		`+runClaimLateral+`
		`+runLedgerLateral+`
		WHERE r.org_id = $1 AND r.task_id = $2
		ORDER BY r.started_at DESC
	`, orgID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []domain.Conversation
	for rows.Next() {
		var r domain.Conversation
		if err := scanConversationRows(rows, &r); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *agentRunStore) ListForTasks(ctx context.Context, orgID string, taskIDs []string) ([]domain.Conversation, error) {
	// task_id is a uuid column: a non-UUID id (these are client-supplied on the
	// batched run-list path) would fail Postgres parsing with 22P02 → 500
	// before the row filter runs, so drop invalid ids up front and treat them
	// as "no rows" — the read-method convention in uuid.go.
	taskIDs = filterValidUUIDs(taskIDs)
	if len(taskIDs) == 0 {
		return nil, nil
	}
	// App pool (RLS-active): rows are team-scoped exactly like ListForTask.
	// task_id is a uuid column, so the slice binds as a uuid[] literal
	// through one $N (pgUUIDArray), like artifactStore.ListByRuns — not a
	// raw []string. Same projection as ListForTask; the caller groups the
	// flat result by run.TaskID.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgRunColumns+`
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		`+runClaimLateral+`
		`+runLedgerLateral+`
		WHERE r.org_id = $1 AND r.task_id = ANY($2)
		ORDER BY r.started_at DESC
	`, orgID, pgUUIDArray(taskIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []domain.Conversation
	for rows.Next() {
		var r domain.Conversation
		if err := scanConversationRows(rows, &r); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// HasActiveAutoRunForEntity: any non-terminal trigger_type='event' run on
// any task that belongs to the entity. Manual delegations are excluded.
// Used by the router's per-entity firing gate.
func (s *agentRunStore) HasActiveAutoRunForEntity(ctx context.Context, orgID, entityID string) (bool, error) {
	return hasActiveAutoRunForEntity(ctx, s.q, orgID, entityID)
}

func (s *agentRunStore) HasActiveAutoRunForEntitySystem(ctx context.Context, orgID, entityID string) (bool, error) {
	return hasActiveAutoRunForEntity(ctx, s.admin, orgID, entityID)
}

func hasActiveAutoRunForEntity(ctx context.Context, q queryer, orgID, entityID string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		WHERE r.org_id = $1
		  AND t.entity_id = $2
		  AND r.trigger_type = 'event'
		  AND (r.status IS NULL
		       OR r.status NOT IN (`+runTerminalStatusesSQL+`))
	`, orgID, entityID).Scan(&count)
	return count > 0, err
}

func (s *agentRunStore) ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error) {
	return activeRunIDsForTask(ctx, s.q, orgID, taskID)
}

func (s *agentRunStore) ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return activeRunIDsForTask(ctx, s.admin, orgID, taskID)
}

func (s *agentRunStore) ActiveAutoRunIDForEntitySystem(ctx context.Context, orgID, entityID string) (string, string, error) {
	var id, taskID string
	err := s.admin.QueryRowContext(ctx, `
		SELECT r.id, r.task_id FROM conversations r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		WHERE r.org_id = $1
		  AND t.entity_id = $2
		  AND r.trigger_type = 'event'
		  AND (r.status IS NULL
		       OR r.status NOT IN (`+runTerminalStatusesSQL+`))
		ORDER BY r.started_at DESC
		LIMIT 1
	`, orgID, entityID).Scan(&id, &taskID)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return id, taskID, err
}

func activeRunIDsForTask(ctx context.Context, q queryer, orgID, taskID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE org_id = $1 AND task_id = $2
		  AND (status IS NULL
		       OR status NOT IN (`+runTerminalStatusesSQL+`))
	`, orgID, taskID)
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

func (s *agentRunStore) ActiveIDsForTeamSystem(ctx context.Context, orgID, teamID string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE org_id = $1 AND team_id = $2
		  AND (status IS NULL
		       OR status NOT IN (`+runTerminalStatusesSQL+`))
	`, orgID, teamID)
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

// ListParkedWorktreePathsSystem returns the worktree dirs the startup sweep
// must keep warm — parked `open` runs whose owning blueprint_run is still
// 'running'. A parked run under an already-terminal blueprint_run is NOT
// resumable (every resume path gates on cr.Status == running), so its
// worktree must NOT be preserved: preserving it would leave a checked-out
// branch on disk that the boot reconcile then orphans by cancelling the row,
// reviving the "refusing to fetch into a branch checked out in a worktree"
// loop. Admin pool — the startup sweep has no JWT-claims context.
func (s *agentRunStore) ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT r.worktree_path FROM conversations r
		LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
		WHERE r.org_id = $1
		  AND r.status = 'open'
		  AND COALESCE(r.worktree_path, '') != ''
		  AND (br.id IS NULL OR br.status = 'running')
	`, orgID)
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

func (s *agentRunStore) EntitiesWithOpenRuns(ctx context.Context, orgID string, entityIDs []string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT t.entity_id
		FROM conversations r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		WHERE r.org_id = $1
		  AND r.status = 'open'
		  AND t.entity_id = ANY($2)
	`, orgID, entityIDs)
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

func (s *agentRunStore) InsertMessage(ctx context.Context, orgID string, msg *domain.Message) (int64, error) {
	return insertRunMessage(ctx, s.q, orgID, msg)
}

func (s *agentRunStore) InsertMessageSystem(ctx context.Context, orgID string, msg *domain.Message) (int64, error) {
	return insertRunMessage(ctx, s.admin, orgID, msg)
}

// InsertMessageForClaimSystem is the transcript write an engagement makes
// while it is streaming: the row is attributed to claimID, and claimID has to
// still be live for it to land at all. The explicit argument overwrites
// msg.ClaimID so the row that persists and the claim the fence validated are
// the same one — a row attributed to a claim the writer does not hold is the
// misattribution this whole path exists to prevent.
func (s *agentRunStore) InsertMessageForClaimSystem(ctx context.Context, orgID, claimID string, msg *domain.Message) (int64, error) {
	var id int64
	err := inTx(ctx, s.admin, func(q queryer) error {
		// The row's own conversation is what the claim must own — the fence
		// and the INSERT below must not be able to name different ones.
		if err := assertClaimActive(ctx, q, orgID, msg.ConversationID, claimID); err != nil {
			return err
		}
		msg.ClaimID = claimID
		var err error
		id, err = insertRunMessage(ctx, q, orgID, msg)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// LastAgentActivityAtSystem returns the created_at of the run's newest non-user
// message (the artifact-change ledger watermark). Ordered by id DESC
// (the monotonic sequence) so the watermark is the genuinely last-inserted agent
// row. Admin pool: the resume path holds no JWT claims. ok=false when the run has
// no agent message yet.
func (s *agentRunStore) LastAgentActivityAtSystem(ctx context.Context, orgID, runID string) (time.Time, bool, error) {
	var at time.Time
	err := s.admin.QueryRowContext(ctx, `
		SELECT created_at FROM messages
		WHERE org_id = $1 AND conversation_id = $2 AND role <> 'user'
		ORDER BY id DESC LIMIT 1
	`, orgID, runID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
}

// insertRunMessage attributes the row to an engagement: an explicit
// non-empty msg.ClaimID always wins (the curator sink names its own
// claim), otherwise claim_id resolves server-side to the conversation's
// active claim. Rows written during an engagement belong to it; rows
// written outside one (pending inputs, queued turns, injections)
// correctly resolve NULL. A message racing its claim's release lands
// NULL — harmless, the terminal settle's newest-row fallback still
// reaches it.
func insertRunMessage(ctx context.Context, q queryer, orgID string, msg *domain.Message) (int64, error) {
	var toolCallsJSON, metadataJSON, reasoningJSON, contentBlocksJSON []byte

	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = b
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = b
	}
	if len(msg.Reasoning) > 0 {
		b, err := json.Marshal(msg.Reasoning)
		if err != nil {
			return 0, fmt.Errorf("marshal reasoning: %w", err)
		}
		reasoningJSON = b
	}
	if len(msg.ContentBlocks) > 0 {
		b, err := json.Marshal(msg.ContentBlocks)
		if err != nil {
			return 0, fmt.Errorf("marshal content_blocks: %w", err)
		}
		contentBlocksJSON = b
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

	// Postgres uses a sequence on messages.id, so we get the
	// auto-assigned id back via RETURNING rather than the
	// LastInsertId Result method (which pgx doesn't implement).
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, user_id, claim_id, role, content, subtype, tool_calls,
		                      tool_call_id, is_error, metadata, model,
		                      input_tokens, output_tokens,
		                      cache_read_tokens, cache_creation_tokens, cost_usd, created_at,
		                      reasoning, content_blocks, delivered, window_state, seq, duration_ms)
		VALUES ($1, $2, $3,
		        COALESCE($4, (SELECT id FROM claims
		                      WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL)),
		        $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		        $16, $17, $18, $19, $20, $21, $22, $23, $24)
		RETURNING id
	`,
		orgID, msg.ConversationID, nullIfEmpty(msg.UserID), nullIfEmpty(msg.ClaimID),
		msg.Role, msg.Content, msg.Subtype,
		nullableJSONB(toolCallsJSON), nullIfEmpty(msg.ToolCallID), msg.IsError,
		nullableJSONB(metadataJSON), nullIfEmpty(msg.Model),
		nullIntPtr(msg.InputTokens), nullIntPtr(msg.OutputTokens),
		nullIntPtr(msg.CacheReadTokens), nullIntPtr(msg.CacheCreationTokens),
		nullFloatPtr(msg.CostUSD), msg.CreatedAt,
		nullableJSONB(reasoningJSON), nullableJSONB(contentBlocksJSON), delivered,
		string(windowState), nullFloatPtr(msg.Seq), nullIntPtr(msg.DurationMs),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func nullFloatPtr(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullableJSONB returns NULL for empty input so the JSONB column
// stays unset (matching SQLite's behavior where the TEXT column is
// NULL when no data). pgx accepts []byte for JSONB binding.
func nullableJSONB(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt64Ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

const pgMessageColumns = `id, conversation_id, COALESCE(user_id::text, ''), COALESCE(claim_id::text, ''),
	role, content, subtype, tool_calls::text, tool_call_id, is_error, metadata::text,
	model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd, created_at,
	reasoning::text, content_blocks::text, delivered, window_state, seq, duration_ms`

// scanMessageRows drains a messages result set selecting
// pgMessageColumns into domain.Message values. Shared by the
// single-run Messages and the batched MessagesForRuns.
func scanMessageRows(rows *sql.Rows) ([]domain.Message, error) {
	var messages []domain.Message
	for rows.Next() {
		var m domain.Message
		var content, subtype, toolCallsStr, toolCallID, metadataStr, model sql.NullString
		var inputTok, outputTok, cacheReadTok, cacheCreateTok sql.NullInt64
		var costUSD sql.NullFloat64
		var reasoningStr, contentBlocksStr sql.NullString
		var delivered bool
		var windowState string
		var seq sql.NullFloat64
		var durationMs sql.NullInt64

		if err := rows.Scan(
			&m.ID, &m.ConversationID, &m.UserID, &m.ClaimID, &m.Role, &content, &subtype, &toolCallsStr,
			&toolCallID, &m.IsError, &metadataStr, &model,
			&inputTok, &outputTok, &cacheReadTok, &cacheCreateTok, &costUSD, &m.CreatedAt,
			&reasoningStr, &contentBlocksStr, &delivered, &windowState, &seq, &durationMs,
		); err != nil {
			return nil, err
		}

		m.Content = content.String
		m.Subtype = subtype.String
		m.ToolCallID = toolCallID.String
		m.Model = model.String

		if toolCallsStr.Valid && toolCallsStr.String != "" {
			_ = json.Unmarshal([]byte(toolCallsStr.String), &m.ToolCalls)
		}
		if metadataStr.Valid && metadataStr.String != "" {
			_ = json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
		}
		// Unlike tool_calls/metadata above, reasoning/content_blocks are part
		// of the canonical replay context a native loop reconstructs via
		// ListForAssembly — a decode failure here must surface, not silently
		// yield an empty Reasoning/ContentBlocks that looks like "no
		// reasoning on this message" when the row actually has some.
		if reasoningStr.Valid && reasoningStr.String != "" {
			if err := json.Unmarshal([]byte(reasoningStr.String), &m.Reasoning); err != nil {
				return nil, fmt.Errorf("unmarshal reasoning (message %d): %w", m.ID, err)
			}
		}
		if contentBlocksStr.Valid && contentBlocksStr.String != "" {
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
// bottom. id is a bigserial, so no row can be at or below 0 and the watermark
// drops out: routing both through one query is what makes the two reads'
// visibility filter identical rather than merely matching.
func (s *agentRunStore) Messages(ctx context.Context, orgID, runID string) ([]domain.Message, error) {
	return s.MessagesSince(ctx, orgID, runID, 0)
}

func (s *agentRunStore) MessagesSince(ctx context.Context, orgID, runID string, sinceID int) ([]domain.Message, error) {
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
		SELECT `+pgMessageColumns+`
		FROM messages
		WHERE org_id = $1 AND conversation_id = $2
		  AND id > $3
		  AND NOT (delivered = false AND window_state = 'inactive')
		ORDER BY COALESCE(seq, (id)::double precision) ASC
	`, orgID, runID, sinceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

func (s *agentRunStore) MessagesForRuns(ctx context.Context, orgID string, runIDs []string) ([]domain.Message, error) {
	// conversation_id is a uuid column; drop any non-UUID id (22P02 → 500
	// guard, same read convention as ListForTasks). These ids are
	// server-derived today, so this is defense in depth against a future
	// caller passing raw input.
	runIDs = filterValidUUIDs(runIDs)
	if len(runIDs) == 0 {
		return nil, nil
	}
	// App pool (RLS-active): conversation_id is a uuid column, so the slice
	// binds as a uuid[] literal through one $N (pgUUIDArray), mirroring
	// artifactStore.ListByRuns. Ordering on (conversation_id, the effective
	// assembly key) so the caller groups by RunID with each run's messages in
	// the same order the single-run display read gives them.
	// Withdrawn-pending rows are hidden, same as Messages.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgMessageColumns+`
		FROM messages
		WHERE org_id = $1 AND conversation_id = ANY($2)
		  AND NOT (delivered = false AND window_state = 'inactive')
		ORDER BY conversation_id ASC, COALESCE(seq, (id)::double precision) ASC
	`, orgID, pgUUIDArray(runIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

// ListForAssemblySystem returns every row a native loop needs to rebuild this
// run's exact LLM context, ordered by the effective assembly key COALESCE(seq,
// id). window_state='inactive' rows are excluded (superseded by
// compaction); 'elided' and undelivered rows are included — see the
// interface doc for the full contract. Pure read over messages; no
// other table or in-process state feeds in.
//
// Admin pool, org bound by argument: the only caller is the native loop, which
// drives a claimed conversation on an executor with no request identity to
// authenticate as. On the app pool this is an RLS read, and RLS evaluation
// calls current_user_id() — which a JWT-less background job has no permission
// to execute, so every assembly would fail at the database rather than return
// an empty window.
func (s *agentRunStore) ListForAssemblySystem(ctx context.Context, orgID, runID string) ([]domain.Message, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgMessageColumns+`
		FROM messages
		WHERE org_id = $1 AND conversation_id = $2 AND window_state <> 'inactive'
		ORDER BY COALESCE(seq, (id)::double precision) ASC
	`, orgID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

// MarkDeliveredForClaimSystem is the engagement's drain flush, behind the
// fence. Consuming a pending row is a claim on it: the rows an engagement
// folds into its assembly must not be marked spent by an engagement that has
// been fenced out, or the successor's own assembly silently loses them.
//
// A non-empty subtype is stamped in the same statement — the flush is the
// moment a mid-work drain marks human input as a steer, and doing it in two
// steps would leave a window in which a delivered row had lost its
// provenance. NULLIF keeps the stamp optional without forking the statement:
// an empty subtype leaves each row's own value in place.
func (s *agentRunStore) MarkDeliveredForClaimSystem(ctx context.Context, orgID, runID, claimID string, ids []int, subtype string) error {
	if len(ids) == 0 {
		// Nothing to consume, so nothing to fence: an empty batch writes no
		// rows on any path, and refusing it would make the caller handle a
		// fence trip that could not have corrupted anything.
		return nil
	}
	return inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, runID, claimID); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `
			UPDATE messages SET delivered = true, subtype = COALESCE(NULLIF($4, ''), subtype)
			WHERE org_id = $1 AND conversation_id = $2 AND id = ANY($3)
		`, orgID, runID, pgIntArray(ids), subtype)
		return err
	})
}

// CompactForClaimSystem commits one compaction atomically, behind the fence —
// see the interface doc for the full contract. The re-seq reads the queued
// rows inside the same transaction that inserts the result row, so the
// fractions are computed against a queue no concurrent enqueue can reorder
// under it: a row inserted after this transaction commits carries an id
// greater than the result row's and sorts after every fraction on its own.
func (s *agentRunStore) CompactForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error {
	return inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		if replyRow != nil {
			replyRow.ConversationID = conversationID
			replyRow.ClaimID = claimID
			replyRow.WindowState = domain.MessageWindowInactive
			id, err := insertRunMessage(ctx, q, orgID, replyRow)
			if err != nil {
				return fmt.Errorf("insert compaction reply row: %w", err)
			}
			replyRow.ID = int(id)
		}
		resultRow.ConversationID = conversationID
		resultRow.ClaimID = claimID
		resultID, err := insertRunMessage(ctx, q, orgID, resultRow)
		if err != nil {
			return fmt.Errorf("insert compaction result row: %w", err)
		}
		resultRow.ID = int(resultID)

		if len(inactiveIDs) > 0 {
			if _, err := q.ExecContext(ctx, `
				UPDATE messages SET window_state = 'inactive'
				WHERE org_id = $1 AND conversation_id = $2 AND id = ANY($3)
			`, orgID, conversationID, pgIntArray(inactiveIDs)); err != nil {
				return fmt.Errorf("flip compacted span: %w", err)
			}
		}

		rows, err := q.QueryContext(ctx, `
			SELECT id FROM messages
			WHERE org_id = $1 AND conversation_id = $2
			  AND delivered = false
			  AND COALESCE(seq, (id)::double precision) < $3
			ORDER BY COALESCE(seq, (id)::double precision) ASC
		`, orgID, conversationID, float64(resultID))
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
				UPDATE messages SET seq = $1 WHERE org_id = $2 AND conversation_id = $3 AND id = $4
			`, seq, orgID, conversationID, id); err != nil {
				return fmt.Errorf("re-seq queued row %d: %w", id, err)
			}
		}
		return nil
	})
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
// the request row, behind the fence — see the interface doc.
func (s *agentRunStore) SettleCompactionRequestForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) error {
	return inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `
			UPDATE messages
			SET input_tokens = $4, output_tokens = $5,
			    cache_read_tokens = $6, cache_creation_tokens = $7,
			    cost_usd = $8,
			    metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object('compaction_failure', $9::text)
			WHERE org_id = $1 AND conversation_id = $2 AND id = $3
		`, orgID, conversationID, requestID,
			inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens,
			nullFloatPtr(costUSD), reason)
		return err
	})
}

// SetWindowStateSystem is the elision/compaction primitive: a batched range
// flip of window_state from `from` to `to`, restricted to rows currently in
// state `from` whose effective assembly key (COALESCE(seq, id)) is strictly
// less than beforeSeq. Returns the number of rows flipped.
//
// Admin pool for the same reason as ListForAssemblySystem: the native loop is
// the only caller and holds no request identity.
func (s *agentRunStore) SetWindowStateSystem(ctx context.Context, orgID, runID string, beforeSeq float64, from, to domain.MessageWindowState) (int, error) {
	result, err := s.admin.ExecContext(ctx, `
		UPDATE messages
		SET window_state = $1
		WHERE org_id = $2 AND conversation_id = $3 AND window_state = $4
		  AND COALESCE(seq, (id)::double precision) < $5
	`, string(to), orgID, runID, string(from), beforeSeq)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

func (s *agentRunStore) TokenTotalsSystem(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error) {
	row := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(model), ''),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COUNT(*)
		FROM messages
		WHERE org_id = $1 AND conversation_id = $2 AND role = 'assistant'
	`, orgID, runID)

	var t domain.TokenTotals
	if err := row.Scan(&t.Model, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheCreationTokens, &t.NumTurns); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *agentRunStore) BlueprintSiblingCostUSDSystem(ctx context.Context, orgID, blueprintRunID, excludeRunID string) (float64, error) {
	var cost sql.NullFloat64
	err := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(m.cost_usd), 0)
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id AND c.org_id = m.org_id
		WHERE c.org_id = $1 AND c.blueprint_run_id = $2 AND c.id <> $3
	`, orgID, blueprintRunID, excludeRunID).Scan(&cost)
	if err != nil {
		return 0, err
	}
	return cost.Float64, nil
}

func (s *agentRunStore) BlueprintSiblingDurationMsSystem(ctx context.Context, orgID, blueprintRunID, excludeRunID string) (int, error) {
	var ms sql.NullInt64
	err := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cl.duration_ms), 0)
		FROM claims cl
		JOIN conversations c ON c.id = cl.conversation_id AND c.org_id = cl.org_id
		WHERE c.org_id = $1 AND c.blueprint_run_id = $2 AND c.id <> $3
	`, orgID, blueprintRunID, excludeRunID).Scan(&ms)
	if err != nil {
		return 0, err
	}
	return int(ms.Int64), nil
}

// --- Helpers ---

// scanConversation fills r from a single-row QueryRow result. Sibling
// scanConversationRows handles the rows.Scan case. Both unpack the
// nullable columns through the same set of intermediates.
func scanConversation(row *sql.Row, r *domain.Conversation) error {
	var queuedAt, claimedAt, completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var blueprintRunID sql.NullString
	var failureKind string

	if err := row.Scan(
		&r.ID, &r.TaskID, &r.Runtime, &r.Status, &r.Model, &r.StartedAt, &queuedAt, &claimedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &r.StopReason, &r.WorktreePath,
		&r.ResultSummary, &r.Outcome, &r.OutcomeReason, &failureKind, &r.SessionID, &r.ActorAgentID, &r.TriggerType, &r.CreatorUserID, &r.TeamID, &r.ExecutorID, &r.Attempts, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	); err != nil {
		return err
	}
	r.FailureKind = domain.RunFailureKind(failureKind)
	finalizeConversation(r, queuedAt, claimedAt, completedAt, costUSD, durationMs, numTurns, blueprintStep, blueprintRunID)
	return nil
}

func scanConversationRows(rows *sql.Rows, r *domain.Conversation) error {
	var queuedAt, claimedAt, completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var blueprintRunID sql.NullString
	var failureKind string

	if err := rows.Scan(
		&r.ID, &r.TaskID, &r.Runtime, &r.Status, &r.Model, &r.StartedAt, &queuedAt, &claimedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &r.StopReason, &r.WorktreePath,
		&r.ResultSummary, &r.Outcome, &r.OutcomeReason, &failureKind, &r.SessionID, &r.ActorAgentID, &r.TriggerType, &r.CreatorUserID, &r.TeamID, &r.ExecutorID, &r.Attempts, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	); err != nil {
		return err
	}
	r.FailureKind = domain.RunFailureKind(failureKind)
	finalizeConversation(r, queuedAt, claimedAt, completedAt, costUSD, durationMs, numTurns, blueprintStep, blueprintRunID)
	return nil
}

func finalizeConversation(r *domain.Conversation, queuedAt, claimedAt, completedAt sql.NullTime, costUSD sql.NullFloat64, durationMs, numTurns, blueprintStep sql.NullInt64, blueprintRunID sql.NullString) {
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
	if claimedAt.Valid {
		r.ClaimedAt = &claimedAt.Time
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
}

// nullIfEmpty is the small reusable helper many Postgres stores want
// — empty string → SQL NULL bind, non-empty passes through. Defined
// once per package; sibling stores that also need it import the same
// symbol.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
