package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PermissionStore owns the conversation_permissions table — the durable record
// of every tool-approval prompt a conversation raised and how it was answered.
//
// The in-memory broker in internal/delegate is still what parks the agent's
// turn and still what unblocks it. This store is a record kept alongside that
// mechanism: it gives a live pending prompt an address (so a refresh, a second
// tab, or a cold load can reconstruct it) and gives every resolution an audit
// row (so "denied by you" is distinguishable from "timed out" from "nobody was
// watching" without parsing agent-facing prose). Nothing drives the agent off
// these rows.
//
// Pooling in Postgres: reads are app-pool under RLS (the policy composes
// through the conversation, mirroring claims), writes are admin-pool — every
// writer is a delegate goroutine with no JWT-claims context, and tf_app holds
// no INSERT/UPDATE grant on the table. SQLite is N=1 and collapses to the one
// connection. Because every write is admin-pool with no app-pool-doored
// twin, there is no RLS-gated write path to converge separately the way
// JiraAppsStore.UpsertForOrg's app-pool conformance arm does — see
// dbtest.RunPermissionStoreConformance's doc.
type PermissionStore interface {
	// Create inserts a freshly surfaced prompt and returns the persisted row.
	// p.State must be domain.PermissionStatePending; the store mints the id
	// when p.ID is empty and defaults RequestedAt to now when zero, so the
	// caller's clock is the one both timestamps and the derived wait are
	// measured against — and the returned row is a caller's only handle to
	// either when it supplied neither.
	//
	// p.ClaimID is REQUIRED, enforced here rather than by the schema (the
	// column stays nullable so an old row survives its claim being deleted).
	// It is what ListPending derives pending against and what ExpireForClaim
	// keys on, so a prompt with no claim would be unreadable by every surface
	// AND unsettleable by every path — a row stuck at 'pending' forever that
	// no one can answer. Failing the write is strictly better: the caller
	// records nothing, logs, and still asks the question.
	//
	// A second insert for the same (conversation_id, tool_call_id) is refused by
	// the unique index rather than silently duplicating the prompt — one row per
	// gated call is what lets a decision tie back to the call it authorized.
	Create(ctx context.Context, orgID string, p domain.ConversationPermission) (domain.ConversationPermission, error)

	// Resolve stamps a terminal on the prompt named by (conversationID,
	// toolCallID) — state, reason, the deciding user when there was one, the
	// decision time, and the derived waited_ms — and returns the row it
	// settled.
	//
	// Only a row still 'pending' is written, so the first resolution wins and a
	// racing second one is a silent no-op — which is exactly the shape the
	// broker's own deregister-then-drain produces (a decision that arrived in
	// the same instant a deadline fired is honored by one path and dropped by
	// the other). (nil, nil) is that no-op: the guard declined because the
	// prompt wasn't pending — already resolved, or never existed — and neither
	// is an error. This is a best-effort record alongside a mechanism that has
	// already made its own decision, never the thing driving it.
	//
	// decidedBy is the answering user's id, empty for every reason but
	// domain.PermissionReasonUser.
	Resolve(ctx context.Context, orgID, conversationID, toolCallID, state, reason, decidedBy string) (*domain.ConversationPermission, error)

	// ListPending returns the conversation's genuinely-open prompts,
	// oldest-first.
	//
	// Pending is DERIVED, not trusted: a row is returned only when it is
	// state='pending' AND owned by the conversation's currently-active claim. A
	// prompt left behind by a process that no longer exists is a question about
	// a workspace that may since have been restored — so the read is correct
	// whether or not anything ever wrote the expiry, which is what makes it
	// robust against the crash it exists to survive. A conversation with no
	// active claim has nothing pending, by construction.
	ListPending(ctx context.Context, orgID, conversationID string) ([]domain.ConversationPermission, error)

	// ExpireForClaim marks every still-pending prompt owned by claimID as
	// expired (reason domain.PermissionReasonClaimLost), stamping the same
	// decided_at / waited_ms a real resolution would.
	//
	// Called best-effort on claim release to keep stored state tidy for audit
	// reads. NOTHING DEPENDS ON IT LANDING — ListPending reaches the same
	// conclusion from the claim alone — and that is precisely what makes the
	// arrangement robust against a crash, which by definition never runs its
	// cleanup.
	//
	// Exempt from the returned-row rule: it expires every outstanding request
	// on a claim in one statement, so there is no single row a return value
	// could name.
	ExpireForClaim(ctx context.Context, orgID, claimID string) error
}
