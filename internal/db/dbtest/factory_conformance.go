package dbtest

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// FactoryStoreFactory is what a per-backend test file hands to
// RunFactoryReadStoreConformance. Returns:
//   - the wired FactoryReadStore impl,
//   - the orgID to pass to every call,
//   - a FactorySeeder the harness uses to drop entities/events/
//     tasks/conversations/memory rows without coupling to per-backend INSERT
//     shapes (SQLite has no org_id column; Postgres has FK chains
//     into orgs+users+teams).
type FactoryStoreFactory func(t *testing.T) (store db.FactoryReadStore, orgID string, seed FactorySeeder)

// FactorySeeder is a bag of callbacks the conformance suite uses to
// stage fixture rows. Each backend test file implements them against
// its own SQL — the harness only cares about returned IDs and the
// timestamp arguments the time-window assertions exercise.
type FactorySeeder struct {
	// Entity inserts an active GitHub PR entity and returns its ID.
	// suffix is appended to source_id so multiple seeds per subtest
	// don't collide on the (source, source_id) unique index. The seeder
	// also makes the entity *visible* on the factory belt for its
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

	// EventNullEntity inserts a system event (entity_id IS NULL).
	// Used to pin that LifetimeDistinctByEventType ignores
	// system-tagged rows. Returns the event ID.
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

	// FinishConversation stamps a terminal status and completed_at on an
	// already-seeded conversation — the shape the team activity node's
	// failed cut reads, which windows on when a run ended rather than on
	// when it started.
	FinishConversation func(t *testing.T, conversationID, status string, completedAt time.Time)

	// SetConversationMemory upserts a conversation_memory row with the given
	// agent_content. content="" inserts a literal empty string;
	// content with whitespace exercises the BTRIM/TRIM derivation
	// for memory_missing. To insert a row with NULL agent_content,
	// pass a sentinel (we use "<NULL>" by convention — the seeder
	// implementation maps it to NULL).
	SetConversationMemory func(t *testing.T, conversationID, entityID, content string)
}

// nullSentinel is the content string callers pass to SetConversationMemory
// when they want agent_content stored as SQL NULL rather than as the
// empty string. The seeder impls translate this sentinel back to a
// NULL bind.
const nullSentinel = "<NULL>"

// RunFactoryReadStoreConformance covers the read contract every
// FactoryReadStore impl must hold:
//
//   - Time-window counters (EventCountsSince, TaskCountsSince) clip
//     correctly across the cutoff.
//   - LifetimeDistinctByEventType collapses re-entries from one
//     entity to one count and ignores system events.
//   - ActiveConversations filters by status, JOINs through tasks+entities,
//     and derives memory_missing from conversation_memory across all four
//     forms of noncompliance (no row, NULL content, empty string,
//     whitespace-only) plus the populated-content baseline.
//   - RecentEventsByEntity returns at most perEntity events per
//     entity, ordered ascending; empty input is a fast path.
//   - Entities returns the active set + closed-grace ride-along,
//     respecting the closed-grace cutoff, and the active limit is
//     isolated from closure pressure.
//
// Cross-org leakage is Postgres-only (SQLite has no org_id column)
// and lives in the backend test file directly.
func RunFactoryReadStoreConformance(t *testing.T, mk FactoryStoreFactory) {
	t.Helper()

	t.Run("EventCountsSince_ClipsAtCutoff", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()
		ent := seed.Entity(t, "ec1")

		// Inside the window.
		seed.Event(t, ent, domain.EventGitHubPROpened, "", now.Add(-30*time.Minute), time.Time{})
		seed.Event(t, ent, domain.EventGitHubPROpened, "", now.Add(-10*time.Minute), time.Time{})
		// Outside the window — must not count.
		seed.Event(t, ent, domain.EventGitHubPRMerged, "", now.Add(-3*time.Hour), time.Time{})

		// Multi-mode scopes event counts to entities in the viewer's
		// tracked set; seed.Entity already registered this entity's repo
		// so its events count. No-op for SQLite (local, unscoped).

		counts, err := store.EventCountsSince(ctx, orgID, now.Add(-1*time.Hour))
		if err != nil {
			t.Fatalf("EventCountsSince: %v", err)
		}
		if counts[domain.EventGitHubPROpened] != 2 {
			t.Errorf("opened count = %d, want 2 (both inside window)", counts[domain.EventGitHubPROpened])
		}
		if counts[domain.EventGitHubPRMerged] != 0 {
			t.Errorf("merged count = %d, want 0 (outside window)", counts[domain.EventGitHubPRMerged])
		}
	})

	t.Run("LifetimeDistinctByEventType_CollapsesReentries", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()

		a := seed.Entity(t, "dec-a")
		b := seed.Entity(t, "dec-b")
		c := seed.Entity(t, "dec-c")

		seed.Event(t, a, domain.EventGitHubPROpened, "", now, time.Time{})
		seed.Event(t, a, domain.EventGitHubPRCICheckPassed, "", now, time.Time{})
		seed.Event(t, a, domain.EventGitHubPRMerged, "", now, time.Time{})

		seed.Event(t, b, domain.EventGitHubPROpened, "", now, time.Time{})
		seed.Event(t, b, domain.EventGitHubPRCICheckFailed, "", now, time.Time{})
		// Re-entry — same entity, same type — must NOT double-count.
		seed.Event(t, b, domain.EventGitHubPRCICheckFailed, "", now, time.Time{})

		seed.Event(t, c, domain.EventGitHubPROpened, "", now, time.Time{})
		seed.Event(t, c, domain.EventGitHubPRMerged, "", now, time.Time{})

		// Multi-mode scopes the lifetime count to the viewer's tracked
		// set; seed.Entity already registered each entity's repo so all
		// three count. No-op for SQLite (local, unscoped).

		counts, err := store.LifetimeDistinctByEventType(ctx, orgID)
		if err != nil {
			t.Fatalf("LifetimeDistinctByEventType: %v", err)
		}
		want := map[string]int{
			domain.EventGitHubPROpened:        3, // A, B, C
			domain.EventGitHubPRCICheckPassed: 1, // A only
			domain.EventGitHubPRCICheckFailed: 1, // B only — twin re-entry collapses
			domain.EventGitHubPRMerged:        2, // A, C
		}
		for et, n := range want {
			if counts[et] != n {
				t.Errorf("counts[%q] = %d, want %d", et, counts[et], n)
			}
		}
	})

	t.Run("LifetimeDistinctByEventType_IgnoresSystemEvents", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()

		a := seed.Entity(t, "sys-a")
		seed.Event(t, a, domain.EventGitHubPROpened, "", now, time.Time{})
		// entity_id IS NULL — system-tagged row, must not contribute.
		seed.EventNullEntity(t, domain.EventGitHubPROpened, now)
		// seed.Entity already made `a` visible (tracked-set in multi-mode)
		// so its event counts. The system row has NULL entity_id and joins
		// to no entity, so the tracked-set semi-join excludes it just as
		// the NOT-NULL guard does.

		counts, err := store.LifetimeDistinctByEventType(ctx, orgID)
		if err != nil {
			t.Fatalf("LifetimeDistinctByEventType: %v", err)
		}
		if counts[domain.EventGitHubPROpened] != 1 {
			t.Errorf("opened count = %d, want 1 (NULL entity_id row must not count)",
				counts[domain.EventGitHubPROpened])
		}
	})

	t.Run("TaskCountsSince_ClipsAtCutoff", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()
		ent := seed.Entity(t, "tc1")
		evID := seed.Event(t, ent, domain.EventGitHubPROpened, "", now, time.Time{})

		// Inside window.
		seed.Task(t, ent, domain.EventGitHubPROpened, "k1", evID, "queued", now.Add(-30*time.Minute))
		seed.Task(t, ent, domain.EventGitHubPROpened, "k2", evID, "queued", now.Add(-10*time.Minute))
		// Outside window — must not count.
		seed.Task(t, ent, domain.EventGitHubPRMerged, "k3", evID, "queued", now.Add(-3*time.Hour))

		counts, err := store.TaskCountsSince(ctx, orgID, now.Add(-1*time.Hour))
		if err != nil {
			t.Fatalf("TaskCountsSince: %v", err)
		}
		if counts[domain.EventGitHubPROpened] != 2 {
			t.Errorf("opened count = %d, want 2", counts[domain.EventGitHubPROpened])
		}
		if counts[domain.EventGitHubPRMerged] != 0 {
			t.Errorf("merged count = %d, want 0 (outside window)", counts[domain.EventGitHubPRMerged])
		}
	})

	t.Run("ActiveConversations_FiltersByStatusAndDerivesMemoryMissing", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()
		ent := seed.Entity(t, "ar1")
		evID := seed.Event(t, ent, domain.EventGitHubPROpened, "", now, time.Time{})
		taskID := seed.Task(t, ent, domain.EventGitHubPROpened, "", evID, "queued", now)

		// One conversation per memory state we need to cover.
		conversationNoRow := seed.Conversation(t, taskID, "running")
		conversationNullContent := seed.Conversation(t, taskID, "running")
		conversationEmptyContent := seed.Conversation(t, taskID, "running")
		conversationWhitespace := seed.Conversation(t, taskID, "running")
		conversationPopulated := seed.Conversation(t, taskID, "running")
		conversationTerminal := seed.Conversation(t, taskID, "completed") // must NOT appear

		seed.SetConversationMemory(t, conversationNullContent, ent, nullSentinel)
		seed.SetConversationMemory(t, conversationEmptyContent, ent, "")
		seed.SetConversationMemory(t, conversationWhitespace, ent, "  \t\n ")
		seed.SetConversationMemory(t, conversationPopulated, ent, "agent wrote real reasoning")

		convs, err := store.ActiveConversations(ctx, orgID)
		if err != nil {
			t.Fatalf("ActiveConversations: %v", err)
		}

		gotMem := map[string]bool{}
		gotIDs := map[string]bool{}
		for _, fr := range convs {
			gotIDs[fr.Conversation.ID] = true
			gotMem[fr.Conversation.ID] = fr.Conversation.MemoryMissing
		}

		if gotIDs[conversationTerminal] {
			t.Errorf("terminal conversation %s leaked into ActiveConversations — status filter failed", conversationTerminal)
		}
		for _, id := range []string{conversationNoRow, conversationNullContent, conversationEmptyContent, conversationWhitespace, conversationPopulated} {
			if !gotIDs[id] {
				t.Errorf("active conversation %s missing from ActiveConversations", id)
			}
		}

		want := map[string]bool{
			conversationNoRow:        true,
			conversationNullContent:  true,
			conversationEmptyContent: true,
			conversationWhitespace:   true,
			conversationPopulated:    false,
		}
		for id, expected := range want {
			if gotMem[id] != expected {
				t.Errorf("conversation %s: memory_missing = %v, want %v", id, gotMem[id], expected)
			}
		}
	})

	t.Run("RecentEventsByEntity_RespectsPerEntityLimit", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()
		a := seed.Entity(t, "re-a")
		b := seed.Entity(t, "re-b")

		// 4 events on A, oldest to newest by created_at.
		seed.Event(t, a, "github:pr:opened", "", now.Add(-4*time.Minute), time.Time{})
		seed.Event(t, a, "github:pr:ready_for_review", "", now.Add(-3*time.Minute), time.Time{})
		seed.Event(t, a, "github:pr:ci_check_failed", "", now.Add(-2*time.Minute), time.Time{})
		seed.Event(t, a, "github:pr:ci_check_passed", "", now.Add(-1*time.Minute), time.Time{})

		// 1 event on B.
		seed.Event(t, b, "github:pr:opened", "", now.Add(-5*time.Minute), time.Time{})

		got, err := store.RecentEventsByEntity(ctx, orgID, []string{a, b}, 2)
		if err != nil {
			t.Fatalf("RecentEventsByEntity: %v", err)
		}
		if len(got[a]) != 2 {
			t.Errorf("entity A: returned %d events, want 2 (perEntity cap)", len(got[a]))
		}
		// Ascending chronological — A's two newest events should be
		// the failed/passed pair (NOT the opened one).
		if len(got[a]) == 2 {
			if got[a][0].EventType != "github:pr:ci_check_failed" || got[a][1].EventType != "github:pr:ci_check_passed" {
				t.Errorf("A's event order = [%s, %s], want [ci_check_failed, ci_check_passed]",
					got[a][0].EventType, got[a][1].EventType)
			}
		}
		if len(got[b]) != 1 {
			t.Errorf("entity B: returned %d events, want 1", len(got[b]))
		}
	})

	t.Run("RecentEventsByEntity_EmptyInputFastPath", func(t *testing.T) {
		store, orgID, _ := mk(t)
		ctx := context.Background()
		got, err := store.RecentEventsByEntity(ctx, orgID, nil, 5)
		if err != nil {
			t.Fatalf("RecentEventsByEntity nil: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("nil input returned %d entries, want 0", len(got))
		}
		got, err = store.RecentEventsByEntity(ctx, orgID, []string{}, 5)
		if err != nil {
			t.Fatalf("RecentEventsByEntity empty: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("empty input returned %d entries, want 0", len(got))
		}
	})

	// visible seeds an entity plus an event so it has a station. Returns
	// the entity ID. seed.Entity already makes the entity appear on the
	// belt for its backend (Postgres: registered in the team's tracked
	// set; SQLite: visible by construction), so the window/limit subtests
	// below — which care about close timing and budgets, not membership —
	// stay green on both backends. The membership-exclusion contract
	// itself is Postgres-only and lives in the backend test file
	// (TestFactoryReadStore_Postgres_ExcludesUntrackedEntity); SQLite is
	// the local-mode backend and applies no membership filter.
	visible := func(t *testing.T, seed FactorySeeder, suffix string) string {
		t.Helper()
		now := time.Now().UTC()
		ent := seed.Entity(t, suffix)
		seed.Event(t, ent, domain.EventGitHubPROpened, "", now, time.Time{})
		return ent
	}

	t.Run("Entities_ActiveAndClosedGraceWindow", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()

		active := visible(t, seed, "ent-active")
		fresh := visible(t, seed, "ent-fresh")
		stale := visible(t, seed, "ent-stale")

		// Inside the grace window — should appear.
		seed.CloseEntity(t, fresh, time.Now().Add(-10*time.Second))
		// Past the grace window — should be excluded.
		seed.CloseEntity(t, stale, time.Now().Add(-db.FactoryClosedGracePeriod-30*time.Second))

		rows, err := store.Entities(ctx, orgID, 100, nil)
		if err != nil {
			t.Fatalf("Entities: %v", err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[r.Entity.ID] = true
		}
		if !got[active] {
			t.Errorf("active entity %s missing from snapshot", active)
		}
		if !got[fresh] {
			t.Errorf("fresh-closed entity %s missing — should ride grace window", fresh)
		}
		if got[stale] {
			t.Errorf("stale-closed entity %s leaked through — outside grace window", stale)
		}
	})

	t.Run("Entities_ActiveLimitIsolatedFromClosureBurst", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()

		a1 := visible(t, seed, "iso-a1")
		a2 := visible(t, seed, "iso-a2")

		// 10 freshly-closed entities, all inside the grace window.
		// Far more than the active limit (2) but inside
		// FactoryClosedGraceLimit (64).
		closedAt := time.Now().Add(-5 * time.Second)
		closedSet := map[string]bool{}
		for i := 0; i < 10; i++ {
			e := visible(t, seed, "iso-c-"+strconv.Itoa(i))
			seed.CloseEntity(t, e, closedAt)
			closedSet[e] = true
		}

		// limit=2 — same as the active count. The closed burst must
		// not crowd active out.
		rows, err := store.Entities(ctx, orgID, 2, nil)
		if err != nil {
			t.Fatalf("Entities: %v", err)
		}
		activeFound := map[string]bool{}
		closedFound := 0
		for _, r := range rows {
			if r.Entity.State == "active" {
				activeFound[r.Entity.ID] = true
			} else if closedSet[r.Entity.ID] {
				closedFound++
			}
		}
		if !activeFound[a1] || !activeFound[a2] {
			t.Errorf("active limit not isolated: got active set %v, want both %s and %s",
				activeFound, a1, a2)
		}
		if closedFound != 10 {
			t.Errorf("expected 10 closed-grace entities in snapshot, got %d", closedFound)
		}
	})

	t.Run("Entities_ClosedGraceLimitCapsBurst", func(t *testing.T) {
		store, orgID, seed := mk(t)
		ctx := context.Background()

		closedAt := time.Now().Add(-5 * time.Second)
		burst := db.FactoryClosedGraceLimit + 5
		for i := 0; i < burst; i++ {
			e := visible(t, seed, "cap-"+strconv.Itoa(i))
			seed.CloseEntity(t, e, closedAt)
		}

		rows, err := store.Entities(ctx, orgID, 100, nil)
		if err != nil {
			t.Fatalf("Entities: %v", err)
		}
		if len(rows) != db.FactoryClosedGraceLimit {
			t.Errorf("len(rows) = %d, want %d (closed-grace cap binding)",
				len(rows), db.FactoryClosedGraceLimit)
		}
	})
}

// NullMemorySentinel exposes the SetConversationMemory convention to backend
// test files so their seeder implementations can recognize the
// "store NULL not empty string" intent.
const NullMemorySentinel = nullSentinel
