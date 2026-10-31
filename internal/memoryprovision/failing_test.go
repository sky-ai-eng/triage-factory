package memoryprovision

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// TestSweep_SyncsFailingGeneration: memory.generation_failing opens once a
// conversation's memory has failed failingStreak attempts in a row, whichever
// sweep made the last of them, and a sweep whose attempt generates the memory
// resolves it.
func TestSweep_SyncsFailingGeneration(t *testing.T) {
	rec := notifytest.Record(t)
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	f.mgr.models = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("%w: nothing is picked", systemllm.ErrNoModel)
	}

	for i := 1; i < failingStreak; i++ {
		f.mgr.sweep(t.Context(), 0, f.orgID)
	}
	rec.Inactive(t, f.orgID, kinds.MemoryGenerationFailing)

	// The ticker's fleet-wide form syncs every org its page worked.
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

// TestNudge_SyncsFailingGeneration: a doorbell that settles the org's last
// owed memory resolves the condition, although no later sweep's page names
// the org.
func TestNudge_SyncsFailingGeneration(t *testing.T) {
	rec := notifytest.Record(t)
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	f.fake.err = fmt.Errorf("500 from upstream")
	for range failingStreak {
		f.mgr.sweep(t.Context(), 0, f.orgID)
	}
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
