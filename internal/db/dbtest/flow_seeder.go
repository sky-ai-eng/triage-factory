package dbtest

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// FlowSeeder is a bag of callbacks the conformance suite uses to
// stage fixture rows. Each backend test file implements them against
// its own SQL — the harness only cares about returned IDs and the
// timestamp arguments the time-window assertions exercise.
type FlowSeeder struct {
	// Entity inserts an active GitHub PR entity and returns its ID.
	// suffix is appended to source_id so multiple seeds per subtest
	// don't collide on the active-key unique index. The seeder
	// also makes the entity *visible* to the entity reads for its
	// backend: in multi-mode (Postgres) that means registering its repo
	// in the team's tracked set; in local mode (SQLite) every entity is
	// visible by construction, so it's a plain insert. The conformance
	// suite asserts read behavior (windows, limits, ordering), not the
	// visibility mechanism — that's pinned per-backend.
	Entity func(t *testing.T, suffix string) string

	// Event inserts an entity-attached event with the given
	// event_type + dedup_key + created_at. occurredAt is optional —
	// pass time.Time{} to leave it NULL (signals "no upstream time").
	// Returns the event ID.
	Event func(t *testing.T, entityID, eventType, dedupKey string, createdAt, occurredAt time.Time) string

	// EventNullEntity inserts a system event (entity_id IS NULL), which
	// no entity-backed read counts or lists. Returns the event ID.
	EventNullEntity func(t *testing.T, eventType string, createdAt time.Time) string

	// Task inserts a task row with the given fields + created_at.
	// Returns the task ID.
	Task func(t *testing.T, entityID, eventType, dedupKey, primaryEventID, status string, createdAt time.Time) string

	// Conversation inserts a conversations row against the given task in the given DISPLAY
	// status, which is not always a stored one: "running" means an
	// engagement is driving the conversation, so the seeder writes a NULL
	// stored status and mints an active claim, exactly as a real claim
	// would. Every other value is written to the column verbatim. Returns
	// the conversation ID. Tests covering memory_missing pair this with
	// SetConversationMemory; tests covering status filtering do not.
	Conversation func(t *testing.T, taskID, status string) string

	// CloseEntity transitions an entity to state='closed' at the
	// given moment. Bypasses any close-side-effects so tests can
	// backdate closures into / out of the grace window.
	CloseEntity func(t *testing.T, entityID string, closedAt time.Time)

	// SetEntityKind rewrites an entity's kind, so a subtest can hold an
	// entity of a kind the seeder has no constructor for (a GitHub issue
	// stub, a vulnerable package) in the same tracked repository as a pull
	// request.
	SetEntityKind func(t *testing.T, entityID, kind string)

	// FinishConversation stamps a terminal status and completed_at on an
	// already-seeded conversation — the shape the team activity node's
	// failed cut reads, which windows on when a run ended rather than on
	// when it started. A zero completedAt writes the status with no stamp:
	// with `open`, a turn that ended with no verdict.
	FinishConversation func(t *testing.T, conversationID, status string, completedAt time.Time)

	// WarmClaim puts the conversation's active claim in phase
	// domain.ClaimPhaseWarm, the claim an engagement keeps after its turn.
	WarmClaim func(t *testing.T, conversationID string)

	// SetConversationMemory inserts a conversation_memory row with the given
	// agent_content and source. content=NullMemorySentinel stores SQL NULL,
	// which is the shape a `none` row has.
	//
	// Raw SQL in every impl, deliberately: the suite's subject here is the
	// memory_missing derivation, so a source the store door would refuse for
	// this content has to be seedable anyway.
	SetConversationMemory func(t *testing.T, conversationID, content string, source domain.MemorySource)

	// GroupedEntity inserts an active entity of source ("github", "jira" or
	// "linear") in the team's tracked set, and returns its id and its unit's:
	// its repository's registry row, its Jira project's registry row, or its
	// Linear team.
	GroupedEntity func(t *testing.T, source string) (entityID, groupingID string)

	// TrackedEntity inserts an active entity of source ("github", "jira",
	// "linear", or in multi-mode "slack") in a unit of its own that the team
	// tracks, and returns its id and the unit's: its repository's registry
	// row, its Jira project's registry row, its Linear team, or its Slack
	// channel's registry row. Every call is a new unit, so a subtest can hold
	// several of one source. Set by the team activity suites, whose
	// tracked-set cut reads the tracking rows in both dialects.
	TrackedEntity func(t *testing.T, source string) (entityID, unitID string)
}

// nullSentinel is the content string callers pass to SetConversationMemory
// when they want agent_content stored as SQL NULL rather than as the
// empty string. The seeder impls translate this sentinel back to a
// NULL bind.
const nullSentinel = "<NULL>"

// NullMemorySentinel exposes the SetConversationMemory convention to backend
// test files so their seeder implementations can recognize the
// "store NULL not empty string" intent.
const NullMemorySentinel = nullSentinel
