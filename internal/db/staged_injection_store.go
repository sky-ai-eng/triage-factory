package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// StagedInjectionStore is the durable, producer-agnostic "stage for next
// resume" queue (TFAC-501), stored as undelivered messages rows
// (role='user', subtype='injection:system-note', delivered=false — the
// producer tag rides metadata). It is the terminal/parked half of the
// staged-injection delivery seam: a producer that can't re-derive its
// injection from a durable row of its own (the new-commits notifier)
// appends here when the target run has no warm process, and the delegate
// spawner flushes the run's pending injections ahead of the user's text on
// the next resume.
//
// All methods are admin-pool (`...System`) only, mirroring the read/write
// paths that touch it: the producer runs in an eventbus subscriber goroutine
// and the consumer in a detached resume goroutine, neither of which carries
// JWT claims. org_id stays in every WHERE/INSERT clause as defense in depth;
// SQLite is N=1 and asserts LocalDefaultOrgID. There is no app-pool
// variant — no request handler reads or writes the queue.
type StagedInjectionStore interface {
	// AppendSystem enqueues one injection for a run. The impl writes an
	// undelivered message row and writes the row id back to n.ID (a
	// decimal string — messages ids are integers); CreatedAt is stamped by
	// the DB default. The injection's Body is the bare, already-rendered
	// line — the flush wraps and bundles. A failure to append is the one
	// drop the queue can suffer, and it is permanent for that injection:
	// the transition that produced it was already committed alongside the
	// snapshot it was diffed against, so re-polling re-derives nothing. A
	// caller may still treat it as acceptable, but only where a LATER
	// transition would carry the same news — never as a retry of this one.
	AppendSystem(ctx context.Context, orgID string, n *domain.StagedInjection) error

	// FlushPendingSystem atomically claims and returns every pending
	// injection for the run, oldest-first (id ASC — insertion order),
	// flipping delivered=true in the same statement (UPDATE ... RETURNING)
	// so an injection is delivered exactly once even if two resumes race.
	// Returns an empty slice (no error) when nothing is staged. The
	// spawner bundles the returned bodies into one <system-note> block
	// prepended ahead of the resuming user's message.
	FlushPendingSystem(ctx context.Context, orgID, runID string) ([]domain.StagedInjection, error)

	// DeleteSystem withdraws one staged injection by id (the decimal
	// message row id AppendSystem returned). Used to clean up a row staged
	// onto a run that a post-stage recheck then decided wasn't (or is no
	// longer) resumable after all — the caller falls through to a normal
	// deferral instead, and the staged row would otherwise be orphaned (or,
	// worse, double-deliver if the run is later boot-recovered and
	// resumed). The impl retires the row (window_state 'inactive', delivered
	// KEPT false — withdrawn means "never happened", so it must not read as
	// delivered transcript history) rather than deleting it — the executor's
	// tf_system role holds no DELETE on messages, and a retired row is
	// equally invisible to the flush, assembly, and the display reads. A
	// no-op (no error) if the row is already gone or was flushed.
	DeleteSystem(ctx context.Context, orgID, id string) error
}
