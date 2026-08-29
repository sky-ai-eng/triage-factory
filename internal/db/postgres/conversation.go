package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/wakebus"
)

// conversationStore is the Postgres impl of db.ConversationStore over the
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
var conversationLog = logging.Component("db/conversation")

type conversationStore struct {
	q     queryer
	admin queryer
}

func newConversationStore(q, admin queryer) db.ConversationStore {
	return &conversationStore{q: q, admin: admin}
}

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
// the claim-desync janitor arms (ConversationQueueStore.ReconcileOrphanedConversations at
// boot in both modes; the leader reaper's HealClaimDesyncs every tick), so
// neither can strand a row past a sweep.
func releaseActiveClaim(ctx context.Context, q queryer, orgID, conversationID, outcome string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = now(), outcome = $1
		WHERE org_id = $2 AND conversation_id = $3 AND released_at IS NULL
	`, outcome, orgID, conversationID)
	return err
}

// Complete settles the cost lump and claim release on the admin pool FIRST,
// then flips + RETURNINGs the conversation on the app pool — non-atomic by
// design; see releaseActiveClaim for why the split is acceptable (the
// janitor arms make both crash shapes self-healing). The order matters
// beyond that: writeConversationReturning's derived columns (total_cost_usd,
// duration_ms, num_turns, executor_id) come from claims/messages, so they
// only agree with a follow-up Get if those tables already hold this call's
// writes by the time the flip runs. Each admin-pool statement commits before
// the next Go call starts, and Postgres's MVCC guarantees any later
// transaction — including the app-pool flip below, on a different connection
// entirely — sees a prior commit regardless of which role made it.
func (s *conversationStore) Complete(ctx context.Context, orgID, conversationID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) (*domain.Conversation, error) {
	if err := settleCompletionCostAndClaim(ctx, s.admin, orgID, conversationID, status, costUSD, durationMs, numTurns); err != nil {
		return nil, err
	}
	return completeConversationFlip(ctx, s.q, orgID, conversationID, status, resultSummary, outcome, outcomeReason, failureKind)
}

func (s *conversationStore) CompleteSystem(ctx context.Context, orgID, conversationID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) (*domain.Conversation, error) {
	var result *domain.Conversation
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := settleCompletionCostAndClaim(ctx, q, orgID, conversationID, status, costUSD, durationMs, numTurns); err != nil {
			return err
		}
		r, err := completeConversationFlip(ctx, q, orgID, conversationID, status, resultSummary, outcome, outcomeReason, failureKind)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CompleteForClaimSystem is CompleteSystem with the fence in front of it, in
// the same transaction as both the settlement and the flip. The claim it
// releases is resolved the same way CompleteSystem resolves it (the
// conversation's active claim) — which the fence has just proven to be
// claimID, since only one claim per conversation can be unreleased at a time.
func (s *conversationStore) CompleteForClaimSystem(ctx context.Context, orgID, conversationID, claimID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) (*domain.Conversation, error) {
	var result *domain.Conversation
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		if err := settleCompletionCostAndClaim(ctx, q, orgID, conversationID, status, costUSD, durationMs, numTurns); err != nil {
			return err
		}
		r, err := completeConversationFlip(ctx, q, orgID, conversationID, status, resultSummary, outcome, outcomeReason, failureKind)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// settleCompletionCostAndClaim does everything Complete's terminal write
// needs to happen BEFORE the conversation flip: locate the active claim,
// settle the cost lump onto the messages ledger, and release the claim with
// its telemetry. Split out from the conversations UPDATE (completeConversationFlip)
// specifically so callers can run it first — see Complete's doc for why the
// order is load-bearing now that the flip RETURNINGs derived columns.
func settleCompletionCostAndClaim(ctx context.Context, q queryer, orgID, conversationID, status string, costUSD float64, durationMs, numTurns int) error {
	// The active claim this terminal write releases identifies the
	// engagement's own message rows (they insert claim-stamped), so the
	// lump settles claim-keyed — every claim release's shape. Read
	// before the release below: a released claim is no longer findable.
	var claimID string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM claims
		WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
	`, orgID, conversationID).Scan(&claimID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	settled := false
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
		`, costUSD, orgID, conversationID, claimID, domain.ModelSynthetic)
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
		`, costUSD, orgID, conversationID, domain.ModelSynthetic)
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
			conversationLog.Warn("conversation cost has no message row to settle on; spend unrecorded",
				"conversation_id", conversationID, "org_id", orgID, "cost_usd", costUSD)
		}
	}
	return releaseActiveClaimWithTelemetry(ctx, q, orgID, conversationID, claimOutcomeForStatus(status), durationMs, numTurns)
}

// completeConversationFlip is the terminal status flip, RETURNING the
// conversation row exactly as Get would read it. Always the LAST statement of
// its caller's write — see settleCompletionCostAndClaim's doc.
func completeConversationFlip(ctx context.Context, q queryer, orgID, conversationID, status, resultSummary, outcome, outcomeReason, failureKind string) (*domain.Conversation, error) {
	// The conversation carries no accounting cache — cost settled as one
	// lump on the invocation's last message row above. A resume's Complete
	// stamps its own invocation's lump on its own last row, so nothing
	// accumulates or doubles.
	return writeConversationReturning(ctx, q, `
		UPDATE conversations
		SET status = $1,
		    completed_at = $2,
		    result_summary = $3,
		    outcome = NULLIF($4, ''),
		    outcome_reason = NULLIF($5, ''),
		    failure_kind = NULLIF($6, '')
		WHERE org_id = $7 AND id = $8
		RETURNING *
	`, status, time.Now().UTC(), resultSummary, outcome, outcomeReason, failureKind, orgID, conversationID)
}

// releaseActiveClaimWithTelemetry is releaseActiveClaim plus the terminal
// duration/turns stamp — the engagement telemetry the runtime reports per
// invocation, which lives on the claim because messages can't derive it.
func releaseActiveClaimWithTelemetry(ctx context.Context, q queryer, orgID, conversationID, outcome string, durationMs, numTurns int) error {
	_, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = now(), outcome = $1, duration_ms = $2, num_turns = $3
		WHERE org_id = $4 AND conversation_id = $5 AND released_at IS NULL
	`, outcome, durationMs, numTurns, orgID, conversationID)
	return err
}

// ParkOpen's release is adjacent, not atomic — see releaseActiveClaim. An
// 'open' row with a dangling claim needs no janitor arm of its own: the resume
// flip (MarkQueuedForResume) releases any active claim on its way back to the
// queue.
func (s *conversationStore) ParkOpen(ctx context.Context, orgID, conversationID string, park db.Park) (bool, error) {
	flipped, err := parkOpen(ctx, s.q, orgID, conversationID, park)
	if err != nil || !flipped {
		return flipped, err
	}
	return true, releaseActiveClaim(ctx, s.admin, orgID, conversationID, park.ClaimOutcome())
}

func (s *conversationStore) ParkOpenSystem(ctx context.Context, orgID, conversationID string, park db.Park) (bool, error) {
	var flipped bool
	err := inTx(ctx, s.admin, func(q queryer) error {
		var err error
		flipped, err = parkOpen(ctx, q, orgID, conversationID, park)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, orgID, conversationID, park.ClaimOutcome())
	})
	return flipped, err
}

// ParkOpenForClaimSystem is ParkOpenSystem behind the fence — the self-park an
// executor writes when its own run's ctx is killed. Its unfenced twin serves
// the user-initiated cancel, which is deliberately not gated on ownership.
func (s *conversationStore) ParkOpenForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, park db.Park) (bool, error) {
	var flipped bool
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		var err error
		flipped, err = parkOpen(ctx, q, orgID, conversationID, park)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, orgID, conversationID, park.ClaimOutcome())
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
// COALESCE on park_reason / result_summary rather than a bare assignment: a
// park that carries neither must not blank what an earlier one recorded.
func parkOpen(ctx context.Context, q queryer, orgID, conversationID string, park db.Park) (bool, error) {
	// A deliberate stop re-parks an already-parked row; an idle turn-end does
	// not. Spelled as an extra excluded status rather than two queries.
	reparkGuard := `, 'open'`
	if park.Deliberate {
		reparkGuard = ``
	}
	res, err := q.ExecContext(ctx, `
		UPDATE conversations
		SET status = 'open',
		    parked_at = COALESCE(parked_at, $1),
		    park_reason = COALESCE(NULLIF($2, ''), park_reason),
		    result_summary = COALESCE(NULLIF($3, ''), result_summary)
		WHERE org_id = $4 AND id = $5
		  AND (status IS NULL
		       OR status NOT IN (`+conversationTerminalStatusesSQL+reparkGuard+`))
	`, time.Now().UTC(), string(park.Reason), park.ResultSummary, orgID, conversationID)
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
// rows it holds — see the interface doc for the two exclusions and why the
// blueprint check is in this statement rather than in the caller.
//
// It goes through tf.blueprint_run_is_running rather than a correlated
// subquery because this statement runs on the app pool: blueprint_runs is
// creator-scoped for manual runs, so a non-creator teammate's subquery would
// find nothing and the guard would fail OPEN on exactly the rows it protects.
//
// Always claims-scoped — resume is always user-initiated, so
// there is no admin-pool "...System" variant. The active claim releases as
// 'requeued' (ownership is re-established when ClaimNextConversation mints a fresh
// claim, exactly like a fresh EnqueueConversation'd row).
//
// preferred_executor_id is re-stamped, in this statement, to the executor of
// the conversation's newest claim. A resume knows something better than a
// fresh rendezvous hash: which executor's engagement drove this conversation
// last, and therefore holds the workspace tree worktree_path names (plus the
// SDK session file, where the runtime keeps one). A stamp that predates the
// park would indeed be stale by now; this one is written here, as the row
// re-enters the queue, so the claim reads it no older than an enqueue-time
// stamp. A conversation with no claims stamps NULL: nothing ever drove it, so
// there is no warmth to chase, and it re-queues unowned/immediately-claimable.
//
// The stamp stays advisory, which is what makes chasing warmth safe: if that
// executor has since died past liveness, drained, or gated, tier 2 admits any
// executor at once; if it is merely busy, the aging window bounds the wait.
//
// The claims read is a plain correlated subquery, unlike the blueprint guard
// above, because claims RLS composes through the conversation — a teammate
// who may UPDATE this row can SELECT its claims by construction, so there is
// no invisible-row arm for the lookup to fall through.
//
// queued_at is NOT re-stamped: it records when the conversation first entered
// the queue and is display-only (the scheduler orders by started_at).
func (s *conversationStore) MarkQueuedForResume(ctx context.Context, orgID, conversationID string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE conversations SET status = NULL,
		                parked_at = NULL, park_reason = NULL,
		                preferred_executor_id = (
		                    SELECT c.executor_id FROM claims c
		                    WHERE c.org_id = conversations.org_id
		                      AND c.conversation_id = conversations.id
		                    `+newestEngagementFirstSQL("c")+`
		                    LIMIT 1)
		WHERE org_id = $1 AND id = $2
		  AND (status = 'open'
		       OR (status = 'completed'
		           AND NOT tf.blueprint_run_is_running(
		                     conversations.blueprint_run_id, conversations.org_id)))
	`, orgID, conversationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	flipped := n > 0
	if flipped {
		if err := releaseActiveClaim(ctx, s.admin, orgID, conversationID, "requeued"); err != nil {
			return false, err
		}
		// tf_wake doorbell: resume-by-enqueue re-queues an
		// EXISTING row rather than inserting one, so it needs its own
		// notify — ConversationQueueStore.EnqueueConversation's doesn't fire for this path.
		// Best-effort, same "never the only path" contract as there.
		_ = wakebus.Publish(ctx, s.q, wakebus.KindEvent, orgID)
	}
	return flipped, nil
}

func (s *conversationStore) ListReapableSnapshotKeysSystem(ctx context.Context, cutoff time.Time) ([]domain.SnapshotReapKey, error) {
	// Snapshot-bearing conversations (parked `open` / any `completed` terminal)
	// grouped by their shared snapshot key (org, blueprint_run_id); a key is
	// reapable once its newest such conversation last parked or concluded before
	// the cutoff. The timestamp is COALESCE(parked_at, completed_at,
	// started_at): parked_at for an open conversation (re-stamped each park, so
	// resumes don't age it), completed_at for a terminal, started_at a legacy
	// fallback. Admin pool — the retention sweep is a tenant-spanning system
	// job with no JWT claims.
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

func (s *conversationStore) ListEvictableWorkspacesSystem(ctx context.Context, cutoff time.Time) ([]domain.EvictableWorkspace, error) {
	// Three gates, in the order they matter. The workspace_snapshots join is
	// the safety one: only a key whose durable blob is recorded `written` has
	// a second copy of the agent's work, so only that key's tree is a cache
	// rather than the original. The correlated MAX is the retention sweep's
	// timestamp rule verbatim (parked_at for an open conversation, re-stamped
	// each park; completed_at for a terminal; started_at a legacy fallback),
	// scoped to the key so a blueprint's steps age as one. The NOT EXISTS is
	// the shared-tree rule: any live claim anywhere under the key means an
	// engagement is working in the directory this enumerates for deletion.
	//
	// DISTINCT over the paths: the steps of one blueprint each record the
	// shared tree on their own row, so the same path arrives once per step.
	// Admin pool — the sweep is a tenant-spanning system job with no JWT
	// claims.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT DISTINCT c.org_id, c.blueprint_run_id, c.worktree_path
		FROM conversations c
		JOIN workspace_snapshots ws
		  ON ws.org_id = c.org_id AND ws.blueprint_run_id = c.blueprint_run_id
		WHERE ws.state = 'written'
		  AND c.status IN ('open', 'completed')
		  AND COALESCE(c.worktree_path, '') <> ''
		  AND (
		        SELECT MAX(COALESCE(aged.parked_at, aged.completed_at, aged.started_at))
		        FROM conversations aged
		        WHERE aged.org_id = c.org_id
		          AND aged.blueprint_run_id = c.blueprint_run_id
		          AND aged.status IN ('open', 'completed')
		      ) < $1
		  AND NOT EXISTS (
		        SELECT 1
		        FROM conversations sib
		        JOIN claims cl ON cl.conversation_id = sib.id AND cl.released_at IS NULL
		        WHERE sib.org_id = c.org_id AND sib.blueprint_run_id = c.blueprint_run_id
		      )
		ORDER BY c.org_id, c.blueprint_run_id, c.worktree_path
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EvictableWorkspace
	for rows.Next() {
		var orgID, blueprintRunID, path string
		if err := rows.Scan(&orgID, &blueprintRunID, &path); err != nil {
			return nil, err
		}
		// The ORDER BY groups a key's paths adjacently, so one pass folds them.
		if n := len(out); n > 0 && out[n-1].OrgID == orgID && out[n-1].BlueprintRunID == blueprintRunID {
			out[n-1].WorktreePaths = append(out[n-1].WorktreePaths, path)
			continue
		}
		out = append(out, domain.EvictableWorkspace{
			OrgID:          orgID,
			BlueprintRunID: blueprintRunID,
			WorktreePaths:  []string{path},
		})
	}
	return out, rows.Err()
}

func (s *conversationStore) HasActiveClaimForBlueprintRunSystem(ctx context.Context, orgID, blueprintRunID string) (bool, error) {
	if !isValidUUID(blueprintRunID) {
		return false, nil
	}
	var exists bool
	err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM conversations c
			JOIN claims cl ON cl.conversation_id = c.id AND cl.released_at IS NULL
			WHERE c.org_id = $1 AND c.blueprint_run_id = $2
		)
	`, orgID, blueprintRunID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *conversationStore) SetSession(ctx context.Context, orgID, conversationID, sessionID string) (*domain.Conversation, error) {
	return setConversationSession(ctx, s.q, orgID, conversationID, sessionID)
}

func (s *conversationStore) SetSessionSystem(ctx context.Context, orgID, conversationID, sessionID string) (*domain.Conversation, error) {
	return setConversationSession(ctx, s.admin, orgID, conversationID, sessionID)
}

// SetSessionForClaimSystem is SetSessionSystem behind the fence. The write is
// byte-identical; what the transaction adds is that a zombie's late init can no
// longer land the successor's resume coordinate on a dead session.
func (s *conversationStore) SetSessionForClaimSystem(ctx context.Context, orgID, conversationID, claimID, sessionID string) (*domain.Conversation, error) {
	var result *domain.Conversation
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		r, err := setConversationSession(ctx, q, orgID, conversationID, sessionID)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func setConversationSession(ctx context.Context, q queryer, orgID, conversationID, sessionID string) (*domain.Conversation, error) {
	return writeConversationReturning(ctx, q, `
		UPDATE conversations SET sdk_session_id = $1 WHERE org_id = $2 AND id = $3
		RETURNING *
	`, sessionID, orgID, conversationID)
}

// updateClaimReturning is writeConversationReturning's claims-row twin: wrap
// a single-row UPDATE/INSERT on claims in a data-modifying CTE, then re-join
// its output through executorClaimSelectCols exactly as executorClaimCols
// does for the live table — see writeConversationReturning for the shared
// caveats (one data-modifying CTE only; any dependent write runs earlier, its
// own statement). updateSQL must itself end in `RETURNING *`.
func updateClaimReturning(ctx context.Context, q queryer, updateSQL string, args ...any) (*domain.ExecutorClaim, error) {
	row := q.QueryRowContext(ctx, `
		WITH updated AS (`+updateSQL+`)
		SELECT `+executorClaimSelectCols+`
		FROM updated c
		LEFT JOIN conversations v ON v.id = c.conversation_id
	`, args...)
	return scanExecutorClaimRow(row)
}

func (s *conversationStore) SetExecutorSystem(ctx context.Context, orgID, conversationID, executorID string, bootEpoch int64) (*domain.ExecutorClaim, error) {
	// An empty executorID keeps the legacy clear semantics: the live
	// engagement is over, so its claim releases as requeued. Written out
	// rather than routed through the shared releaseActiveClaim helper (which
	// ParkOpen/MarkFailedIfActive also use, unconverted) so this arm can
	// RETURNING the row it just released.
	if executorID == "" {
		return updateClaimReturning(ctx, s.admin, `
			UPDATE claims SET released_at = now(), outcome = 'requeued'
			WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
			RETURNING *
		`, orgID, conversationID)
	}
	// Idempotent go-live confirmation: update the active claim's identity
	// if one exists, mint one if none does — the live process must never
	// run unattributed.
	var result *domain.ExecutorClaim
	err := inTx(ctx, s.admin, func(q queryer) error {
		claim, err := updateClaimReturning(ctx, q, `
			UPDATE claims SET executor_id = $1, boot_epoch = $2
			WHERE org_id = $3 AND conversation_id = $4 AND released_at IS NULL
			RETURNING *
		`, executorID, bootEpoch, orgID, conversationID)
		if err != nil {
			return err
		}
		if claim != nil {
			result = claim
			return nil
		}
		claim, err = updateClaimReturning(ctx, q, `
			INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch, claimed_at)
			VALUES ($1, $2, $3, $4, now())
			RETURNING *
		`, orgID, conversationID, executorID, bootEpoch)
		if err != nil {
			return err
		}
		result = claim
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SetExecutorForClaimSystem writes the identity onto the NAMED claim rather
// than whichever one is active, so the fence and the write cannot disagree
// about their target. No mint arm — see the interface doc.
func (s *conversationStore) SetExecutorForClaimSystem(ctx context.Context, orgID, conversationID, claimID, executorID string, bootEpoch int64) (*domain.ExecutorClaim, error) {
	var result *domain.ExecutorClaim
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		claim, err := updateClaimReturning(ctx, q, `
			UPDATE claims SET executor_id = $1, boot_epoch = $2
			WHERE org_id = $3 AND id = $4 AND conversation_id = $5
			RETURNING *
		`, executorID, bootEpoch, orgID, claimID, conversationID)
		if err != nil {
			return err
		}
		result = claim
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SetActiveClaimPhaseSystem scopes the write to the ACTIVE claim only: a
// released claim's phase is inert history, and a conversation with no live
// engagement has no sub-state to record — both fall through as a no-op,
// (nil, nil), the guard declining.
func (s *conversationStore) SetActiveClaimPhaseSystem(ctx context.Context, orgID, conversationID, phase string) (*domain.ExecutorClaim, error) {
	return updateClaimReturning(ctx, s.admin, `
		UPDATE claims SET phase = NULLIF($1, '')
		WHERE org_id = $2 AND conversation_id = $3 AND released_at IS NULL
		RETURNING *
	`, phase, orgID, conversationID)
}

// SetClaimPhaseSystem is the claim-keyed phase write, fenced: an engagement
// reporting its own setup progress must still own the conversation. Without
// the fence a zombie's stale phase would land on whatever claim happened to
// be active — the successor's — and surface as its setup sub-state on every
// display read.
func (s *conversationStore) SetClaimPhaseSystem(ctx context.Context, orgID, conversationID, claimID, phase string) (*domain.ExecutorClaim, error) {
	var result *domain.ExecutorClaim
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		claim, err := updateClaimReturning(ctx, q, `
			UPDATE claims SET phase = NULLIF($1, '')
			WHERE org_id = $2 AND id = $3 AND conversation_id = $4
			RETURNING *
		`, phase, orgID, claimID, conversationID)
		if err != nil {
			return err
		}
		result = claim
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// newestEngagementFirstSQL orders one conversation's claims by how recently
// each engagement held it, nearest first, for the alias the caller gave the
// claims table. Two reads take this order — the resume flip's affinity stamp
// and the predecessor lookup below — and "which engagement came later" is one
// question, so it has one answer here rather than a copy at each site.
//
// The sort must be TOTAL, not merely usually-total. claimed_at and created_at
// both default to now(), which is the transaction timestamp, so two claims
// minted in one transaction tie on every timestamp column and the row Postgres
// returns would be whatever the plan happened to produce — a nondeterministic
// answer feeding a sentence about what the agent's workspace has been through,
// or a stamp pointing at either of two machines. released_at breaks the tie
// meaningfully (of two claims that started together, the one that finished
// later is the nearer engagement, and an unreleased one is nearer still, hence
// NULLS FIRST); the primary key is the backstop that makes the order total
// when even that ties, where "which came first" is genuinely undefined and
// only stability is left to want.
func newestEngagementFirstSQL(alias string) string {
	return `ORDER BY ` + alias + `.claimed_at DESC, ` + alias + `.released_at DESC NULLS FIRST, ` + alias + `.id DESC`
}

// PriorClaimExecutorSystem reads the predecessor engagement's executor.
//
// The caller's own claim is excluded by id rather than by liveness: it is the
// newest row by construction (one active claim per conversation), and keying
// on the id the caller actually holds means a fenced-out caller reads the
// claim before its own, not itself. The id comparison is on text so a
// malformed id is simply no match, the same answer as a missing row.
func (s *conversationStore) PriorClaimExecutorSystem(ctx context.Context, orgID, conversationID, claimID string) (string, error) {
	var executorID string
	err := s.admin.QueryRowContext(ctx, `
		SELECT c.executor_id FROM claims c
		WHERE c.org_id = $1 AND c.conversation_id = $2 AND c.id::text <> $3
		`+newestEngagementFirstSQL("c")+`
		LIMIT 1
	`, orgID, conversationID, claimID).Scan(&executorID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return executorID, err
}

// RecordClaimSandboxStatsSystem is keyed on the claim id alone (org bound as
// defense in depth) with NO released_at predicate — the teardown that
// measures these numbers runs after the claim is released.
func (s *conversationStore) RecordClaimSandboxStatsSystem(ctx context.Context, orgID, claimID string, peakMemMB *int, cpuUsec *int64) (*domain.ExecutorClaim, error) {
	return updateClaimReturning(ctx, s.admin, `
		UPDATE claims SET peak_mem_mb = $1, cpu_usec = $2
		WHERE org_id = $3 AND id = $4
		RETURNING *
	`, nullIntPtr(peakMemMB), nullInt64Ptr(cpuUsec), orgID, claimID)
}

func (s *conversationStore) SetWorktreePath(ctx context.Context, orgID, conversationID, path string) (*domain.Conversation, error) {
	return setConversationWorktreePath(ctx, s.q, orgID, conversationID, path)
}

func (s *conversationStore) SetWorktreePathSystem(ctx context.Context, orgID, conversationID, path string) (*domain.Conversation, error) {
	return setConversationWorktreePath(ctx, s.admin, orgID, conversationID, path)
}

// SetWorktreePathForClaimSystem is SetWorktreePathSystem behind the fence — the
// workspace stamp an engagement writes once its setup or rehydrate resolves a
// path, refused once a successor holds the conversation.
func (s *conversationStore) SetWorktreePathForClaimSystem(ctx context.Context, orgID, conversationID, claimID, path string) (*domain.Conversation, error) {
	var result *domain.Conversation
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		r, err := setConversationWorktreePath(ctx, q, orgID, conversationID, path)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func setConversationWorktreePath(ctx context.Context, q queryer, orgID, conversationID, path string) (*domain.Conversation, error) {
	return writeConversationReturning(ctx, q, `
		UPDATE conversations SET worktree_path = $1 WHERE org_id = $2 AND id = $3
		RETURNING *
	`, path, orgID, conversationID)
}

// MarkFailedIfActive's release is adjacent, not atomic — see
// releaseActiveClaim for the crash shapes and the janitor arms that heal
// them.
func (s *conversationStore) MarkFailedIfActive(ctx context.Context, orgID, conversationID, failureKind string) (bool, error) {
	flipped, err := markFailedIfActive(ctx, s.q, orgID, conversationID, failureKind)
	if err != nil || !flipped {
		return flipped, err
	}
	return true, releaseActiveClaim(ctx, s.admin, orgID, conversationID, "failed")
}

func (s *conversationStore) MarkFailedIfActiveSystem(ctx context.Context, orgID, conversationID, failureKind string) (bool, error) {
	var flipped bool
	err := inTx(ctx, s.admin, func(q queryer) error {
		var err error
		flipped, err = markFailedIfActive(ctx, q, orgID, conversationID, failureKind)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, orgID, conversationID, "failed")
	})
	return flipped, err
}

// MarkFailedIfActiveForClaimSystem is MarkFailedIfActiveSystem behind the
// fence. The two negative answers stay distinct: ok=false is the guarded
// flip's own "somebody else reached the terminal first", ErrClaimReleased is
// "you are not the one who gets to decide".
func (s *conversationStore) MarkFailedIfActiveForClaimSystem(ctx context.Context, orgID, conversationID, claimID, failureKind string) (bool, error) {
	var flipped bool
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		var err error
		flipped, err = markFailedIfActive(ctx, q, orgID, conversationID, failureKind)
		if err != nil || !flipped {
			return err
		}
		return releaseActiveClaim(ctx, q, orgID, conversationID, "failed")
	})
	if err != nil {
		return false, err
	}
	return flipped, nil
}

func markFailedIfActive(ctx context.Context, q queryer, orgID, conversationID, failureKind string) (bool, error) {
	// 'open' is deliberately failable here — see
	// ConversationStore.MarkFailedIfActive: a warm 'open' conversation has no
	// durable snapshot yet, so an infra error reaching failConversation must
	// terminate it.
	res, err := q.ExecContext(ctx, `
		UPDATE conversations SET status = 'failed', completed_at = COALESCE(completed_at, $1),
		    failure_kind = NULLIF($2, '')
		WHERE org_id = $3 AND id = $4
		  AND (status IS NULL
		       OR status NOT IN (`+conversationTerminalStatusesSQL+`))
	`, time.Now().UTC(), failureKind, orgID, conversationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- Queries ---

// pgConversationColumns is the SELECT list scanned into a domain.Conversation
// via scanConversation. Owned here on ConversationStore; sibling Postgres
// stores that need to project a conversation (e.g.
// factoryReadStore.ActiveConversations) already use their own copy because
// they also project task+entity JOINs. Keeping this here keeps the simple
// "just the conversation" projection uncoupled from those.
// task_id/status/team_id COALESCE to ” because a
// non-delegation conversation may carry NULLs there; the claim-derived
// fields (incl. duration/turns telemetry) come from the `cl` lateral and
// the ledger-derived accounting (cost + tokens) from the `msum` lateral
// (see conversationClaimLateral / conversationLedgerLateral). Status is the derived display
// ladder (pgDisplayStatusSQL) rather than the stored column.
const pgConversationColumns = `
	r.id, COALESCE(r.task_id::text, ''), COALESCE(r.runtime, ''), ` + pgDisplayStatusSQL + `, COALESCE(r.model, ''), r.started_at, r.queued_at, cl.claimed_at, r.completed_at,
	msum.total_cost_usd, cl.duration_ms, cl.num_turns,
	COALESCE(r.park_reason, ''), COALESCE(r.worktree_path, ''),
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

// pgQueuePositionCTE ranks the org's display-`queued` conversations by
// (started_at, id) — the CTE body ConversationStore.List joins its page
// against, so a row that is queued carries its place in line and every other
// row carries SQL NULL. It binds $1 as the org id, which is what
// pgConversationListWhere always puts first.
//
// The rank is a window over one pass, not a per-row correlated count:
// pgDisplayStatusSQL is itself three correlated subqueries, and counting the
// rows ahead of each row would evaluate the whole ladder once per pair.
//
// It rides the page's own statement, where the SQLite twin reads the same
// ranking as a statement of its own. This dialect binds its id set as one
// array and so runs one statement per read: the CTE is evaluated once, and
// the page sees the rank in the snapshot it was ranked in. SQLite has to
// chunk its IN list across statements, where an inline CTE would re-rank the
// whole queue per chunk.
//
// Two things this deliberately does NOT read, and must not learn to: anything
// about another org (the position is org-local by contract — see
// domain.Conversation.QueuePosition), and anything about the fleet — executor
// identity, placement tier, occupancy. It also runs on the caller's own
// connection, so under RLS it ranks within the set that caller may already
// see; a reader can never learn a queued conversation exists by watching the
// numbers move.
const pgQueuePositionCTE = `
	queued_positions AS (
		SELECT r.id AS conversation_id,
		       (ROW_NUMBER() OVER (ORDER BY r.started_at, r.id))::int AS position
		FROM conversations r
		WHERE r.org_id = $1 AND (` + pgDisplayStatusSQL + `) = 'queued'
	)
`

// pgConversationLiveStatusesSQL is the display statuses that mean a LIVE
// engagement — `running`, plus every claim phase — as a SQL IN-list body.
// Mirrors domain.IsActiveConversationStatus, which is what the run view
// lights as WORKING and what the rail counts as `running`.
//
// SQL cannot import a Go const, so the set is spelled here and held to the Go
// one by the dual-dialect conformance suite, whose coverage is derived from
// domain.AllClaimPhases(): a phase added there and not taught to the stores
// fails on both backends.
const pgConversationLiveStatusesSQL = `'running','fetching','cloning','agent_starting','awaiting_credentials'`

// pgUnresolvedArtifactSQL is the SQL twin of domain.HasUnresolvedArtifacts,
// written against an artifacts alias `art`: a draft pull request, or a pending
// review that reached the ready sentinel (details.review_event, set by
// finalize-review). A review that was started and never finalized is
// deliberately NOT unresolved — it would strand a human on an approval card
// with nothing to approve.
//
// The kind/state literals mirror domain.ArtifactKindPullRequest /
// ArtifactStatePRDraft / ArtifactKindReview / ArtifactStateReviewPending; the
// conformance suite is what holds them to the Go predicate, since the two
// definitions have to agree for a count to mean what every card means.
//
// details_json is a text column here, so the sentinel read is guarded on the
// value looking like an object before the jsonb cast — CASE evaluates its
// WHEN first, so a NULL, an empty string, or anything else that is not an
// object reads as not-ready, exactly as domain's ParseReviewArtifactDetails
// error/zero arms do. A value that opens like an object and is not valid JSON
// is corruption no writer can produce (every writer marshals the struct), and
// this deliberately raises on it rather than silently counting it as resolved.
const pgUnresolvedArtifactSQL = `
	   (art.kind = 'pull_request' AND art.state = 'draft')
	OR (art.kind = 'review' AND art.state = 'pending'
	    AND COALESCE(
	          CASE WHEN LEFT(BTRIM(art.details_json), 1) = '{'
	               THEN art.details_json::jsonb ->> 'review_event' END, '') <> '')`

// pgConversationAttentionSQL is the "YOUR MOVE" predicate — the rail's
// `needs`, counted per conversation — written against the conversation alias
// `r`: an unanswered permission prompt, OR a conversation that is not live and
// still holds an unresolved artifact.
//
// Pending is derived the way PermissionStore.ListPending derives it (state
// 'pending' AND owned by the conversation's currently-active claim), so a
// prompt left behind by a process that no longer exists doesn't hold the count
// up without anything having written its expiry.
//
// The liveness half reads the DISPLAY status, not the stored column: a
// conversation mid-claim carries no stored status at all, and counting it as
// needing a human while its agent is still working is the exact
// disagreement-with-the-surface this predicate exists to avoid.
const pgConversationAttentionSQL = `(
		EXISTS (
			SELECT 1 FROM conversation_permissions p
			WHERE p.org_id = r.org_id AND p.conversation_id = r.id AND p.state = 'pending'
			  AND p.claim_id = (SELECT cl_p.id FROM claims cl_p
			                    WHERE cl_p.conversation_id = r.id AND cl_p.released_at IS NULL))
		OR ((` + pgDisplayStatusSQL + `) NOT IN (` + pgConversationLiveStatusesSQL + `)
		    AND EXISTS (
		        SELECT 1 FROM artifacts art
		        WHERE art.org_id = r.org_id AND art.conversation_id = r.id
		          AND (` + pgUnresolvedArtifactSQL + `)))
	)`

// conversationClaimLateral derives the claim-facing Conversation fields from claims:
// ClaimedAt is the latest claim's claimed_at, Attempts the count of
// engagements, ExecutorID and phase the active (unreleased) claim's, and
// duration_ms/num_turns the SUM of the per-engagement telemetry. The
// aggregate lateral always yields exactly one row, so a never-claimed
// conversation reads (NULL, 0, NULL, NULL, NULL, NULL).
//
// `attempts` here is the LIFETIME claim count — engagement history for a
// human, matching the lifetime sums beside it. It is deliberately not the
// same quantity ClaimNextConversation returns under that name, which is the retry
// budget's current-episode counter (EpisodeAttemptsSQL, conversation_queue.go). Two
// questions, one field; see domain.Conversation.Attempts before carrying
// either one somewhere new.
const conversationClaimLateral = `
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

// conversationLedgerLateral derives the money/token accounting from the messages
// ledger: total_cost_usd is the SUM of the settlement stamps (NULL until
// anything settles — the frozen wire semantics of the former stored
// column), the token columns the SUM over every row. Always exactly one
// row, so a message-less conversation reads (NULL, 0, 0, 0, 0).
const conversationLedgerLateral = `
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

// writeConversationReturning wraps a single-row INSERT or UPDATE on
// conversations in a data-modifying CTE, then re-joins its output row through
// the exact same projection getConversation uses (pgConversationColumns, the
// conversation_memory/agents LEFT JOINs, the claim/ledger laterals) — so a
// conversations-row write shares the point read's column list and scanner
// without hand-duplicating those joins as scalar subqueries the way the
// SQLite dialect has to (Postgres's WITH...RETURNING has no such
// restriction). This stays ONE statement — a data-modifying CTE plus the
// SELECT that reads it — so it is still "the write statement itself", not a
// follow-up read.
//
// Only sound when nothing else in writeSQL's own WHERE/SET depends on a
// sibling data-modifying CTE in the same statement: Postgres evaluates
// multiple data-modifying CTEs against one shared snapshot, so a second CTE
// would NOT see this one's write. There is exactly one data-modifying CTE
// here, so that caveat doesn't apply — but it is why any prep work this
// write's derived columns depend on (a claim release, a cost settlement) must
// run as its own EARLIER statement in the same transaction, never folded into
// this WITH. See completeConversationFlip for the write that has such prep
// work.
//
// writeSQL must itself end in `RETURNING *`.
func writeConversationReturning(ctx context.Context, q queryer, writeSQL string, args ...any) (*domain.Conversation, error) {
	row := q.QueryRowContext(ctx, `
		WITH updated AS (`+writeSQL+`)
		SELECT `+pgConversationColumns+`
		FROM updated r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		`+conversationClaimLateral+`
		`+conversationLedgerLateral+`
	`, args...)
	var res domain.Conversation
	if err := scanConversation(row, &res); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, db.ErrNoSuchConversation
		}
		return nil, err
	}
	return &res, nil
}

func (s *conversationStore) Get(ctx context.Context, orgID, conversationID string) (*domain.Conversation, error) {
	return getConversation(ctx, s.q, orgID, conversationID)
}

func (s *conversationStore) GetSystem(ctx context.Context, orgID, conversationID string) (*domain.Conversation, error) {
	return getConversation(ctx, s.admin, orgID, conversationID)
}

func (s *conversationStore) LookupOrgForConversationSystem(ctx context.Context, conversationID string) (string, error) {
	var orgID string
	err := s.admin.QueryRowContext(ctx, `SELECT org_id::text FROM conversations WHERE id = $1`, conversationID).Scan(&orgID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return orgID, err
}

func getConversation(ctx context.Context, q queryer, orgID, conversationID string) (*domain.Conversation, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+pgConversationColumns+`
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		`+conversationClaimLateral+`
		`+conversationLedgerLateral+`
		WHERE r.org_id = $1 AND r.id = $2
	`, orgID, conversationID)

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
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgConversationColumns+`
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		`+conversationClaimLateral+`
		`+conversationLedgerLateral+`
		WHERE r.org_id = $1 AND r.task_id = $2
		ORDER BY r.started_at DESC
	`, orgID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convs []domain.Conversation
	for rows.Next() {
		var r domain.Conversation
		if err := scanConversation(rows, &r); err != nil {
			return nil, err
		}
		convs = append(convs, r)
	}
	return convs, rows.Err()
}

func (s *conversationStore) List(ctx context.Context, orgID string, filter db.ConversationListFilter, opts db.ListOpts) ([]domain.Conversation, int, error) {
	where, args, ok := pgConversationListWhere(orgID, filter)
	if !ok {
		return nil, 0, nil
	}
	var total int
	// The count runs the same WHERE against the same alias as the page, so a
	// count-only read (the rail's) and a paged one can't disagree about what
	// they are counting. App pool (RLS-active): the filtered total is the
	// caller's own visible set, which is what makes an unnarrowed read a
	// legitimate answer rather than a cross-team leak.
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations r WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Conversation{}, total, nil
	}
	// ListForTask's projection plus the queue position, which is a list-only
	// column: the surface that renders a place in line renders a line. A
	// caller that named task ids groups the flat result by TaskID.
	query := `
		WITH ` + pgQueuePositionCTE + `
		SELECT ` + pgConversationColumns + `, qp.position
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		` + conversationClaimLateral + `
		` + conversationLedgerLateral + `
		LEFT JOIN queued_positions qp ON qp.conversation_id = r.id
		WHERE ` + where + `
		ORDER BY r.task_id, r.started_at DESC, r.id`
	if opts.Limit > 0 {
		query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var convs []domain.Conversation
	for rows.Next() {
		var r domain.Conversation
		var position sql.NullInt64
		if err := scanConversation(rows, &r, &position); err != nil {
			return nil, 0, err
		}
		if position.Valid {
			p := int(position.Int64)
			r.QueuePosition = &p
		}
		convs = append(convs, r)
	}
	return convs, total, rows.Err()
}

// pgConversationListWhere renders ConversationStore.List's filter as a WHERE
// body over the conversation alias `r`, plus its bind args. ok=false means the
// filter can match nothing at all, so the caller answers with an empty result
// instead of running two queries to learn it.
//
// The artifacts / conversation_permissions reads inside the attention
// predicate are correlated subqueries rather than joins, so nothing here can
// multiply the row count — a conversation with three draft PRs is one row and
// one unit of `needs`.
func pgConversationListWhere(orgID string, filter db.ConversationListFilter) (string, []any, bool) {
	where := `r.org_id = $1`
	args := []any{orgID}
	if len(filter.TaskIDs) > 0 {
		// task_id is a uuid column: a non-UUID id (these are client-supplied
		// on the batched conversation-list path) would fail Postgres parsing
		// with 22P02 → 500 before the row filter runs, so drop invalid ids up
		// front and treat them as "no rows" — the read-method convention in
		// uuid.go. A caller that named ids and had every one dropped asked
		// about nothing, which is not the same request as naming none.
		ids := filterValidUUIDs(filter.TaskIDs)
		if len(ids) == 0 {
			return "", nil, false
		}
		// The slice binds as a uuid[] literal through one $N (pgUUIDArray),
		// like artifactStore.ListByConversations — not a raw []string.
		args = append(args, pgUUIDArray(ids))
		where += fmt.Sprintf(` AND r.task_id = ANY($%d)`, len(args))
	}
	if len(filter.TeamIDs) > 0 {
		// team_id is a uuid column too, so the same drop-invalid-then-refuse
		// treatment TaskIDs gets: a set that named only unusable ids asked
		// about nothing, which must not widen back to every team.
		ids := filterValidUUIDs(filter.TeamIDs)
		if len(ids) == 0 {
			return "", nil, false
		}
		args = append(args, pgUUIDArray(ids))
		where += fmt.Sprintf(` AND r.team_id = ANY($%d)`, len(args))
	}
	if len(filter.Statuses) > 0 {
		// One placeholder per value rather than an array literal: these are
		// client-supplied names, and a placeholder needs no quoting rules to
		// be safe under one.
		marks := make([]string, len(filter.Statuses))
		for i, st := range filter.Statuses {
			args = append(args, st)
			marks[i] = fmt.Sprintf("$%d", len(args))
		}
		where += ` AND (` + pgDisplayStatusSQL + `) IN (` + strings.Join(marks, ",") + `)`
	}
	if filter.Attention {
		where += ` AND ` + pgConversationAttentionSQL
	}
	return where, args, true
}

func (s *conversationStore) ListPRCoherenceTargetsSystem(ctx context.Context, orgID string, query db.PRCoherenceTargetQuery) ([]domain.PRCoherenceTarget, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT r.id::text, COALESCE(r.task_id::text, ''), COALESCE(r.status, ''),
		       COALESCE(r.outcome, ''),
		       EXISTS (SELECT 1 FROM claims cl WHERE cl.org_id = $1 AND cl.conversation_id = r.id AND cl.released_at IS NULL)
		FROM conversations r
		LEFT JOIN tasks t ON t.org_id = $1 AND t.id = r.task_id
		WHERE r.org_id = $1
		  AND r.type = 'delegation'
		  AND (
		       t.entity_id = $2
		       OR EXISTS (
		           SELECT 1 FROM artifacts a
		           WHERE a.org_id = $1 AND a.conversation_id = r.id
		             AND a.kind = $6 AND a.state = $7 AND a.target = $8
		       )
		       OR EXISTS (
		           SELECT 1
		           FROM conversation_worktrees w
		           JOIN repositories repo ON repo.org_id = $1 AND repo.id = w.repository_id
		           WHERE w.org_id = $1 AND w.conversation_id = r.id
		             AND (
		                  (LOWER(repo.owner || '/' || repo.repo) = LOWER($4) AND w.ref = $9)
		                  OR (w.ref = $10 AND (
		                       LOWER(repo.owner || '/' || repo.repo) = LOWER($4)
		                       OR LOWER(repo.owner || '/' || repo.repo) = LOWER($5)
		                  ))
		             )
		       )
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM task_events te
		      WHERE te.org_id = $1 AND te.task_id = r.task_id AND te.event_id = $3 AND te.kind = 'injected'
		  )
		ORDER BY r.started_at ASC, r.id ASC
	`, orgID, query.EntityID, query.EventID, query.BaseRepo, query.HeadRepo, domain.ArtifactKindReview, domain.ArtifactStateReviewPending, query.ReviewTarget, query.PRRef, query.BranchRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PRCoherenceTarget
	for rows.Next() {
		var target domain.PRCoherenceTarget
		if err := rows.Scan(&target.ConversationID, &target.TaskID, &target.Status, &target.Outcome, &target.Active); err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

// HasActiveAutoConversationForTask: any non-terminal trigger_type='event'
// conversation on the task. Manual delegations are excluded. Used by the
// router's per-task firing gate.
func (s *conversationStore) HasActiveAutoConversationForTask(ctx context.Context, orgID, taskID string) (bool, error) {
	return hasActiveAutoConversationForTask(ctx, s.q, orgID, taskID)
}

func (s *conversationStore) HasActiveAutoConversationForTaskSystem(ctx context.Context, orgID, taskID string) (bool, error) {
	return hasActiveAutoConversationForTask(ctx, s.admin, orgID, taskID)
}

func hasActiveAutoConversationForTask(ctx context.Context, q queryer, orgID, taskID string) (bool, error) {
	if !isValidUUID(taskID) {
		return false, nil
	}
	var count int
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations r
		WHERE r.org_id = $1
		  AND r.task_id = $2
		  AND r.trigger_type = 'event'
		  AND (r.status IS NULL
		       OR r.status NOT IN (`+conversationTerminalStatusesSQL+`))
	`, orgID, taskID).Scan(&count)
	return count > 0, err
}

func (s *conversationStore) ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error) {
	return activeConversationIDsForTask(ctx, s.q, orgID, taskID)
}

func (s *conversationStore) ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return activeConversationIDsForTask(ctx, s.admin, orgID, taskID)
}

func (s *conversationStore) ActiveAutoConversationIDForTaskSystem(ctx context.Context, orgID, taskID string) (string, error) {
	if !isValidUUID(taskID) {
		return "", nil
	}
	var id string
	err := s.admin.QueryRowContext(ctx, `
		SELECT r.id FROM conversations r
		WHERE r.org_id = $1
		  AND r.task_id = $2
		  AND r.trigger_type = 'event'
		  AND (r.status IS NULL
		       OR r.status NOT IN (`+conversationTerminalStatusesSQL+`))
		ORDER BY r.started_at DESC
		LIMIT 1
	`, orgID, taskID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func activeConversationIDsForTask(ctx context.Context, q queryer, orgID, taskID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE org_id = $1 AND task_id = $2
		  AND (status IS NULL
		       OR status NOT IN (`+conversationTerminalStatusesSQL+`))
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

func (s *conversationStore) ActiveIDsForTeamSystem(ctx context.Context, orgID, teamID string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE org_id = $1 AND team_id = $2
		  AND (status IS NULL
		       OR status NOT IN (`+conversationTerminalStatusesSQL+`))
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
// must keep warm — parked `open` conversations whose owning blueprint_run is
// still 'running'. A parked conversation under an already-terminal
// blueprint_run is NOT resumable (every resume path gates on cr.Status ==
// running), so its worktree must NOT be preserved: preserving it would leave
// a checked-out branch on disk that the boot reconcile then orphans by
// cancelling the row, reviving the "refusing to fetch into a branch checked
// out in a worktree" loop. Admin pool — the startup sweep has no JWT-claims
// context.
func (s *conversationStore) ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error) {
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

func (s *conversationStore) EntitiesWithOpenConversations(ctx context.Context, orgID string, entityIDs []string) (map[string]struct{}, error) {
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

func (s *conversationStore) InsertMessage(ctx context.Context, orgID string, msg *domain.Message) (int64, error) {
	return insertConversationMessage(ctx, s.q, orgID, msg)
}

func (s *conversationStore) InsertMessageSystem(ctx context.Context, orgID string, msg *domain.Message) (int64, error) {
	return insertConversationMessage(ctx, s.admin, orgID, msg)
}

// InsertMessageForClaimSystem is the transcript write an engagement makes
// while it is streaming: the row is attributed to claimID, and claimID has to
// still be live for it to land at all. The explicit argument overwrites
// msg.ClaimID so the row that persists and the claim the fence validated are
// the same one — a row attributed to a claim the writer does not hold is the
// misattribution this whole path exists to prevent.
func (s *conversationStore) InsertMessageForClaimSystem(ctx context.Context, orgID, claimID string, msg *domain.Message) (int64, error) {
	var id int64
	err := inTx(ctx, s.admin, func(q queryer) error {
		// The row's own conversation is what the claim must own — the fence
		// and the INSERT below must not be able to name different ones.
		if err := assertClaimActive(ctx, q, orgID, msg.ConversationID, claimID); err != nil {
			return err
		}
		msg.ClaimID = claimID
		var err error
		id, err = insertConversationMessage(ctx, q, orgID, msg)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// LastAgentActivityAtSystem returns the created_at of the conversation's
// newest non-user message (the artifact-change ledger watermark). Ordered by id
// DESC (the monotonic sequence) so the watermark is the genuinely last-inserted
// agent row. Admin pool: the resume path holds no JWT claims. ok=false when the
// conversation has no agent message yet.
func (s *conversationStore) LastAgentActivityAtSystem(ctx context.Context, orgID, conversationID string) (time.Time, bool, error) {
	var at time.Time
	err := s.admin.QueryRowContext(ctx, `
		SELECT created_at FROM messages
		WHERE org_id = $1 AND conversation_id = $2 AND role <> 'user'
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

// insertConversationMessage attributes the row to an engagement: an explicit
// non-empty msg.ClaimID always wins (a caller that already knows its own
// claim id names it), otherwise claim_id resolves server-side to the
// conversation's active claim. Rows written during an engagement belong to it; rows
// written outside one (pending inputs, queued turns, injections)
// correctly resolve NULL. A message racing its claim's release lands
// NULL — harmless, the terminal settle's newest-row fallback still
// reaches it.
func insertConversationMessage(ctx context.Context, q queryer, orgID string, msg *domain.Message) (int64, error) {
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
		                      reasoning, content_blocks, delivered, window_state, seq, duration_ms,
		                      stop_reason)
		VALUES ($1, $2, $3,
		        COALESCE($4, (SELECT id FROM claims
		                      WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL)),
		        $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		        $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
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
		nullIfEmpty(msg.StopReason),
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
	reasoning::text, content_blocks::text, delivered, window_state, seq, duration_ms,
	COALESCE(stop_reason, '')`

// scanMessageRows drains a messages result set selecting
// pgMessageColumns into domain.Message values. Shared by the
// single-conversation Messages and the batched MessagesForConversations.
func scanMessageRows(rows *sql.Rows) ([]domain.Message, error) {
	var messages []domain.Message
	for rows.Next() {
		m, err := scanOneMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// messageScanner is satisfied by both *sql.Row and *sql.Rows, so
// scanOneMessage serves the multi-row read above and single-row RETURNING
// reads (SettleCompactionRequestForClaimSystem) off one column layout.
type messageScanner interface {
	Scan(dest ...any) error
}

func scanOneMessage(row messageScanner) (domain.Message, error) {
	var m domain.Message
	var content, subtype, toolCallsStr, toolCallID, metadataStr, model sql.NullString
	var inputTok, outputTok, cacheReadTok, cacheCreateTok sql.NullInt64
	var costUSD sql.NullFloat64
	var reasoningStr, contentBlocksStr sql.NullString
	var delivered bool
	var windowState string
	var seq sql.NullFloat64
	var durationMs sql.NullInt64

	if err := row.Scan(
		&m.ID, &m.ConversationID, &m.UserID, &m.ClaimID, &m.Role, &content, &subtype, &toolCallsStr,
		&toolCallID, &m.IsError, &metadataStr, &model,
		&inputTok, &outputTok, &cacheReadTok, &cacheCreateTok, &costUSD, &m.CreatedAt,
		&reasoningStr, &contentBlocksStr, &delivered, &windowState, &seq, &durationMs, &m.StopReason,
	); err != nil {
		return domain.Message{}, err
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
			return domain.Message{}, fmt.Errorf("unmarshal reasoning (message %d): %w", m.ID, err)
		}
	}
	if contentBlocksStr.Valid && contentBlocksStr.String != "" {
		if err := json.Unmarshal([]byte(contentBlocksStr.String), &m.ContentBlocks); err != nil {
			return domain.Message{}, fmt.Errorf("unmarshal content_blocks (message %d): %w", m.ID, err)
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
	return m, nil
}

// scanMessageRow scans a single RETURNING pgMessageColumns row into a
// domain.Message, or ErrNoSuchMessage on sql.ErrNoRows.
func scanMessageRow(row *sql.Row) (*domain.Message, error) {
	m, err := scanOneMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, db.ErrNoSuchMessage
		}
		return nil, err
	}
	return &m, nil
}

// Messages is the whole-transcript display read — MessagesSince from the
// bottom. id is a bigserial, so no row can be at or below 0 and the watermark
// drops out: routing both through one query is what makes the two reads'
// visibility filter identical rather than merely matching.
func (s *conversationStore) Messages(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	return s.MessagesSince(ctx, orgID, conversationID, 0)
}

func (s *conversationStore) MessagesSince(ctx context.Context, orgID, conversationID string, sinceID int) ([]domain.Message, error) {
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
	`, orgID, conversationID, sinceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

func (s *conversationStore) MessagesWindow(ctx context.Context, orgID, conversationID string, w db.MessageWindow) ([]domain.Message, error) {
	// Same visibility filter and same placement ordering as MessagesSince —
	// see its comment. What differs is the window: a backward page selects
	// the NEWEST rows below a ceiling, which means ordering DESC to pick them
	// and reversing afterwards so the caller always receives oldest-first.
	const orderKey = `COALESCE(seq, (id)::double precision)`
	where := `org_id = $1 AND conversation_id = $2 AND NOT (delivered = false AND window_state = 'inactive')`
	args := []any{orgID, conversationID}
	order := ` ORDER BY ` + orderKey + ` ASC`
	reverse := false
	switch {
	case w.SinceID > 0:
		args = append(args, w.SinceID)
		where += fmt.Sprintf(" AND id > $%d", len(args))
	case w.BeforeID > 0:
		args = append(args, w.BeforeID)
		where += fmt.Sprintf(" AND id < $%d", len(args))
		order = ` ORDER BY ` + orderKey + ` DESC`
		reverse = true
	default:
		if w.Limit > 0 {
			order = ` ORDER BY ` + orderKey + ` DESC`
			reverse = true
		}
	}
	query := `SELECT ` + pgMessageColumns + ` FROM messages WHERE ` + where + order
	if w.Limit > 0 {
		args = append(args, w.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
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
	// conversation_id is a uuid column; drop any non-UUID id (22P02 → 500
	// guard, same read convention as ListForTasks). These ids are
	// server-derived today, so this is defense in depth against a future
	// caller passing raw input.
	conversationIDs = filterValidUUIDs(conversationIDs)
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	// App pool (RLS-active): conversation_id is a uuid column, so the slice
	// binds as a uuid[] literal through one $N (pgUUIDArray), mirroring
	// artifactStore.ListByConversations. Ordering on (conversation_id, the effective
	// assembly key) so the caller groups by ConversationID with each
	// conversation's messages in the same order the single-conversation display
	// read gives them. Withdrawn-pending rows are hidden, same as Messages.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgMessageColumns+`
		FROM messages
		WHERE org_id = $1 AND conversation_id = ANY($2)
		  AND NOT (delivered = false AND window_state = 'inactive')
		ORDER BY conversation_id ASC, COALESCE(seq, (id)::double precision) ASC
	`, orgID, pgUUIDArray(conversationIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

// ListForAssemblySystem returns every row a native loop needs to rebuild this
// conversation's exact LLM context, ordered by the effective assembly key
// COALESCE(seq, id). window_state='inactive' rows are excluded (superseded by
// compaction); 'elided' and undelivered rows are included — see the interface
// doc for the full contract. Pure read over messages; no other table or
// in-process state feeds in.
//
// Admin pool, org bound by argument: the only caller is the native loop, which
// drives a claimed conversation on an executor with no request identity to
// authenticate as. On the app pool this is an RLS read, and RLS evaluation
// calls current_user_id() — which a JWT-less background job has no permission
// to execute, so every assembly would fail at the database rather than return
// an empty window.
func (s *conversationStore) ListForAssemblySystem(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgMessageColumns+`
		FROM messages
		WHERE org_id = $1 AND conversation_id = $2 AND window_state <> 'inactive'
		ORDER BY COALESCE(seq, (id)::double precision) ASC
	`, orgID, conversationID)
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
func (s *conversationStore) MarkDeliveredForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, ids []int, subtype string) error {
	if len(ids) == 0 {
		// Nothing to consume, so nothing to fence: an empty batch writes no
		// rows on any path, and refusing it would make the caller handle a
		// fence trip that could not have corrupted anything.
		return nil
	}
	return inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `
			UPDATE messages SET delivered = true, subtype = COALESCE(NULLIF($4, ''), subtype)
			WHERE org_id = $1 AND conversation_id = $2 AND id = ANY($3)
		`, orgID, conversationID, pgIntArray(ids), subtype)
		return err
	})
}

// CompactForClaimSystem commits one compaction atomically, behind the fence —
// see the interface doc for the full contract. The re-seq reads the queued
// rows inside the same transaction that inserts the result row, so the
// fractions are computed against a queue no concurrent enqueue can reorder
// under it: a row inserted after this transaction commits carries an id
// greater than the result row's and sorts after every fraction on its own.
//
// Exempt from the returned-row rule (bare error stays the return type): this
// writes up to two inserted rows, a batch flip, and a re-seq of every queued
// row ahead of the result — no single row a return value could name.
// replyRow/resultRow's assigned IDs are written back to the caller's own
// pointers, which stands in for "the row it persisted" here.
func (s *conversationStore) CompactForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error {
	return inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		if replyRow != nil {
			replyRow.ConversationID = conversationID
			replyRow.ClaimID = claimID
			replyRow.WindowState = domain.MessageWindowInactive
			id, err := insertConversationMessage(ctx, q, orgID, replyRow)
			if err != nil {
				return fmt.Errorf("insert compaction reply row: %w", err)
			}
			replyRow.ID = int(id)
		}
		resultRow.ConversationID = conversationID
		resultRow.ClaimID = claimID
		resultID, err := insertConversationMessage(ctx, q, orgID, resultRow)
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
func (s *conversationStore) SettleCompactionRequestForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) (*domain.Message, error) {
	var result *domain.Message
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, conversationID, claimID); err != nil {
			return err
		}
		row := q.QueryRowContext(ctx, `
			UPDATE messages
			SET input_tokens = $4, output_tokens = $5,
			    cache_read_tokens = $6, cache_creation_tokens = $7,
			    cost_usd = $8,
			    metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object('compaction_failure', $9::text)
			WHERE org_id = $1 AND conversation_id = $2 AND id = $3
			RETURNING `+pgMessageColumns, orgID, conversationID, requestID,
			inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens,
			nullFloatPtr(costUSD), reason)
		m, err := scanMessageRow(row)
		if err != nil {
			return err
		}
		result = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SetWindowStateSystem is the elision/compaction primitive: a batched range
// flip of window_state from `from` to `to`, restricted to rows currently in
// state `from` whose effective assembly key (COALESCE(seq, id)) is strictly
// less than beforeSeq. Returns the number of rows flipped.
//
// Admin pool for the same reason as ListForAssemblySystem: the native loop is
// the only caller and holds no request identity.
func (s *conversationStore) SetWindowStateSystem(ctx context.Context, orgID, conversationID string, beforeSeq float64, from, to domain.MessageWindowState) (int, error) {
	result, err := s.admin.ExecContext(ctx, `
		UPDATE messages
		SET window_state = $1
		WHERE org_id = $2 AND conversation_id = $3 AND window_state = $4
		  AND COALESCE(seq, (id)::double precision) < $5
	`, string(to), orgID, conversationID, string(from), beforeSeq)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

func (s *conversationStore) TokenTotalsSystem(ctx context.Context, orgID, conversationID string) (*domain.TokenTotals, error) {
	row := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(model), ''),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COUNT(*)
		FROM messages
		WHERE org_id = $1 AND conversation_id = $2 AND role = 'assistant'
	`, orgID, conversationID)

	var t domain.TokenTotals
	if err := row.Scan(&t.Model, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheCreationTokens, &t.NumTurns); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *conversationStore) BlueprintSiblingCostUSDSystem(ctx context.Context, orgID, blueprintRunID, excludeConversationID string) (float64, error) {
	var cost sql.NullFloat64
	err := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(m.cost_usd), 0)
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id AND c.org_id = m.org_id
		WHERE c.org_id = $1 AND c.blueprint_run_id = $2 AND c.id <> $3
	`, orgID, blueprintRunID, excludeConversationID).Scan(&cost)
	if err != nil {
		return 0, err
	}
	return cost.Float64, nil
}

func (s *conversationStore) BlueprintSiblingDurationMsSystem(ctx context.Context, orgID, blueprintRunID, excludeConversationID string) (int, error) {
	var ms sql.NullInt64
	err := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cl.duration_ms), 0)
		FROM claims cl
		JOIN conversations c ON c.id = cl.conversation_id AND c.org_id = cl.org_id
		WHERE c.org_id = $1 AND c.blueprint_run_id = $2 AND c.id <> $3
	`, orgID, blueprintRunID, excludeConversationID).Scan(&ms)
	if err != nil {
		return 0, err
	}
	return int(ms.Int64), nil
}

// --- Helpers ---

// conversationScanner is satisfied by both *sql.Row and *sql.Rows, so
// scanConversation serves the point read, the write-returning read and the
// list reads off one destination list rather than copies kept in step by hand.
type conversationScanner interface {
	Scan(dest ...any) error
}

// scanConversation scans pgConversationColumns into r. extra takes the
// destinations for whatever a caller appended to that list — the list read's
// queue position, today — in the order it appended them.
func scanConversation(sc conversationScanner, r *domain.Conversation, extra ...any) error {
	var queuedAt, claimedAt, completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var blueprintRunID sql.NullString
	var failureKind, parkReason string

	dest := []any{
		&r.ID, &r.TaskID, &r.Runtime, &r.Status, &r.Model, &r.StartedAt, &queuedAt, &claimedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &parkReason, &r.WorktreePath,
		&r.ResultSummary, &r.Outcome, &r.OutcomeReason, &failureKind, &r.SessionID, &r.ActorAgentID, &r.TriggerType, &r.CreatorUserID, &r.TeamID, &r.ExecutorID, &r.Attempts, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	r.FailureKind = domain.ConversationFailureKind(failureKind)
	r.ParkReason = domain.ParkReason(parkReason)
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
