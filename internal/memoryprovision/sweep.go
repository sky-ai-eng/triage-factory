package memoryprovision

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// Run starts the manager under ctx: it arms the doorbell and spawns the
// backstop sweep, both of which stop when ctx is cancelled — brain-owned and
// started/stopped with the rest of the brain exactly like
// credprovision.RunAwaitingSweep. A nil Manager is a no-op, the same
// nil-checked shape every other brain-unit member uses.
//
// Arming is this call's own work rather than the spawned loop's, so a doorbell
// rung the instant startBrain returns finds a running manager: the gate the
// ringer passes (internal/app's isBrainHolder) flips with the lease, not with
// whenever a goroutine first gets scheduled, and a nudge landing in that gap
// would be dropped by a brain that is in fact running.
//
// Everything the manager generates hangs off ctx, the doorbell's attempts as
// much as the sweep's. That is what makes a demoted holder provably done
// generating before its successor starts: the successor's sweep sees the same
// debt and, once the backoff ages out, begins its own attempt on it.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()
	go m.sweepLoop(ctx, interval)
}

// sweepLoop settles every conversation owing a memory, on a ticker, until ctx
// is cancelled.
//
// It is the completion path, not the fast one: the doorbell shortens the wait
// from a boundary to its memory, but the relay is lossy by contract and this
// is what makes the debt settle anyway. Without a doorbell at all, latency is
// one interval.
//
// It also resyncs kinds.MemoryGenerationFailing every resyncEvery in every org
// where it is open. A sweep syncs only the orgs its page names, which are the
// orgs owing a memory, so an org whose last owed memory settled with no sync
// after it would keep its row open: the doorbell's attempts sync nothing, and
// a sync can fail or be cut off by a demotion. The resync is what makes every
// open row resolve, and on a successor brain as much as on this one.
func (m *Manager) sweepLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	resync := time.NewTicker(m.resyncEvery)
	defer resync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweep(ctx, AttemptBackoff, "")
		case <-resync.C:
			m.resyncFailing(ctx)
		}
	}
}

// running returns the context the manager was last started under, or nil when
// it is not running — never started, or started by a brain since demoted. One
// answer for both, because a caller does the same thing with either: this pod
// is not the one that should be generating.
func (m *Manager) running() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base == nil || m.base.Err() != nil {
		return nil
	}
	return m.base
}

// Nudge is the doorbell: a boundary that just ended a conversation, or a
// configuration save that may have fixed what the last attempts failed on.
//
// With a conversation id it settles that one. With an empty id it re-sweeps
// the org ignoring the backoff — which is what makes a save of the
// background-jobs model or an LLM credential the re-kick for every task that
// setting left waiting, rather than a wait for the backoff to age out.
//
// An empty orgID is refused, and that is load-bearing rather than defensive:
// sweep treats an empty org as EVERY org, so a doorbell that reached it would
// turn one notification into a fleet-wide pass with the backoff disabled —
// amplifying a single malformed relay message into every tenant's backlog. No
// caller here ever legitimately wants that; the ticker's fleet-wide scope calls
// sweep directly. Refused at the CONSUMER, for the same reason
// ai.Manager.Trigger refuses its own empty org: a relay dispatch hands on
// whatever arrived on the wire, so the door that knows what an empty value
// would mean is the one that has to say no.
//
// Fire-and-forget, and bounded twice: by nudgeTimeout, because the caller is a
// relay dispatch that must not block on a model call or leak a goroutine per
// notification; and by the context the manager is RUNNING under, because a
// generation that outlived its brain is the second one — the successor sweeps
// the same debt. So a nudge to a manager that is not running is dropped rather
// than given a context of its own: the sweep is the completion path, and the
// relay only ever rings the holder.
func (m *Manager) Nudge(orgID, conversationID string) {
	if m == nil {
		return
	}
	if orgID == "" {
		log.Warn("memory doorbell: empty org; dropping the nudge", "conversation", conversationID)
		return
	}
	base := m.running()
	if base == nil {
		log.Warn("memory doorbell: the provisioner is not running; dropping the nudge",
			"org", orgID, "conversation", conversationID)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(base, nudgeTimeout)
		defer cancel()
		if conversationID == "" {
			m.sweep(ctx, 0, orgID)
			return
		}
		if err := m.Fulfil(ctx, orgID, conversationID); err != nil {
			log.Warn("memory doorbell: settling the owed memory failed", "conversation", conversationID, "org", orgID, "error", err)
		}
	}()
}

// sweep works one page of owed conversations. backoff is how recently a
// conversation must have been attempted for the read to skip it — zero takes
// everything, which is the doorbell's org-wide form. orgID empty is every org,
// which is the ticker's; one org's configuration save is not a reason to
// re-sweep every other org's backlog ahead of schedule.
//
// Both scopes are the read's, never a filter over its result: the page is
// capped, so an org's rows taken out of the fleet's first hundred are not that
// org's page — on a busy deployment they can be none of them while that org
// owes plenty.
//
// Serial by design: the page is ordered open tasks first and then oldest
// boundary first, which is the order a person waiting would choose, and
// running it in parallel would spend an org's whole background budget on a
// backlog that nobody is watching all of.
//
// Each org the page works has kinds.MemoryGenerationFailing synced as the page
// goes, at most once per pageSyncEvery, and again when the page ends if it was
// worked since: a page whose attempts run to AttemptTimeout can take far longer
// than failingFor, and syncing only at its end would hold the condition back
// for all of it. A one-org sweep syncs its org even when the page is empty, so
// a re-sweep of an org that owes nothing resolves it.
func (m *Manager) sweep(ctx context.Context, backoff time.Duration, orgID string) {
	owed, err := m.stores.Conversations.ListMemoryOwedSystem(ctx, orgID, backoff, sweepLimit)
	if err != nil {
		log.Warn("list conversations owing a memory failed; retrying next tick", "error", err)
		return
	}
	// synced is when each org was last synced, and unsynced the orgs worked
	// since.
	synced := map[string]time.Time{}
	unsynced := map[string]bool{}
	if orgID != "" {
		unsynced[orgID] = true
	}
	for _, o := range owed {
		if ctx.Err() != nil {
			return
		}
		if err := m.Fulfil(ctx, o.OrgID, o.ConversationID); err != nil {
			log.Warn("settling an owed memory failed", "conversation", o.ConversationID, "org", o.OrgID, "error", err)
		}
		unsynced[o.OrgID] = true
		if ctx.Err() == nil && time.Since(synced[o.OrgID]) >= pageSyncEvery {
			m.syncFailing(ctx, o.OrgID)
			synced[o.OrgID] = time.Now()
			delete(unsynced, o.OrgID)
		}
	}
	for org := range unsynced {
		if ctx.Err() != nil {
			return
		}
		m.syncFailing(ctx, org)
	}
}

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

// pageSyncEvery is how often a sweep syncs an org while it is still working
// that org's conversations.
const pageSyncEvery = 30 * time.Second

// failingResyncEvery is how often sweepLoop resyncs every org where
// kinds.MemoryGenerationFailing is open, which bounds how long a row outlives
// the failures that opened it once no sweep reaches its org.
const failingResyncEvery = time.Minute

// resyncFailing syncs every org where kinds.MemoryGenerationFailing is open.
// A list that fails is retried on the next tick.
func (m *Manager) resyncFailing(ctx context.Context) {
	orgs, err := kinds.MemoryGenerationFailing.OpenOrgs(ctx)
	if err != nil {
		log.Warn("list orgs with failing memory generation failed; resyncing next tick", "error", err)
		return
	}
	for _, org := range orgs {
		if ctx.Err() != nil {
			return
		}
		m.syncFailing(ctx, org)
	}
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
