package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrStagedInjectionSubtype refuses a staged injection whose subtype is not
// one of the two a staged note is stored under.
var ErrStagedInjectionSubtype = errors.New("a staged injection is an injection:event or an injection:system-note row")

// RequireStagedInjectionSubtype refuses a subtype outside
// domain.StagedInjectionSubtypes with ErrStagedInjectionSubtype. Both
// dialects' AppendSystem call it before touching the database.
func RequireStagedInjectionSubtype(subtype string) error {
	if domain.IsStagedInjectionSubtype(subtype) {
		return nil
	}
	return fmt.Errorf("%w, not %q", ErrStagedInjectionSubtype, subtype)
}

// StagedInjectionStore is the durable, producer-agnostic "stage for next
// resume" queue, stored as undelivered messages rows (role='user',
// delivered=false, the producer tag in metadata) under one of two subtypes:
// 'injection:event' for a new-event injection and 'injection:system-note' for
// every other note. A producer that can't re-derive its injection from a
// durable row of its own (the PR coherence feed, the router's new-event
// injection) appends here. A native loop reads the rows itself before its next
// model call; an SDK conversation has them flushed ahead of the user's text on
// its next resume.
//
// All methods are admin-pool (`...System`) only, mirroring the read/write
// paths that touch it: the producer runs in an eventbus subscriber goroutine
// and the consumer in a detached resume goroutine, neither of which carries
// JWT claims. org_id stays in every WHERE/INSERT clause as defense in depth;
// SQLite is N=1 and asserts LocalDefaultOrgID. There is no app-pool
// variant — no request handler reads or writes the queue.
type StagedInjectionStore interface {
	// AppendSystem enqueues one injection for a conversation and returns the
	// row it persisted — id (a decimal string; messages ids are integers)
	// and CreatedAt come from the DB, not the caller's input. The injection's
	// Body is the bare, already-rendered line — the flush wraps and
	// bundles. Its Subtype is written as the row's subtype and must be one of
	// domain.StagedInjectionSubtypes; any other value is refused with
	// ErrStagedInjectionSubtype before anything is written.
	//
	// A failure to append is the one drop the queue can suffer, and it is
	// permanent for that injection: the transition that produced it was
	// already committed alongside the snapshot it was diffed against, so
	// re-polling re-derives nothing. A caller may still treat it as
	// acceptable, but only where a LATER transition would carry the same
	// news — never as a retry of this one.
	AppendSystem(ctx context.Context, orgID string, n domain.StagedInjection) (domain.StagedInjection, error)

	// FlushPendingSystem atomically claims and returns every pending
	// injection for the conversation, of both subtypes, oldest-first (id ASC
	// — insertion order), flipping delivered=true in the same statement
	// (UPDATE ... RETURNING) so an injection is delivered exactly once even
	// if two resumes race.
	// Returns an empty slice (no error) when nothing is staged. The
	// spawner bundles the returned bodies into one <system-note> block
	// prepended ahead of the resuming user's message.
	FlushPendingSystem(ctx context.Context, orgID, conversationID string) ([]domain.StagedInjection, error)

	// DeleteSystem withdraws one staged injection of either subtype by id
	// (the decimal message row id AppendSystem returned). Used to clean up a
	// row staged onto a conversation that a post-stage recheck then decided
	// nothing will read — the caller falls through to a normal deferral
	// instead, and the staged row would otherwise be orphaned (or, worse,
	// double-deliver if the conversation is later boot-recovered and
	// resumed). The impl retires the row (window_state 'inactive', delivered
	// KEPT false — withdrawn means "never happened", so it must not read as
	// delivered transcript history) rather than deleting it — the executor's
	// tf_system role holds no DELETE on messages, and a retired row is
	// equally invisible to the flush, assembly, and the display reads.
	//
	// A row a reader already consumed is left as it is, and consumed reports
	// it: the note reached the agent between the stage and the recheck, so
	// the caller must not defer as if it had not. A row that is gone, or an id
	// that names none, is a no-op that reports consumed=false.
	//
	// Exempt from the returned-row rule: it is a delete.
	DeleteSystem(ctx context.Context, orgID, id string) (consumed bool, err error)

	// HasNoteForEventSystem reports whether producer has told the
	// conversation of eventID: a note it staged with that event in its
	// provenance, delivered or still waiting to be. A withdrawn note
	// (DeleteSystem) never happened and does not count.
	HasNoteForEventSystem(ctx context.Context, orgID, conversationID, producer, eventID string) (bool, error)
}
