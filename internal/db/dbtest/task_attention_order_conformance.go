package dbtest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskAttentionOrderFactory is what a per-backend test file hands to
// RunTaskAttentionOrderConformance. It returns the wired TaskStore, the orgID
// every method takes, and a seeder staging the fixture shapes.
//
// Its own factory rather than TaskStoreFactory's seeder: the order under test
// is a statement about rows in four other tables — conversations, claims,
// conversation_permissions, artifacts — and the backends disagree on nearly
// every column of them.
type TaskAttentionOrderFactory func(t *testing.T) (store db.TaskStore, orgID string, seed TaskAttentionOrderSeeder)

// TaskAttentionFixture describes one seeded task. Priority is explicit on every
// fixture because the tier's whole claim is that it outranks the queue's own
// priority: a lane whose fixtures tied on priority could not tell the tier from
// the seed order.
type TaskAttentionFixture struct {
	// Suffix distinguishes this fixture's entity from its siblings'. The dedup
	// index is keyed on (entity, event_type, dedup_key), so each fixture needs
	// its own entity or two of them collapse into one task.
	Suffix string
	// Title is the entity title — the key the `title` sort orders on, so a
	// subtest can stage a reader's sort that disagrees with the tier.
	Title    string
	Status   string
	Priority float64
	// ClosedAt lands in tasks.closed_at. Required on a done/dismissed fixture:
	// the closed partition reads the column, not the status.
	ClosedAt *time.Time
}

// TaskAttentionOrderSeeder stages the conversation graph the tier reads.
// Production mints these rows through several stores at once; the callbacks
// write them directly against each backend's own schema so the suite stays
// schema-blind.
type TaskAttentionOrderSeeder struct {
	// Task stages a fresh entity + event + task and returns the task id.
	Task func(t *testing.T, f TaskAttentionFixture) (taskID string)

	// Conversation stages one delegation conversation on the task with the
	// given STORED status — "" inserts SQL NULL, the mid-flight state a
	// conversation carries until it reaches an outcome. Returns its id.
	Conversation func(t *testing.T, taskID, storedStatus string) (conversationID string)

	// ActiveClaim mints an unreleased claim on the conversation and returns
	// its id. This is what makes a conversation LIVE: `running` is derived
	// from the claim table, never stored.
	ActiveClaim func(t *testing.T, conversationID string) (claimID string)

	// PendingPermission stages an unanswered tool prompt owned by claimID.
	PendingPermission func(t *testing.T, conversationID, claimID string)

	// Artifact stages one artifacts row on the conversation.
	Artifact func(t *testing.T, conversationID, kind, state, detailsJSON string)
}

// RunTaskAttentionOrderConformance is the shared suite for the attention tier
// at the front of TaskStore.List's lane ordering — whose move is it, ahead of
// the queue's own priority.
//
// The pressure is on two things. First that the tier outranks priority: every
// fixture here carries a priority that would order the lane differently, so a
// tier that silently stopped applying would show up as the priority order
// rather than as an empty result. Second that it never reaches a closed row —
// a closure still holding a draft pull request matches the needs-you predicate,
// and the tier is read before the recency term, so the terminal tail is where
// this would go wrong. Two subtests cover that from both ends: a `done`-only
// read, and the mixed read where the tier is live for the open rows in the same
// statement.
func RunTaskAttentionOrderConformance(t *testing.T, mk TaskAttentionOrderFactory) {
	t.Helper()
	ctx := context.Background()

	inProgress := func() db.TaskListFilter {
		return db.TaskListFilter{Statuses: []string{"in_progress"}}
	}
	list := func(t *testing.T, s db.TaskStore, orgID string, f db.TaskListFilter) ([]string, int) {
		t.Helper()
		tasks, total, err := s.List(ctx, orgID, f, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		ids := make([]string, len(tasks))
		for i, task := range tasks {
			ids[i] = task.ID
		}
		return ids, total
	}

	t.Run("InProgressLaneLeadsWithWhoseMoveItIs", func(t *testing.T) {
		s, orgID, seed := mk(t)

		// Priorities run against the wanted order: by priority alone the lane
		// reads quiet, flight, none, failed, needsYou — so what the
		// assertions below see is the tier and not the middle.
		needsYou := seed.Task(t, TaskAttentionFixture{Suffix: "attn-needs", Title: "e needs you", Status: "in_progress", Priority: 0.1})
		failed := seed.Task(t, TaskAttentionFixture{Suffix: "attn-failed", Title: "d failed", Status: "in_progress", Priority: 0.2})
		flight := seed.Task(t, TaskAttentionFixture{Suffix: "attn-flight", Title: "c in flight", Status: "in_progress", Priority: 0.3})
		none := seed.Task(t, TaskAttentionFixture{Suffix: "attn-none", Title: "b no conversation", Status: "in_progress", Priority: 0.25})
		quiet := seed.Task(t, TaskAttentionFixture{Suffix: "attn-quiet", Title: "a concluded", Status: "in_progress", Priority: 0.9})

		// A concluded conversation still holding a draft pull request: the
		// agent stopped, the PR is nobody's but a human's to finish.
		needsYouConv := seed.Conversation(t, needsYou, domain.StatusCompleted)
		seed.Artifact(t, needsYouConv, domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, "")
		seed.Conversation(t, failed, domain.StatusFailed)
		// Mid-flight and claimed — `running` is derived from the live claim.
		seed.ActiveClaim(t, seed.Conversation(t, flight, ""))
		// Concluded with nothing unresolved, and the highest priority in the
		// lane: reading is the last thing a lane wants shown.
		seed.Conversation(t, quiet, domain.StatusCompleted)
		// `none` gets no conversation at all — not anybody's move either, so
		// it ties with the in-flight row and the middle orders the two.

		got, total := list(t, s, orgID, inProgress())
		want := []string{needsYou, failed, flight, none, quiet}
		if !slices.Equal(got, want) {
			t.Errorf("In Progress lane = %v,\n                    want %v\n(needs-you, failed, then the two in-flight rows by priority, then concluded)", got, want)
		}
		if total != len(want) {
			t.Errorf("total_count = %d, want %d — the tier is an ordering, not a filter", total, len(want))
		}
	})

	t.Run("AnUnansweredPromptIsWhoseMoveItIs", func(t *testing.T) {
		// The other half of tier 0, and the half a live conversation reaches:
		// a prompt owned by the conversation's active claim outranks the same
		// conversation without one, whatever its priority.
		s, orgID, seed := mk(t)
		prompted := seed.Task(t, TaskAttentionFixture{Suffix: "attn-prompted", Title: "prompted", Status: "in_review", Priority: 0.1})
		working := seed.Task(t, TaskAttentionFixture{Suffix: "attn-working", Title: "working", Status: "in_review", Priority: 0.9})

		promptedConv := seed.Conversation(t, prompted, "")
		seed.PendingPermission(t, promptedConv, seed.ActiveClaim(t, promptedConv))
		seed.ActiveClaim(t, seed.Conversation(t, working, ""))

		got, _ := list(t, s, orgID, db.TaskListFilter{Statuses: []string{"in_review"}})
		if want := []string{prompted, working}; !slices.Equal(got, want) {
			t.Errorf("In Review lane = %v, want %v — an unanswered prompt leads the lane", got, want)
		}
	})

	t.Run("AReaderSortDoesNotDisplaceTheTier", func(t *testing.T) {
		// A sort_key replaces the middle of the order, not the tier: the lane
		// is still "whose move is it", ordered by the reader's key within
		// each tier.
		s, orgID, seed := mk(t)
		needsYou := seed.Task(t, TaskAttentionFixture{Suffix: "attn-sort-needs", Title: "zzz needs you", Status: "in_progress", Priority: 0.5})
		quiet := seed.Task(t, TaskAttentionFixture{Suffix: "attn-sort-quiet", Title: "aaa concluded", Status: "in_progress", Priority: 0.5})

		conv := seed.Conversation(t, needsYou, domain.StatusCompleted)
		seed.Artifact(t, conv, domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, "")
		seed.Conversation(t, quiet, domain.StatusCompleted)

		f := inProgress()
		f.SortKey, f.SortDir = db.TaskSortTitle, db.TaskSortDirAsc
		got, _ := list(t, s, orgID, f)
		if want := []string{needsYou, quiet}; !slices.Equal(got, want) {
			t.Errorf("title-ascending In Progress lane = %v, want %v — 'aaa' leads the tier below, not the lane", got, want)
		}
	})

	t.Run("AMixedReadStillEndsInRecency", func(t *testing.T) {
		// The unfiltered read — every lane at once — is the one query where the
		// tier and the closed tail meet. The tier orders the open rows, and the
		// closed tail stays a log: a stale closure still holding a draft PR
		// must not climb over a later one that left nothing behind, because
		// inside the closed partition the tier would otherwise be read before
		// the recency term.
		s, orgID, seed := mk(t)
		older := time.Now().UTC().Add(-48 * time.Hour)
		newer := time.Now().UTC().Add(-1 * time.Hour)
		openNeeds := seed.Task(t, TaskAttentionFixture{Suffix: "attn-mix-open-needs", Title: "open needs", Status: "in_progress", Priority: 0.1})
		openQuiet := seed.Task(t, TaskAttentionFixture{Suffix: "attn-mix-open-quiet", Title: "open quiet", Status: "in_progress", Priority: 0.9})
		closedStale := seed.Task(t, TaskAttentionFixture{Suffix: "attn-mix-done-stale", Title: "done stale", Status: "done", Priority: 0.5, ClosedAt: &older})
		closedRecent := seed.Task(t, TaskAttentionFixture{Suffix: "attn-mix-done-recent", Title: "done recent", Status: "done", Priority: 0.5, ClosedAt: &newer})

		needsConv := seed.Conversation(t, openNeeds, domain.StatusCompleted)
		seed.Artifact(t, needsConv, domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, "")
		seed.Conversation(t, openQuiet, domain.StatusCompleted)
		staleConv := seed.Conversation(t, closedStale, domain.StatusCompleted)
		seed.Artifact(t, staleConv, domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, "")
		seed.Conversation(t, closedRecent, domain.StatusCompleted)

		got, _ := list(t, s, orgID, db.TaskListFilter{})
		want := []string{openNeeds, openQuiet, closedRecent, closedStale}
		if !slices.Equal(got, want) {
			t.Errorf("unfiltered read = %v,\n                 want %v\n(the tier orders the open rows; the closed tail stays newest-first)", got, want)
		}
	})

	t.Run("TheDoneLaneKeepsItsRecency", func(t *testing.T) {
		// The gate. A closure still holding a draft PR matches the needs-you
		// predicate, so a tier that reached this lane would sort it above a
		// more recent closure and the Done column would stop reading as a log.
		s, orgID, seed := mk(t)
		older := time.Now().UTC().Add(-48 * time.Hour)
		newer := time.Now().UTC().Add(-1 * time.Hour)
		stale := seed.Task(t, TaskAttentionFixture{Suffix: "attn-done-stale", Title: "stale", Status: "done", Priority: 0.5, ClosedAt: &older})
		recent := seed.Task(t, TaskAttentionFixture{Suffix: "attn-done-recent", Title: "recent", Status: "done", Priority: 0.5, ClosedAt: &newer})

		conv := seed.Conversation(t, stale, domain.StatusCompleted)
		seed.Artifact(t, conv, domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, "")
		seed.Conversation(t, recent, domain.StatusCompleted)

		got, _ := list(t, s, orgID, db.TaskListFilter{Statuses: []string{"done"}})
		if want := []string{recent, stale}; !slices.Equal(got, want) {
			t.Errorf("Done lane = %v, want %v — newest closure first, whatever it left unresolved", got, want)
		}
	})
}
