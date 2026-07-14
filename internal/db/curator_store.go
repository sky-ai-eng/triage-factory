package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CuratorStore owns the three curator-runtime tables: curator_requests,
// curator_messages, curator_pending_context. Each write attributes to
// the requesting user via creator_user_id — the multi-tenant RLS
// policies (curator_requests_modify, curator_messages_modify,
// curator_pending_context_modify) gate every row on the (org_id,
// creator_user_id) pair matching tf.current_user_id() /
// tf.current_org_id(), so every method here must run inside a
// SyntheticClaimsWithTx (or admin pool) in Postgres.
//
// Wires the app pool in Postgres for the curator goroutine's normal
// dispatch path — each turn opens short-lived txs under the
// requesting user's identity (read from curator_requests.creator_user_id
// at dequeue time). The curator's own system-service cancel paths
// (process shutdown, project-delete drain, SendMessage shutdown /
// queue-full fallbacks) route through the admin-pool …System methods
// below (TFAC-64). The one raw-*sql.DB curator helper still live is the
// handler-side user-cancel in internal/server/curator.go — that has a
// real request context and is the D9 handler sweep's job (TFAC-168),
// out of scope here.
//
// Read methods (Get/List) mostly live in the package-level helpers
// for now — the goroutine's writes are the auth surface that matters
// under RLS. The reads that DO live here (GetRequest,
// ListPendingContext) belong on the interface because their callers
// need a per-resource handle they can wire through (the curator
// goroutine reads GetRequest inside the same per-turn synthetic-
// claims wrap as MarkRunning; the project-bundle export reads
// ListPendingContext alongside the handler-side InsertPendingContext
// path). Both must honor RLS in Postgres, so callers run them under
// claims-bound execution (SyntheticClaimsWithTx or WithTx).
type CuratorStore interface {
	// CreateRequest inserts a new queued curator_request row and
	// returns its id. creatorUserID is the requesting user — in
	// local mode the handler passes runmode.LocalDefaultUserID
	// (D9 retrofit will plumb the real user from request context).
	// In Postgres the value is bound directly; in SQLite the
	// column has a DEFAULT and the value is bound for parity.
	//
	// homeInstanceID is the executor that will run this turn — the resolved
	// curator home (spec §6.3). "" for the untouched local / role=all path
	// (which never forwards and sweeps globally); the serving pod's own id in
	// the no-executor-available fallback; a remote executor id when a control
	// pod forwards. Stored NULL when empty; the executor claim loop and the
	// ownership-scoped sweeps select on it.
	CreateRequest(ctx context.Context, orgID, projectID, creatorUserID, homeInstanceID, userInput string) (string, error)

	// GetRequest reads a single request row, or (nil, nil) if not
	// found. App-pool in Postgres so curator_requests_select RLS
	// gates the read on (org_id, creator_user_id).
	GetRequest(ctx context.Context, orgID, id string) (*domain.CuratorRequest, error)

	// MarkRequestRunning flips queued → running and stamps started_at.
	// Returns sql.ErrNoRows if the row is not currently queued
	// (cancel raced ahead of pickup).
	MarkRequestRunning(ctx context.Context, orgID, id string) error

	// CompleteRequest writes a terminal status + accounting, but
	// ONLY if the row is non-terminal. Returns true if the flip
	// happened. Status is one of done|cancelled|failed. The four token
	// columns are SET from the curator_messages SUM in the same UPDATE
	// (the streaming sink wrote every message row before this terminal
	// write), aggregating the turn's token breakdown onto curator_requests
	// for the unified spend view — same roll-up shape runs uses over
	// run_messages (TFAC-473). The cancel path (MarkRequestCancelledIfActive)
	// runs the same SUM, so the token columns are kept current at every
	// terminal write, not just on done/failed — a request cancelled mid-turn
	// still reflects the tokens it streamed.
	CompleteRequest(ctx context.Context, orgID, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error)

	// MarkRequestCancelledIfActive flips any non-terminal row to
	// cancelled. Returns true if the flip happened. Used by the
	// goroutine's own cancel-observation paths (markCancelled) under
	// the requesting user's synthetic claims.
	MarkRequestCancelledIfActive(ctx context.Context, orgID, id, errMsg string) (bool, error)

	// MarkRequestCancelledIfActiveSystem is the admin-pool variant of
	// MarkRequestCancelledIfActive for the curator's system-driven
	// cancel paths — process shutdown, project-delete drain, and the
	// SendMessage "curator is shut down" fallback. These fire outside
	// any request/JWT context (no live user to attribute to), so the
	// app-pool RLS UPDATE would match zero rows and silently leave the
	// request dangling non-terminal. The row already records who
	// created it (creator_user_id); a system cancel is not a user
	// action, so it routes through the admin pool (BYPASSRLS) rather
	// than reconstructing per-user claims. orgID still scopes the
	// UPDATE for defense-in-depth even though the admin pool bypasses
	// RLS. TFAC-64.
	MarkRequestCancelledIfActiveSystem(ctx context.Context, orgID, id, errMsg string) (bool, error)

	// CompleteRequestSystem is the admin-pool variant of CompleteRequest
	// for the curator's queue-full fallback in SendMessage — it runs
	// from the handler goroutine with no claims context, so the
	// app-pool UPDATE would be rejected by RLS and leave the freshly
	// created request stuck in `queued`. See
	// MarkRequestCancelledIfActiveSystem for the pool-attribution
	// rationale. TFAC-64. Token columns roll up from curator_messages,
	// same as CompleteRequest (TFAC-473).
	CompleteRequestSystem(ctx context.Context, orgID, id, status, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error)

	// QueuedRequestsForProjectSystem lists the queued curator_request
	// rows for a project via the admin pool (BYPASSRLS). The
	// project-delete drain (Curator.CancelProject → cancelQueuedRows)
	// has no live request context, so an app-pool SELECT under RLS
	// would see zero rows and the drain would no-op, leaving queued
	// rows to dangle past the project FK cascade. orgID scopes the
	// list defensively. TFAC-64.
	QueuedRequestsForProjectSystem(ctx context.Context, orgID, projectID string) ([]domain.CuratorRequest, error)

	// InsertMessage writes one curator_messages row and returns
	// its id. The struct's CreatedAt is set to now if zero.
	InsertMessage(ctx context.Context, orgID string, msg *domain.CuratorMessage) (int64, error)

	// DeleteMessagesBySubtype removes every curator_messages row
	// for a request with the given subtype. Used during pending-
	// context revert to drop the `context_change` audit row so the
	// chat history doesn't show a phantom "context noted" entry.
	DeleteMessagesBySubtype(ctx context.Context, orgID, requestID, subtype string) error

	// ConsumePendingContext atomically claims every unconsumed row
	// for the given (project, request) and returns them alongside
	// a fresh snapshot of the project — both reads happen inside
	// the same tx so the diff at the call site is computed against
	// project state consistent with the rows being returned. See
	// the package-level helper for the locking-order rationale.
	//
	// When this method is invoked from inside a SyntheticClaimsWithTx,
	// the outer tx is the locking boundary; the impl does not open
	// its own tx in that case. When invoked against *sql.DB (the
	// future non-curator-goroutine call path), the impl opens a
	// short-lived tx internally.
	ConsumePendingContext(ctx context.Context, orgID, projectID, requestID string) (*domain.Project, []domain.CuratorPendingContext, error)

	// FinalizePendingContext deletes every row consumed by
	// requestID. Called on terminal `done` so the agent's
	// successful absorption of the deltas retires them.
	FinalizePendingContext(ctx context.Context, orgID, requestID string) error

	// RevertPendingContext un-consumes the rows claimed by
	// requestID so the next user message picks them up again.
	// Used on terminal `cancelled` or `failed`. See the package-
	// level helper for the merge semantics.
	RevertPendingContext(ctx context.Context, orgID, requestID string) error

	// InsertPendingContext queues a context-change delta for the
	// next curator dispatch on (orgID, projectID, sessionID,
	// changeType). Coalescing is enforced by the partial unique
	// index on (project_id, curator_session_id, change_type)
	// WHERE consumed_at IS NULL: a second PATCH between user
	// messages hits ON CONFLICT DO NOTHING and the *earliest*
	// baseline_value wins, which is the correct "snapshot before
	// the first unconsumed change" anchor for diffing at consume
	// time. baselineJSON must be a JSON-encoded representation of
	// the value before the PATCH applied.
	//
	// Caller is responsible for ensuring sessionID is non-empty —
	// there is no point queueing pending rows for a project whose
	// Curator has never been spun up.
	InsertPendingContext(ctx context.Context, orgID, projectID, sessionID, changeType, baselineJSON string) error

	// ListPendingContext returns every row for a project regardless
	// of consumed status, ordered by created_at. Used by the
	// project-bundle export to inspect outstanding deltas.
	ListPendingContext(ctx context.Context, orgID, projectID string) ([]domain.CuratorPendingContext, error)

	// DeletePendingContextForSession removes every pending or
	// consumed row for a (projectID, sessionID). Used when the
	// session is reset so the new session's envelope renders
	// current values directly without phantom deltas describing
	// transitions the new agent never witnessed.
	DeletePendingContextForSession(ctx context.Context, orgID, projectID, sessionID string) error

	// ListRequestsByProject returns the project's curator requests in
	// chronological order — the chat-history read. Claims-bound: call
	// inside WithTx. In Postgres the curator_requests_select policy is
	// deliberately self-only (creator_user_id = tf.current_user_id()),
	// so a multi-mode caller sees their own turns, not teammates'; the
	// local single user sees everything. TFAC-109 (replaces the raw
	// package-level ListCuratorRequestsByProject helper).
	ListRequestsByProject(ctx context.Context, orgID, projectID string) ([]domain.CuratorRequest, error)

	// ListMessagesByRequestIDs returns the agent-side stream rows for a
	// batch of request ids, grouped by request_id — the batched half of
	// the chat-history read (avoids an N+1 over the request list).
	// Same claims-bound / self-only visibility contract as
	// ListRequestsByProject. Empty input returns an empty map, not nil.
	ListMessagesByRequestIDs(ctx context.Context, orgID string, requestIDs []string) (map[string][]domain.CuratorMessage, error)

	// InFlightRequestForProject returns the queued or running request
	// for a project, or (nil, nil) if none — the cancel endpoint's
	// lookup. Claims-bound / self-only in Postgres: a user can locate
	// (and therefore cancel) only their own in-flight turn.
	InFlightRequestForProject(ctx context.Context, orgID, projectID string) (*domain.CuratorRequest, error)

	// ResetForProject wipes the caller-visible curator artifacts for a
	// project so the next message starts a brand-new Claude Code
	// session: clears curator_session_id on the project row, deletes
	// pending-context rows, and deletes curator_request rows (cascading
	// to messages). Returns ErrCuratorInFlight if any user has a
	// queued/running request.
	//
	// Claims-bound — call inside WithTx so the deletes run under the
	// caller's RLS. In Postgres the modify policies are self-only, so a
	// reset removes the RESETTING user's history and clears the shared
	// session id; other users' history rows survive (they cannot be
	// destroyed by someone else) and the next turn simply starts a new
	// session. The in-flight guard intentionally checks across ALL
	// users in the org (org-scoped, admin-pool read in Postgres):
	// resetting the shared session under a teammate's live turn is the
	// race the guard exists to prevent, and a self-only check couldn't
	// see it.
	ResetForProject(ctx context.Context, orgID, projectID string) error

	// ImportRequest restores one historical curator_request row from a
	// project bundle: caller-supplied id, terminal status, accounting,
	// and timestamps are preserved verbatim; creator_user_id is stamped
	// to the importing user (the original creator is not a user in the
	// destination install); team_id snapshots from the destination
	// project row per the TFAC-476 invariant. Claims-bound — call
	// inside WithTx after the project row exists in the same tx.
	ImportRequest(ctx context.Context, orgID string, req domain.CuratorRequest) error

	// ImportPendingContext restores one historical pending-context row
	// from a project bundle, preserving consumed_at /
	// consumed_by_request_id / created_at (unlike InsertPendingContext,
	// which queues a fresh unconsumed delta). creator_user_id stamps to
	// the importing user. Claims-bound — call inside WithTx.
	ImportPendingContext(ctx context.Context, orgID string, row domain.CuratorPendingContext) error

	// CancelOrphanedNonTerminalRequests sweeps every queued/running
	// curator_request row across every org in the database, flipping
	// them to cancelled with finished_at stamped to now. Called once
	// at process startup as the recovery pass: a binary restart kills
	// every per-project curator goroutine + agentproc subprocess in
	// this process, so any row left non-terminal from a previous
	// process is by definition stranded (running rows lost their
	// goroutine, queued rows lost the goroutine that would have
	// picked them up). Auto-replaying a stale message after restart
	// would surprise the user more than dropping it; cancelling lets
	// the user re-send if they actually wanted that message
	// processed. Returns the row-count flipped.
	//
	// No orgID parameter by design: this is a cross-tenant system
	// service running outside any request context. In Postgres the
	// impl routes through the admin pool (BYPASSRLS); in SQLite the
	// single tenant means the sweep is equivalent to a single-org
	// reset. Multi-pod per-org sharding would let us scope this
	// per-pod, but pod sharding doesn't exist (single-pod multi-mode
	// in v1).
	CancelOrphanedNonTerminalRequests(ctx context.Context) (int, error)

	// ListQueuedRequestsForHomeSystem returns the queued curator_request rows
	// homed to homeInstanceID (id, org, project, creator), oldest first — the
	// executor claim loop's scan (curator homing, spec §6.3). Admin-pool /
	// BYPASSRLS: a cross-user, cross-org system read of turns this executor
	// owns, with home_instance_id bound by argument. Only ever returns rows the
	// resolver stamped to this executor, so it never needs a per-row lock — one
	// home owns a project, and the per-project session serializes turns.
	ListQueuedRequestsForHomeSystem(ctx context.Context, homeInstanceID string) ([]domain.HomedCuratorRequest, error)

	// CancelStrandedRequestsForHomeSystem flips every queued/running
	// curator_request homed to homeInstanceID to cancelled with errMsg — the
	// ownership-scoped recovery pass (curator homing, spec §6.3). Each pod
	// sweeps only turns homed to ITSELF: an executor cancels its own prior-boot
	// stranded turns on boot, and the leader reaper cancels turns homed to a
	// dead executor (both via home_instance_id = the target). This replaces the
	// fleet-unsafe global CancelOrphanedNonTerminalRequests on multi-mode split
	// roles — a control restart must never cancel an executor's live turns.
	// Admin-pool / BYPASSRLS, home_instance_id bound by argument. Returns the
	// row count flipped.
	CancelStrandedRequestsForHomeSystem(ctx context.Context, homeInstanceID, errMsg string) (int, error)
}
