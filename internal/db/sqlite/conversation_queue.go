package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// conversationQueueStore is the SQLite impl of db.ConversationQueueStore — the
// durable conversation queue the delegation dispatcher drains. SQLite/local is single-worker (N=1),
// so ClaimNextConversation doesn't need the FOR UPDATE SKIP LOCKED the Postgres impl
// uses; a short transaction pairing the status flip with the claims-row mint
// is atomic enough with one dispatcher.
type conversationQueueStore struct {
	conn *sql.DB
}

func newConversationQueueStore(conn *sql.DB) db.ConversationQueueStore {
	return &conversationQueueStore{conn: conn}
}

var _ db.ConversationQueueStore = (*conversationQueueStore)(nil)

// conversationTerminalStatusesSQL is the terminal conversation statuses as a SQL
// IN-list body — two names, one owner each: the agent concluded, or the
// infrastructure died. It describes stored rows as faithfully as new writes,
// because every retired status was rewritten by migration rather than carried
// forward (202608010002, SQLite; Postgres had no rows to migrate). Mirrors
// domain.AllTerminalConversationStatuses.
//
// Every exclusion predicate in this package interpolates this rather than
// re-spelling the literals. That matters more than the saved keystrokes: these
// guards are exclusions (`status NOT IN (…)`), so a status missing from one
// doesn't fail closed — it readmits a finished conversation to parking, cancelling, or
// the active-work counters. Sixteen hand-copied copies is how the set drifted
// a value at a time.
const conversationTerminalStatusesSQL = `'completed','failed'`

// --- The needs-driving predicate ---------------------------------------
//
// The SQLite mirror of the Postgres fragments — see
// internal/db/postgres/conversation_queue.go for the model. Stored conversation
// status is outcome-or-nothing ('open' | a terminal | NULL); "queued" and
// "running" are derived from the claim table and these predicates, never
// stored. Every fragment is written against the conversation alias `r`.

// activeClaimExistsSQL is the derived "running": an unreleased claim is the
// engagement driving this conversation. Served by idx_claims_one_active.
const activeClaimExistsSQL = `EXISTS (
		SELECT 1 FROM claims cl_a
		WHERE cl_a.conversation_id = r.id AND cl_a.released_at IS NULL)`

// sqliteNowExpr is fresh database time in the layout the claims table stores
// its timestamps in, and the ONE spelling every lease comparison on this
// dialect reads. The layout is load-bearing: both sides of a `>` on
// lease_expires_at are compared as text, so a second spelling that rendered
// the same instant differently would silently compare wrong.
//
// SQLite advances 'now' across statements inside a transaction, which is what
// an expiry guard needs: the reading is taken at the guard, not at BEGIN —
// the property the Postgres twin gets from statement_timestamp().
const sqliteNowExpr = `strftime('%Y-%m-%d %H:%M:%f', 'now')`

// sqliteNowPlusExpr is sqliteNowExpr offset by a bound modifier — the one
// spelling every lease STAMP on this dialect is written with, for the reason
// above: a stamp rendered in a layout the comparisons do not share would pass
// its own test and fail theirs. The bind takes sqliteLeaseModifier's output.
const sqliteNowPlusExpr = `strftime('%Y-%m-%d %H:%M:%f', 'now', ?)`

// sqliteLeaseModifier renders a lease as SQLite's signed "NNN.NNN seconds"
// date-function modifier, the offset sqliteNowPlusExpr binds. Millisecond
// resolution matches what %f stores.
func sqliteLeaseModifier(d time.Duration) string {
	return fmt.Sprintf("%+.3f seconds", d.Seconds())
}

// liveClaimExistsSQL is the narrower question the DISPLAY asks — an
// engagement actually alive on this conversation right now. The Postgres twin
// carries the model, including why the ownership predicates keep reading
// released_at alone.
const liveClaimExistsSQL = `EXISTS (
		SELECT 1 FROM claims cl_a
		WHERE cl_a.conversation_id = r.id AND cl_a.released_at IS NULL
		  AND cl_a.lease_expires_at > ` + sqliteNowExpr + `)`

// undeliveredInputExistsSQL matches drivable input: a plain user message
// still awaiting delivery. Injections ride whatever engagement runs next and
// never wake one on their own; a withdrawn row (undelivered + window_state
// 'inactive') never happened. Nothing here reads `seq`, which compaction
// re-writes to fractional values. Served by idx_messages_undelivered.
const undeliveredInputExistsSQL = `EXISTS (
		SELECT 1 FROM messages m_i
		WHERE m_i.conversation_id = r.id AND m_i.delivered = 0
		  AND m_i.role = 'user' AND m_i.subtype = '' AND m_i.window_state = 'active')`

// needsDrivingSQL is the eligibility predicate, identical for every surface:
// nobody is driving it, it has not been retired, and it is either mid-flight
// (fresh mint, or a claim that released without writing an outcome) or
// parked and woken by new input. A terminal conversation is never eligible,
// and neither is one with a pending stop — the settlement parks it instead.
const needsDrivingSQL = `r.archived_at IS NULL
	  AND r.stop_requested_at IS NULL
	  AND NOT ` + activeClaimExistsSQL + `
	  AND (r.status IS NULL OR (r.status = 'open' AND ` + undeliveredInputExistsSQL + `))`

// eligibleForDrivingSQL is the surface-agnostic "waiting to be driven" —
// what the queue-depth counters and the display projection's derived
// `queued` rung read.
const eligibleForDrivingSQL = needsDrivingSQL

// blueprintDrivableSQL is the delegation arm's gate, applied over a LEFT
// JOIN so a conversation with no blueprint parent (interactive, tomorrow)
// is not filtered out by the join itself. The rest — why a called-off
// blueprint drives nothing and is checked on both of its columns, why
// a blueprint drives only the one conversation its `current_step_index`
// names whatever its status, why only the task's live conversation is
// drivable at all, and why a task still owing a memory drives nothing — is
// the Postgres twin's; this is the same predicate in the other dialect.
var blueprintDrivableSQL = `((r.blueprint_run_id IS NULL
	    OR (br.cancel_requested = 0 AND br.status <> 'cancelled'
	        AND r.blueprint_step_index = br.current_step_index))
	   AND (r.task_id IS NULL OR r.id = ` + taskLiveConversationSQL("r.org_id", "r.task_id") + `)
	   AND (r.task_id IS NULL OR NOT ` + taskMemoryPendingSQL("r.org_id", "r.task_id") + `))`

// taskLiveConversationSQL is the SQLite spelling of the conversation that owns
// the task's tree: the newest row of liveTopLevelConversationSQL
// (conversation.go). Postgres holds the twin definition in
// internal/db/postgres/conversation_queue.go, and with it why the boundary
// rather than the status decides which conversation that is. Same words, same
// index (idx_conversations_task_open).
func taskLiveConversationSQL(orgExpr, taskExpr string) string {
	return `(SELECT live.id FROM conversations live
		WHERE live.org_id = ` + orgExpr + ` AND live.task_id = ` + taskExpr + `
		  AND ` + liveTopLevelConversationSQL("live") + `
		ORDER BY live.started_at DESC, live.id DESC
		LIMIT 1)`
}

// handedBackOutcomesSQL is every claim outcome that records nothing about the
// conversation, so none of them ends a queue episode. The Postgres twin
// carries what each one means and which budget it spends.
const handedBackOutcomesSQL = `'requeued','requeued_credentials','reaped','requeued_shutdown'`

// episodeHandBacksSQL counts the current queue episode's hand-backs whose
// outcome is in outcomesSQL, against the conversation alias convAlias. The one
// definition of where an episode starts, shared by every count below so they
// cannot disagree about it; the Postgres twin carries the model.
func episodeHandBacksSQL(convAlias, outcomesSQL string) string {
	return `(SELECT COUNT(*) FROM claims c2
	WHERE c2.conversation_id = ` + convAlias + `.id
	  AND c2.outcome IN (` + outcomesSQL + `)
	  AND NOT EXISTS (
	      SELECT 1 FROM claims c3
	      WHERE c3.conversation_id = c2.conversation_id
	        AND c3.outcome IS NOT NULL
	        AND c3.outcome NOT IN (` + handedBackOutcomesSQL + `)
	        AND c3.claimed_at >= COALESCE(c2.released_at, c2.claimed_at)))`
}

// EpisodeSetupFailuresSQL is the setup budget's unit: the current episode's
// 'requeued' hand-backs, against the conversation alias convAlias.
func EpisodeSetupFailuresSQL(convAlias string) string {
	return episodeHandBacksSQL(convAlias, `'requeued'`)
}

// EpisodeLostEngagementsSQL is the loss budget's unit: the current episode's
// 'reaped' hand-backs, against the conversation alias convAlias.
func EpisodeLostEngagementsSQL(convAlias string) string {
	return episodeHandBacksSQL(convAlias, `'reaped'`)
}

// conversationQueueClaimCols is the column list ClaimNextConversation returns, shared with the
// scan helper. visibility is left at its row default by the mint; team_id is
// surfaced so the construction-path ConversationInfo built off a claimed
// conversation carries the owning team for the capture writers. The claim
// identity fields (ExecutorID/ClaimedAt) and the episode counts are hydrated from the
// freshly minted claims row, not this projection.
const conversationQueueClaimCols = `r.id, r.org_id, COALESCE(r.type, ''), COALESCE(r.task_id, ''), COALESCE(r.prompt_id, ''),
	COALESCE(r.model, ''), COALESCE(r.runtime, ''),
	COALESCE(r.worktree_path, ''), COALESCE(r.sdk_session_id, ''), r.trigger_type, COALESCE(r.trigger_id, ''),
	COALESCE(r.creator_user_id, ''), COALESCE(r.team_id, ''),
	COALESCE(r.blueprint_run_id, ''), r.blueprint_step_index`

// insertConversation is the mint a delegation conversation is written by. It
// takes the queryer because it always runs on the transaction that also
// commits the blueprint_run or the current_step_index pointer the row belongs
// to — a step arrives with the write that implies it, never on a statement of
// its own. Mirrors the Postgres helper of the same name.
//
// The row carries NO status — the absence of an outcome is what makes it
// claimable, so the mint writes nothing to the column and queued_at carries
// the moment it entered the queue. runtime is stamped 'sdk': SQLite is local
// mode, which keeps the Claude Code SDK runtime. The Postgres sibling stamps
// 'native' — the dialect IS the mode, so the split lands where the row is
// written rather than as a caller-passed knob.
func insertConversation(ctx context.Context, q queryer, orgID string, conv domain.Conversation) (*domain.Conversation, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if err := db.AssertBlueprintStepIndexed(conv); err != nil {
		return nil, err
	}
	triggerType := conv.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	if triggerType == "manual" && conv.CreatorUserID == "" {
		conv.CreatorUserID = runmode.LocalDefaultUserID
	}
	var stepIdx any
	if conv.BlueprintStepIndex != nil {
		stepIdx = *conv.BlueprintStepIndex
	}
	row := q.QueryRowContext(ctx, `
		INSERT INTO conversations (id, type, runtime, task_id, prompt_id, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility,
		                  creator_user_id, actor_agent_id, blueprint_run_id, blueprint_step_index,
		                  preferred_executor_id, queued_at)
		VALUES (?, 'delegation', 'sdk', ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?, ?, NULLIF(?, ''), CURRENT_TIMESTAMP)
		RETURNING `+sqliteConversationReturningColumns, conv.ID, conv.TaskID, nullIfEmpty(conv.PromptID), conv.Model, conv.WorktreePath,
		triggerType, nullIfEmpty(conv.TriggerID), runmode.LocalDefaultTeamID,
		nullIfEmpty(conv.CreatorUserID), nullIfEmpty(conv.ActorAgentID),
		nullIfEmpty(conv.BlueprintRunID), stepIdx, conv.PreferredExecutorID)
	return scanConversationReturning(row)
}

func (s *conversationQueueStore) ClaimNextConversation(ctx context.Context, executorID string, bootEpoch int64, _ db.ClaimPlacement, lease time.Duration) (*domain.Conversation, error) {
	// One scan, every surface: pick the oldest conversation matching the
	// needs-driving predicate (plus the blueprint gate — a
	// sequence-cancelled blueprint's step is never claimed) and mint the
	// claims row that records this engagement's ownership, inside one short
	// transaction. Nothing on the conversation row changes: the claim IS the
	// ownership. No candidate means no row scanned, which the scan helper
	// reports as (nil, nil).
	//
	// The ClaimPlacement arg is ignored: SQLite is N=1 (local mode, always
	// role=all), so there is exactly one executor and the two-tier claim is
	// vacuous — every queued conversation's preferred_executor_id is either this one
	// instance (tier 1 self-hits) or NULL, and both resolve to "claim the
	// oldest". Placement is a multi-executor concern, Postgres-only.
	//
	// Per-org fairness + the max_concurrent_runs cap are likewise
	// Postgres-only and absent here: SQLite is one org and one executor, so the
	// fairness comparison is trivially won by the sole org and a single-process
	// semaphore already bounds local concurrency.
	//
	// An empty executorID (the un-wired test-spawner path) stores the ''
	// sentinel on the claim — claims.executor_id is NOT NULL by schema.
	var conv *domain.Conversation
	claimedAt := time.Now().UTC()
	err := inTx(ctx, s.conn, func(q queryer) error {
		row := q.QueryRowContext(ctx, `
			SELECT `+conversationQueueClaimCols+`
			FROM conversations r
			LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
			WHERE `+eligibleForDrivingSQL+`
			  AND `+blueprintDrivableSQL+`
			ORDER BY r.started_at, r.id
			LIMIT 1`)
		claimed, err := scanSqliteClaimedConversation(row)
		if err != nil || claimed == nil {
			return err
		}
		// The claim id is minted here and handed back on the claimed conversation: the
		// executor needs to name this engagement at teardown, when it has
		// already been released and can no longer be found as the
		// conversation's active claim.
		// Un-park: a claim taken on the `open` arm of the predicate ends the
		// park by definition, so the row goes back to mid-flight (NULL).
		// That keeps "parked" and "an engagement is driving this" disjoint at
		// every instant, which is what every recovery guard downstream reads.
		//
		// All THREE park columns clear together, and park_reason is the one
		// that bites if it doesn't: it answers "why is this parked", so on a
		// row that is no longer parked it is not history, it is a wrong
		// answer. Left behind, it rides through this engagement onto whatever
		// terminal follows — and the RunStation prints it beside a failed
		// conversation as "stopped by user" on a conversation nobody stopped.
		if _, err := q.ExecContext(ctx, `
			UPDATE conversations SET status = NULL, parked_at = NULL, park_reason = NULL,
			                         stop_requested_at = NULL, stop_requested_by = NULL, stop_requested_reason = NULL
			WHERE id = ? AND status IS NOT NULL
		`, claimed.ID); err != nil {
			return err
		}
		claimID := uuid.New().String()
		// The lease is stamped here, in the statement that mints the row: a
		// live claim never exists without one, which is what every fenced
		// write presents and what the conformance suite asserts on this
		// dialect (SQLite cannot hold it as a CHECK added by ALTER TABLE).
		// Rendered by strftime rather than formatted in Go so the `>`
		// comparisons in the fence and the renewal compare one text layout
		// against itself. claimed_at keeps its Go-side value.
		if _, err := q.ExecContext(ctx, `
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at, lease_expires_at)
			VALUES (?, ?, ?, ?, ?, ?, `+sqliteNowPlusExpr+`)
		`, claimID, claimed.OrgID, claimed.ID, executorID, bootEpoch, claimedAt, sqliteLeaseModifier(lease)); err != nil {
			return err
		}
		claimed.ClaimID = claimID
		var handBacks int
		if err := q.QueryRowContext(ctx, `
			SELECT `+episodeHandBacksSQL("r", handedBackOutcomesSQL)+`,
			       `+EpisodeSetupFailuresSQL("r")+`,
			       `+EpisodeLostEngagementsSQL("r")+`
			FROM conversations r WHERE r.id = ?
		`, claimed.ID).Scan(&handBacks, &claimed.SetupFailures, &claimed.LostEngagements); err != nil {
			return err
		}
		claimed.ExecutorID = executorID
		claimed.ClaimedAt = &claimedAt
		claimed.Attempts = handBacks + 1
		conv = claimed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return conv, nil
}

// RequeueConversation releases the claim FIRST and flips the conversation
// row SECOND, in that order, because the returned row's
// derived display status (sqliteReturningDisplayStatusSQL, folded into
// sqliteConversationReturningColumns) reads 'queued' only once no active
// claim remains, so the release has to be visible to the LAST statement's
// RETURNING for the answer to agree with a follow-up Get. The guard — a
// mid-flight conversation with a live claim — moves onto the claims release
// itself (matched only when the owning conversation's status IS NULL), so
// checking RowsAffected there tells the whole guard's outcome without a
// separate probe.
func (s *conversationQueueStore) RequeueConversation(ctx context.Context, orgID, conversationID string, outcome db.RequeueOutcome, lastErr string) (*domain.Conversation, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if !outcome.Valid() {
		return nil, fmt.Errorf("%w: %q", db.ErrInvalidRequeueOutcome, outcome)
	}
	var result *domain.Conversation
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET released_at = ?, outcome = ?
			WHERE conversation_id = ? AND released_at IS NULL
			  AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = claims.conversation_id AND c.status IS NULL)
		`, time.Now().UTC(), string(outcome), conversationID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		row := q.QueryRowContext(ctx, `
			UPDATE conversations SET result_summary = ?, preferred_executor_id = NULL
			WHERE id = ?
			RETURNING `+sqliteConversationReturningColumns, lastErr, conversationID)
		r, err := scanConversationReturning(row)
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

// MarkAwaitingCredentials mirrors the Postgres impl: the phase park and
// sidecar pubkey land on the ACTIVE claim in one statement (the
// conversation stays 'running'), guarded on phase IS NULL so a duplicate
// can't re-park or overwrite the key. No ctlbus doorbell — the tf_ctl
// fabric is Postgres-only (local mode has no LISTEN/NOTIFY), and never
// reached in practice anyway: local mode is always role=all, which never
// parks a claim awaiting credentials.
func (s *conversationQueueStore) MarkAwaitingCredentials(ctx context.Context, orgID, conversationID, credPubKey string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.conn.ExecContext(ctx, `
		UPDATE claims SET phase = 'awaiting_credentials', cred_pubkey = NULLIF(?, '')
		WHERE conversation_id = ? AND released_at IS NULL AND phase IS NULL
	`, credPubKey, conversationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// awaitingCredentialsCols is the shared projection for the claim-identity
// reads: the conversation joined to its active (unreleased) claim.
// claimed_at/started_at are selected bare and coalesced in Go: wrapping
// them in COALESCE strips the declared column type the driver needs to
// hand back a time.Time, so the scan would see a raw string.
const awaitingCredentialsCols = `r.id, r.org_id, COALESCE(r.type, ''), COALESCE(r.team_id, ''), COALESCE(r.task_id, ''),
	COALESCE(cl.executor_id, ''), COALESCE(cl.boot_epoch, 0), cl.claimed_at, r.started_at,
	COALESCE(cl.cred_pubkey, '')`

func (s *conversationQueueStore) GetClaim(ctx context.Context, orgID, conversationID string) (db.AwaitingCredentialsConversation, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return db.AwaitingCredentialsConversation{}, false, err
	}
	var (
		r         db.AwaitingCredentialsConversation
		claimedAt sql.NullTime
		startedAt time.Time
	)
	err := s.conn.QueryRowContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		LEFT JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		WHERE r.org_id = ? AND r.id = ?
	`, orgID, conversationID).Scan(&r.ConversationID, &r.OrgID, &r.ConversationType, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &claimedAt, &startedAt, &r.CredPubKey)
	if err == sql.ErrNoRows {
		return db.AwaitingCredentialsConversation{}, false, nil
	}
	if err != nil {
		return db.AwaitingCredentialsConversation{}, false, err
	}
	r.ClaimedAt = startedAt
	if claimedAt.Valid {
		r.ClaimedAt = claimedAt.Time
	}
	return r, true, nil
}

// ClaimExecutorSystem resolves one claim id to the executor that took it.
// Local mode is N=1, so the answer is always this process — the read is still
// real rather than a constant, because the caller's question is about a
// specific historical engagement and an id no claim carries has to answer
// "unknown" here exactly as it does in Postgres.
func (s *conversationQueueStore) ClaimExecutorSystem(ctx context.Context, orgID, claimID string) (string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", false, err
	}
	var executorID string
	err := s.conn.QueryRowContext(ctx, `
		SELECT executor_id FROM claims WHERE org_id = ? AND id = ?
	`, orgID, claimID).Scan(&executorID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return executorID, true, nil
}

func (s *conversationQueueStore) ListAwaitingCredentials(ctx context.Context) ([]db.AwaitingCredentialsConversation, error) {
	// The parked set is keyed off the active claim's phase (served by the
	// idx_claims_active_phase partial index); an inner join is right here —
	// a parked claim by definition exists. No type filter: parking is a
	// property of the engagement, not the surface.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		WHERE cl.phase = 'awaiting_credentials'
		ORDER BY cl.claimed_at ASC, cl.id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAwaitingCredentialsConversations(rows)
}

func (s *conversationQueueStore) ListActiveNeedingCredentialRefresh(ctx context.Context, olderThan time.Time) ([]db.AwaitingCredentialsConversation, error) {
	// Local mode has no claim_credentials table (the bundle channel is
	// Postgres-only; local reads the live secret store), so there is never
	// a sealed bundle to refresh.
	return nil, nil
}

func scanAwaitingCredentialsConversations(rows *sql.Rows) ([]db.AwaitingCredentialsConversation, error) {
	var out []db.AwaitingCredentialsConversation
	for rows.Next() {
		// Same bare-column + Go-side coalesce as GetClaim — see the comment
		// on awaitingCredentialsCols.
		var (
			r         db.AwaitingCredentialsConversation
			claimedAt sql.NullTime
			startedAt time.Time
		)
		if err := rows.Scan(&r.ConversationID, &r.OrgID, &r.ConversationType, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &claimedAt, &startedAt, &r.CredPubKey); err != nil {
			return nil, err
		}
		r.ClaimedAt = startedAt
		if claimedAt.Valid {
			r.ClaimedAt = claimedAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *conversationQueueStore) ResetProcessingConversations(ctx context.Context, executorID string, bootEpoch int64) (int, error) {
	// Every live claim this executor minted in an earlier boot, whatever its
	// conversation's state. The Postgres twin carries why the boot epoch, not
	// the lease, is the guard.
	var count int
	err := inTx(ctx, s.conn, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			SELECT id, conversation_id
			FROM claims
			WHERE released_at IS NULL
			  AND executor_id = ?
			  AND boot_epoch < ?
		`, executorID, bootEpoch)
		if err != nil {
			return err
		}
		var claimIDs, convIDs []string
		for rows.Next() {
			var claimID, convID string
			if err := rows.Scan(&claimID, &convID); err != nil {
				rows.Close()
				return err
			}
			claimIDs = append(claimIDs, claimID)
			convIDs = append(convIDs, convID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := time.Now().UTC()
		for i := range claimIDs {
			if _, err := q.ExecContext(ctx, `
				UPDATE claims SET released_at = ?, outcome = 'reaped' WHERE id = ?
			`, now, claimIDs[i]); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `
				UPDATE conversations SET preferred_executor_id = NULL WHERE id = ?
			`, convIDs[i]); err != nil {
				return err
			}
		}
		count = len(claimIDs)
		return nil
	})
	return count, err
}

func (s *conversationQueueStore) FleetQueueShares(ctx context.Context) ([]db.OrgQueueShare, error) {
	// SQLite is N=1 — at most the one local org has any rows — but the shape
	// mirrors the Postgres impl so the conformance suite runs identically:
	// active is the org's unreleased claims, queued its conversations
	// matching the needs-driving predicate. A NULL or non-positive
	// max_concurrent_runs maps to a nil cap (unlimited).
	rows, err := s.conn.QueryContext(ctx, `
		SELECT counts.org_id, counts.active, counts.queued, os.max_concurrent_runs
		FROM (
			SELECT org_id, SUM(active) AS active, SUM(queued) AS queued
			FROM (
				SELECT org_id, 1 AS active, 0 AS queued FROM claims WHERE released_at IS NULL
				UNION ALL
				SELECT r.org_id, 0, 1 FROM conversations r WHERE `+eligibleForDrivingSQL+`
			)
			GROUP BY org_id
		) counts
		LEFT JOIN org_settings os ON os.org_id = counts.org_id
		ORDER BY (counts.active + counts.queued) DESC, counts.org_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.OrgQueueShare
	for rows.Next() {
		var (
			share   db.OrgQueueShare
			maxRuns sql.NullInt64
		)
		if err := rows.Scan(&share.OrgID, &share.Active, &share.Queued, &maxRuns); err != nil {
			return nil, err
		}
		if maxRuns.Valid && maxRuns.Int64 > 0 {
			v := int(maxRuns.Int64)
			share.MaxConcurrentRuns = &v
		}
		out = append(out, share)
	}
	return out, rows.Err()
}

func (s *conversationQueueStore) ReconcileOrphanedConversations(ctx context.Context) (int, db.OrphanedStepCheck, error) {
	// Boot self-heal: park child conversations left mid-flight under a
	// blueprint_run that is already terminal. The boot reset beside it
	// releases claims and writes no status, and a child alive under a
	// terminal parent will never be claimed (ClaimNextConversation gates on a
	// running parent), so without this it sits mid-flight forever —
	// the dispatcher treats it as live work and its worktree pins the feature
	// branch, requeuing any sibling fetch into a forever-failing loop.
	//
	// `open`, not a terminal: nothing about an orphan failed, and nothing
	// about it concluded either. Read the park as "stopped without
	// concluding", NOT as "resumable" — its blueprint is terminal, so the
	// claim gate refuses it and no resume path will wake it. The scope is
	// status IS NULL alone (mid-flight is exactly what "still looks live"
	// means); an already-parked orphan is already in the state this writes,
	// and re-stamping it every boot would just churn the row and its
	// snapshot-retention clock.
	// Any active claim on a parked row releases as 'cancelled' with it — the
	// engagement was ended from outside, which is what the claim outcome
	// vocabulary calls a cancellation.
	var count int
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations
			SET status = 'open',
			    parked_at = COALESCE(parked_at, ?),
			    park_reason = COALESCE(park_reason, 'blueprint_terminal'),
			    result_summary = COALESCE(NULLIF(result_summary, ''), ?),
			    stop_requested_at = NULL, stop_requested_by = NULL, stop_requested_reason = NULL
			WHERE status IS NULL
			  AND blueprint_run_id IN (
			      SELECT id FROM blueprint_runs
			      WHERE status IN ('completed','aborted','failed','cancelled')
			  )
		`, time.Now().UTC(), "Stopped: owning blueprint run reached a terminal state")
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		count = int(n)
		if count == 0 {
			return nil
		}
		_, err = q.ExecContext(ctx, `
			UPDATE claims SET released_at = ?, outcome = 'cancelled'
			WHERE released_at IS NULL
			  AND conversation_id IN (
			      SELECT id FROM conversations
			      WHERE status = 'open'
			        AND blueprint_run_id IN (
			            SELECT id FROM blueprint_runs
			            WHERE status IN ('completed','aborted','failed','cancelled')
			        )
			  )
		`, time.Now().UTC())
		return err
	})
	if err != nil {
		return 0, db.OrphanedStepCheck{}, err
	}

	// Claim-desync CHECKER — the SQLite mirror of the Postgres twin's
	// countClaimDesyncs. It repairs nothing: every status write releases its
	// claim on the same transaction, so a survivor here is a bug to look at.
	desyncs, err := countClaimDesyncs(ctx, s.conn)
	if err != nil {
		return count, db.OrphanedStepCheck{}, err
	}

	// Orphaned-step CHECKER — the shape the park above cannot see, one level
	// up: that arm heals a live child under a dead parent, this one only
	// REPORTS a live parent with no child at the step it is pointing at. It
	// repairs nothing, and its count stays out of the healed total, because
	// counting is not healing.
	//
	// A firing commits its blueprint_run and its first step in one
	// transaction, and an advance commits its pointer and the step it names in
	// another, so no reader can observe one without the other and there is no
	// window for the shape to appear in — which is why there is no grace here.
	// What an installed database can still hold is a survivor from before that
	// was true; the forward migration that fails those is what makes one
	// turning up here worth shouting about rather than sweeping.
	check, err := countBlueprintRunsMissingCurrentStep(ctx, s.conn)
	if err != nil {
		return count, db.OrphanedStepCheck{}, err
	}
	check.ClaimDesyncs, check.ClaimDesyncSample = desyncs.ClaimDesyncs, desyncs.ClaimDesyncSample
	return count, check, nil
}

// countClaimDesyncs counts terminal conversations still holding an unreleased
// claim and samples the oldest few. It writes nothing. Same window-function
// shape as countBlueprintRunsMissingCurrentStep.
func countClaimDesyncs(ctx context.Context, q queryer) (db.OrphanedStepCheck, error) {
	var out db.OrphanedStepCheck
	rows, err := q.QueryContext(ctx, `
		SELECT c.id, count(*) OVER ()
		FROM conversations c
		WHERE c.status IN (`+conversationTerminalStatusesSQL+`)
		  AND EXISTS (SELECT 1 FROM claims cl WHERE cl.conversation_id = c.id AND cl.released_at IS NULL)
		ORDER BY c.started_at, c.id
		LIMIT ?
	`, db.OrphanedStepSampleLimit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &out.ClaimDesyncs); err != nil {
			return db.OrphanedStepCheck{}, err
		}
		out.ClaimDesyncSample = append(out.ClaimDesyncSample, id)
	}
	return out, rows.Err()
}

// countBlueprintRunsMissingCurrentStep counts 'running' blueprint_runs holding
// no conversation at the step current_step_index names, and samples the oldest
// few. It writes nothing. Mirrors the Postgres twin, window function included:
// `count(*) OVER ()` is evaluated before LIMIT, so one statement gives both the
// full count and the bounded sample without the two disagreeing about which
// rows they describe.
func countBlueprintRunsMissingCurrentStep(ctx context.Context, q queryer) (db.OrphanedStepCheck, error) {
	var out db.OrphanedStepCheck
	rows, err := q.QueryContext(ctx, `
		SELECT id, count(*) OVER ()
		FROM blueprint_runs
		WHERE status = 'running'
		  AND NOT EXISTS (
		      SELECT 1 FROM conversations c
		      WHERE c.blueprint_run_id = blueprint_runs.id
		        AND c.blueprint_step_index = blueprint_runs.current_step_index
		  )
		ORDER BY started_at, id
		LIMIT ?
	`, db.OrphanedStepSampleLimit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &out.Count); err != nil {
			return db.OrphanedStepCheck{}, err
		}
		out.Sample = append(out.Sample, id)
	}
	return out, rows.Err()
}

func (s *conversationQueueStore) CountQueuedSystem(ctx context.Context) (int, error) {
	var n int
	err := s.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations r WHERE `+eligibleForDrivingSQL).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// conversationTimingClaimCols selects the claim-derived timing fields: duration_ms
// is the SUM of the per-engagement telemetry, then the latest claim's
// identity. claimed_at loses its declared column type inside the subselect,
// so it scans as text and parses via parseDBDatetime.
const conversationTimingClaimCols = `
	(SELECT SUM(cl.duration_ms) FROM claims cl WHERE cl.conversation_id = conversations.id),
	(SELECT cl.executor_id FROM claims cl WHERE cl.conversation_id = conversations.id ORDER BY cl.claimed_at DESC, cl.rowid DESC LIMIT 1),
	(SELECT MAX(cl.claimed_at) FROM claims cl WHERE cl.conversation_id = conversations.id)`

// conversationTimingStatusSQL is the timing projection's status: the stored outcome
// when there is one, else the derived in-flight state. The percentile read
// buckets by failure kind, so a mid-flight row must still name itself
// rather than scan as NULL. A claim whose lease has lapsed is not running,
// for the same reason the display ladder says so (liveClaimExistsSQL).
const conversationTimingStatusSQL = `COALESCE(status, CASE WHEN EXISTS (
		SELECT 1 FROM claims cl WHERE cl.conversation_id = conversations.id AND cl.released_at IS NULL
		  AND cl.lease_expires_at > ` + sqliteNowExpr + `)
	THEN 'running' ELSE 'queued' END)`

func (s *conversationQueueStore) RecentConversationTimingsSystem(ctx context.Context, since time.Time, limit int) ([]domain.ConversationTiming, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT org_id, `+conversationTimingStatusSQL+`, COALESCE(failure_kind, ''),
		       started_at, completed_at, `+conversationTimingClaimCols+`
		FROM conversations
		WHERE type = 'delegation' AND started_at >= ?
		ORDER BY started_at DESC
		LIMIT ?
	`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSqliteConversationTimings(rows)
}

func (s *conversationQueueStore) QueuedConversationAgesSystem(ctx context.Context) ([]domain.QueuedConversation, error) {
	return s.queuedConversationAges(ctx, "")
}

func (s *conversationQueueStore) QueuedConversationAgesForOrgSystem(ctx context.Context, orgID string) ([]domain.QueuedConversation, error) {
	return s.queuedConversationAges(ctx, orgID)
}

func (s *conversationQueueStore) queuedConversationAges(ctx context.Context, orgID string) ([]domain.QueuedConversation, error) {
	q := `SELECT r.org_id, r.started_at, COALESCE(r.preferred_executor_id, '')
	      FROM conversations r WHERE ` + eligibleForDrivingSQL
	args := []any{}
	if orgID != "" {
		q += ` AND r.org_id = ?`
		args = append(args, orgID)
	}
	q += ` ORDER BY r.started_at ASC`
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.QueuedConversation
	for rows.Next() {
		var qr domain.QueuedConversation
		if err := rows.Scan(&qr.OrgID, &qr.EnqueuedAt, &qr.PreferredExecutor); err != nil {
			return nil, err
		}
		out = append(out, qr)
	}
	return out, rows.Err()
}

func (s *conversationQueueStore) RecentConversationTimingsForOrgSystem(ctx context.Context, orgID string, since, until time.Time, limit int) ([]domain.ConversationTiming, error) {
	if limit <= 0 {
		limit = 5000
	}
	// Raw string comparison on started_at, which is sound only because every
	// writer is UTC: the driver renders a bound time.Time with its offset
	// attached, so a local-zone value would compare as its own wall clock
	// against a CURRENT_TIMESTAMP row's UTC. Bind .UTC() here and write .UTC()
	// everywhere (the store-package ratchet enforces it) and the two shapes
	// differ only in trailing width, which sorts as a tie the id breaks.
	q := `SELECT org_id, ` + conversationTimingStatusSQL + `, COALESCE(failure_kind, ''),
	             started_at, completed_at, ` + conversationTimingClaimCols + `
	      FROM conversations WHERE type = 'delegation' AND org_id = ? AND started_at >= ?`
	args := []any{orgID, since.UTC()}
	if !until.IsZero() {
		q += ` AND started_at < ?`
		args = append(args, until.UTC())
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSqliteConversationTimings(rows)
}

// executorClaimCols is the shared projection behind both operator claim reads,
// so the per-executor list and the single-claim lookup can never drift into
// disagreeing about the same row. LEFT JOIN on the conversation: the claim is
// the subject here, and a claim whose conversation is gone must still report
// its measured cost rather than vanishing from the box's occupancy.
//
// No org predicate, and none is possible: this is the deployment-wide operator
// read. SQLite is N=1, so the distinction is moot locally — the arm exists
// because the store is one dual-dialect contract with one conformance suite.
var executorClaimCols = `
	SELECT c.id, c.org_id, c.conversation_id,
	       c.claimed_at, c.released_at, c.lease_expires_at, COALESCE(c.outcome, ''),
	       c.peak_mem_mb, c.cpu_usec,
	       COALESCE(v.status, CASE WHEN ` + claimLeaseLiveSQL("c") + ` THEN 'running' ELSE 'queued' END, ''),
	       COALESCE(v.failure_kind, ''),
	       c.last_activity_at, COALESCE(c.current_op, '')
	FROM claims c
	LEFT JOIN conversations v ON v.id = c.conversation_id`

// claimLeaseLiveSQL is one claims row's own liveness, for the alias the
// caller gave it: unreleased AND its lease still in the future. An expired
// claim renders 'queued' because nothing is driving its conversation — the
// executor that held it is gone, and what the row records is an engagement
// waiting to be taken over.
func claimLeaseLiveSQL(alias string) string {
	return alias + ".released_at IS NULL AND " + alias + ".lease_expires_at > " + sqliteNowExpr
}

func (s *conversationQueueStore) RecentClaimsForExecutorSystem(ctx context.Context, executorID string, limit int) ([]domain.ExecutorClaim, error) {
	if limit <= 0 {
		limit = 25
	}
	// Tie-break on id so a batch of claims minted in the same instant orders
	// deterministically across repeated polls — the console refetches on a
	// timer and a shuffling table reads as churn that isn't happening.
	rows, err := s.conn.QueryContext(ctx, executorClaimCols+`
		WHERE c.executor_id = ?
		ORDER BY c.claimed_at DESC, c.id DESC
		LIMIT ?
	`, executorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSqliteExecutorClaims(rows)
}

func (s *conversationQueueStore) ClaimByIDSystem(ctx context.Context, claimID string) (*domain.ExecutorClaim, error) {
	rows, err := s.conn.QueryContext(ctx, executorClaimCols+` WHERE c.id = ?`, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanSqliteExecutorClaims(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func (s *conversationQueueStore) RenewClaimLeaseSystem(ctx context.Context, orgID, conversationID, claimID string, lease, idle time.Duration, op string) (db.ClaimRenewal, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return db.ClaimRenewal{}, err
	}
	if claimID == "" {
		return db.ClaimRenewal{}, fmt.Errorf("%w: no claim id supplied", db.ErrClaimReleased)
	}
	// One statement, guard and write together: splitting them would open a
	// window in which an expired lease renews. The guard's expiry term is what
	// makes a late renewal terminal — an already-lapsed lease matches nothing
	// and the caller gets the same ErrClaimReleased a released claim gives.
	// Authority does not come back.
	//
	// Both sides of the comparison are strftime-rendered text in the layout
	// the column stores, so the `>` is one layout against itself.
	//
	// last_activity_at is database now offset back by the idle the executor
	// read on its monotonic clock, in the same layout and through the same
	// modifier rendering as the lease stamp.
	var out db.ClaimRenewal
	err := s.conn.QueryRowContext(ctx, `
		UPDATE claims
		SET lease_expires_at = `+sqliteNowPlusExpr+`,
		    last_activity_at = `+sqliteNowPlusExpr+`,
		    current_op = NULLIF(?, '')
		WHERE id = ? AND org_id = ? AND conversation_id = ?
		  AND released_at IS NULL AND lease_expires_at > `+sqliteNowExpr+`
		RETURNING lease_expires_at,
		          (SELECT r.stop_requested_at IS NOT NULL FROM conversations r WHERE r.id = claims.conversation_id),
		          (SELECT COALESCE(r.stop_requested_by, '') FROM conversations r WHERE r.id = claims.conversation_id)
	`, sqliteLeaseModifier(lease), sqliteLeaseModifier(-idle), op, claimID, orgID, conversationID).Scan(&out.ExpiresAt, &out.StopRequested, &out.StopRequestedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ClaimRenewal{}, fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimReleased, claimID, conversationID)
	}
	if err != nil {
		return db.ClaimRenewal{}, err
	}
	return out, nil
}

// SettleUnclaimedStopsSystem is the Postgres twin's settlement as a read then
// per-row writes on one transaction. The IMMEDIATE transaction serializes it
// against every other writer on the file, so neither the row locks nor the
// run-first lock order the twin needs have anything to do here.
func (s *conversationQueueStore) SettleUnclaimedStopsSystem(ctx context.Context) ([]db.SettledStop, error) {
	return s.settleUnclaimedStops(ctx, "")
}

func (s *conversationQueueStore) SettleUnclaimedStopsForTaskSystem(ctx context.Context, orgID, taskID string) ([]db.SettledStop, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return s.settleUnclaimedStops(ctx, `AND r.task_id = ?`, taskID)
}

// settleUnclaimedStops runs the settlement over the conversations scope
// narrows to; scope is an AND-clause over the victims' alias r, binding args.
func (s *conversationQueueStore) settleUnclaimedStops(ctx context.Context, scope string, args ...any) ([]db.SettledStop, error) {
	type victim struct {
		id, orgID, status, by, reason string
		intent                        bool
		runID                         sql.NullString
		step                          sql.NullInt64
	}
	var out []db.SettledStop
	err := inTx(ctx, s.conn, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			SELECT r.id, r.org_id, COALESCE(r.status, ''), COALESCE(r.stop_requested_by, ''),
			       r.stop_requested_at IS NOT NULL, COALESCE(r.stop_requested_reason, ''),
			       r.blueprint_run_id, r.blueprint_step_index
			FROM conversations r
			LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
			WHERE NOT EXISTS (SELECT 1 FROM claims cl WHERE cl.conversation_id = r.id AND cl.released_at IS NULL)
			  AND (r.stop_requested_at IS NOT NULL
			       OR ((r.status IS NULL OR r.status = 'open')
			           AND br.status = 'running' AND br.cancel_requested = 1))
			  `+scope+`
			ORDER BY r.id
		`, args...)
		if err != nil {
			return err
		}
		var victims []victim
		for rows.Next() {
			var v victim
			if err := rows.Scan(&v.id, &v.orgID, &v.status, &v.by, &v.intent, &v.reason, &v.runID, &v.step); err != nil {
				rows.Close()
				return err
			}
			victims = append(victims, v)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, v := range victims {
			terminal := v.status == "completed" || v.status == "failed"
			// An intent names who stopped the row, and why when it says; a
			// row with none is here because its run's cancel never reached
			// it, and the reactor's cancel is what it records.
			parkReason, abortReason := "user_cancelled", "user_cancelled"
			switch {
			case !v.intent:
				parkReason, abortReason = "blueprint_cancelled", "cancelled"
			case v.reason != "":
				parkReason, abortReason = v.reason, v.reason
			case v.by == "":
				parkReason, abortReason = "system_cancelled", "system_cancelled"
			}
			if terminal {
				if _, err := q.ExecContext(ctx, `
					UPDATE conversations SET stop_requested_at = NULL, stop_requested_by = NULL, stop_requested_reason = NULL WHERE id = ?
				`, v.id); err != nil {
					return err
				}
			} else if _, err := q.ExecContext(ctx, `
				UPDATE conversations
				SET status = 'open',
				    parked_at = COALESCE(parked_at, ?),
				    park_reason = ?,
				    stop_requested_at = NULL,
				    stop_requested_by = NULL,
				    stop_requested_reason = NULL
				WHERE id = ?
			`, now, parkReason, v.id); err != nil {
				return err
			}
			st := db.SettledStop{OrgID: v.orgID, ConversationID: v.id}
			if v.step.Valid {
				idx := int(v.step.Int64)
				st.StepIndex = &idx
			}
			if !terminal && v.runID.Valid {
				res, err := q.ExecContext(ctx, `
					UPDATE blueprint_runs
					SET status = 'cancelled', completed_at = ?, abort_reason = ?, aborted_at_step = ?
					WHERE id = ? AND status = 'running' AND cancel_requested = 1
				`, now, abortReason, sqliteNullInt(st.StepIndex), v.runID.String)
				if err != nil {
					return err
				}
				if n, err := res.RowsAffected(); err != nil {
					return err
				} else if n > 0 {
					st.BlueprintRunID = v.runID.String
				}
			}
			out = append(out, st)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *conversationQueueStore) ExpiredClaimsSystem(ctx context.Context) (int, time.Duration, error) {
	// Whole seconds: the %f fraction is dropped because the answer feeds a
	// gauge, where a sub-second reading says nothing a scrape interval could
	// act on. COALESCE over the aggregate rather than a second statement —
	// with no matching rows min() is NULL and so is the subtraction, which
	// collapses to the zero the empty case wants.
	var count, oldestSeconds int64
	err := s.conn.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(strftime('%s','now') - strftime('%s', min(lease_expires_at)), 0)
		FROM claims
		WHERE released_at IS NULL AND lease_expires_at <= `+sqliteNowExpr).Scan(&count, &oldestSeconds)
	if err != nil {
		return 0, 0, err
	}
	if oldestSeconds < 0 {
		oldestSeconds = 0
	}
	return int(count), time.Duration(oldestSeconds) * time.Second, nil
}

func (s *conversationQueueStore) OldestIdleClaimSystem(ctx context.Context) (time.Duration, error) {
	// Seconds at millisecond resolution: julianday keeps the %f fraction the
	// stamp carries. min() over no rows is NULL, and the COALESCE collapses it
	// to the zero the empty case wants. A claim that has not renewed yet
	// carries no stamp and is not counted.
	var seconds float64
	err := s.conn.QueryRowContext(ctx, `
		SELECT COALESCE((julianday('now') - julianday(min(last_activity_at))) * 86400.0, 0)
		FROM claims
		WHERE released_at IS NULL AND lease_expires_at > `+sqliteNowExpr+`
		  AND last_activity_at IS NOT NULL`).Scan(&seconds)
	if err != nil {
		return 0, err
	}
	if seconds < 0 {
		seconds = 0
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (s *conversationQueueStore) ExpiredClaimsOfExecutorSystem(ctx context.Context, executorID string, bootEpoch int64) ([]db.ClaimRef, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id, org_id, conversation_id
		FROM claims
		WHERE executor_id = ? AND boot_epoch = ?
		  AND released_at IS NULL AND lease_expires_at <= `+sqliteNowExpr+`
		ORDER BY lease_expires_at, id
	`, executorID, bootEpoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.ClaimRef
	for rows.Next() {
		var c db.ClaimRef
		if err := rows.Scan(&c.ClaimID, &c.OrgID, &c.ConversationID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *conversationQueueStore) ReleaseExpiredClaimSystem(ctx context.Context, orgID, conversationID, claimID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.conn.ExecContext(ctx, `
		UPDATE claims SET released_at = ?, outcome = 'reaped'
		WHERE id = ? AND org_id = ? AND conversation_id = ?
		  AND released_at IS NULL AND lease_expires_at <= `+sqliteNowExpr+`
	`, time.Now().UTC(), claimID, orgID, conversationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// TakeOverExpiredClaimsSystem is the Postgres twin's takeover as a read then
// per-row guarded releases on one transaction. The guard repeats the read's
// predicate, so a claim renewed or released between the two is left alone and
// not reported.
func (s *conversationQueueStore) TakeOverExpiredClaimsSystem(ctx context.Context, executorID string, bootEpoch int64, limit int) ([]db.ClaimRef, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []db.ClaimRef
	err := inTx(ctx, s.conn, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			SELECT id, org_id, conversation_id
			FROM claims
			WHERE released_at IS NULL
			  AND lease_expires_at <= `+sqliteNowExpr+`
			  AND NOT (executor_id = ? AND boot_epoch = ?)
			ORDER BY lease_expires_at, id
			LIMIT ?
		`, executorID, bootEpoch, limit)
		if err != nil {
			return err
		}
		var candidates []db.ClaimRef
		for rows.Next() {
			var c db.ClaimRef
			if err := rows.Scan(&c.ClaimID, &c.OrgID, &c.ConversationID); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, c := range candidates {
			res, err := q.ExecContext(ctx, `
				UPDATE claims SET released_at = ?, outcome = 'reaped'
				WHERE id = ? AND released_at IS NULL AND lease_expires_at <= `+sqliteNowExpr+`
			`, now, c.ClaimID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				continue
			}
			if _, err := q.ExecContext(ctx, `
				UPDATE conversations SET preferred_executor_id = NULL WHERE id = ?
			`, c.ConversationID); err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *conversationQueueStore) LiveClaimsOfExecutorSystem(ctx context.Context, executorID string, bootEpoch int64) ([]db.ClaimRef, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id, org_id, conversation_id
		FROM claims
		WHERE executor_id = ? AND boot_epoch = ? AND released_at IS NULL
		ORDER BY claimed_at, id
	`, executorID, bootEpoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.ClaimRef
	for rows.Next() {
		var c db.ClaimRef
		if err := rows.Scan(&c.ClaimID, &c.OrgID, &c.ConversationID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *conversationQueueStore) ReleaseOwnClaimsOnShutdownSystem(ctx context.Context, executorID string, bootEpoch int64, conversationIDs []string) (int, error) {
	if len(conversationIDs) == 0 {
		return 0, nil
	}
	var count int
	err := inTx(ctx, s.conn, func(q queryer) error {
		now := time.Now().UTC()
		for _, id := range conversationIDs {
			res, err := q.ExecContext(ctx, `
				UPDATE claims SET released_at = ?, outcome = 'requeued_shutdown'
				WHERE executor_id = ? AND boot_epoch = ? AND released_at IS NULL AND conversation_id = ?
			`, now, executorID, bootEpoch, id)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			if _, err := q.ExecContext(ctx, `
				UPDATE conversations SET preferred_executor_id = NULL WHERE id = ?
			`, id); err != nil {
				return err
			}
			count += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *conversationQueueStore) ReleaseClaimOnShutdownSystem(ctx context.Context, orgID, conversationID, claimID string) error {
	return inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET released_at = ?, outcome = 'requeued_shutdown'
			WHERE id = ? AND org_id = ? AND conversation_id = ? AND released_at IS NULL
		`, time.Now().UTC(), claimID, orgID, conversationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimReleased, claimID, conversationID)
		}
		_, err = q.ExecContext(ctx, `
			UPDATE conversations SET preferred_executor_id = NULL WHERE id = ?
		`, conversationID)
		return err
	})
}

// StrandedBlueprintRunsSystem measures the grace on this process's clock,
// bound as a time the way completed_at and released_at are written, so each
// comparison is one layout against itself.
func (s *conversationQueueStore) StrandedBlueprintRunsSystem(ctx context.Context, grace time.Duration, limit int) ([]db.StrandedRun, error) {
	if limit <= 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-grace)
	rows, err := s.conn.QueryContext(ctx, `
		SELECT r.org_id, br.id, r.id
		FROM blueprint_runs br
		JOIN conversations r ON r.blueprint_run_id = br.id AND r.blueprint_step_index = br.current_step_index
		WHERE br.status = 'running'
		  AND r.status IN (`+conversationTerminalStatusesSQL+`)
		  AND COALESCE(r.completed_at, r.started_at) <= ?
		  AND NOT EXISTS (
		      SELECT 1 FROM claims cl
		      WHERE cl.conversation_id = r.id
		        AND (cl.released_at IS NULL OR cl.released_at > ?)
		  )
		ORDER BY br.started_at, br.id
		LIMIT ?
	`, cutoff, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.StrandedRun
	for rows.Next() {
		var r db.StrandedRun
		if err := rows.Scan(&r.OrgID, &r.BlueprintRunID, &r.ConversationID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanSqliteExecutorClaims(rows *sql.Rows) ([]domain.ExecutorClaim, error) {
	var out []domain.ExecutorClaim
	for rows.Next() {
		c, err := scanOneExecutorClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// executorClaimScanner is satisfied by both *sql.Row and *sql.Rows, so
// scanOneExecutorClaim serves the multi-row read above and the single-row
// RETURNING reads on ConversationStore's claims-row writes (executorClaimCols
// order — see the RETURNING-safe mirror in conversation.go) without
// duplicating the column layout.
type executorClaimScanner interface {
	Scan(dest ...any) error
}

func scanOneExecutorClaim(row executorClaimScanner) (domain.ExecutorClaim, error) {
	var c domain.ExecutorClaim
	var releasedAt, leaseExpiresAt, lastActivityAt sql.NullTime
	var peakMem, cpuUsec sql.NullInt64
	if err := row.Scan(
		&c.ID, &c.OrgID, &c.ConversationID,
		&c.ClaimedAt, &releasedAt, &leaseExpiresAt, &c.Outcome,
		&peakMem, &cpuUsec, &c.Status, &c.FailureKind,
		&lastActivityAt, &c.CurrentOp,
	); err != nil {
		return domain.ExecutorClaim{}, err
	}
	if lastActivityAt.Valid {
		v := lastActivityAt.Time
		c.LastActivityAt = &v
	}
	if releasedAt.Valid {
		v := releasedAt.Time
		c.ReleasedAt = &v
	}
	if leaseExpiresAt.Valid {
		v := leaseExpiresAt.Time
		c.LeaseExpiresAt = &v
	}
	c.PeakMemMB = intPtrFromNull(peakMem)
	c.CPUUsec = int64PtrFromNull(cpuUsec)
	return c, nil
}

// scanSqliteExecutorClaimRow scans a single RETURNING row into a
// domain.ExecutorClaim, or (nil, nil) on sql.ErrNoRows — the guard-declined
// shape every claims-row write in conversation.go uses for "nothing matched".
func scanSqliteExecutorClaimRow(row *sql.Row) (*domain.ExecutorClaim, error) {
	c, err := scanOneExecutorClaim(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

func scanSqliteConversationTimings(rows *sql.Rows) ([]domain.ConversationTiming, error) {
	var out []domain.ConversationTiming
	for rows.Next() {
		var t domain.ConversationTiming
		var executorID, claimedAt sql.NullString
		var completedAt sql.NullTime
		var durationMS sql.NullInt64
		if err := rows.Scan(
			&t.OrgID, &t.Status, &t.FailureKind,
			&t.StartedAt, &completedAt, &durationMS, &executorID, &claimedAt,
		); err != nil {
			return nil, err
		}
		t.ExecutorID = executorID.String
		if claimedAt.Valid && claimedAt.String != "" {
			at, err := parseDBDatetime(claimedAt.String)
			if err != nil {
				return nil, err
			}
			t.ClaimedAt = &at
		}
		if completedAt.Valid {
			v := completedAt.Time
			t.CompletedAt = &v
		}
		if durationMS.Valid {
			v := int(durationMS.Int64)
			t.DurationMS = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanSqliteClaimedConversation scans a claim candidate row into
// *domain.Conversation. (nil, nil) on sql.ErrNoRows so callers treat "nothing
// claimable" as a non-error empty result. Status is deliberately left empty:
// a claimable conversation's stored status is NULL by construction, and the
// dispatcher branches on Type and Runtime, never on it.
func scanSqliteClaimedConversation(row *sql.Row) (*domain.Conversation, error) {
	var (
		r       domain.Conversation
		stepIdx sql.NullInt64
	)
	err := row.Scan(&r.ID, &r.OrgID, &r.Type, &r.TaskID, &r.PromptID, &r.Model, &r.Runtime,
		&r.WorktreePath, &r.SessionID, &r.TriggerType, &r.TriggerID,
		&r.CreatorUserID, &r.TeamID, &r.BlueprintRunID, &stepIdx)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stepIdx.Valid {
		v := int(stepIdx.Int64)
		r.BlueprintStepIndex = &v
	}
	return &r, nil
}
