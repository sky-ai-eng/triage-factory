package memoryprovision

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// failingFor is how long memory generation must keep failing before
// kinds.MemoryGenerationFailing opens: from the first failed attempt on a
// conversation still owing a memory, since the org last generated one, to the
// newest. A provider blip fails every owed conversation's attempt within
// moments, however many are owed, and never spans it; a provider that stays
// down, or a model nobody picked, does within about three attempts.
const failingFor = 2 * time.Minute

// failingWindow is how shortly before the newest failure a kind must have
// failed to stay open. An outage interleaves kinds, a provider's errors with
// the backoffs they trip, and each recurs well inside it; a kind that has
// stopped, no_model once a model is picked, resolves within it.
const failingWindow = 5 * time.Minute

// failingSyncEvery is how often syncLoop syncs, which bounds how far
// kinds.MemoryGenerationFailing trails the attempts that open or resolve it.
const failingSyncEvery = 30 * time.Second

// syncLoop syncs kinds.MemoryGenerationFailing every syncEvery until ctx is
// cancelled. It runs beside the sweep rather than inside it: a page whose
// attempts run to AttemptTimeout can take far longer than failingFor, and a
// sync that waited on the page would hold the condition back for all of it.
func (m *Manager) syncLoop(ctx context.Context) {
	t := time.NewTicker(m.syncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.syncPass(ctx)
		}
	}
}

// syncPass syncs every org an attempt has closed in since the last pass, and
// every org where kinds.MemoryGenerationFailing is open. Between them that is
// every org whose answer can have changed, by construction rather than by each
// caller remembering to sync: a failure lands in an org that still owes a
// memory, so its attempts go on closing until it opens, and a row that should
// resolve is open. Neither depends on which caller made the attempt, on a sync
// before this one having succeeded, or on this brain being the one that opened
// the row.
func (m *Manager) syncPass(ctx context.Context) {
	orgs := m.takeTouched()
	open, err := kinds.MemoryGenerationFailing.OpenOrgs(ctx)
	if err != nil {
		log.Warn("list orgs with failing memory generation failed; syncing only the orgs attempted since the last pass", "error", err)
	}
	for _, org := range open {
		orgs[org] = struct{}{}
	}
	for org := range orgs {
		if ctx.Err() != nil {
			return
		}
		m.syncFailing(ctx, org)
	}
}

// touch records that an attempt in orgID has closed.
func (m *Manager) touch(orgID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touched[orgID] = struct{}{}
}

// takeTouched returns the orgs touched since the last take and starts a new
// set, so an attempt that closes while a pass is reading lands in the next.
func (m *Manager) takeTouched() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	orgs := m.touched
	m.touched = map[string]struct{}{}
	return orgs
}

// syncFailing opens kinds.MemoryGenerationFailing for each error kind the
// org's memory generation is failing with and resolves the rest. A read that
// fails syncs nothing, so an open condition is neither bumped nor resolved on
// no evidence.
func (m *Manager) syncFailing(ctx context.Context, orgID string) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	f, err := m.stores.MemoryAttempts.FailingSystem(ctx, orgID, failingFor, failingWindow)
	if err != nil {
		log.Warn("read failing memory generation failed; leaving the condition as it is", "org", orgID, "error", err)
		return
	}
	kinds.MemoryGenerationFailing.Sync(ctx, orgID, kinds.MemoryFailuresFor(f))
}
