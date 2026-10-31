package memoryprovision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// ageAttempts moves every attempt in the fixture's database d into the past,
// as the backoff between sweeps would have.
func (f *fixture) ageAttempts(d time.Duration) {
	f.t.Helper()
	rows, err := f.database.Query(`SELECT id, started_at, completed_at FROM conversation_memory_attempts`)
	if err != nil {
		f.t.Fatalf("read attempts: %v", err)
	}
	type times struct {
		id        string
		started   time.Time
		completed *time.Time
	}
	var all []times
	for rows.Next() {
		var a times
		if err := rows.Scan(&a.id, &a.started, &a.completed); err != nil {
			f.t.Fatalf("scan attempt: %v", err)
		}
		all = append(all, a)
	}
	_ = rows.Close()
	for _, a := range all {
		var completed any
		if a.completed != nil {
			completed = a.completed.Add(-d).UTC()
		}
		if _, err := f.database.Exec(`UPDATE conversation_memory_attempts SET started_at = ?, completed_at = ? WHERE id = ?`,
			a.started.Add(-d).UTC(), completed, a.id); err != nil {
			f.t.Fatalf("age attempt: %v", err)
		}
	}
}

// failFor sweeps the fixture's org once and ages the attempt to a second short
// of failingFor ago, so the next sweep's failure falls just short of the
// debounce.
func (f *fixture) failFor() {
	f.t.Helper()
	f.mgr.sweep(f.t.Context(), 0, f.orgID)
	f.ageAttempts(failingFor - time.Second)
}

// TestSweep_SyncsFailingGeneration: memory.generation_failing opens once the
// org's failures span failingFor, whichever sweep made the last of them, and a
// sweep whose attempt generates the memory resolves it.
func TestSweep_SyncsFailingGeneration(t *testing.T) {
	rec := notifytest.Record(t)
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	f.mgr.models = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("%w: nothing is picked", systemllm.ErrNoModel)
	}

	f.failFor()
	f.mgr.sweep(t.Context(), 0, f.orgID)
	rec.Inactive(t, f.orgID, kinds.MemoryGenerationFailing)

	// The ticker's fleet-wide form syncs every org its page worked.
	f.ageAttempts(2 * time.Second)
	f.mgr.sweep(t.Context(), 0, "")
	rec.Active(t, f.orgID, kinds.MemoryGenerationFailing, string(domain.MemoryAttemptErrNoModel))
	open := rec.Open(f.orgID, kinds.MemoryGenerationFailing)
	if p, _ := open[0].Params.(kinds.MemoryFailure); p.TasksWaiting != 1 || !strings.Contains(p.ErrorMessage, "nothing is picked") {
		t.Errorf("params = %+v, want the one waiting task and the newest attempt's message", open[0].Params)
	}

	f.mgr.models = testModels
	f.mgr.sweep(t.Context(), 0, f.orgID)
	if f.memory() == nil {
		t.Fatal("the sweep after the fix generates the memory")
	}
	rec.Inactive(t, f.orgID, kinds.MemoryGenerationFailing)
}

// holdingCompleter holds the completion for one conversation until released,
// and passes every other call through.
type holdingCompleter struct {
	inner    completer
	hold     string
	released chan struct{}
}

func (h holdingCompleter) Complete(ctx context.Context, opts systemllm.CompleteOptions) (*systemllm.CompleteResult, error) {
	if opts.Metadata["conversation_id"] == h.hold {
		select {
		case <-h.released:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.inner.Complete(ctx, opts)
}

// TestSweep_SyncsAsThePageGoes: a page syncs an org while it is still working
// the org's conversations, so a slow attempt later in the page does not hold
// back a condition an earlier one opened.
func TestSweep_SyncsAsThePageGoes(t *testing.T) {
	rec := notifytest.Record(t)
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	later := "conv-owing-later"
	dbtest.SeedConversation(t, f.database, domain.Conversation{ID: later, TaskID: f.taskID, Model: testModel})
	if _, err := f.stores.Conversations.InsertMessageSystem(t.Context(), f.orgID, &domain.Message{
		ConversationID: later, Role: roleAssistant, Content: "worked on it too",
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := f.stores.Conversations.EndConversationSystem(t.Context(), f.orgID, later, domain.EndedRequeued); err != nil {
		t.Fatalf("end conversation: %v", err)
	}
	f.fake.err = errors.New("500 from upstream")
	released := make(chan struct{})
	f.mgr.complete = holdingCompleter{inner: f.fake, hold: later, released: released}

	// An earlier failure on the first conversation, so its next one spans
	// the debounce.
	attempt, err := f.stores.MemoryAttempts.BeginAttemptSystem(t.Context(), f.orgID, f.conversationID)
	if err != nil {
		t.Fatalf("BeginAttemptSystem: %v", err)
	}
	if _, err := f.stores.MemoryAttempts.CompleteAttemptSystem(t.Context(), f.orgID, attempt.ID,
		domain.MemoryAttemptFailed, domain.MemoryAttemptErrProviderError, "the model call failed", "", 1, 1); err != nil {
		t.Fatalf("CompleteAttemptSystem: %v", err)
	}
	f.ageAttempts(failingFor + time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.mgr.sweep(t.Context(), 0, "")
	}()
	waitFor(t, func() bool { return len(rec.Open(f.orgID, kinds.MemoryGenerationFailing)) == 1 })
	close(released)
	<-done
	rec.Active(t, f.orgID, kinds.MemoryGenerationFailing, string(domain.MemoryAttemptErrProviderError))
}

// TestNudge_SyncsFailingGeneration: a doorbell that settles the org's last
// owed memory resolves the condition, although no later sweep's page names
// the org.
func TestNudge_SyncsFailingGeneration(t *testing.T) {
	rec := notifytest.Record(t)
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	f.fake.err = errors.New("500 from upstream")
	f.failFor()
	f.mgr.sweep(t.Context(), 0, f.orgID)
	f.ageAttempts(2 * time.Second)
	f.mgr.sweep(t.Context(), 0, f.orgID)
	rec.Active(t, f.orgID, kinds.MemoryGenerationFailing, string(domain.MemoryAttemptErrProviderError))

	f.fake.err = nil
	f.mgr.Nudge(f.orgID, f.conversationID)
	waitFor(t, func() bool { return len(rec.Open(f.orgID, kinds.MemoryGenerationFailing)) == 0 })
}

// TestSweep_OneOrgSweepThatOwesNothingResolves: "Retry now" rings the org-wide
// doorbell, and an org with nothing owed has nothing failing.
func TestSweep_OneOrgSweepThatOwesNothingResolves(t *testing.T) {
	rec := notifytest.Record(t)
	f := newFixture(t)
	kinds.MemoryGenerationFailing.Sync(t.Context(), f.orgID, []kinds.MemoryFailure{{ErrorKind: domain.MemoryAttemptErrTimeout}})
	rec.Active(t, f.orgID, kinds.MemoryGenerationFailing)

	f.mgr.sweep(t.Context(), 0, f.orgID)
	rec.Inactive(t, f.orgID, kinds.MemoryGenerationFailing)
}
