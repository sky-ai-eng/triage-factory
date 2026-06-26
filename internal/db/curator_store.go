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
	CreateRequest(ctx context.Context, orgID, projectID, creatorUserID, userInput string) (string, error)

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
	// run_messages (TFAC-473).
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
}
