package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

// insertConversation is the mint a delegation conversation is written by. It
// takes the queryer because it always runs on the transaction that also
// commits the blueprint_run or the current_step_index pointer the row belongs
// to — a step arrives with the write that implies it, never on a commit of
// its own. The wake doorbell is NOT rung here: it announces a committed row,
// so it belongs to whoever owns the commit.
//
// The row carries NO status — the absence of an outcome is what makes it
// claimable, so the mint writes nothing to the column and queued_at carries
// the moment it entered the queue. runtime is stamped 'native' here and 'sdk'
// in the SQLite sibling: the dialect IS the mode (Postgres is multi-only,
// SQLite is local-only), so the split lands where the row is written rather
// than as a caller-passed knob.
//
// Both arms stamp it explicitly, which is what makes the SDK engine
// unreachable for a multi delegation: the ratchet means no conversation ever
// changes engines, so an engine no mint writes is an engine nothing runs.
// That is the whole enforcement — there is no claim-side exclusion, because
// there is no row for one to exclude. Leaving either arm to the column
// DEFAULT (still 'sdk') would quietly undo it.
//
// Returns the minted row via writeConversationReturning, sharing
// ConversationStore.Get's projection — a fresh mint has no claims/messages/
// memory rows yet, so the claim/ledger laterals and the memory/agent LEFT
// JOINs read exactly as a Get immediately afterward would (NULL/zero
// defaults throughout).
func insertConversation(ctx context.Context, q queryer, orgID string, conv domain.Conversation) (*domain.Conversation, error) {
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
		return writeConversationReturning(ctx, q, `
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
	}
	// Manual: the creator has to arrive on the row, for the same reason and on
	// the same terms as the blueprint_runs row this helper's other caller
	// commits beside it — see db.ErrManualCreatorRequired. Refused rather than
	// defaulted to the org owner, whose reads conversations_select would then
	// show someone else's delegation on.
	if conv.CreatorUserID == "" || conv.CreatorUserID == runmode.LocalDefaultUserID {
		return nil, fmt.Errorf("insert conversation (manual delegation): %w", db.ErrManualCreatorRequired)
	}
	return writeConversationReturning(ctx, q, `
		INSERT INTO conversations (id, org_id, type, runtime, task_id, prompt_id, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
		                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
		                  queued_at)
		VALUES ($1, $2, 'delegation', 'native', $3, $4, $5, $6, 'manual', $7,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team',
		        $8::uuid,
		        $9, $10, $11, NULLIF($12, ''), now())
		RETURNING *
	`, conv.ID, orgID, conv.TaskID, nullIfEmpty(conv.PromptID), conv.Model, conv.WorktreePath,
		nullIfEmpty(conv.TriggerID), conv.CreatorUserID, nullIfEmpty(conv.ActorAgentID),
		nullIfEmpty(conv.BlueprintRunID), stepIdx, conv.PreferredExecutorID)
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

// liveClaimExistsSQL is the narrower question the DISPLAY asks: is an
// engagement actually alive on this conversation right now. An unreleased
// claim whose lease has lapsed answers no — whatever process held it is gone,
// and the row is waiting to be taken over.
//
// It is deliberately not what the ownership predicates read. Those ask "may a
// second claim exist", and the answer to that is still no: idx_claims_one_active
// refuses one while the expired row is unreleased, so a predicate that treated
// it as absent would offer work the insert would then reject.
//
// statement_timestamp(), never now(): a lease is a question about this
// instant, and now() is frozen at the start of a transaction that may have
// begun well before the read. Every lease timestamp spells it this way, which
// is what makes SQLite's per-statement 'now' the same question rather than a
// near-miss.
const liveClaimExistsSQL = `EXISTS (
		SELECT 1 FROM claims cl_a
		WHERE cl_a.conversation_id = r.id AND cl_a.released_at IS NULL
		  AND cl_a.lease_expires_at > statement_timestamp())`

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
//
// A pending stop takes the row out of the queue: nothing may start driving a
// conversation somebody asked to stop. The dispatcher's settlement parks it
// instead, and the park clears the intent.
const needsDrivingSQL = `r.archived_at IS NULL
	  AND r.stop_requested_at IS NULL
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
// and a snapshot blob keyed on the task, so two driven at once means two agents
// in one git tree and whichever concludes last overwriting the other's
// snapshot. Storage forces the rule; the predicate states it.
//
// Equality alone admits every legitimate dispatch because the pointer and the
// row it names are committed together (see
// BlueprintStore.AdvanceRunToStepSystem), and nothing else moves it — no
// terminal write touches it, so a blueprint that stopped at step N leaves it
// at N, the conversation a follow-up must land on.
//
// Sequencing lives on the blueprint_runs row the claim already joins, so this
// stays a column comparison rather than a correlated scan over sibling
// conversations. That matters: this is the hot claim scan. A NULL step index
// compares NULL and is not drivable — a step that never recorded its position
// cannot prove it holds the workspace, and the mint refuses to write one.
//
// The second clause is the memory gate, and it is about the task rather than
// the blueprint: a task holding a conversation that ended owing a memory
// drives nothing until that memory exists. Otherwise a requeue-then-delegate
// opens the next conversation without the handoff the ended one still owes,
// and no later write can put it back — the opening turn is composed once. A
// conversation with no task cannot owe one, hence the NULL arm.
//
// This gate is the ONLY place the memory rule is enforced, and it is
// re-applied on every claim — which is what makes it sufficient for every
// claim except one already in flight. delegate.blueprintDrivableForClaim, the
// Go predicate the dispatcher re-checks with after claiming, mirrors the
// blueprint clause alone; its doc carries what that costs, when it costs
// anything, and what closes it.
//
// The third clause is the task's, and it carries the ceiling. The workspace is
// one per TASK, not one per blueprint run, so "two driven at once means two
// agents in one git tree" spans every conversation the task has ever held — a
// second blueprint's step as much as a sibling step of the same one. The
// task's live conversation is its newest non-ended top-level row, and nothing
// else on it is drivable.
var blueprintDrivableSQL = `((r.blueprint_run_id IS NULL
	    OR (br.cancel_requested = false AND br.status <> 'cancelled'
	        AND r.blueprint_step_index = br.current_step_index))
	   AND (r.task_id IS NULL OR r.id = ` + taskLiveConversationSQL("r.org_id", "r.task_id") + `)
	   AND (r.task_id IS NULL OR NOT ` + taskMemoryPendingSQL("r.org_id", "r.task_id") + `))`

// taskLiveConversationSQL is the conversation that owns the task's tree: the
// newest row of liveTopLevelConversationSQL (conversation.go), which is where
// that base predicate and the two other questions asked of it are written
// down. It is a scalar subquery rather than a NOT EXISTS anti-join because
// the answer IS an id — the claim gate compares the candidate against it, and
// a reader that wants the row itself gets the same definition rather than a
// second spelling of it.
//
// The ordering decides only a task holding more than one un-ended row.
// Newest-wins is the reading the boundary column committed to; the id
// tiebreak makes it total.
func taskLiveConversationSQL(orgExpr, taskExpr string) string {
	return `(SELECT live.id FROM conversations live
		WHERE live.org_id = ` + orgExpr + ` AND live.task_id = ` + taskExpr + `
		  AND ` + liveTopLevelConversationSQL("live") + `
		ORDER BY live.started_at DESC, live.id DESC
		LIMIT 1)`
}

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

// handedBackOutcomesSQL is every claim outcome that records nothing about the
// conversation, so none of them ends a queue episode:
//
//   - 'requeued': an engagement failed before its agent ran (a workspace that
//     would not build, a runtime that would not launch), or a resume re-queued
//     a claim someone else still held. Counts toward the setup budget.
//   - 'requeued_credentials': the credentials wait timed out. The brain's
//     provisioner did not answer, which says nothing about the conversation,
//     so it counts toward neither budget.
//   - 'reaped': the engagement's lease lapsed with nobody driving it — a
//     takeover by another dispatcher, the minting executor's own release, or
//     a boot reset. Counts toward the loss budget.
//   - 'requeued_shutdown': the executor stopped cleanly and handed its idle
//     claims back. A deliberate stop is not a loss; it counts toward neither.
//
// Every other outcome is an engagement speaking for itself: it concluded, it
// failed, it parked, it was stopped.
const handedBackOutcomesSQL = `'requeued','requeued_credentials','reaped','requeued_shutdown'`

// episodeHandBacksSQL counts the current queue episode's hand-backs whose
// outcome is in outcomesSQL, against the conversation alias convAlias. It is
// the one place the episode's start is defined, so the budgets built on it
// (EpisodeSetupFailuresSQL, EpisodeLostEngagementsSQL, and the Attempts
// telemetry) cannot disagree about where an episode begins.
//
// An episode is the run of consecutive hand-backs at the tail of the
// conversation's claim history. It ends at the most recent claim that recorded
// an outcome of its own, because that is an engagement that got somewhere —
// and the budgets exist to stop retrying one that never does.
//
// Counting the conversation's LIFETIME claims instead is not a harsher budget
// but a wrong one. Every stop, resume and wake mints a claim, so a
// conversation picked up four times would reach its next claim already over
// budget, and the first transient hiccup it ever met would be misread as a
// deterministic crash.
//
// The in-flight claim is excluded by construction — it has no outcome yet,
// and the count requires one.
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
func episodeHandBacksSQL(convAlias, outcomesSQL string) string {
	return `(SELECT COUNT(*) FROM claims c2
	WHERE c2.conversation_id = ` + convAlias + `.id
	  AND c2.outcome IN (` + outcomesSQL + `)
	  AND NOT EXISTS (
	      SELECT 1 FROM claims c3
	      WHERE c3.conversation_id = c2.conversation_id
	        AND c3.outcome IS NOT NULL
	        AND c3.outcome NOT IN (` + handedBackOutcomesSQL + `)
	        AND c3.claimed_at >= COALESCE(c2.released_at, c2.claimed_at)))::int`
}

// EpisodeSetupFailuresSQL renders the setup budget's unit: how many of the
// current queue episode's engagements failed before their agent ran
// ('requeued'). Exported and alias-parameterized (convAlias names the
// conversation in the enclosing query) because the counting rule is a
// decision about what a setup failure IS, and a second copy of it would be a
// second answer to that.
func EpisodeSetupFailuresSQL(convAlias string) string {
	return episodeHandBacksSQL(convAlias, `'requeued'`)
}

// EpisodeLostEngagementsSQL renders the loss budget's unit: how many of the
// current queue episode's engagements were lost ('reaped') — taken over after
// their lease lapsed, released by their own executor, or released by a boot
// reset. A clean shutdown's hand-back is not in it, which is what keeps a
// deploy from spending the budget a crash-looping conversation is failed on.
func EpisodeLostEngagementsSQL(convAlias string) string {
	return episodeHandBacksSQL(convAlias, `'reaped'`)
}

// conversationQueueClaimReturning is the outer-SELECT projection of ClaimNextConversation. The
// claim identity (executor/claim id/claimed_at) and the episode counts ride
// the same statement: the id + claimed_at come from the freshly inserted
// claims row, the counts from the current queue episode (see
// episodeHandBacksSQL). The claim id is returned because the executor needs
// to name this engagement later — at teardown, once it has been released and
// can no longer be found by looking for the conversation's active claim.
var conversationQueueClaimReturning = `candidate.id::text, candidate.org_id::text, candidate.type, candidate.task_id, candidate.prompt_id,
	candidate.model, candidate.runtime, candidate.worktree_path, candidate.sdk_session_id,
	candidate.trigger_type, candidate.trigger_id, candidate.creator_user_id,
	candidate.team_id,
	candidate.blueprint_run_id, candidate.blueprint_step_index,
	minted.id::text AS claim_id, minted.claimed_at,
	1 + ` + episodeHandBacksSQL("candidate", handedBackOutcomesSQL) + ` AS attempts,
	` + EpisodeSetupFailuresSQL("candidate") + ` AS setup_failures,
	` + EpisodeLostEngagementsSQL("candidate") + ` AS lost_engagements`

func (s *conversationQueueStore) ClaimNextConversation(ctx context.Context, executorID string, bootEpoch int64, placement db.ClaimPlacement, lease time.Duration) (*domain.Conversation, error) {
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
	// The lease binds after whatever placement appends, so the placement
	// predicate below keeps naming $3/$4 literally.
	leaseArg := func() string { args = append(args, lease.Seconds()); return "$" + strconv.Itoa(len(args)) }
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
			UPDATE conversations SET status = NULL, parked_at = NULL, park_reason = NULL,
			                         stop_requested_at = NULL, stop_requested_by = NULL, stop_requested_reason = NULL
			FROM candidate
			WHERE conversations.id = candidate.id AND conversations.status IS NOT NULL
			RETURNING conversations.id
		),
		minted AS (
			-- The lease is stamped here, in the statement that mints the row:
			-- a live claim never exists without one, which is the invariant
			-- claims_live_has_lease states and every fenced write presents.
			INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch, claimed_at, lease_expires_at)
			SELECT candidate.org_id, candidate.id, $1, $2::bigint, now(), statement_timestamp() + make_interval(secs => ` + leaseArg() + `) FROM candidate
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

func (s *conversationQueueStore) RenewClaimLeaseSystem(ctx context.Context, orgID, conversationID, claimID string, lease time.Duration, activity db.ClaimActivity) (db.ClaimRenewal, error) {
	// A malformed id is a caller wiring fault, and the honest answer to it is
	// the one the fence gives for every other way of not being the owner:
	// Postgres would otherwise reject the bind (22P02) and the loop would read
	// a driver error as a transient failure worth retrying forever.
	if claimID == "" || !isValidUUID(claimID) || !isValidUUID(conversationID) {
		return db.ClaimRenewal{}, fmt.Errorf("%w: claim %q on conversation %q", db.ErrClaimReleased, claimID, conversationID)
	}
	// statement_timestamp() rather than now(): the guard has to read fresh
	// database time, not the instant this statement's transaction began, or a
	// long transaction could renew a lease that lapsed while it was open. It
	// is also the reading that is fixed for one statement, so the expiry
	// written here is measured from the very instant the guard tested.
	//
	// The guard's expiry term is what makes a late renewal terminal: an
	// already-lapsed lease matches nothing, and the caller gets
	// ErrClaimLeaseExpired, which is the ErrClaimReleased a released claim
	// gives to every caller that asks only that. Authority does not come back
	// through the renewal; ReacquireClaimLeaseSystem is a separate verb with
	// its own proof obligation.
	//
	// last_activity_at and last_checkpoint_at are measured back from the same
	// statement_timestamp() the lease is measured forward from, so the ages
	// the executor read on its monotonic clock land as database timestamps
	// with no executor wall clock in them. A NULL checkpoint age stamps NULL.
	var checkpointAge any
	if activity.CheckpointAge != nil {
		checkpointAge = activity.CheckpointAge.Seconds()
	}
	var out db.ClaimRenewal
	err := s.conn.QueryRowContext(ctx, `
		UPDATE claims
		SET lease_expires_at = statement_timestamp() + make_interval(secs => $1),
		    last_activity_at = statement_timestamp() - make_interval(secs => $5),
		    current_op = NULLIF($6, ''),
		    last_checkpoint_at = CASE WHEN $7::float8 IS NULL THEN NULL
		                              ELSE statement_timestamp() - make_interval(secs => $7::float8) END
		WHERE id = $2 AND org_id = $3 AND conversation_id = $4
		  AND released_at IS NULL AND lease_expires_at > statement_timestamp()
		RETURNING lease_expires_at,
		          (SELECT r.stop_requested_at IS NOT NULL FROM conversations r WHERE r.id = claims.conversation_id),
		          (SELECT COALESCE(r.stop_requested_by, '') FROM conversations r WHERE r.id = claims.conversation_id)
	`, lease.Seconds(), claimID, orgID, conversationID, activity.Idle.Seconds(), activity.Op, checkpointAge).Scan(&out.ExpiresAt, &out.StopRequested, &out.StopRequestedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ClaimRenewal{}, renewalRefusal(ctx, s.conn, orgID, conversationID, claimID)
	}
	if err != nil {
		return db.ClaimRenewal{}, wrapAdminPoolPermErr(err, "conversation_queue.RenewClaimLeaseSystem")
	}
	return out, nil
}

// renewalRefusal classifies a renewal its guard refused, with one follow-up
// read of the claim. It runs only on the refusal path, so the renewal itself
// stays the one guarded statement it has to be.
//
// A claim the follow-up finds live was restored between the two statements,
// and the only write that takes an unreleased claim from lapsed to live is
// ReacquireClaimLeaseSystem: the refusal this renewal met was still the lapse.
// A follow-up that fails answers the unclassified refusal, which is what every
// caller treated a refusal as before the lapse had its own name.
func renewalRefusal(ctx context.Context, q queryer, orgID, conversationID, claimID string) error {
	err := claimRefusal(ctx, q, orgID, conversationID, claimID, "")
	switch {
	case err == nil:
		return fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimLeaseExpired, claimID, conversationID)
	case errors.Is(err, db.ErrClaimReleased):
		return err
	}
	return fmt.Errorf("%w: claim %s on conversation %s (classifying the refusal failed: %v)", db.ErrClaimReleased, claimID, conversationID, err)
}

// ReacquireClaimLeaseSystem is the renewal's statement with the expiry term
// dropped from the guard and the owner terms added. The row lock the UPDATE
// takes is what serializes it against the takeover, whose CTE locks the
// expired rows it releases: whichever commits first decides, and the other
// re-reads a row that no longer matches it. The activity columns are left to
// the next renewal, which the caller makes within one cadence.
func (s *conversationQueueStore) ReacquireClaimLeaseSystem(ctx context.Context, orgID, conversationID, claimID, executorID string, bootEpoch int64, lease time.Duration) (db.ClaimRenewal, error) {
	if claimID == "" || !isValidUUID(claimID) || !isValidUUID(conversationID) {
		return db.ClaimRenewal{}, fmt.Errorf("%w: claim %q on conversation %q", db.ErrClaimReleased, claimID, conversationID)
	}
	var out db.ClaimRenewal
	err := s.conn.QueryRowContext(ctx, `
		UPDATE claims
		SET lease_expires_at = statement_timestamp() + make_interval(secs => $1)
		WHERE id = $2 AND org_id = $3 AND conversation_id = $4
		  AND executor_id = $5 AND boot_epoch = $6
		  AND released_at IS NULL
		RETURNING lease_expires_at,
		          (SELECT r.stop_requested_at IS NOT NULL FROM conversations r WHERE r.id = claims.conversation_id),
		          (SELECT COALESCE(r.stop_requested_by, '') FROM conversations r WHERE r.id = claims.conversation_id)
	`, lease.Seconds(), claimID, orgID, conversationID, executorID, bootEpoch).Scan(&out.ExpiresAt, &out.StopRequested, &out.StopRequestedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ClaimRenewal{}, fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimReleased, claimID, conversationID)
	}
	if err != nil {
		return db.ClaimRenewal{}, wrapAdminPoolPermErr(err, "conversation_queue.ReacquireClaimLeaseSystem")
	}
	return out, nil
}

// SettleUnclaimedStopsSystem is two statements in one transaction, and the
// split is the lock order. The first locks every run the pass may cancel; the
// second settles, and admits a conversation whose settlement would write its
// run only when that run is one the first statement locked. A run's terminal
// write (markBlueprintRunStatus) locks the run and then parks its children, so
// a settlement that locked a child first and its run second could wait on it
// while it waited on the child. Both now take the run first, and a run another
// writer holds is skipped (SKIP LOCKED) and settled on the next pass.
//
// Inside the second statement the victims' FOR UPDATE serializes against
// ClaimNextConversation's FOR UPDATE OF r: whichever commits second re-reads
// its own predicate and matches nothing.
//
// The run cancel keys off the conversation's status as the victims read it
// (`was`), because a conversation that concluded before the settlement reached
// it had its terminal handled by the holder, and this pass only clears the
// stale intent.
func (s *conversationQueueStore) SettleUnclaimedStopsSystem(ctx context.Context) ([]db.SettledStop, error) {
	return s.settleUnclaimedStops(ctx, "")
}

func (s *conversationQueueStore) SettleUnclaimedStopsForTaskSystem(ctx context.Context, orgID, taskID string) ([]db.SettledStop, error) {
	if !isValidUUID(orgID) || !isValidUUID(taskID) {
		return nil, nil
	}
	return s.settleUnclaimedStops(ctx, `AND r.org_id = $1 AND r.task_id = $2`, orgID, taskID)
}

// settleNoLiveClaimSQL is the settlement's first condition, against the
// conversation alias r: nothing holds the conversation. released_at alone —
// see SettleUnclaimedStopsSystem's contract.
const settleNoLiveClaimSQL = `NOT EXISTS (SELECT 1 FROM claims cl WHERE cl.conversation_id = r.id AND cl.released_at IS NULL)`

// settleUnclaimedStops runs the settlement over the conversations scope
// narrows to; scope is an AND-clause over the victims' alias r, binding args.
func (s *conversationQueueStore) settleUnclaimedStops(ctx context.Context, scope string, args ...any) ([]db.SettledStop, error) {
	lockedArg := "$" + strconv.Itoa(len(args)+1)
	var out []db.SettledStop
	err := inTx(ctx, s.conn, func(q queryer) error {
		// 1. The runs a settlement may cancel: running, cancel-requested, and
		// holding a non-terminal conversation nobody drives. Every victim that
		// cancels a run is one of those, whether it carries an intent or not.
		rows, err := q.QueryContext(ctx, `
			SELECT br.id::text FROM blueprint_runs br
			WHERE br.status = 'running' AND br.cancel_requested = true
			  AND EXISTS (
			      SELECT 1 FROM conversations r
			      WHERE r.blueprint_run_id = br.id
			        AND (r.status IS NULL OR r.status = 'open')
			        AND `+settleNoLiveClaimSQL+`
			        `+scope+`
			  )
			ORDER BY br.id
			FOR UPDATE SKIP LOCKED
		`, args...)
		if err != nil {
			return err
		}
		var locked []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			locked = append(locked, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}

		// 2. The settlement. The first victim arm is the intent; the second is
		// a non-terminal step under a cancel-requested run that no intent ever
		// reached. The last clause is the lock order: a victim whose
		// settlement writes its run is admitted only when that run is locked.
		rows, err = q.QueryContext(ctx, `
			WITH victims AS (
				SELECT r.id, r.org_id, r.blueprint_run_id, r.blueprint_step_index, r.status,
				       r.stop_requested_at, r.stop_requested_by, r.stop_requested_reason
				FROM conversations r
				LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
				WHERE `+settleNoLiveClaimSQL+`
				  AND (r.stop_requested_at IS NOT NULL
				       OR ((r.status IS NULL OR r.status = 'open')
				           AND br.status = 'running' AND br.cancel_requested = true))
				  AND (br.id IS NULL
				       OR NOT (br.status = 'running' AND br.cancel_requested = true)
				       OR r.status IN (`+conversationTerminalStatusesSQL+`)
				       OR br.id = ANY(`+lockedArg+`::uuid[]))
				  `+scope+`
				ORDER BY r.id
				FOR UPDATE OF r SKIP LOCKED
			),
			settled AS (
				UPDATE conversations c
				SET status = CASE WHEN v.status IN (`+conversationTerminalStatusesSQL+`) THEN v.status ELSE 'open' END,
				    parked_at = CASE WHEN v.status IN (`+conversationTerminalStatusesSQL+`) THEN c.parked_at ELSE COALESCE(c.parked_at, now()) END,
				    park_reason = CASE WHEN v.status IN (`+conversationTerminalStatusesSQL+`) THEN c.park_reason
				                       WHEN v.stop_requested_at IS NULL THEN 'blueprint_cancelled'
				                       WHEN v.stop_requested_reason IS NOT NULL THEN v.stop_requested_reason
				                       WHEN v.stop_requested_by IS NULL THEN 'system_cancelled'
				                       ELSE 'user_cancelled' END,
				    stop_requested_at = NULL,
				    stop_requested_by = NULL,
				    stop_requested_reason = NULL
				FROM victims v WHERE c.id = v.id
				RETURNING c.id, c.org_id, c.blueprint_run_id, c.blueprint_step_index, v.status AS was,
				          v.stop_requested_at AS intent_at, v.stop_requested_by, v.stop_requested_reason
			),
			cancelled AS (
				UPDATE blueprint_runs br
				SET status = 'cancelled', completed_at = now(),
				    abort_reason = CASE WHEN s.intent_at IS NULL THEN 'cancelled'
				                        WHEN s.stop_requested_reason IS NOT NULL THEN s.stop_requested_reason
				                        WHEN s.stop_requested_by IS NULL THEN 'system_cancelled'
				                        ELSE 'user_cancelled' END,
				    aborted_at_step = s.blueprint_step_index
				FROM settled s
				WHERE br.id = s.blueprint_run_id AND br.status = 'running' AND br.cancel_requested = true
				  AND br.id = ANY(`+lockedArg+`::uuid[])
				  AND s.was IS DISTINCT FROM 'completed' AND s.was IS DISTINCT FROM 'failed'
				RETURNING br.id
			)
			SELECT s.org_id::text, s.id::text,
			       CASE WHEN row_number() OVER (PARTITION BY s.blueprint_run_id ORDER BY s.id) = 1
			            THEN COALESCE(c.id::text, '') ELSE '' END,
			       s.blueprint_step_index
			FROM settled s LEFT JOIN cancelled c ON c.id = s.blueprint_run_id
		`, append(append([]any{}, args...), pgUUIDArray(locked))...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st db.SettledStop
			var step sql.NullInt64
			if err := rows.Scan(&st.OrgID, &st.ConversationID, &st.BlueprintRunID, &step); err != nil {
				return err
			}
			if step.Valid {
				v := int(step.Int64)
				st.StepIndex = &v
			}
			out = append(out, st)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "conversation_queue.SettleUnclaimedStopsSystem")
	}
	return out, nil
}

func (s *conversationQueueStore) ExpiredClaimsSystem(ctx context.Context) (int, time.Duration, error) {
	// COALESCE over the aggregate rather than a second statement: with no
	// matching rows min() is NULL and the subtraction with it, so the whole
	// expression collapses to the zero the empty case wants.
	var count int
	var oldestSeconds float64
	err := s.conn.QueryRowContext(ctx, `
		SELECT count(*),
		       COALESCE(EXTRACT(EPOCH FROM (statement_timestamp() - min(lease_expires_at))), 0)
		FROM claims
		WHERE released_at IS NULL AND lease_expires_at <= statement_timestamp()
	`).Scan(&count, &oldestSeconds)
	if err != nil {
		return 0, 0, wrapAdminPoolPermErr(err, "conversation_queue.ExpiredClaimsSystem")
	}
	return count, time.Duration(oldestSeconds * float64(time.Second)), nil
}

func (s *conversationQueueStore) OldestIdleClaimSystem(ctx context.Context) (time.Duration, error) {
	// max() over no rows is NULL, and so is the subtraction with it; the
	// COALESCE collapses that to the zero the empty case wants. A claim that
	// has not renewed yet carries no stamp and is not counted.
	var seconds float64
	err := s.conn.QueryRowContext(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM (statement_timestamp() - min(last_activity_at))), 0)
		FROM claims
		WHERE released_at IS NULL AND lease_expires_at > statement_timestamp()
		  AND last_activity_at IS NOT NULL
	`).Scan(&seconds)
	if err != nil {
		return 0, wrapAdminPoolPermErr(err, "conversation_queue.OldestIdleClaimSystem")
	}
	if seconds < 0 {
		seconds = 0
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (s *conversationQueueStore) OldestCheckpointAgeSystem(ctx context.Context) (time.Duration, error) {
	// The same shape as OldestIdleClaimSystem: a claim whose engagement does
	// not checkpoint carries no stamp and is not counted.
	var seconds float64
	err := s.conn.QueryRowContext(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM (statement_timestamp() - min(last_checkpoint_at))), 0)
		FROM claims
		WHERE released_at IS NULL AND lease_expires_at > statement_timestamp()
		  AND last_checkpoint_at IS NOT NULL
	`).Scan(&seconds)
	if err != nil {
		return 0, wrapAdminPoolPermErr(err, "conversation_queue.OldestCheckpointAgeSystem")
	}
	if seconds < 0 {
		seconds = 0
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (s *conversationQueueStore) ExpiredClaimsOfExecutorSystem(ctx context.Context, executorID string, bootEpoch int64) ([]db.ClaimRef, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id::text, org_id::text, conversation_id::text
		FROM claims
		WHERE executor_id = $1 AND boot_epoch = $2
		  AND released_at IS NULL AND lease_expires_at <= statement_timestamp()
		ORDER BY lease_expires_at, id
	`, executorID, bootEpoch)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "conversation_queue.ExpiredClaimsOfExecutorSystem")
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
	if !isValidUUID(orgID) || !isValidUUID(conversationID) || !isValidUUID(claimID) {
		return false, nil
	}
	res, err := s.conn.ExecContext(ctx, `
		UPDATE claims SET released_at = now(), outcome = 'reaped'
		WHERE id = $1 AND org_id = $2 AND conversation_id = $3
		  AND released_at IS NULL AND lease_expires_at <= statement_timestamp()
	`, claimID, orgID, conversationID)
	if err != nil {
		return false, wrapAdminPoolPermErr(err, "conversation_queue.ReleaseExpiredClaimSystem")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// TakeOverExpiredClaimsSystem locks claim rows only, and with SKIP LOCKED, so
// a claim under its holder's fence read (FOR SHARE) is skipped rather than
// waited on; the guard is re-evaluated on the locked row, so a renewal that
// committed first leaves it live. The preferred-executor clear that follows
// skips locked conversations too (see clearPreferredExecutor) — a run's
// terminal parking its children is a writer that locks a conversation before
// its claim.
func (s *conversationQueueStore) TakeOverExpiredClaimsSystem(ctx context.Context, executorID string, bootEpoch int64, limit int) ([]db.ClaimRef, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []db.ClaimRef
	err := inTx(ctx, s.conn, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			WITH expired AS (
				SELECT id FROM claims
				WHERE released_at IS NULL
				  AND lease_expires_at <= statement_timestamp()
				  AND NOT (executor_id = $1 AND boot_epoch = $2)
				ORDER BY lease_expires_at, id
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			)
			UPDATE claims c SET released_at = now(), outcome = 'reaped'
			FROM expired e WHERE c.id = e.id
			RETURNING c.id::text, c.org_id::text, c.conversation_id::text
		`, executorID, bootEpoch, limit)
		if err != nil {
			return err
		}
		ids := make([]string, 0, limit)
		for rows.Next() {
			var c db.ClaimRef
			if err := rows.Scan(&c.ClaimID, &c.OrgID, &c.ConversationID); err != nil {
				rows.Close()
				return err
			}
			out = append(out, c)
			ids = append(ids, c.ConversationID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return clearPreferredExecutor(ctx, q, ids)
	})
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "conversation_queue.TakeOverExpiredClaimsSystem")
	}
	return out, nil
}

func (s *conversationQueueStore) LiveClaimsOfExecutorSystem(ctx context.Context, executorID string, bootEpoch int64) ([]db.ClaimRef, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id::text, org_id::text, conversation_id::text
		FROM claims
		WHERE executor_id = $1 AND boot_epoch = $2 AND released_at IS NULL
		ORDER BY claimed_at, id
	`, executorID, bootEpoch)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "conversation_queue.LiveClaimsOfExecutorSystem")
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
	ids := make([]string, 0, len(conversationIDs))
	for _, id := range conversationIDs {
		if isValidUUID(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	var released []string
	err := inTx(ctx, s.conn, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			UPDATE claims SET released_at = now(), outcome = 'requeued_shutdown'
			WHERE executor_id = $1 AND boot_epoch = $2 AND released_at IS NULL
			  AND conversation_id = ANY($3::uuid[])
			RETURNING conversation_id::text
		`, executorID, bootEpoch, pgUUIDArray(ids))
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			released = append(released, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return clearPreferredExecutor(ctx, q, released)
	})
	if err != nil {
		return 0, wrapAdminPoolPermErr(err, "conversation_queue.ReleaseOwnClaimsOnShutdownSystem")
	}
	return len(released), nil
}

// ReleaseClaimOnShutdownSystem releases the claim and then clears the stamp,
// in that order, the order RequeueConversation takes.
func (s *conversationQueueStore) ReleaseClaimOnShutdownSystem(ctx context.Context, orgID, conversationID, claimID string) error {
	if !isValidUUID(orgID) || !isValidUUID(conversationID) || !isValidUUID(claimID) {
		return fmt.Errorf("%w: claim %q on conversation %q", db.ErrClaimReleased, claimID, conversationID)
	}
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET released_at = now(), outcome = 'requeued_shutdown'
			WHERE id = $1 AND org_id = $2 AND conversation_id = $3 AND released_at IS NULL
		`, claimID, orgID, conversationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: claim %s on conversation %s", db.ErrClaimReleased, claimID, conversationID)
		}
		return clearPreferredExecutor(ctx, q, []string{conversationID})
	})
	if err != nil && !errors.Is(err, db.ErrClaimReleased) {
		return wrapAdminPoolPermErr(err, "conversation_queue.ReleaseClaimOnShutdownSystem")
	}
	return err
}

// clearPreferredExecutor drops the placement stamp on conversations whose
// claim a transaction just released. Each row's lock is taken with SKIP
// LOCKED: the claim is already locked by the caller, and a writer that locks
// a conversation before its claim would otherwise wait on this transaction
// while this one waited on it. The stamp is advisory, so a row somebody else
// holds keeps it.
func clearPreferredExecutor(ctx context.Context, q queryer, conversationIDs []string) error {
	if len(conversationIDs) == 0 {
		return nil
	}
	_, err := q.ExecContext(ctx, `
		UPDATE conversations SET preferred_executor_id = NULL
		WHERE id IN (
			SELECT id FROM conversations
			WHERE id = ANY($1::uuid[]) AND preferred_executor_id IS NOT NULL
			FOR UPDATE SKIP LOCKED
		)
	`, pgUUIDArray(conversationIDs))
	return err
}

// StrandedBlueprintRunsSystem measures the grace on database time, against
// the conversation's completion stamp and its claims' releases alike.
func (s *conversationQueueStore) StrandedBlueprintRunsSystem(ctx context.Context, grace time.Duration, limit int) ([]db.StrandedRun, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT br.org_id::text, br.id::text, r.id::text
		FROM blueprint_runs br
		JOIN conversations r ON r.blueprint_run_id = br.id AND r.blueprint_step_index = br.current_step_index
		WHERE br.status = 'running'
		  AND r.status IN (`+conversationTerminalStatusesSQL+`)
		  AND COALESCE(r.completed_at, r.started_at) <= now() - make_interval(secs => $1)
		  AND NOT EXISTS (
		      SELECT 1 FROM claims cl
		      WHERE cl.conversation_id = r.id
		        AND (cl.released_at IS NULL OR cl.released_at > now() - make_interval(secs => $1))
		  )
		ORDER BY br.started_at, br.id
		LIMIT $2
	`, grace.Seconds(), limit)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "conversation_queue.StrandedBlueprintRunsSystem")
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
func (s *conversationQueueStore) RequeueConversation(ctx context.Context, orgID, conversationID string, outcome db.RequeueOutcome, lastErr string) (*domain.Conversation, error) {
	if !outcome.Valid() {
		return nil, fmt.Errorf("%w: %q", db.ErrInvalidRequeueOutcome, outcome)
	}
	var result *domain.Conversation
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET released_at = now(), outcome = $3
			WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
			  AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = claims.conversation_id AND c.status IS NULL)
		`, orgID, conversationID, string(outcome))
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
	// Every live claim this executor minted in an earlier boot, whatever its
	// conversation's state: the flock proves the process that minted it is
	// gone, and a clean shutdown releases its own, so each one is a loss.
	// Scoped by boot epoch rather than by lease — the epoch is the stronger
	// proof here, and a claim of the previous boot can still carry a lease
	// its dead holder renewed seconds ago.
	//
	// Claims first and the stamp second, the takeover's order: a run's
	// terminal locks a conversation and then its claim, so the clear takes
	// each conversation's lock with SKIP LOCKED (clearPreferredExecutor).
	var released []string
	err := inTx(ctx, s.conn, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			UPDATE claims SET released_at = now(), outcome = 'reaped'
			WHERE released_at IS NULL AND executor_id = $1 AND boot_epoch < $2
			RETURNING conversation_id::text
		`, executorID, bootEpoch)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			released = append(released, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return clearPreferredExecutor(ctx, q, released)
	})
	if err != nil {
		return 0, err
	}
	return len(released), nil
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

func (s *conversationQueueStore) ReconcileOrphanedConversations(ctx context.Context) (int, db.OrphanedStepCheck, error) {
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
	var check db.OrphanedStepCheck
	err := inTx(ctx, s.conn, func(q queryer) error {
		var parked int
		if err := q.QueryRowContext(ctx, `
			WITH parked AS (
				UPDATE conversations
				SET status = 'open',
				    parked_at = COALESCE(parked_at, now()),
				    park_reason = COALESCE(park_reason, 'blueprint_terminal'),
				    result_summary = COALESCE(NULLIF(result_summary, ''), $1),
				    stop_requested_at = NULL, stop_requested_by = NULL, stop_requested_reason = NULL
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

		// Orphaned-step CHECKER — the shape the park above cannot see, one
		// level up: a live parent with no child at the step it is pointing
		// at. It repairs nothing, and its count stays out of the healed
		// total, because counting is not healing.
		var err error
		check, err = countBlueprintRunsMissingCurrentStep(ctx, q)
		if err != nil {
			return err
		}
		// Claim-desync CHECKER: a terminal conversation still holding an
		// unreleased claim. Counted, never repaired, for the same reason.
		desyncs, err := countClaimDesyncs(ctx, q)
		if err != nil {
			return err
		}
		check.ClaimDesyncs, check.ClaimDesyncSample = desyncs.ClaimDesyncs, desyncs.ClaimDesyncSample
		total = parked
		return nil
	})
	if err != nil {
		return 0, db.OrphanedStepCheck{}, err
	}
	return total, check, nil
}

// countClaimDesyncs counts terminal conversations still holding an unreleased
// claim, and samples the oldest few. It writes nothing.
//
// No writer can produce the shape: the holder's terminal and park writes and
// the dispatcher's settlement each release the claim on the same transaction
// as the status flip, and a request path writes no status at all. A nonzero
// count is therefore a bug report, and a repair here would hide the writer
// that caused it. (A lost engagement's claim on such a row is still released
// once its lease lapses — by the takeover, which releases every expired claim
// — but that is recovery from the bug, not a reason to stop reporting it.)
func countClaimDesyncs(ctx context.Context, q queryer) (db.OrphanedStepCheck, error) {
	var out db.OrphanedStepCheck
	rows, err := q.QueryContext(ctx, `
		SELECT c.id::text, count(*) OVER ()
		FROM conversations c
		WHERE c.status IN (`+conversationTerminalStatusesSQL+`)
		  AND EXISTS (SELECT 1 FROM claims cl WHERE cl.conversation_id = c.id AND cl.released_at IS NULL)
		ORDER BY c.started_at, c.id
		LIMIT $1
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
// few. It writes nothing.
//
// A firing commits its run and its first step in one transaction and an
// advance commits its pointer and the step it names in another, so no reader
// can observe one without the other and there is no window for this shape to
// appear in — which is why there is no grace here, and why a repair would be
// the wrong answer: it would quietly absorb a broken invariant that should be
// looked at instead. What can still turn up is a survivor from before that was
// true, in an installed local database.
//
// `count(*) OVER ()` is evaluated before LIMIT, so one statement gives both
// the full count and the bounded sample without the two disagreeing about
// which rows they describe.
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
		LIMIT $1
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
		       COUNT(*) FILTER (WHERE c2.released_at IS NULL AND c2.lease_expires_at > statement_timestamp()) > 0 AS has_active,
		       SUM(c2.duration_ms)::bigint AS duration_ms
		FROM claims c2
		WHERE c2.conversation_id = r.id
	) cl ON true`

// conversationTimingStatusSQL is the timing projection's status: the stored outcome
// when there is one, else the derived in-flight state. The percentile read
// buckets by failure kind, so a mid-flight row must still name itself
// rather than scan as NULL. A claim whose lease has lapsed is not running,
// for the same reason the display ladder says so (liveClaimExistsSQL).
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
var executorClaimSelectCols = `
	c.id::text, c.org_id::text, c.conversation_id::text,
	c.claimed_at, c.released_at, c.lease_expires_at, COALESCE(c.outcome, ''),
	c.peak_mem_mb, c.cpu_usec,
	COALESCE(v.status, CASE WHEN ` + claimLeaseLiveSQL("c") + ` THEN 'running' ELSE 'queued' END, ''),
	COALESCE(v.failure_kind, ''),
	c.last_activity_at, COALESCE(c.current_op, ''), c.last_checkpoint_at`

// claimLeaseLiveSQL is one claims row's own liveness, for the alias the
// caller gave it: unreleased AND its lease still in the future. An expired
// claim renders 'queued' because nothing is driving its conversation — the
// executor that held it is gone, and what the row records is an engagement
// waiting to be taken over.
func claimLeaseLiveSQL(alias string) string {
	return alias + ".released_at IS NULL AND " + alias + ".lease_expires_at > statement_timestamp()"
}

// executorClaimCols is the shared projection behind both operator claim reads,
// so the per-executor list and the single-claim lookup can never drift into
// disagreeing about the same row. LEFT JOIN on the conversation: the claim is
// the subject here, and a claim whose conversation is gone must still report
// its measured cost rather than vanishing from the box's occupancy.
var executorClaimCols = `
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
	var releasedAt, leaseExpiresAt, lastActivityAt, lastCheckpointAt sql.NullTime
	var peakMem, cpuUsec sql.NullInt64
	if err := row.Scan(
		&c.ID, &c.OrgID, &c.ConversationID,
		&c.ClaimedAt, &releasedAt, &leaseExpiresAt, &c.Outcome,
		&peakMem, &cpuUsec, &c.Status, &c.FailureKind,
		&lastActivityAt, &c.CurrentOp, &lastCheckpointAt,
	); err != nil {
		return domain.ExecutorClaim{}, err
	}
	if lastActivityAt.Valid {
		v := lastActivityAt.Time
		c.LastActivityAt = &v
	}
	if lastCheckpointAt.Valid {
		v := lastCheckpointAt.Time
		c.LastCheckpointAt = &v
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
// episode counts. (nil, nil) on sql.ErrNoRows so callers treat "nothing
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
		&r.ClaimID, &claimedAt, &r.Attempts, &r.SetupFailures, &r.LostEngagements)
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
