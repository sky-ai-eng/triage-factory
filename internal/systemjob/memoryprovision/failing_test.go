package memoryprovision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/systemjob/systemllm"
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

// failFor sweeps the fixture's org once and ages the attempt to half of
// failingFor ago, so the next sweep's failure falls well short of the debounce
// however long the test takes to make it.
func (f *fixture) failFor() {
	f.t.Helper()
	f.mgr.sweep(f.t.Context(), 0, f.orgID)
	f.ageAttempts(failingFor / 2)
}

// spanFailing ages every attempt past the rest of failingFor, so the next
// sweep's failure spans the debounce.
func (f *fixture) spanFailing() {
	f.t.Helper()
	f.ageAttempts(failingFor/2 + time.Second)
}

// TestSyncPass_SyncsFailingGeneration: memory.generation_failing opens on the
// first pass after the org's failures span failingFor, whichever sweep made
// them, and the pass after an attempt generates the memory resolves it.
func TestSyncPass_SyncsFailingGeneration(t *testing.T) {
	rec := notifytest.Record(t)
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	f.mgr.models = func(context.Context, string) (string, error) {
		return systemllm.ModelForSettings(domain.OrgSettings{})
	}

	f.failFor()
	f.mgr.sweep(t.Context(), 0, "")
	f.mgr.syncPass(t.Context())
	rec.Inactive(t, f.orgID, kinds.MemoryGenerationFailing)

	// The ticker's fleet-wide sweep syncs nothing itself; the pass after it
	// does.
	f.spanFailing()
	f.mgr.sweep(t.Context(), 0, "")
	rec.Inactive(t, f.orgID, kinds.MemoryGenerationFailing)
	f.mgr.syncPass(t.Context())
	rec.Active(t, f.orgID, kinds.MemoryGenerationFailing, string(domain.MemoryAttemptErrNoModel))
	open := rec.Open(f.orgID, kinds.MemoryGenerationFailing)
	if len(open) != 1 {
		t.Fatalf("open = %+v; want the one no_model occurrence", open)
	}
	want := "the background jobs model setting is empty — pick one in Settings"
	if p, _ := open[0].Params.(kinds.MemoryFailure); p.TasksWaiting != 1 || p.ErrorMessage != want {
		t.Errorf("params = %+v, want the one waiting task and %q", open[0].Params, want)
	}

	f.mgr.models = testModels
	f.mgr.sweep(t.Context(), 0, "")
	if f.memory() == nil {
		t.Fatal("the sweep after the fix generates the memory")
	}
	f.mgr.syncPass(t.Context())
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

// TestSyncPass_SeesAPageStillRunning: a pass syncs an org while a page is still
// working the org's conversations, so a slow attempt later in the page does not
// hold back a condition an earlier one opened.
func TestSyncPass_SeesAPageStillRunning(t *testing.T) {
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
	waitFor(t, func() bool {
		f.mgr.syncPass(t.Context())
		return len(rec.Open(f.orgID, kinds.MemoryGenerationFailing)) == 1
	})
	close(released)
	<-done
	rec.Active(t, f.orgID, kinds.MemoryGenerationFailing, string(domain.MemoryAttemptErrProviderError))
}

// TestSyncLoop_ResolvesARowNoAttemptReaches: a row open in an org that owes
// nothing has no attempt left to touch the org, as on a brain that took over
// after the sync that would have resolved it never ran. The loop's pass
// reaches it through OpenOrgs.
func TestSyncLoop_ResolvesARowNoAttemptReaches(t *testing.T) {
	rec := notifytest.Record(t)
	f := newFixture(t)
	kinds.MemoryGenerationFailing.Sync(t.Context(), f.orgID, []kinds.MemoryFailure{{ErrorKind: domain.MemoryAttemptErrProviderError}})
	f.mgr.sweep(t.Context(), 0, "")
	rec.Active(t, f.orgID, kinds.MemoryGenerationFailing, string(domain.MemoryAttemptErrProviderError))

	// A second manager: the fixture's is already running, and its loop reads
	// syncEvery, so setting it there would race.
	syncing := f.newManager(nil)
	syncing.syncEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	syncing.Run(ctx, time.Hour)
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
