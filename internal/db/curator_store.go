package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrCuratorInFlight is returned by CuratorStore.ArchiveLiveConversation when
// the conversation has an active claim (a live turn) at the moment of the
// reset call. The HTTP handler maps this to 409 so the client can prompt the
// user to cancel first.
var ErrCuratorInFlight = errors.New("curator request in flight")

// CuratorTurnStart is what BeginTurn returns: everything the dispatch needs
// to run one turn, read/written inside a single transaction so the
// pending-changes diff is computed against project state consistent with the
// injection rows being consumed (a PATCH landing between two independent
// reads could otherwise be claimed here while the note — built from an older
// project read — suppressed its diff, silently losing the delta).
type CuratorTurnStart struct {
	// UserInput is the turn's user message content.
	UserInput string
	// SDKSessionID is the conversation's --resume handle ("" = fresh session).
	SDKSessionID string
	// Project is the owning project's state, snapshotted in the same tx.
	Project *domain.Project
	// Consumed is every 'injection:context' row delivered alongside the turn
	// (ids remembered for the failure/cancel revert), oldest first.
	Consumed []domain.CuratorContextChange
}

// CuratorInFlightTurn is the cancel endpoint's lookup result: the requesting
// user's live turn on a project, either running (the conversation's active
// claim) or queued (an undelivered user message that never entered context).
type CuratorInFlightTurn struct {
	ConversationID string
	// MessageID is the turn's user message id — the request id on the wire.
	MessageID int64
	// Running is true when an active claim exists (cancel = release it);
	// false for a queued turn (cancel = delete the undelivered row).
	Running bool
}

// CuratorStore owns the curator's view of the shared conversations /
// messages / claims tables: one conversation per (project, creator_user_id)
// with type='curator' and visibility='private', a queued turn as an
// UNDELIVERED user messages row, and one claims row per executed turn.
//
// Pool split, load-bearing in Postgres:
//
//   - App-pool methods run claims-bound (WithTx / SyntheticClaimsWithTx) —
//     the conversations/messages RLS private-visibility arm scopes every row
//     to the requesting creator. tf_app holds SELECT,INSERT,DELETE,UPDATE on
//     messages/conversations but SELECT ONLY on claims.
//   - Claim WRITES (mint / release / cred_pubkey stamp) therefore always
//     route through the admin-pool `...System` methods, mirroring how the
//     delegation side's claim writes landed — there is no app-pool variant.
//     The org_id stays bound in every WHERE clause as defense in depth.
//
// SQLite collapses both pools onto its single connection and asserts the
// local sentinel org on the org-scoped methods.
type CuratorStore interface {
	// --- Send path (app pool, claims-bound) ---

	// GetOrCreateConversation returns the creator's live curator conversation
	// for the project, minting one when none exists: type='curator',
	// visibility='private', origin='curator', trigger_type='manual', status
	// NULL, team_id snapshotted from the project row (point-in-time, like
	// conversations.team_id — curator spend attributes to the project's team via the
	// llm_spend view).
	GetOrCreateConversation(ctx context.Context, orgID, projectID, creatorUserID string) (*domain.Conversation, error)

	// GetLiveConversation returns the creator's live (archived_at IS NULL)
	// curator conversation for the project, or nil when none. In Postgres the
	// private-visibility RLS arm already scopes to the caller; the explicit
	// creatorUserID filter is defense in depth and SQLite parity.
	GetLiveConversation(ctx context.Context, orgID, projectID, creatorUserID string) (*domain.Conversation, error)

	// EnqueueUserMessage inserts the turn's user message UNDELIVERED
	// (delivered=false, role='user', blank subtype, user_id=userID) and
	// returns its id — the turn id on the wire (rendered decimal). The
	// undelivered row IS the durable queued turn; delivery happens at
	// BeginTurn.
	EnqueueUserMessage(ctx context.Context, orgID, conversationID, userID, content string) (int64, error)

	// --- History (app pool, claims-bound) ---

	// ListConversationMessages returns the conversation's messages rows in
	// assembly order (COALESCE(seq, id)), with UserID / ClaimID / Delivered
	// populated so the handler can synthesize the turn wire shape (a plain
	// user row starts a turn; its status derives from Delivered and the
	// claim its following rows carry, falling back to the claim stamped on
	// the user row itself when nothing streamed). Withdrawn-pending rows
	// (delivered=false AND window_state='inactive') are excluded — withdrawn
	// means "never happened".
	ListConversationMessages(ctx context.Context, orgID, conversationID string) ([]domain.Message, error)

	// ListClaims returns the conversation's claims oldest-first. App pool —
	// tf_app has SELECT on claims and the RLS policy composes through the
	// conversation.
	ListClaims(ctx context.Context, orgID, conversationID string) ([]domain.Claim, error)

	// --- In-flight lookup + cancel (app pool, claims-bound) ---

	// InFlightTurn returns the creator's live turn on the project — running
	// (active claim; MessageID = the latest delivered plain user row) wins
	// over queued (the oldest undelivered plain user row) — or nil when
	// neither exists.
	InFlightTurn(ctx context.Context, orgID, projectID, creatorUserID string) (*CuratorInFlightTurn, error)

	// DeleteQueuedTurn removes a queued turn's undelivered user message —
	// the cancel of a turn that never entered context. Returns false when
	// the row is gone or already delivered (the dispatch won the race).
	DeleteQueuedTurn(ctx context.Context, orgID, conversationID string, messageID int64) (bool, error)

	// ArchiveLiveConversation is the reset: stamps archived_at on the
	// creator's live conversation and deletes its undelivered rows (queued
	// turns and pending injections never entered context). Returns the
	// archived conversation's id, or "" when there was no live conversation
	// to reset (a no-op) — the handler broadcasts the reset only on a real
	// archive and carries the id so a client that already began a fresh
	// conversation ignores the stale event. Returns ErrCuratorInFlight when
	// an active claim exists — a live subprocess must be cancelled before its
	// transcript is retired. The next send mints a fresh conversation, which
	// is what makes reset start a fresh SDK session.
	ArchiveLiveConversation(ctx context.Context, orgID, projectID, creatorUserID string) (string, error)

	// --- Dispatch (session goroutine) ---

	// BeginTurn delivers the turn in one transaction: flips the turn's user
	// message delivered=true (sql.ErrNoRows when the row is gone or already
	// delivered — a queued-cancel or duplicate dispatch raced ahead) and
	// stamps it with the conversation's active claim — the explicit
	// turn↔claim link. The stamp rides the tx, so a failed BeginTurn leaves
	// no stamp behind and a retried delivery can't accumulate stale ones;
	// its flip side is that every claim that DID deliver a turn owns at
	// least the user message row, which is what lets
	// FailedTurnAttemptsSystem recognize a message-less failed claim as a
	// failed pickup. Also consumes every undelivered 'injection:context' row
	// (delivered=true, ids remembered in Consumed for the revert), and
	// snapshots the project row + the conversation's sdk_session_id in the
	// same tx.
	BeginTurn(ctx context.Context, orgID, projectID, conversationID string, messageID int64) (*CuratorTurnStart, error)

	// FailedTurnAttemptsSystem counts the turn's prior failed pickups —
	// released claims minted for exactly this message (claims.message_id —
	// no time window, so two queued turns on one conversation never count
	// each other's failures), outcome='failed', with no messages attached
	// (a claim that reached BeginTurn successfully always owns at least the
	// turn's user row, which keeps post-delivery run crashes out of the
	// cap) — and returns the most recent one's error. The dispatch's
	// dead-letter gate: a turn whose count reaches the cap is poisoned input
	// (BeginTurn itself keeps failing), and feeding it again just leaks
	// another zero-message failed claim. Admin pool.
	FailedTurnAttemptsSystem(ctx context.Context, orgID, conversationID string, messageID int64) (count int, lastError string, err error)

	// DeadLetterTurnSystem terminally retires a poisoned queued turn the
	// claim loop has already claimed: in one transaction it marks the user
	// message delivered=true + window_state='inactive' (out of every future
	// assembly and out of the claim predicate, but still visible transcript
	// history), stamps it with the conversation's ACTIVE claim, and releases
	// that claim (outcome='failed', error=errMsg) so history synthesis
	// renders the turn failed.
	//
	// The claim is released either way — the engagement holding it is giving
	// up regardless — but matched is false, and the message untouched, when
	// the row is gone or already delivered (a cancel raced ahead). Admin
	// pool.
	DeadLetterTurnSystem(ctx context.Context, orgID, conversationID string, messageID int64, errMsg string) (matched bool, err error)

	// SetSDKSession persists the SDK resume handle captured from the agent's
	// init event onto the conversation.
	SetSDKSession(ctx context.Context, orgID, conversationID, sessionID string) error

	// ReleaseActiveTurnSystem stamps the conversation's active claim
	// released: released_at, outcome ('completed'|'failed'|'cancelled'),
	// error, and the duration/turns engagement telemetry — then settles
	// costUSD as one lump on the claim's LAST message row (the user row
	// when nothing streamed; BeginTurn stamped it with this claim). Returns
	// false when no active claim exists (a racing cancel/shutdown already
	// released it) — first writer wins, exactly the old terminal-status
	// filter. Admin pool.
	ReleaseActiveTurnSystem(ctx context.Context, orgID, conversationID, outcome, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error)

	// RevertTurnContext un-consumes a failed/cancelled turn's injection rows
	// (delivered back to false by remembered id) and deletes the
	// 'context_change' audit message (auditMessageID 0 = none was written).
	// A row whose change_type meanwhile gained a NEWER undelivered injection
	// is NOT resurrected — the replacement row already carries the pending
	// delta, and flipping the stale one back would double it.
	RevertTurnContext(ctx context.Context, orgID, conversationID string, consumed []domain.CuratorContextChange, auditMessageID int64) error

	// --- Boot sweeps (admin pool) ---
	//
	// Finding a claimable curator turn is NOT here: one claim loop scans
	// every surface (RunQueueStore.ClaimNextRun), and a curator conversation
	// holding an undelivered user message is simply one of the shapes its
	// needs-driving predicate matches.

	// CancelStrandedTurnsForHomeSystem is the executor's ownership-scoped
	// boot recovery: releases this executor's own active curator claims from
	// strictly earlier boots (outcome 'cancelled', errMsg recorded) — a
	// restart killed every session goroutine, so those engagements are by
	// definition stranded. Queued turns are untouched: they are unowned
	// undelivered messages now and survive to be re-claimed. Returns the
	// count released.
	CancelStrandedTurnsForHomeSystem(ctx context.Context, homeInstanceID string, bootEpoch int64, errMsg string) (int, error)

	// CancelOrphanedTurnsSystem is local/role=all's global boot recovery:
	// releases EVERY active curator claim across every org (outcome
	// 'cancelled') — the single process owned every engagement, and none
	// survive a restart. Queued turns are untouched, same as the
	// ownership-scoped sweep. Returns the count released.
	CancelOrphanedTurnsSystem(ctx context.Context) (int, error)

	// DeleteQueuedTurnsForProjectSystem is the force-stop drain: deletes
	// every queued (undelivered plain user) turn across the project's
	// curator conversations. A cancelled project's queued turns must not
	// linger claimable — the executor claim loop or a pending retry timer
	// would resurrect work for a project that was just force-stopped (team
	// archive). Returns the number of rows deleted. Admin pool — the drain
	// spans every creator's conversation, which the app pool's
	// private-visibility arm cannot reach.
	DeleteQueuedTurnsForProjectSystem(ctx context.Context, orgID, projectID string) (int, error)

	// --- Pending-context producer (admin pool) ---

	// QueueContextChangeSystem records a project-config delta for every LIVE
	// curator conversation of the project (all creators — the app-pool
	// private-visibility arm couldn't reach teammates' conversations, which
	// is why this is a System method): one undelivered 'injection:context'
	// message per conversation with {"change_type","baseline_value"} in
	// metadata. One-pending-per-type: an existing undelivered row of the
	// same change_type for the conversation is replaced. No live
	// conversations = a no-op (nothing has a session to notify).
	QueueContextChangeSystem(ctx context.Context, orgID, projectID, changeType, baselineJSON string) error

	// --- Per-turn credentials (multi only; admin pool) ---

	// PublishTurnCredPubKeySystem parks the conversation's ACTIVE claim in
	// phase='awaiting_credentials' with the per-turn credential sidecar's
	// pubkey — the same park RunQueueStore.MarkAwaitingCredentials writes for
	// a delegated run, which is what lets ONE brain-side scan
	// (ListAwaitingCredentials) serve both surfaces. Postgres also fires the
	// tf_ctl curator_cred_request doorbell (payload keyed by the conversation
	// id) so the brain seals this turn's bundle without waiting for that
	// sweep. Guarded on "active AND no key yet" so a late/duplicate publish
	// can't touch a finished turn or overwrite a key the brain may already be
	// sealing to. matched=false when the guard didn't hold.
	PublishTurnCredPubKeySystem(ctx context.Context, orgID, conversationID, pubkey string) (matched bool, err error)

	// GetTurnProvisionInfoSystem reads the credential-provisioning
	// projection of the conversation's active claim (joined to the
	// conversation for project/team). ok=false when no active claim exists —
	// the turn finished or was never claimed, so there is nothing to seal
	// for.
	GetTurnProvisionInfoSystem(ctx context.Context, orgID, conversationID string) (*domain.CuratorTurnProvision, bool, error)

	// --- Project bundle import (admin pool) ---

	// ImportConversationStateSystem restores one exported curator
	// conversation — the conversation row (caller supplies the fresh id,
	// creator, and sdk_session_id; team_id snapshots from the destination
	// project), its claims, and its messages — with claim/conversation ids
	// already remapped by the caller. Admin pool because claims have no
	// app-pool write door; runs after the import's main tx committed the
	// project row.
	ImportConversationStateSystem(ctx context.Context, orgID string, conv domain.Conversation, claims []domain.Claim, msgs []domain.Message) error
}
