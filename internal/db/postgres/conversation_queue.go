package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/wakebus"
)

// conversationQueueStore is the Postgres impl of db.ConversationQueueStore —
// the durable conversation queue the delegation dispatcher drains. Wired
// against the admin pool in postgres.New: the dispatcher is a system worker
// with no per-user identity. The RLS policies are defense-in-depth (admin
// bypasses them) and org_id is bound where it is known. Ownership state lives
// on claims: a claim row is minted at ClaimNextConversation and released
// (requeued/reaped) by the recovery paths, all inside the claiming statement's
// data-modifying CTEs so a crash can't separate the status flip from the claim
// bookkeeping.
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

// EnqueueConversation mints a delegation conversation with NO status — the absence of
// an outcome is what makes it claimable, so the mint writes nothing to the
// column and queued_at carries the enqueue moment. runtime is stamped
// 'native' here and 'sdk' in the SQLite sibling: the dialect IS the mode
// (Postgres is multi-only, SQLite is local-only), so the split lands where
// the row is written rather than as a caller-passed knob.
//
// Both arms stamp it explicitly, which is what makes the SDK engine
// unreachable for a multi delegation: the ratchet means no conversation ever
// changes engines, so an engine no mint writes is an engine nothing runs.
// That is the whole enforcement — there is no claim-side exclusion, because
// there is no row for one to exclude. Leaving either arm to the column
// DEFAULT (still 'sdk') would quietly undo it.
// Returns the minted row via writeConversationReturning, sharing
// ConversationStore.Get's projection — a fresh mint has no claims/messages/
// memory rows yet, so the claim/ledger laterals and the memory/agent LEFT
// JOINs read exactly as a Get immediately afterward would (NULL/zero
// defaults throughout).
func (s *conversationQueueStore) EnqueueConversation(ctx context.Context, orgID string, conv domain.Conversation) (*domain.Conversation, error) {
	if err := db.AssertBlueprintStepIndexed(conv); err != nil {
		return nil, err
	}
	triggerType := conv.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	var stepIdx any
	if conv.BlueprintStepIndex != nil {
		stepIdx = *conv.BlueprintStepIndex
	}
	// team_id is derived from the task in-SQL (multi-team). The dispatcher mints
	// work items on the admin pool, which bypasses RLS, so both manual and
	// event rows insert here; the schema CHECK pairing trigger_type with
	// creator_user_id nullability is satisfied by the branch below.
	if triggerType == "event" {
		r, err := writeConversationReturning(ctx, s.conn, `
			INSERT INTO conversations (id, org_id, type, runtime, task_id, prompt_id, model, worktree_path,
			                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
			                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
			                  queued_at)
			VALUES ($1, $2, 'delegation', 'native', $3, $4, $5, $6, 'event', $7,
			        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
			        'team', NULL, $8, $9, $10, NULLIF($11, ''), now())
			RETURNING *
		`, conv.ID, orgID, conv.TaskID, nullIfEmpty(conv.PromptID), conv.Model, conv.WorktreePath,
			nullIfEmpty(conv.TriggerID), nullIfEmpty(conv.ActorAgentID),
			nullIfEmpty(conv.BlueprintRunID), stepIdx, conv.PreferredExecutorID)
		if err != nil {
			return nil, err
		}
		s.notifyWake(ctx, orgID)
		return r, nil
	}
	// Manual: the local sentinel user has no FK target in multi-mode; filter it
	// so the COALESCE walks to the org owner. There is no tf.current_user_id()
	// on the admin pool, so the creator must arrive on the row or fall back to
	// the org owner (the schema CHECK requires a non-NULL creator for manual).
	creatorBind := conv.CreatorUserID
	if creatorBind == runmode.LocalDefaultUserID {
		creatorBind = ""
	}
	r, err := writeConversationReturning(ctx, s.conn, `
		INSERT INTO conversations (id, org_id, type, runtime, task_id, prompt_id, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
		                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
		                  queued_at)
		VALUES ($1, $2, 'delegation', 'native', $3, $4, $5, $6, 'manual', $7,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team',
		        COALESCE(NULLIF($8, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $2)),
		        $9, $10, $11, NULLIF($12, ''), now())
		RETURNING *
	`, conv.ID, orgID, conv.TaskID, nullIfEmpty(conv.PromptID), conv.Model, conv.WorktreePath,
		nullIfEmpty(conv.TriggerID), creatorBind, nullIfEmpty(conv.ActorAgentID),
		nullIfEmpty(conv.BlueprintRunID), stepIdx, conv.PreferredExecutorID)
	if err != nil {
		return nil, err
	}
	s.notifyWake(ctx, orgID)
	return r, nil
}

// notifyWake fires the tf_wake doorbell after a row lands
// claimable, so an idle executor claims within milliseconds instead of
// waiting out its scan interval. Best-effort by contract (wakebus's "never
// the only path" rule) — a notify failure is swallowed, never surfaced to
// the caller, since the write it announces already committed successfully
// and the scan-interval backstop covers a dropped doorbell.
func (s *conversationQueueStore) notifyWake(ctx context.Context, orgID string) {
	_ = wakebus.Publish(ctx, s.conn, wakebus.KindRun, orgID)
}

// --- The needs-driving predicate ---------------------------------------
//
// Stored conversation status is outcome-or-nothing: 'open' (a deliberate
// park), a terminal, or NULL. NULL is not "unknown" — it is the mid-flight
// state: either an active claim is driving the conversation right now, or
// its last claim released without an outcome and nobody has picked it up
// yet. "Queued" and "running" are therefore never stored; they are read off
// the claim table and these predicates.
//
// Every fragment below is written against the conversation alias `r`.

// activeClaimExistsSQL is the derived "running": an unreleased claim is the
// engagement driving this conversation. Served by idx_claims_one_active
// (the partial unique index that is also the sole mutual-exclusion
// primitive for the claim).
const activeClaimExistsSQL = `EXISTS (
		SELECT 1 FROM claims cl_a
		WHERE cl_a.conversation_id = r.id AND cl_a.released_at IS NULL)`

// undeliveredInputExistsSQL matches drivable input: a plain user message
// still awaiting delivery. Injections (subtype 'injection:…', including a
// compaction request — which is inserted delivered anyway) ride whatever
// engagement runs next and never wake one on their own; a withdrawn row
// (undelivered + window_state 'inactive') never happened. No assumption is
// made about `seq` — a compaction commit re-seqs undelivered rows to
// fractional values and nothing here reads the column. Served by
// idx_messages_undelivered.
const undeliveredInputExistsSQL = `EXISTS (
		SELECT 1 FROM messages m_i
		WHERE m_i.conversation_id = r.id AND m_i.delivered = false
		  AND m_i.role = 'user' AND m_i.subtype = '' AND m_i.window_state = 'active')`

// needsDrivingSQL is the eligibility predicate, identical for every surface:
// nobody is driving it, it has not been retired, and it is either mid-flight
// (fresh mint, or a claim that released without writing an outcome) or
// parked and woken by new input. A terminal conversation is never eligible,
// whatever rows it holds — resuming one takes the explicit un-terminal write
// (MarkQueuedForResume).
//
// The NULL arm is what makes both reap cases work with no write beyond the
// claim release: a claim reaped mid-setup still has its undelivered prompt
// row (either arm matches), and a claim reaped mid-engagement has all its input
// delivered and matches here.
const needsDrivingSQL = `r.archived_at IS NULL
	  AND NOT ` + activeClaimExistsSQL + `
	  AND (r.status IS NULL OR (r.status = 'open' AND ` + undeliveredInputExistsSQL + `))`

// eligibleForDrivingSQL is the surface-agnostic "waiting to be driven",
// without the placement/blueprint gates that decide WHICH executor may take
// it. This is the queue-depth answer the fleet counters and the display
// projection's derived `queued` rung read.
const eligibleForDrivingSQL = needsDrivingSQL

// blueprintDrivableSQL is the delegation arm's gate, applied as a LEFT JOIN
// rather than an inner one: a conversation with no blueprint parent —
// interactive, tomorrow — must not be filtered out by the join at all.
//
// A blueprint that was CALLED OFF drives nothing, ever. That is the
// `cancel_requested`/`cancelled` pair: the signal a cancel raises while the
// sequence still runs, and the terminal it settles on. Both are checked
// because they are written by different paths at different moments, and a gate
// that depends on two columns staying in lockstep is a gate that opens the day
// they don't.
//
// Otherwise a blueprint drives exactly ONE of its conversations, whatever its
// status: the one `current_step_index` names — the step being dispatched while
// it runs, the step it came to rest on once it stopped.
//
// One is the ceiling because the workspace is one. Every step shares a worktree
// and a snapshot blob keyed on the blueprint run, so two driven at once means
// two agents in one git tree and whichever concludes last overwriting the
// other's snapshot. Storage forces the rule; the predicate states it.
//
// Equality alone admits every legitimate dispatch because the pointer moves
// BEFORE the row it names is enqueued (see reactToStepTerminal), and nothing
// else moves it — no terminal write touches it, so a blueprint that stopped at
// step N leaves it at N, the conversation a follow-up must land on.
//
// Sequencing lives on the blueprint_runs row the claim already joins, so this
// stays a column comparison rather than a correlated scan over sibling
// conversations. That matters: this is the hot claim scan. A NULL step index
// compares NULL and is not drivable — a step that never recorded its position
// cannot prove it holds the workspace, and EnqueueConversation refuses to mint one.
const blueprintDrivableSQL = `(r.blueprint_run_id IS NULL
	    OR (br.cancel_requested = false AND br.status <> 'cancelled'
	        AND r.blueprint_step_index = br.current_step_index))`

// conversationQueueClaimSelect is the candidate CTE's projection —
// everything the dispatcher needs to branch on and drive the claimed
// conversation.
const conversationQueueClaimSelect = `r.id, r.org_id,
	COALESCE(r.type, '')                  AS type,
	COALESCE(r.task_id::text, '')         AS task_id,
	COALESCE(r.prompt_id, '')             AS prompt_id,
	COALESCE(r.model, '')                 AS model,
	COALESCE(r.runtime, '')               AS runtime,
	COALESCE(r.worktree_path, '')         AS worktree_path,
	COALESCE(r.sdk_session_id, '')        AS sdk_session_id,
	r.trigger_type,
	COALESCE(r.trigger_id::text, '')      AS trigger_id,
	COALESCE(r.creator_user_id::text, '') AS creator_user_id,
	COALESCE(r.team_id::text, '')         AS team_id,
	COALESCE(r.blueprint_run_id::text, '') AS blueprint_run_id,
	r.blueprint_step_index`

// handedBackOutcomesSQL is the pair of claim outcomes that record nothing
// about the conversation: 'requeued' (a recovery seam gave the claim back — a
// setup hiccup, a runtime that would not launch, a handoff nobody could take)
// and 'reaped' (the engagement's process died, and a boot sweep or the reaper
// released the claim it left behind). Every other outcome is an engagement
// speaking for itself: it concluded, it failed, it parked, it was stopped.
const handedBackOutcomesSQL = `'requeued','reaped'`

// EpisodeAttemptsSQL renders the retry budget's unit: how many times THIS
// queue episode has been attempted, the in-flight claim included — whether
// that claim is being minted or handed back. Exported and alias-parameterized
// (convAlias names the conversation in the enclosing query) because the
// enclosing query is the only thing that varies: the counting rule below is a
// decision about what an attempt IS, and a second copy of it would be a second
// answer to that.
//
// An episode is the run of consecutive hand-backs at the tail of the
// conversation's claim history. It ends at the most recent claim that recorded
// an outcome of its own, because that is an engagement that got somewhere —
// and the budget exists to stop retrying one that never does.
//
// Counting the conversation's LIFETIME claims instead — which this was, back
// when a conversation had exactly one engagement — is not a harsher budget but
// a wrong one. Every stop, resume and wake mints a claim, so a conversation
// picked up four times reached its next claim already over budget, and the
// first transient hiccup it ever met was misread as a deterministic crash: the
// blueprint failed and the worktree went with it, on a conversation whose four
// prior engagements had all been healthy.
//
// Reaps stay inside the episode deliberately. A conversation that hard-kills
// its executor is handed back by the next boot's sweep looking exactly like a
// conversation that was never claimed, so excluding them would let a process-killing
// conversation crash-loop the dispatcher forever — the one thing the lifetime
// count did get right.
//
// The in-flight claim is excluded from the COUNT by construction rather than
// by the CTE snapshot — it has no outcome yet, and both arms require one. It
// is what the +1 stands for.
//
// "Later than" is spelled against the hand-back's RELEASE, not its claim, and
// non-strictly. Engagements on one conversation never overlap (the claim is
// mutually exclusive by idx_claims_one_active), so a claim taken at or after
// another's release is unambiguously the later one — which makes the
// comparison independent of how finely the stored timestamps resolve. Spelled
// the obvious way instead — claimed_at > claimed_at — two claims that landed
// inside one clock tick would hide the boundary between them and the budget
// would over-count, and over-counting is the direction that poison-pills a
// step early. This way a degenerate clock collapses toward finding a boundary,
// which costs at most one more round of retries. COALESCE covers a released
// row that somehow carries an outcome without a release timestamp: it degrades
// to comparing claims rather than silently never matching.
func EpisodeAttemptsSQL(convAlias string) string {
	return `(SELECT COUNT(*) + 1 FROM claims c2
	WHERE c2.conversation_id = ` + convAlias + `.id
	  AND c2.outcome IN (` + handedBackOutcomesSQL + `)
	  AND NOT EXISTS (
	      SELECT 1 FROM claims c3
	      WHERE c3.conversation_id = c2.conversation_id
	        AND c3.outcome IS NOT NULL
	        AND c3.outcome NOT IN (` + handedBackOutcomesSQL + `)
	        AND c3.claimed_at >= COALESCE(c2.released_at, c2.claimed_at)))::int`
}

// conversationQueueClaimReturning is the outer-SELECT projection of ClaimNextConversation. The
// claim identity (executor/claim id/claimed_at/attempts) rides the same
// statement: the id + claimed_at come from the freshly inserted claims row,
// attempts from the current queue episode (see EpisodeAttemptsSQL). The
// claim id is returned because the executor needs to name this engagement
// later — at teardown, once it has been released and can no longer be found
// by looking for the conversation's active claim.
var conversationQueueClaimReturning = `candidate.id::text, candidate.org_id::text, candidate.type, candidate.task_id, candidate.prompt_id,
	candidate.model, candidate.runtime, candidate.worktree_path, candidate.sdk_session_id,
	candidate.trigger_type, candidate.trigger_id, candidate.creator_user_id,
	candidate.team_id,
	candidate.blueprint_run_id, candidate.blueprint_step_index,
	minted.id::text AS claim_id, minted.claimed_at,
	` + EpisodeAttemptsSQL("candidate") + ` AS attempts`

func (s *conversationQueueStore) ClaimNextConversation(ctx context.Context, executorID string, bootEpoch int64, placement db.ClaimPlacement) (*domain.Conversation, error) {
	// One scan, every surface: the needs-driving predicate is type-agnostic
	// and the blueprint gate for delegation rides alongside it. An empty
	// queue matches no row and the scan reports ErrNoRows -> (nil, nil).
	//
	// Mutual exclusion is idx_claims_one_active, the partial unique index on
	// unreleased claims: nothing on the conversation row changes at claim
	// time any more, so FOR UPDATE SKIP LOCKED is a contention optimizer
	// (concurrent claimers pick different rows) rather than the guarantee.
	// The guarantee is the index, and claimRetryAttempts is how a loser
	// finds out — see the unique-violation retry below.
	//
	// The claims row is minted in the same statement (a data-modifying CTE)
	// so ownership is never ambiguous between claim and the process actually
	// going live — see ResetProcessingConversations. An empty executorID (the un-wired
	// test-spawner path) stores the '' sentinel; claims.executor_id is NOT
	// NULL by schema.
	//
	// The one conversation write left on this path is the un-park: a claim
	// taken on the `open` arm of the predicate ends the park by definition,
	// so the row goes back to mid-flight (NULL). That keeps "parked" and
	// "an engagement is driving this" disjoint at every instant, which is
	// what every recovery guard downstream reads — a claimed conversation is
	// always NULL, a NULL conversation with no claim is always claimable.
	//
	// Per-org fairness + cap are ALWAYS applied here, independent of
	// placement: the org_active CTE counts each org's active (slot-occupying)
	// runs once per statement, the cap filter hides a queued conversation whose org is
	// at or above its max_concurrent_runs, and the fairness key orders
	// claimable rows fewest-active-org first. Both degrade to a no-op at N=1 /
	// single org — the fairness key is constant across all candidates (so the
	// order collapses to started_at, id — the old global-oldest) and the
	// default NULL cap never filters — so existing single-org callers see the
	// same conversation claimed. The active count reads committed state, so under a
	// burst of concurrent claimers an org can momentarily exceed its cap by the
	// number of in-flight claims (a soft ceiling); sequential claims are exact.
	//
	// candidatePredicate + the tier prefix on the order are the only things
	// placement changes. Placement composes with fairness by
	// prefixing the tier term to the ORDER BY — fairness then orders WITHIN
	// each tier. Disabled: no tier term, no candidate predicate — the claim is
	// the globally-oldest claimable conversation modulo fairness/cap. Enabled: the
	// two-tier claim (see the SQL below).
	candidatePredicate := ""
	tierPrefix := "" // placement tier ordering, prepended to the fairness key
	args := []any{executorID, bootEpoch}
	if placement.Enabled {
		// $3 = aging seconds, $4 = liveness seconds. A conversation is claimable by me
		// when it is mine (tier 1), unowned, aged past the tier-2 window, or
		// its stamped preferred is not a live claimant — dead (heartbeat
		// stale past liveness), draining, or dispatch-gated. Tier 1 sorts
		// first, so a fresh conversation with a live owner is exclusively that owner's
		// until it ages (warm cache), while a saturated/dead owner never
		// head-of-line-blocks its shard.
		//
		// The aging arm anchors on queued_at — the moment the row entered the
		// queue in THIS episode (the enqueue for a fresh conversation, the
		// wake for a resume) — never on started_at, which is stamped once at
		// mint. The window bounds how long a claimable conversation waits for
		// its preferred executor, so it has to open when the wait begins:
		// anchored on the mint stamp, a resume older than the window would
		// have no exclusive period at all and the re-stamped affinity would
		// only order its owner's own scan. started_at is the fallback for
		// rows that predate the queue column.
		candidatePredicate = `
			  AND (
			    (r.preferred_executor_id IS NOT NULL AND r.preferred_executor_id = $1)
			    OR r.preferred_executor_id IS NULL
			    OR COALESCE(r.queued_at, r.started_at) < now() - make_interval(secs => $3)
			    OR NOT EXISTS (
			        SELECT 1 FROM instances i
			        WHERE i.id = r.preferred_executor_id
			          AND i.last_heartbeat_at >= now() - make_interval(secs => $4)
			          AND i.draining = false
			          AND i.dispatch_gated IS NOT TRUE
			    )
			  )`
		tierPrefix = "(r.preferred_executor_id IS NOT NULL AND r.preferred_executor_id = $1) DESC, "
		args = append(args, placement.AgingInterval.Seconds(), placement.Liveness.Seconds())
	}
	// Fairness orders WITHIN each placement tier: fewest-active-org first, then
	// oldest. COALESCE covers an org with zero active conversations (no org_active row).
	orderBy := tierPrefix + "COALESCE(oa.active, 0), r.started_at, r.id"
	// org_active counts engagements, not stored statuses: an unreleased claim
	// IS an occupied slot, on either surface. Read straight off claims (whose
	// org_id is the conversation's) so the count is an index-only scan of
	// idx_claims_one_active rather than a pass over conversation history.
	query := `
		WITH org_active AS (
			SELECT org_id, count(*)::int AS active
			FROM claims
			WHERE released_at IS NULL
			GROUP BY org_id
		),
		candidate AS (
			SELECT ` + conversationQueueClaimSelect + `
			FROM conversations r
			LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
			LEFT JOIN org_active oa ON oa.org_id = r.org_id
			LEFT JOIN org_settings os ON os.org_id = r.org_id
			WHERE ` + eligibleForDrivingSQL + `
			  AND ` + blueprintDrivableSQL + `
			  AND (
			    os.max_concurrent_runs IS NULL
			    OR os.max_concurrent_runs <= 0
			    OR COALESCE(oa.active, 0) < os.max_concurrent_runs
			  )` + candidatePredicate + `
			ORDER BY ` + orderBy + `
			FOR UPDATE OF r SKIP LOCKED
			LIMIT 1
		),
		unparked AS (
			-- Every park column clears together — see the SQLite twin for why
			-- park_reason in particular must not survive its own park.
			UPDATE conversations SET status = NULL, parked_at = NULL, park_reason = NULL
			FROM candidate
			WHERE conversations.id = candidate.id AND conversations.status IS NOT NULL
			RETURNING conversations.id
		),
		minted AS (
			INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch, claimed_at)
			SELECT candidate.org_id, candidate.id, $1, $2::bigint, now() FROM candidate
			RETURNING claims.id, claims.conversation_id, claims.claimed_at
		)
		SELECT ` + conversationQueueClaimReturning + `
		FROM candidate JOIN minted ON minted.conversation_id = candidate.id
	`
	// A concurrent claimer that read its snapshot before our claim committed
	// can pick the same conversation and lose on idx_claims_one_active. That
	// is the index doing its job, not an error worth surfacing: re-scan and
	// take the next eligible row instead.
	for attempt := 0; ; attempt++ {
		conv, err := scanPgClaimedConversation(s.conn.QueryRowContext(ctx, query, args...))
		if isActiveClaimConflict(err) && attempt < claimRetryAttempts {
			continue
		}
		if conv != nil {
			conv.ExecutorID = executorID
		}
		if isActiveClaimConflict(err) {
			return nil, nil
		}
		return conv, wrapAdminPoolPermErr(err, "conversation_queue.ClaimNextConversation")
	}
}

// claimRetryAttempts bounds the re-scan after a lost race on
// idx_claims_one_active. Losing twice in a row means real contention, and
// the caller's next scan tick is a better place to try again than a spin.
const claimRetryAttempts = 2

// isActiveClaimConflict reports whether err is the unique violation on the
// one-active-claim index — the "somebody else claimed this conversation
// between my snapshot and my insert" signal.
func isActiveClaimConflict(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == "idx_claims_one_active"
}

// RequeueConversation releases the claim FIRST and flips the conversation
// row SECOND, as two separate statements in one transaction — NOT the single
// combined CTE the pre-conversion version used. Postgres runs every
// data-modifying CTE in a WITH clause against one shared snapshot (they
// "cannot see one another's effects on the target tables" per the Postgres
// docs), so a single statement's claims release would be invisible to that
// same statement's conversationClaimLateral read — the returned row would
// show the just-released claim as still active. Two statements in the same
// transaction don't have that restriction: the second sees the first's write
// directly, which is what makes the final RETURNING's derived display status
// ('queued', no active claim) agree with a follow-up Get. See
// writeConversationReturning's doc for the single-CTE version of this
// caveat.
//
// The guard — a mid-flight conversation with a live claim — moves onto the
// claims release itself (matched only when the owning conversation's status
// IS NULL), so RowsAffected there tells the whole guard's outcome; the
// conversations flip that follows needs no guard of its own; nothing else in
// this transaction could have changed the row in between.
func (s *conversationQueueStore) RequeueConversation(ctx context.Context, orgID, conversationID, lastErr string) (*domain.Conversation, error) {
	var result *domain.Conversation
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET released_at = now(), outcome = 'requeued'
			WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
			  AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = claims.conversation_id AND c.status IS NULL)
		`, orgID, conversationID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		// preferred_executor_id is cleared: a requeue's stamp likely points at
		// the executor that just failed the conversation; NULL means "unowned,
		// claimable by anyone now" — a live executor re-warms it with no aging
		// delay, the correct placement-is-advisory answer on a recovery path
		// (affinity is re-earned on the next enqueue, never carried stale).
		r, err := writeConversationReturning(ctx, q, `
			UPDATE conversations SET result_summary = $1, preferred_executor_id = NULL
			WHERE org_id = $2 AND id = $3
			RETURNING *
		`, lastErr, orgID, conversationID)
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

func (s *conversationQueueStore) MarkAwaitingCredentials(ctx context.Context, orgID, conversationID, credPubKey string) (bool, error) {
	// The sidecar pubkey lands on the ACTIVE claim in the same statement as
	// the phase park, so the provisioner never sees a parked conversation without
	// the key it needs; the conversation row is untouched (it stays
	// 'running'). Guarded on phase IS NULL — the same protection window
	// the former stored-status guard gave: a duplicate can't re-park or
	// overwrite the pubkey while the claim is parked or mid-setup.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE claims SET phase = 'awaiting_credentials', cred_pubkey = NULLIF($1, '')
		WHERE org_id = $2 AND conversation_id = $3 AND released_at IS NULL AND phase IS NULL
	`, credPubKey, orgID, conversationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		_ = ctlbus.Publish(ctx, s.conn, ctlbus.Message{Kind: "cred_request", OrgID: orgID, ConversationID: conversationID})
	}
	return n > 0, nil
}

// awaitingCredentialsCols is the shared claim-identity projection: the
// conversation joined to its active (unreleased) claim, with the claim's
// executor/boot/pubkey and claimed_at falling back to started_at for a
// never-claimed row.
const awaitingCredentialsCols = `r.id::text, r.org_id::text, COALESCE(r.type, ''), COALESCE(r.team_id::text, ''), COALESCE(r.task_id::text, ''),
	COALESCE(cl.executor_id, ''), COALESCE(cl.boot_epoch, 0), COALESCE(cl.claimed_at, r.started_at), COALESCE(cl.cred_pubkey, '')`

func (s *conversationQueueStore) GetClaim(ctx context.Context, orgID, conversationID string) (db.AwaitingCredentialsConversation, bool, error) {
	var r db.AwaitingCredentialsConversation
	err := s.conn.QueryRowContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		LEFT JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		WHERE r.org_id = $1 AND r.id = $2
	`, orgID, conversationID).Scan(&r.ConversationID, &r.OrgID, &r.ConversationType, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &r.ClaimedAt, &r.CredPubKey)
	if errors.Is(err, sql.ErrNoRows) {
		return db.AwaitingCredentialsConversation{}, false, nil
	}
	if err != nil {
		return db.AwaitingCredentialsConversation{}, false, err
	}
	return r, true, nil
}

// ClaimExecutorSystem resolves one claim id to the executor that took it. The
// id is cast rather than compared as text so a malformed value is refused here
// instead of matching nothing; the caller reads an error as "cannot tell",
// which is a different answer from "no such claim".
func (s *conversationQueueStore) ClaimExecutorSystem(ctx context.Context, orgID, claimID string) (string, bool, error) {
	var executorID string
	err := s.conn.QueryRowContext(ctx, `
		SELECT executor_id FROM claims WHERE org_id = $1 AND id = $2::uuid
	`, orgID, claimID).Scan(&executorID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return executorID, true, nil
}

// RequeueAwaitingCredentials releases a conversation whose active claim is
// parked in phase='awaiting_credentials' — the executor-side timeout path:
// the brain never responded to this conversation's cred_request within the
// awaiting-credentials deadline, so the claim is released and the (still mid-flight) conversation
// is claimable again by the next dispatcher, this instance or a sibling.
// Guarded on that parked claim so a stale/duplicate timeout can't act on a
// row that already moved on (bundle arrived just after the deadline check,
// or the conversation was reaped).
func (s *conversationQueueStore) RequeueAwaitingCredentials(ctx context.Context, orgID, conversationID string) (bool, error) {
	var flipped int
	err := s.conn.QueryRowContext(ctx, `
		WITH req AS (
			UPDATE conversations SET preferred_executor_id = NULL
			WHERE org_id = $1 AND id = $2
			  AND EXISTS (
			      SELECT 1 FROM claims cl
			      WHERE cl.conversation_id = conversations.id
			        AND cl.released_at IS NULL AND cl.phase = 'awaiting_credentials'
			  )
			RETURNING id
		),
		rel AS (
			UPDATE claims SET released_at = now(), outcome = 'requeued'
			FROM req
			WHERE claims.conversation_id = req.id AND claims.released_at IS NULL
			RETURNING claims.id
		)
		SELECT count(*) FROM req
	`, orgID, conversationID).Scan(&flipped)
	if err != nil {
		return false, err
	}
	return flipped > 0, nil
}

func (s *conversationQueueStore) ListAwaitingCredentials(ctx context.Context) ([]db.AwaitingCredentialsConversation, error) {
	// The parked set is keyed off the active claim's phase (served by the
	// idx_claims_active_phase partial index); an inner join is right here —
	// a parked claim by definition exists. No type filter: parking is a
	// property of the engagement, not the surface, so this one scan is the
	// whole provisioner's input and the caller routes on the type it
	// returns.
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
	// The active-claim join IS the "actively running" filter (a claim is the
	// engagement; there is no stored status to check); the bundle age check
	// joins claim_credentials on the claim id. A claim parked awaiting
	// credentials is excluded — its bundle is being minted fresh, not
	// refreshed.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		JOIN claim_credentials cc ON cc.claim_id = cl.id
		WHERE cl.phase IS DISTINCT FROM 'awaiting_credentials'
		  AND cc.created_at < $1
	`, olderThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAwaitingCredentialsConversations(rows)
}

func scanAwaitingCredentialsConversations(rows *sql.Rows) ([]db.AwaitingCredentialsConversation, error) {
	var out []db.AwaitingCredentialsConversation
	for rows.Next() {
		var r db.AwaitingCredentialsConversation
		if err := rows.Scan(&r.ConversationID, &r.OrgID, &r.ConversationType, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &r.ClaimedAt, &r.CredPubKey); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *conversationQueueStore) ResetProcessingConversations(ctx context.Context, executorID string, bootEpoch int64) (int, error) {
	// Ownership-scoped through claims: only ACTIVE claims this instance
	// itself holds (executor_id = $1) from a strictly earlier boot
	// (boot_epoch < $2) on mid-flight conversations (status NULL — nothing
	// wrote an outcome, so the engagement was cut off) are released
	// ('reaped'). The release IS the requeue: the conversation was already
	// mid-flight, so the moment it has no claim it matches the needs-driving
	// predicate again. A live sibling's claim carries a different
	// executor_id and is never touched — that's what makes a booting process
	// safe alongside a still-live one. Parked (`open`) and terminal
	// conversations are not mid-flight and stay put.
	var count int
	err := s.conn.QueryRowContext(ctx, `
		WITH victims AS (
			SELECT cl.id AS claim_id, cl.conversation_id
			FROM claims cl
			JOIN conversations r ON r.id = cl.conversation_id
			WHERE cl.released_at IS NULL
			  AND cl.executor_id = $1
			  AND cl.boot_epoch < $2
			  AND r.status IS NULL
			  AND r.blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running')
		),
		clr AS (
			UPDATE conversations SET preferred_executor_id = NULL
			FROM victims WHERE conversations.id = victims.conversation_id
			RETURNING conversations.id
		),
		rel AS (
			UPDATE claims SET released_at = now(), outcome = 'reaped'
			FROM victims WHERE claims.id = victims.claim_id
			RETURNING claims.id
		)
		SELECT count(*) FROM rel
	`, executorID, bootEpoch).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *conversationQueueStore) FleetQueueShares(ctx context.Context) ([]db.OrgQueueShare, error) {
	// Both counts are derived: active is the org's unreleased claims (an
	// engagement IS the occupied slot), queued is the org's conversations
	// matching the needs-driving predicate. Two index-backed scans unioned
	// into one grouping, then a PK lookup on org_settings for the cap. An
	// operator-cadence read, not the claim hot path. Ordered most-pressure
	// first so the fleet view leads with the busiest tenants.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT counts.org_id::text, counts.active, counts.queued, os.max_concurrent_runs
		FROM (
			SELECT org_id, SUM(active)::int AS active, SUM(queued)::int AS queued
			FROM (
				SELECT org_id, 1 AS active, 0 AS queued FROM claims WHERE released_at IS NULL
				UNION ALL
				SELECT r.org_id, 0, 1 FROM conversations r WHERE `+eligibleForDrivingSQL+`
			) parts
			GROUP BY org_id
		) counts
		LEFT JOIN org_settings os ON os.org_id = counts.org_id
		ORDER BY (counts.active + counts.queued) DESC, counts.org_id
	`)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "conversation_queue.FleetQueueShares")
	}
	defer rows.Close()
	return scanOrgQueueShares(rows)
}

func (s *conversationQueueStore) ReconcileOrphanedConversations(ctx context.Context) (int, error) {
	// Boot self-heal — see ConversationQueueStore.ReconcileOrphanedConversations and the
	// SQLite mirror. Admin pool (BYPASSRLS): a cross-org system sweep with no
	// per-user identity, the same posture as ResetProcessingConversations.
	//
	// `open`, not a terminal: nothing about an orphan failed, and nothing
	// about it concluded either. Read the park as "stopped without
	// concluding", NOT as "resumable" — its blueprint is terminal, so the
	// claim gate refuses it and no resume path will wake it. The scope is
	// status IS NULL alone (mid-flight is exactly what "still looks live"
	// means); an already-parked orphan is already in the state this writes.
	// Any active claim on a parked row releases as 'cancelled' in the same
	// statement — the engagement was ended from outside.
	var total int
	err := inTx(ctx, s.conn, func(q queryer) error {
		var parked int
		if err := q.QueryRowContext(ctx, `
			WITH parked AS (
				UPDATE conversations
				SET status = 'open',
				    parked_at = COALESCE(parked_at, now()),
				    park_reason = COALESCE(park_reason, 'blueprint_terminal'),
				    result_summary = COALESCE(NULLIF(result_summary, ''), $1)
				WHERE status IS NULL
				  AND blueprint_run_id IN (
				      SELECT id FROM blueprint_runs
				      WHERE status IN ('completed','aborted','failed','cancelled')
				  )
				RETURNING id
			),
			rel AS (
				UPDATE claims SET released_at = now(), outcome = 'cancelled'
				FROM parked
				WHERE claims.conversation_id = parked.id AND claims.released_at IS NULL
				RETURNING claims.id
			)
			SELECT count(*) FROM parked
		`, "Stopped: owning blueprint run reached a terminal state").Scan(&parked); err != nil {
			return err
		}

		// Claim-desync janitor arm (after the blueprint-terminal park, whose
		// own claim release it therefore never has to redo).
		released, err := healClaimDesyncs(ctx, q)
		if err != nil {
			return err
		}

		// Mint-crash arm: the mirror of the park above, one level up. That arm
		// heals a live child under a dead parent; this one heals a live parent
		// with no child at all. Order between the two is immaterial — a parent
		// this arm fails has no child for the park to owe anything to, which is
		// the whole reason it needs its own arm.
		orphaned, err := failBlueprintRunsOrphanedAtMint(ctx, q)
		if err != nil {
			return err
		}
		total = parked + released + orphaned
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// healClaimDesyncs is the janitor for the one state the app-pool
// (non-System) terminal writes can still strand a delegation conversation
// in: a terminal conversation with a still-active claim, because the
// conversation flip and the claim release commit independently (tf_app holds
// no claims UPDATE grant, so one atomic tx is structurally unavailable) and
// the flip can land without the release. Release the claim, outcome mapped
// from the status the same way the terminal writes map it. Idempotent, and
// safe to run concurrently with healthy terminal writes.
//
// The former second arm — an in-flight conversation whose claim released but
// whose status flip rolled back, requeued by writing 'queued' — no longer
// exists as a desync at all: a released claim on a mid-flight (status NULL)
// conversation IS the requeue, so the shape it healed is now the ordinary
// claimable state.
func healClaimDesyncs(ctx context.Context, q queryer) (released int, err error) {
	res, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = now(),
		    outcome = CASE c.status
		        WHEN 'completed' THEN 'completed'
		        ELSE 'failed'
		    END
		FROM conversations c
		WHERE claims.conversation_id = c.id AND claims.released_at IS NULL
		  AND c.status IN (`+conversationTerminalStatusesSQL+`)
	`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// failBlueprintRunsOrphanedAtMint terminal-fails every 'running' blueprint_run
// that holds no child conversation and is older than the shared grace: the
// firing path's mint→enqueue crash window, where the parent committed and its
// first step never did. With no child there is no claim and no conversation, so
// every other recovery arm — here, in ResetProcessingConversations, in the leader reaper
// — joins straight past it while it keeps holding the one-active-auto-run index
// against its task.
//
// Own DB time, and nothing but the parent row to write: no child to park, no
// claim to release, and no task touch. Freeing the index is the point — the
// task's already-queued firing intent then drains into a fresh, fully-minted
// run, which is why this fails rather than trying to re-mint the step (that
// would duplicate the firing path's config derivation inside a recovery path).
//
// The grace makes racing a live mint impossible rather than unlikely. Retention
// can't manufacture a false positive either: nothing purges conversation rows,
// and archival keeps the row.
func failBlueprintRunsOrphanedAtMint(ctx context.Context, q queryer) (int, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE blueprint_runs
		SET status = 'failed', completed_at = now(), abort_reason = $2
		WHERE status = 'running'
		  AND started_at < now() - make_interval(secs => $1)
		  AND NOT EXISTS (
		      SELECT 1 FROM conversations c WHERE c.blueprint_run_id = blueprint_runs.id
		  )
	`, domain.BlueprintOrphanedAtMintGrace.Seconds(), domain.BlueprintAbortOrphanedAtMint)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// scanOrgQueueShares reads FleetQueueShares rows, mapping a NULL or
// non-positive max_concurrent_runs to a nil cap (unlimited).
func scanOrgQueueShares(rows *sql.Rows) ([]db.OrgQueueShare, error) {
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

func (s *conversationQueueStore) CountQueuedSystem(ctx context.Context) (int, error) {
	var n int
	err := s.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations r WHERE `+eligibleForDrivingSQL).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// conversationTimingClaimLateral derives the timing projection's claim fields — the
// LATEST claim's executor + claimed_at (the engagement that most recently
// drove the conversation, released or not), whether one is still live, plus
// the SUM of the per-engagement duration telemetry.
const conversationTimingClaimLateral = `
	LEFT JOIN LATERAL (
		SELECT MAX(c2.claimed_at) AS claimed_at,
		       (ARRAY_AGG(c2.executor_id ORDER BY c2.claimed_at DESC))[1] AS executor_id,
		       COUNT(*) FILTER (WHERE c2.released_at IS NULL) > 0 AS has_active,
		       SUM(c2.duration_ms)::bigint AS duration_ms
		FROM claims c2
		WHERE c2.conversation_id = r.id
	) cl ON true`

// conversationTimingStatusSQL is the timing projection's status: the stored outcome
// when there is one, else the derived in-flight state. The percentile read
// buckets by failure kind, so a mid-flight row must still name itself
// rather than scan as NULL.
const conversationTimingStatusSQL = `COALESCE(r.status, CASE WHEN cl.has_active THEN 'running' ELSE 'queued' END)`

func (s *conversationQueueStore) RecentConversationTimingsSystem(ctx context.Context, since time.Time, limit int) ([]domain.ConversationTiming, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT r.org_id::text, COALESCE(cl.executor_id, ''), `+conversationTimingStatusSQL+`, COALESCE(r.failure_kind, ''),
		       r.started_at, cl.claimed_at, r.completed_at, cl.duration_ms
		FROM conversations r
		`+conversationTimingClaimLateral+`
		WHERE r.type = 'delegation' AND r.started_at >= $1
		ORDER BY r.started_at DESC
		LIMIT $2
	`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationTimings(rows)
}

func (s *conversationQueueStore) QueuedConversationAgesSystem(ctx context.Context) ([]domain.QueuedConversation, error) {
	return s.queuedConversationAges(ctx, "")
}

func (s *conversationQueueStore) QueuedConversationAgesForOrgSystem(ctx context.Context, orgID string) ([]domain.QueuedConversation, error) {
	return s.queuedConversationAges(ctx, orgID)
}

func (s *conversationQueueStore) queuedConversationAges(ctx context.Context, orgID string) ([]domain.QueuedConversation, error) {
	q := `SELECT r.org_id::text, r.started_at, COALESCE(r.preferred_executor_id, '')
	      FROM conversations r WHERE ` + eligibleForDrivingSQL
	args := []any{}
	if orgID != "" {
		q += ` AND r.org_id = $1`
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
	q := `SELECT r.org_id::text, COALESCE(cl.executor_id, ''), ` + conversationTimingStatusSQL + `, COALESCE(r.failure_kind, ''),
	             r.started_at, cl.claimed_at, r.completed_at, cl.duration_ms
	      FROM conversations r
	      ` + conversationTimingClaimLateral + `
	      WHERE r.type = 'delegation' AND r.org_id = $1 AND r.started_at >= $2`
	args := []any{orgID, since.UTC()}
	if !until.IsZero() {
		args = append(args, until.UTC())
		q += ` AND r.started_at < $3`
	}
	args = append(args, limit)
	q += ` ORDER BY r.started_at DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationTimings(rows)
}

// executorClaimSelectCols is the column list both operator claim reads and
// ConversationStore's claims-row writes (conversation.go) project, so they
// can never drift into disagreeing about the same row's shape. Split from its
// FROM clause below: the reads select from the live `claims` table, while a
// converted write selects from the `updated` CTE its own RETURNING produced —
// same columns, different row source.
const executorClaimSelectCols = `
	c.id::text, c.org_id::text, c.conversation_id::text,
	c.claimed_at, c.released_at, COALESCE(c.outcome, ''),
	c.peak_mem_mb, c.cpu_usec,
	COALESCE(v.status, CASE WHEN c.released_at IS NULL THEN 'running' ELSE 'queued' END, ''),
	COALESCE(v.failure_kind, '')`

// executorClaimCols is the shared projection behind both operator claim reads,
// so the per-executor list and the single-claim lookup can never drift into
// disagreeing about the same row. LEFT JOIN on the conversation: the claim is
// the subject here, and a claim whose conversation is gone must still report
// its measured cost rather than vanishing from the box's occupancy.
const executorClaimCols = `
	SELECT ` + executorClaimSelectCols + `
	FROM claims c
	LEFT JOIN conversations v ON v.id = c.conversation_id`

func (s *conversationQueueStore) RecentClaimsForExecutorSystem(ctx context.Context, executorID string, limit int) ([]domain.ExecutorClaim, error) {
	if limit <= 0 {
		limit = 25
	}
	// Tie-break on id so a batch of claims minted in the same instant orders
	// deterministically across repeated polls — the console refetches on a
	// timer and a shuffling table reads as churn that isn't happening.
	rows, err := s.conn.QueryContext(ctx, executorClaimCols+`
		WHERE c.executor_id = $1
		ORDER BY c.claimed_at DESC, c.id DESC
		LIMIT $2
	`, executorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExecutorClaims(rows)
}

func (s *conversationQueueStore) ClaimByIDSystem(ctx context.Context, claimID string) (*domain.ExecutorClaim, error) {
	// A non-uuid id is a caller typo, not a server fault: Postgres would
	// reject the bind with a 22P02 and the handler would 500 on what is
	// really a miss. Report it as "no such claim" and let the caller 404.
	if _, err := uuid.Parse(claimID); err != nil {
		return nil, nil
	}
	rows, err := s.conn.QueryContext(ctx, executorClaimCols+` WHERE c.id = $1`, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanExecutorClaims(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func scanExecutorClaims(rows *sql.Rows) ([]domain.ExecutorClaim, error) {
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
// RETURNING reads on ConversationStore's claims-row writes (conversation.go)
// off one column layout (executorClaimSelectCols).
type executorClaimScanner interface {
	Scan(dest ...any) error
}

func scanOneExecutorClaim(row executorClaimScanner) (domain.ExecutorClaim, error) {
	var c domain.ExecutorClaim
	var releasedAt sql.NullTime
	var peakMem, cpuUsec sql.NullInt64
	if err := row.Scan(
		&c.ID, &c.OrgID, &c.ConversationID,
		&c.ClaimedAt, &releasedAt, &c.Outcome,
		&peakMem, &cpuUsec, &c.Status, &c.FailureKind,
	); err != nil {
		return domain.ExecutorClaim{}, err
	}
	if releasedAt.Valid {
		v := releasedAt.Time
		c.ReleasedAt = &v
	}
	c.PeakMemMB = intPtrFromNull(peakMem)
	c.CPUUsec = int64PtrFromNull(cpuUsec)
	return c, nil
}

// scanExecutorClaimRow scans a single RETURNING executorClaimSelectCols row
// into a domain.ExecutorClaim, or (nil, nil) on sql.ErrNoRows — the
// guard-declined shape every claims-row write in conversation.go uses for
// "nothing matched".
func scanExecutorClaimRow(row *sql.Row) (*domain.ExecutorClaim, error) {
	c, err := scanOneExecutorClaim(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

func scanConversationTimings(rows *sql.Rows) ([]domain.ConversationTiming, error) {
	var out []domain.ConversationTiming
	for rows.Next() {
		var t domain.ConversationTiming
		var claimedAt, completedAt sql.NullTime
		var durationMS sql.NullInt64
		if err := rows.Scan(
			&t.OrgID, &t.ExecutorID, &t.Status, &t.FailureKind,
			&t.StartedAt, &claimedAt, &completedAt, &durationMS,
		); err != nil {
			return nil, err
		}
		if claimedAt.Valid {
			v := claimedAt.Time
			t.ClaimedAt = &v
		}
		if completedAt.Valid {
			v := completedAt.Time
			t.CompletedAt = &v
		}
		t.DurationMS = intPtrFromNull(durationMS)
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanPgClaimedConversation scans ClaimNextConversation's outer projection into
// *domain.Conversation, including the freshly minted claim's claimed_at and the
// attempts count. (nil, nil) on sql.ErrNoRows so callers treat "nothing
// claimable" as a non-error empty result. Status is deliberately left empty:
// a claimed conversation's stored status is NULL by construction, and the
// dispatcher branches on Type and Runtime, never on it.
func scanPgClaimedConversation(row *sql.Row) (*domain.Conversation, error) {
	var (
		r         domain.Conversation
		stepIdx   sql.NullInt64
		claimedAt time.Time
	)
	err := row.Scan(&r.ID, &r.OrgID, &r.Type, &r.TaskID, &r.PromptID, &r.Model, &r.Runtime,
		&r.WorktreePath, &r.SessionID, &r.TriggerType, &r.TriggerID,
		&r.CreatorUserID, &r.TeamID, &r.BlueprintRunID, &stepIdx,
		&r.ClaimID, &claimedAt, &r.Attempts)
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
	r.ClaimedAt = &claimedAt
	return &r, nil
}
