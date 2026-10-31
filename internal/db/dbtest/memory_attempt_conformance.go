package dbtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// MemoryAttemptStoreFactory is what a per-backend test file hands to
// RunMemoryAttemptStoreConformance. Returns the wired store, the orgID to
// scope every call, and the seeder that stages the FK chain the attempts row
// hangs off — conversations for the row itself, system_llm_runs for the
// spend-ledger link.
type MemoryAttemptStoreFactory func(t *testing.T) (store db.MemoryAttemptStore, orgID string, seed MemoryAttemptSeeder)

// MemoryAttemptSeeder stages the rows conversation_memory_attempts FKs but
// does not own. Each backend implements them against its own SQL, the same
// division TaskMemorySeeder draws.
type MemoryAttemptSeeder struct {
	// Conversation inserts the entity → event → task → blueprint_run →
	// conversation FK chain and returns the conversation id. suffix
	// discriminates per-subtest seeds so the unique indexes don't collide.
	Conversation func(t *testing.T, suffix string) (conversationID string)

	// SystemLLMRun inserts one system_llm_runs row and returns its id — a
	// valid target for the attempt's system_llm_run_id link.
	SystemLLMRun func(t *testing.T, suffix string) (systemLLMRunID string)

	// DanglingSystemLLMRunID returns a well-formed id that names no
	// system_llm_runs row: the shape the provisioner hands the store when the
	// spend-ledger insert (best-effort) never landed. The backends differ on
	// what "well-formed" means for their id column, which is why this is the
	// seeder's answer rather than a constant here.
	DanglingSystemLLMRunID func(t *testing.T) string

	// OwingConversation is Conversation ended with no memory row: one the
	// provisioner owes a memory, on a task that is open when taskOpen and done
	// otherwise.
	OwingConversation func(t *testing.T, suffix string, taskOpen bool) (conversationID string)

	// AgeAttempt rewrites an attempt's started_at, and its completed_at when
	// it has one, to `age` ago, so attempts made back to back have an order
	// that cannot tie.
	AgeAttempt func(t *testing.T, attemptID string, age time.Duration)
}

// RunMemoryAttemptStoreConformance is the shared assertion suite for any
// db.MemoryAttemptStore impl: the two writes round-trip as the rows they
// persisted, the close-out CAS fires exactly once, a dangling spend-ledger id
// stores NULL rather than failing the attempt's own record, and the outcome /
// error_kind vocabularies are refused at the door.
//
// There is no writer yet — the memory provisioner is a sibling ticket — so
// this suite is the ledger's whole contract until that lands.
func RunMemoryAttemptStoreConformance(t *testing.T, mk MemoryAttemptStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("begin_returns_the_stored_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		convID := seed.Conversation(t, "begin")

		begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("BeginAttemptSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "BeginAttemptSystem", begun, func() (*domain.MemoryAttempt, error) {
			return store.NewestAttemptForConversationSystem(ctx, orgID, convID)
		})

		// A fresh attempt is running: an id and a start, and nothing said yet
		// about how it went. The counters default rather than arriving from
		// the caller, which has not read the window at this point.
		if begun.ID == "" {
			t.Error("BeginAttemptSystem returned no id — the caller needs it to close the attempt out")
		}
		if begun.OrgID != orgID || begun.ConversationID != convID {
			t.Errorf("BeginAttemptSystem returned %+v, want org %s conversation %s", begun, orgID, convID)
		}
		if begun.StartedAt.IsZero() {
			t.Error("BeginAttemptSystem left started_at unstamped")
		}
		if begun.CompletedAt != nil {
			t.Errorf("a running attempt must have no completed_at, got %v", *begun.CompletedAt)
		}
		if begun.Outcome != "" || begun.ErrorKind != "" || begun.ErrorMessage != "" {
			t.Errorf("a running attempt must carry no verdict, got %+v", begun)
		}
		if begun.SystemLLMRunID != "" {
			t.Errorf("a running attempt must carry no spend-ledger link, got %q", begun.SystemLLMRunID)
		}
		if begun.WindowRowsTotal != 0 || begun.WindowRowsSent != 0 {
			t.Errorf("window counters default to 0, got %d/%d", begun.WindowRowsTotal, begun.WindowRowsSent)
		}
	})

	t.Run("complete_returns_the_stored_row", func(t *testing.T) {
		store, orgID, seed := mk(t)
		convID := seed.Conversation(t, "complete")
		runID := seed.SystemLLMRun(t, "complete")

		begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("BeginAttemptSystem: %v", err)
		}
		done, err := store.CompleteAttemptSystem(ctx, orgID, begun.ID,
			domain.MemoryAttemptGenerated, "", "", runID, 41, 12)
		if err != nil {
			t.Fatalf("CompleteAttemptSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "CompleteAttemptSystem", done, func() (*domain.MemoryAttempt, error) {
			return store.NewestAttemptForConversationSystem(ctx, orgID, convID)
		})

		if done.ID != begun.ID {
			t.Errorf("CompleteAttemptSystem moved the attempt: %s → %s", begun.ID, done.ID)
		}
		if done.CompletedAt == nil {
			t.Fatal("CompleteAttemptSystem left completed_at NULL")
		}
		if done.Outcome != domain.MemoryAttemptGenerated {
			t.Errorf("outcome = %q, want %q", done.Outcome, domain.MemoryAttemptGenerated)
		}
		if done.SystemLLMRunID != runID {
			t.Errorf("system_llm_run_id = %q, want the seeded ledger row %q", done.SystemLLMRunID, runID)
		}
		if done.WindowRowsTotal != 41 || done.WindowRowsSent != 12 {
			t.Errorf("window counters = %d/%d, want 41/12", done.WindowRowsTotal, done.WindowRowsSent)
		}
	})

	t.Run("every_outcome_and_error_kind_roundtrips", func(t *testing.T) {
		// Derived from the domain vocabularies rather than listed here, so a
		// value added to either set and never taught to a store fails on both
		// backends instead of passing unnoticed.
		store, orgID, seed := mk(t)
		for _, outcome := range domain.AllMemoryAttemptOutcomes() {
			if outcome == domain.MemoryAttemptFailed {
				continue
			}
			convID := seed.Conversation(t, "outcome-"+string(outcome))
			begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
			if err != nil {
				t.Fatalf("BeginAttemptSystem for %q: %v", outcome, err)
			}
			done, err := store.CompleteAttemptSystem(ctx, orgID, begun.ID, outcome, "", "", "", 3, 3)
			if err != nil {
				t.Fatalf("CompleteAttemptSystem %q: %v", outcome, err)
			}
			if done.Outcome != outcome {
				t.Errorf("outcome %q round-tripped as %q", outcome, done.Outcome)
			}
			if done.ErrorKind != "" || done.ErrorMessage != "" {
				t.Errorf("outcome %q must store no error columns, got %+v", outcome, done)
			}
		}
		for _, kind := range domain.AllMemoryAttemptErrorKinds() {
			convID := seed.Conversation(t, "kind-"+string(kind))
			begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
			if err != nil {
				t.Fatalf("BeginAttemptSystem for %q: %v", kind, err)
			}
			done, err := store.CompleteAttemptSystem(ctx, orgID, begun.ID,
				domain.MemoryAttemptFailed, kind, "the model was not reachable", "", 7, 0)
			if err != nil {
				t.Fatalf("CompleteAttemptSystem failed/%q: %v", kind, err)
			}
			if done.Outcome != domain.MemoryAttemptFailed || done.ErrorKind != kind {
				t.Errorf("failed/%q round-tripped as %q/%q", kind, done.Outcome, done.ErrorKind)
			}
			if done.ErrorMessage != "the model was not reachable" {
				t.Errorf("error_message = %q, want the wording the caller passed", done.ErrorMessage)
			}
		}
	})

	t.Run("complete_is_a_CAS_closed_out_once", func(t *testing.T) {
		store, orgID, seed := mk(t)
		convID := seed.Conversation(t, "cas")
		begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("BeginAttemptSystem: %v", err)
		}
		if _, err := store.CompleteAttemptSystem(ctx, orgID, begun.ID,
			domain.MemoryAttemptGenerated, "", "", "", 5, 5); err != nil {
			t.Fatalf("first CompleteAttemptSystem: %v", err)
		}

		// The second closer is told so rather than overwriting the first
		// verdict — which is the whole reason the UPDATE carries
		// `completed_at IS NULL`.
		_, err = store.CompleteAttemptSystem(ctx, orgID, begun.ID,
			domain.MemoryAttemptFailed, domain.MemoryAttemptErrTimeout, "too slow", "", 5, 0)
		if !errors.Is(err, db.ErrNoSuchMemoryAttempt) {
			t.Fatalf("second CompleteAttemptSystem err = %v, want ErrNoSuchMemoryAttempt", err)
		}
		stored, err := store.NewestAttemptForConversationSystem(ctx, orgID, convID)
		if err != nil || stored == nil {
			t.Fatalf("read back after the refused close-out: %v (%+v)", err, stored)
		}
		if stored.Outcome != domain.MemoryAttemptGenerated || stored.ErrorKind != "" {
			t.Errorf("the refused close-out still landed: %+v", stored)
		}
	})

	t.Run("complete_on_an_unknown_id_is_the_sentinel", func(t *testing.T) {
		store, orgID, seed := mk(t)
		// A well-formed id that names no row — the same shape the dangling
		// ledger id takes, reused here because the attempts id column has the
		// same type as the ledger's in both backends.
		_, err := store.CompleteAttemptSystem(ctx, orgID, seed.DanglingSystemLLMRunID(t),
			domain.MemoryAttemptEmpty, "", "", "", 0, 0)
		if !errors.Is(err, db.ErrNoSuchMemoryAttempt) {
			t.Fatalf("CompleteAttemptSystem on an unknown id err = %v, want ErrNoSuchMemoryAttempt", err)
		}
	})

	t.Run("dangling_spend_ledger_id_stores_NULL", func(t *testing.T) {
		// systemllm's recorder is best-effort, so the provisioner can hold an
		// id whose ledger row never landed. That must not cost it the record
		// of its own attempt, which is why the write binds the id through a
		// subselect instead of straight into the FK column.
		store, orgID, seed := mk(t)
		convID := seed.Conversation(t, "dangling")
		begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("BeginAttemptSystem: %v", err)
		}
		done, err := store.CompleteAttemptSystem(ctx, orgID, begun.ID,
			domain.MemoryAttemptGenerated, "", "", seed.DanglingSystemLLMRunID(t), 9, 9)
		if err != nil {
			t.Fatalf("CompleteAttemptSystem with a dangling ledger id: %v", err)
		}
		if done.SystemLLMRunID != "" {
			t.Errorf("system_llm_run_id = %q, want empty (the dangling id stores NULL)", done.SystemLLMRunID)
		}
		AssertWriteReturnedStoredRow(t, "CompleteAttemptSystem (dangling ledger id)", done, func() (*domain.MemoryAttempt, error) {
			return store.NewestAttemptForConversationSystem(ctx, orgID, convID)
		})
	})

	t.Run("empty_spend_ledger_id_stores_NULL", func(t *testing.T) {
		store, orgID, seed := mk(t)
		convID := seed.Conversation(t, "no-ledger")
		begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("BeginAttemptSystem: %v", err)
		}
		done, err := store.CompleteAttemptSystem(ctx, orgID, begun.ID,
			domain.MemoryAttemptEmpty, "", "", "", 2, 2)
		if err != nil {
			t.Fatalf("CompleteAttemptSystem with no ledger id: %v", err)
		}
		if done.SystemLLMRunID != "" {
			t.Errorf("system_llm_run_id = %q, want empty", done.SystemLLMRunID)
		}
	})

	t.Run("vocabulary_is_validated_at_the_door", func(t *testing.T) {
		store, orgID, seed := mk(t)
		convID := seed.Conversation(t, "vocab")
		begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("BeginAttemptSystem: %v", err)
		}

		cases := []struct {
			name    string
			outcome domain.MemoryAttemptOutcome
			errKind domain.MemoryAttemptErrorKind
			errMsg  string
		}{
			// The columns carry no CHECK, so the store door is the only thing
			// standing between a caller and a verdict nobody can read back.
			{"unknown outcome", "sideways", "", ""},
			{"empty outcome", "", "", ""},
			{"failed with no error_kind", domain.MemoryAttemptFailed, "", "something broke"},
			{"failed with an unknown error_kind", domain.MemoryAttemptFailed, "gremlins", ""},
			{"generated with an error_kind", domain.MemoryAttemptGenerated, domain.MemoryAttemptErrTimeout, ""},
			{"empty with an error_message", domain.MemoryAttemptEmpty, "", "something broke"},
		}
		for _, tc := range cases {
			if _, err := store.CompleteAttemptSystem(ctx, orgID, begun.ID,
				tc.outcome, tc.errKind, tc.errMsg, "", 0, 0); err == nil {
				t.Errorf("%s was accepted, want a refusal", tc.name)
			}
		}

		// Every refusal is a refusal: the attempt is still open afterwards.
		stored, err := store.NewestAttemptForConversationSystem(ctx, orgID, convID)
		if err != nil || stored == nil {
			t.Fatalf("read back after the refusals: %v (%+v)", err, stored)
		}
		if stored.CompletedAt != nil || stored.Outcome != "" {
			t.Errorf("a refused verdict landed anyway: %+v", stored)
		}
	})

	t.Run("newest_is_the_newest_and_absence_is_nil", func(t *testing.T) {
		store, orgID, seed := mk(t)
		convID := seed.Conversation(t, "newest")

		none, err := store.NewestAttemptForConversationSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("NewestAttemptForConversationSystem on a never-attempted conversation: %v", err)
		}
		if none != nil {
			t.Fatalf("never attempted must read as nil, got %+v", none)
		}

		first, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("first BeginAttemptSystem: %v", err)
		}
		if _, err := store.CompleteAttemptSystem(ctx, orgID, first.ID,
			domain.MemoryAttemptFailed, domain.MemoryAttemptErrProviderBackoff, "slow down", "", 6, 0); err != nil {
			t.Fatalf("CompleteAttemptSystem: %v", err)
		}
		second, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("second BeginAttemptSystem: %v", err)
		}

		got, err := store.NewestAttemptForConversationSystem(ctx, orgID, convID)
		if err != nil || got == nil {
			t.Fatalf("NewestAttemptForConversationSystem: %v (%+v)", err, got)
		}
		if got.ID != second.ID {
			t.Errorf("newest attempt = %s, want the retry %s", got.ID, second.ID)
		}

		// A second conversation's attempts are its own — the read is keyed on
		// the conversation, not on the org.
		otherID := seed.Conversation(t, "newest-other")
		other, err := store.NewestAttemptForConversationSystem(ctx, orgID, otherID)
		if err != nil {
			t.Fatalf("NewestAttemptForConversationSystem on the other conversation: %v", err)
		}
		if other != nil {
			t.Errorf("the other conversation read %+v, want nil", other)
		}
	})
	// attempt makes one attempt on convID, closes it out with outcome unless
	// outcome is empty, and ages it to `age` ago.
	attempt := func(t *testing.T, store db.MemoryAttemptStore, orgID string, seed MemoryAttemptSeeder, convID string,
		outcome domain.MemoryAttemptOutcome, kind domain.MemoryAttemptErrorKind, msg string, age time.Duration) {
		t.Helper()
		begun, err := store.BeginAttemptSystem(ctx, orgID, convID)
		if err != nil {
			t.Fatalf("BeginAttemptSystem: %v", err)
		}
		if outcome != "" {
			if _, err := store.CompleteAttemptSystem(ctx, orgID, begun.ID, outcome, kind, msg, "", 1, 1); err != nil {
				t.Fatalf("CompleteAttemptSystem %s/%s: %v", outcome, kind, err)
			}
		}
		seed.AgeAttempt(t, begun.ID, age)
	}
	fail := func(t *testing.T, store db.MemoryAttemptStore, orgID string, seed MemoryAttemptSeeder, convID string,
		kind domain.MemoryAttemptErrorKind, msg string, age time.Duration) {
		t.Helper()
		attempt(t, store, orgID, seed, convID, domain.MemoryAttemptFailed, kind, msg, age)
	}
	// failing reads with a two-minute debounce and a five-minute window.
	failing := func(t *testing.T, store db.MemoryAttemptStore, orgID string) domain.MemoryFailing {
		t.Helper()
		got, err := store.FailingSystem(ctx, orgID, 2*time.Minute, 5*time.Minute)
		if err != nil {
			t.Fatalf("FailingSystem: %v", err)
		}
		return got
	}
	assertKinds := func(t *testing.T, got domain.MemoryFailing, want ...domain.MemoryFailingKind) {
		t.Helper()
		if !slices.Equal(got.Kinds, want) {
			t.Errorf("Kinds = %+v, want %+v", got.Kinds, want)
		}
	}

	t.Run("failing_waits_for_the_failures_to_span_the_debounce", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conv := seed.OwingConversation(t, "debounce", true)
		fail(t, store, orgID, seed, conv, domain.MemoryAttemptErrTimeout, "first", 3*time.Minute)
		fail(t, store, orgID, seed, conv, domain.MemoryAttemptErrTimeout, "second", 2*time.Minute)

		got := failing(t, store, orgID)
		if got.TasksWaiting != 1 {
			t.Errorf("TasksWaiting = %d, want the one open task", got.TasksWaiting)
		}
		assertKinds(t, got)

		fail(t, store, orgID, seed, conv, domain.MemoryAttemptErrTimeout, "third", 30*time.Second)
		assertKinds(t, failing(t, store, orgID), domain.MemoryFailingKind{Kind: domain.MemoryAttemptErrTimeout, ErrorMessage: "third"})
	})

	t.Run("a_burst_across_conversations_does_not_span_the_debounce", func(t *testing.T) {
		// A blip fails every owed conversation within moments of each other,
		// however many there are. The span is the org's, though, so an
		// earlier failure on another conversation counts toward it.
		store, orgID, seed := mk(t)
		for i := range 4 {
			conv := seed.OwingConversation(t, fmt.Sprintf("burst-%d", i), true)
			fail(t, store, orgID, seed, conv, domain.MemoryAttemptErrProviderBackoff, "in backoff",
				time.Minute+time.Duration(i)*100*time.Millisecond)
		}
		assertKinds(t, failing(t, store, orgID))

		earlier := seed.OwingConversation(t, "earlier", true)
		fail(t, store, orgID, seed, earlier, domain.MemoryAttemptErrProviderError, "the model call failed", 3*time.Minute+30*time.Second)
		assertKinds(t, failing(t, store, orgID),
			domain.MemoryFailingKind{Kind: domain.MemoryAttemptErrProviderBackoff, ErrorMessage: "in backoff"},
			domain.MemoryFailingKind{Kind: domain.MemoryAttemptErrProviderError, ErrorMessage: "the model call failed"},
		)
	})

	t.Run("failing_kinds_are_the_ones_inside_the_window", func(t *testing.T) {
		store, orgID, seed := mk(t)
		// The no_model is more than the window before the newest failure, so
		// it is no longer what generation fails with.
		mixed := seed.OwingConversation(t, "mixed", true)
		fail(t, store, orgID, seed, mixed, domain.MemoryAttemptErrNoModel, "no model", 9*time.Minute)
		fail(t, store, orgID, seed, mixed, domain.MemoryAttemptErrProviderError, "the model call failed", 4*time.Minute)
		fail(t, store, orgID, seed, mixed, domain.MemoryAttemptErrProviderBackoff, "in backoff", 3*time.Minute)
		fail(t, store, orgID, seed, mixed, domain.MemoryAttemptErrProviderError, "no memory text", time.Minute)
		// A done task's conversation still owes its memory and still fails;
		// it is not a task waiting.
		closed := seed.OwingConversation(t, "closed", false)
		fail(t, store, orgID, seed, closed, domain.MemoryAttemptErrTimeout, "exceeded", 2*time.Minute)

		got := failing(t, store, orgID)
		if got.TasksWaiting != 1 {
			t.Errorf("TasksWaiting = %d, want the one open task", got.TasksWaiting)
		}
		assertKinds(t, got,
			domain.MemoryFailingKind{Kind: domain.MemoryAttemptErrProviderError, ErrorMessage: "no memory text"},
			domain.MemoryFailingKind{Kind: domain.MemoryAttemptErrTimeout, ErrorMessage: "exceeded"},
			domain.MemoryFailingKind{Kind: domain.MemoryAttemptErrProviderBackoff, ErrorMessage: "in backoff"},
		)
	})

	t.Run("a_memory_generated_anywhere_since_resets_the_debounce", func(t *testing.T) {
		store, orgID, seed := mk(t)
		stuck := seed.OwingConversation(t, "stuck", true)
		for _, age := range []time.Duration{6 * time.Minute, 5 * time.Minute, 4 * time.Minute} {
			fail(t, store, orgID, seed, stuck, domain.MemoryAttemptErrProviderError, "failed", age)
		}
		settled := seed.Conversation(t, "settled")
		attempt(t, store, orgID, seed, settled, domain.MemoryAttemptGenerated, "", "", 3*time.Minute)
		assertKinds(t, failing(t, store, orgID))

		fail(t, store, orgID, seed, stuck, domain.MemoryAttemptErrProviderError, "failed", 2*time.Minute+30*time.Second)
		fail(t, store, orgID, seed, stuck, domain.MemoryAttemptErrProviderError, "failed", time.Minute)
		assertKinds(t, failing(t, store, orgID))
		fail(t, store, orgID, seed, stuck, domain.MemoryAttemptErrProviderError, "again", 15*time.Second)
		assertKinds(t, failing(t, store, orgID), domain.MemoryFailingKind{Kind: domain.MemoryAttemptErrProviderError, ErrorMessage: "again"})
	})

	t.Run("a_running_attempt_is_neither_a_failure_nor_a_success", func(t *testing.T) {
		store, orgID, seed := mk(t)
		conv := seed.OwingConversation(t, "running", true)
		fail(t, store, orgID, seed, conv, domain.MemoryAttemptErrTimeout, "exceeded", 4*time.Minute)
		attempt(t, store, orgID, seed, conv, "", "", "", time.Minute)
		assertKinds(t, failing(t, store, orgID))

		fail(t, store, orgID, seed, conv, domain.MemoryAttemptErrTimeout, "exceeded", 30*time.Second)
		attempt(t, store, orgID, seed, conv, "", "", "", 0)
		assertKinds(t, failing(t, store, orgID), domain.MemoryFailingKind{Kind: domain.MemoryAttemptErrTimeout, ErrorMessage: "exceeded"})
	})

	t.Run("a_conversation_owing_nothing_is_not_failing", func(t *testing.T) {
		store, orgID, seed := mk(t)
		live := seed.Conversation(t, "live")
		for _, age := range []time.Duration{4 * time.Minute, time.Minute} {
			fail(t, store, orgID, seed, live, domain.MemoryAttemptErrTimeout, "exceeded", age)
		}
		got := failing(t, store, orgID)
		if got.TasksWaiting != 0 || len(got.Kinds) != 0 {
			t.Errorf("FailingSystem = %+v, want nothing: the conversation has not ended", got)
		}
	})

	t.Run("failing_refuses_a_negative_span_and_an_empty_window", func(t *testing.T) {
		store, orgID, _ := mk(t)
		if _, err := store.FailingSystem(ctx, orgID, -time.Second, time.Minute); !errors.Is(err, db.ErrInvalidMemoryFailingWindow) {
			t.Errorf("a negative span: err = %v, want ErrInvalidMemoryFailingWindow", err)
		}
		if _, err := store.FailingSystem(ctx, orgID, time.Minute, 0); !errors.Is(err, db.ErrInvalidMemoryFailingWindow) {
			t.Errorf("an empty window: err = %v, want ErrInvalidMemoryFailingWindow", err)
		}
	})
}
