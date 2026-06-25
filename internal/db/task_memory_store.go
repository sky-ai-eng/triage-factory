package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=TaskMemoryStore --output=./mocks --case=underscore --with-expecter

// TaskMemoryStore is the per-resource store for the run_memory table —
// the durable agent-side narrative + the human's post-run verdict for
// every run on every task. Lifted out of the pre-D2 package-level
// functions in internal/db/task_memory.go so multi-mode Postgres
// callers route through $N placeholders + explicit org_id + the
// dual-pool admin/app split.
//
// Method naming follows the dual-pool convention introduced with
// UsersStore / EntityStore / EventStore:
//
//   - Plain methods (UpsertAgentMemory, UpdateRunMemoryHumanContent,
//     GetMemoriesForEntity, GetRunMemory) run on the app pool in
//     Postgres (RLS-active). Callers are request-handler equivalents
//     (review submit, PR submit, swipe-discard cleanup, factory
//     read-back) and must be inside WithTx in multi-mode so JWT
//     claims (org_id, sub) are set for RLS evaluation.
//   - `...System` methods (UpsertAgentMemorySystem,
//     GetMemoriesForEntitySystem) run on the admin pool (BYPASSRLS).
//     The consumers are background goroutines without a JWT-claims
//     context — the delegate spawner's post-completion gate teardown
//     and the run-start materializer both fire from inside the
//     spawner's `runAgent` goroutine which has no request scope.
//     org_id stays bound in the INSERT/SELECT as defense in depth.
//
// No System variants exist for UpdateRunMemoryHumanContent (only
// called from HTTP handlers under request claims) or GetRunMemory (no
// goroutine-internal caller — audit of internal/delegate/* confirmed
// today's reads happen on the handler side only). Adding speculative
// System variants would just be dead code the admin-pool conformance
// suite would have to cover for no consumer; the precedent (e.g.
// EventStore's missing app-side GetMetadata) is to omit unused
// variants until a real caller arrives.
//
// SQLite collapses both pools onto the single connection. The
// `...System` methods are thin wrappers around their non-System
// counterparts; assertLocalOrg gates every entry point.
type TaskMemoryStore interface {
	// UpsertAgentMemory writes the agent-side memory row for a run.
	// Empty / whitespace-only content canonicalizes to SQL NULL on
	// the way in so downstream consumers (factory's memory_missing
	// derivation) get a single truth condition for "agent didn't
	// comply with the gate."
	//
	// blueprintRunID is the run's blueprint run (denormalized from the
	// run, like entityID); pass empty for a standalone run, where it
	// canonicalizes to SQL NULL. It groups one blueprint run's memory
	// so the materializer can fold each step's file into a shared
	// namespace folder.
	//
	// Idempotent on (run_id) via ON CONFLICT — re-running the gate
	// after a retry overwrites agent_content but preserves the row's
	// id, created_at, and any human_content the user has already
	// attached.
	UpsertAgentMemory(ctx context.Context, orgID, runID, entityID, blueprintRunID, content string) error

	// UpsertAgentMemorySystem is the admin-pool variant for the
	// delegate spawner's post-completion gate teardown. Fires inside
	// the runAgent goroutine, which has no JWT-claims context, so the
	// write routes around RLS via BYPASSRLS. Same idempotency +
	// NULL-on-empty contract as the non-System variant.
	UpsertAgentMemorySystem(ctx context.Context, orgID, runID, entityID, blueprintRunID, content string) error

	// UpdateRunMemoryHumanContent records the human's verdict on a
	// run's agent draft into the run_memory row keyed by runID. The
	// gate-teardown upsert at termination guarantees the row exists
	// by the time the human writes a verdict, so this is a plain
	// UPDATE with no INSERT-or-UPDATE branching.
	//
	// Empty / whitespace-only content canonicalizes to NULL, matching
	// UpsertAgentMemory's agent_content handling.
	//
	// A missing row is logged-and-returned-nil rather than failing
	// the call: the only way a runID with no row reaches here is a
	// non-agent review path or a cleanup race, and failing the
	// response after GitHub already accepted the review would be
	// worse than the missed memory write.
	//
	// App pool only — every caller (reviews handler, artifact-PR approve
	// handler, swipe-discard cleanup) runs under request claims.
	UpdateRunMemoryHumanContent(ctx context.Context, orgID, runID, content string) error

	// GetMemoriesForEntity returns all memories across all runs on
	// this entity (and linked entities via entity_links), oldest
	// first. The returned TaskMemory.Content is materialized from
	// agent_content + human_content via the stable separator format
	// the next agent's prompt context parses. Each row carries its
	// BlueprintRunID so the materializer can drop the file into the
	// right per-blueprint-run namespace folder.
	GetMemoriesForEntity(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error)

	// GetMemoriesForEntitySystem mirrors GetMemoriesForEntity but
	// routes through the admin pool. The consumer is the delegate
	// spawner's run-start materializer (materializePriorMemories),
	// which fires inside the runAgent goroutine with no JWT-claims
	// context. org_id stays in the WHERE clause as defense in depth.
	GetMemoriesForEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error)

	// GetRunMemory returns the single memory row for a run, or nil
	// when no row exists. Same materialization contract as
	// GetMemoriesForEntity. Used by the factory run-summary projection
	// and the resume-picker on the handler side.
	GetRunMemory(ctx context.Context, orgID, runID string) (*domain.TaskMemory, error)
}
