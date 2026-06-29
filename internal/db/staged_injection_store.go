package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// StagedInjectionStore owns the staged_agent_injections table — the durable,
// producer-agnostic "stage for next resume" queue (TFAC-501). It is the
// terminal/parked half of the staged-injection delivery seam: a producer that can't
// re-derive its injection from a durable row of its own (the new-commits notifier)
// appends here when the target run has no warm process, and the delegate spawner
// flushes the run's pending injections ahead of the user's text on the next resume.
//
// Both methods are admin-pool (`...System`) only, mirroring the read/write paths
// that touch it: the producer runs in an eventbus subscriber goroutine and the
// consumer in a detached resume goroutine, neither of which carries JWT claims.
// org_id stays in every WHERE/INSERT clause as defense in depth alongside the
// team-scoped RLS the table inherits via runs (Postgres); SQLite is N=1 and
// asserts LocalDefaultOrgID. There is no app-pool variant — no request handler
// reads or writes the queue.
type StagedInjectionStore interface {
	// AppendSystem enqueues one injection for a run. n.ID may be empty (the impl
	// mints a uuid); CreatedAt is stamped by the DB default. The injection's Body is
	// the bare, already-rendered line — the flush wraps and bundles. A failure
	// to append is the one drop the queue can suffer; the caller logs it and the
	// producer's natural retry (the next poll re-diffs the head) covers a
	// transient miss.
	AppendSystem(ctx context.Context, orgID string, n *domain.StagedInjection) error

	// FlushPendingSystem atomically claims and returns every pending injection for
	// the run, oldest-first (created_at ASC, id ASC), removing them in the same
	// statement (DELETE ... RETURNING) so an injection is delivered exactly once even
	// if two resumes race. Returns an empty slice (no error) when nothing is
	// staged. The spawner bundles the returned bodies into one <system-note>
	// block prepended ahead of the resuming user's message.
	FlushPendingSystem(ctx context.Context, orgID, runID string) ([]domain.StagedInjection, error)
}
