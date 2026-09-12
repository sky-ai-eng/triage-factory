package dbtest

import (
	"context"
	"errors"
	"testing"

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
}
