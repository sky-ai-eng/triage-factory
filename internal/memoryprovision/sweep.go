package memoryprovision

import (
	"context"
	"slices"
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
func (m *Manager) sweepLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweep(ctx, AttemptBackoff, "")
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
		// The sweep syncs only the orgs its page names, and a doorbell that
		// settled an org's last owed memory leaves it out of every page.
		if ctx.Err() == nil {
			m.syncFailing(ctx, orgID)
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
// After the page, each org it worked has kinds.MemoryGenerationFailing synced
// from the attempts it just made. A one-org sweep syncs its org even when the
// page is empty, so a re-sweep of an org that owes nothing resolves it.
func (m *Manager) sweep(ctx context.Context, backoff time.Duration, orgID string) {
	owed, err := m.stores.Conversations.ListMemoryOwedSystem(ctx, orgID, backoff, sweepLimit)
	if err != nil {
		log.Warn("list conversations owing a memory failed; retrying next tick", "error", err)
		return
	}
	var orgs []string
	if orgID != "" {
		orgs = append(orgs, orgID)
	}
	for _, o := range owed {
		if ctx.Err() != nil {
			return
		}
		if !slices.Contains(orgs, o.OrgID) {
			orgs = append(orgs, o.OrgID)
		}
		if err := m.Fulfil(ctx, o.OrgID, o.ConversationID); err != nil {
			log.Warn("settling an owed memory failed", "conversation", o.ConversationID, "org", o.OrgID, "error", err)
		}
	}
	for _, org := range orgs {
		if ctx.Err() != nil {
			return
		}
		m.syncFailing(ctx, org)
	}
}

// failingStreak is how many attempts in a row a conversation's memory must
// fail, with no memory generated anywhere in the org since the first of them,
// before kinds.MemoryGenerationFailing opens. The sweep retries a conversation
// once per AttemptBackoff, so three is a few minutes of failing: longer than a
// provider blip, which fails each owed conversation's attempt once however
// many there are, and short enough that an admin hears about blocked tasks
// within minutes.
const failingStreak = 3

// syncFailing opens kinds.MemoryGenerationFailing for each error kind the
// org's stuck conversations are failing with and resolves the rest. A read
// that fails syncs nothing, so an open condition is neither bumped nor
// resolved on no evidence.
func (m *Manager) syncFailing(ctx context.Context, orgID string) {
	f, err := m.stores.MemoryAttempts.FailingSystem(ctx, orgID, failingStreak)
	if err != nil {
		log.Warn("read failing memory generation failed; syncing next sweep", "org", orgID, "error", err)
		return
	}
	kinds.MemoryGenerationFailing.Sync(ctx, orgID, kinds.MemoryFailuresFor(f))
}
