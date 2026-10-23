package lease

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Manager drives one lease's acquire/renew/self-demote lifecycle against a
// Store. Construct with NewManager and run it with Run(ctx) — Run blocks
// until ctx is cancelled, so callers start it in its own goroutine.
//
// Two tickers, serviced by Run's single goroutine:
//   - renew ticks every renewInterval and does the DB work (attempt
//     acquisition when not holder, renew when holder).
//   - deadline ticks at a finer grain and ONLY checks the holder's own
//     monotonic clock against demoteDeadline — so a DB partition (Renew
//     erroring, not just returning renewed=false) is caught close to the
//     deadline itself, not up to one whole renewInterval late.
//
// A holder's renew tick also checks the deadline before it issues Renew and
// again after Renew returns, and every store call in tick is bounded by
// storeCallTimeout. A renewal blocked on a black-holed connection therefore
// holds the goroutine for at most one timeout, and the deadline is checked
// as soon as it returns, whichever ticker Run's select services next. While
// holding, the deadline is checked at least once per
// max(deadlineCheckInterval, storeCallTimeout); demoteMarginError reserves
// that much room under the TTL.
//
// A renewal that explicitly reports "0 rows matched" (someone else's
// TryAcquire already won) demotes immediately, on the spot — that's a
// definite signal, not an ambiguous one, so it doesn't wait for the
// deadline ticker.
type Manager struct {
	store    Store
	name     string
	holderID string

	renewInterval  time.Duration
	ttl            time.Duration
	demoteDeadline time.Duration

	// fenced reports whether this instance's identity fence has latched
	// (delegate.Spawner.IdentityFenced) — a fenced holder must stop
	// renewing and demote immediately, the same hazard as a partitioned
	// one (see the ticket's "identity-fence interaction" invariant).
	// Never nil (NewManager defaults it to "never fenced").
	fenced func() bool

	// onAcquire/onDemote run SYNCHRONOUSLY on Run's own goroutine — the
	// same one that services renewTicker/deadlineTicker (see Run/tick).
	// This is deliberate, not an oversight: making these async would let
	// a fast-following demote's onDemote race a stale promote's onAcquire
	// goroutine (e.g. a flapping lease), risking the brain ending up
	// "started" after this instance has already demoted — the exact
	// double-brain hazard this whole mechanism exists to prevent. A safe
	// async version would need its own term-fencing, which is more
	// machinery than warranted unless a caller actually needs to block.
	//
	// The cost of staying synchronous: onAcquire MUST be non-blocking
	// (fire off goroutines for anything that isn't a fast, synchronous
	// kickoff — see internal/app's startBrain). While onAcquire runs,
	// NOTHING renews and NOTHING re-checks the demote deadline; if it
	// ever ran longer than demoteDeadline, the very next deadline check
	// would self-demote this instance immediately on return, even though
	// its last DB-confirmed renewal (the winning TryAcquire, stamped with
	// its issue time — see promote) was perfectly healthy. promote/demote
	// time these calls and log loudly if one eats into that margin, as an
	// early-warning net rather than a behavior change.
	onAcquire func(term int64)
	onDemote  func(reason string)

	mu                    sync.RWMutex
	status                Status
	lastSuccessfulRenewal time.Time
}

// NewManager builds a Manager. Returns an error for knobs that fail
// demoteMarginError (KnobsFromEnv applies the same check on the env path;
// this is the belt-and-suspenders check for direct construction, e.g. from
// tests). fenced may be nil (treated as "never fenced" — local
// callers that don't wire identity-fence detection get plain lease
// semantics). onAcquire/onDemote may be nil (a Manager with no brain to
// drive, useful for pure election tests).
func NewManager(store Store, name, holderID string, renewInterval, ttl, demoteDeadline time.Duration, fenced func() bool, onAcquire func(term int64), onDemote func(reason string)) (*Manager, error) {
	if err := demoteMarginError(demoteDeadline, ttl); err != nil {
		return nil, err
	}
	if fenced == nil {
		fenced = func() bool { return false }
	}
	return &Manager{
		store:          store,
		name:           name,
		holderID:       holderID,
		renewInterval:  renewInterval,
		ttl:            ttl,
		demoteDeadline: demoteDeadline,
		fenced:         fenced,
		onAcquire:      onAcquire,
		onDemote:       onDemote,
		status:         Status{Name: name},
	}, nil
}

// Status returns a snapshot of this process's current view of the lease —
// safe to call from any goroutine, at any time (including before Run
// starts, where it reads the zero/not-holder state).
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// IsHolder reports whether this process currently believes it holds the
// lease. Safe from any goroutine.
func (m *Manager) IsHolder() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status.IsHolder
}

// deadlineCheckInterval derives the (finer-grained) deadline-check ticker
// period from the demote deadline itself, floored at 1s so a very tight
// test-only deadline doesn't spin. A fifth of the deadline gives up to
// ~5 checks before the deadline elapses, without adding a fourth env knob.
func deadlineCheckInterval(demoteDeadline time.Duration) time.Duration {
	d := demoteDeadline / 5
	if d < time.Second {
		d = time.Second
	}
	return d
}

// demoteMarginError rejects knobs under which a partitioned holder can still
// report itself holder when a standby becomes eligible to acquire. Once the
// demote deadline elapses, the next check runs within one
// deadlineCheckInterval if Run is idle in its select, or within one
// storeCallTimeout if Run is inside a hung Renew (tick checks on both sides
// of the call). A standby may acquire one TTL after the holder's last
// renewal committed, and the holder measures its deadline from that
// renewal's issue time, which is no later than the commit. So demoteDeadline
// plus the longer of the two terms must stay strictly under ttl, or a
// demoting holder and its successor can overlap (double-brain). Which term
// is larger depends on the knobs: at the defaults it is the check interval
// (3s against 2.5s), and with a 40s TTL over a 15s deadline it is the
// store-call timeout (5s against 3s). Above its 1s floor, storeCallTimeout is
// at most half the margin, so that term never causes a refusal on its own.
// Shared by NewManager
// and KnobsFromEnv so direct construction and the env path refuse the same
// configurations.
func demoteMarginError(demoteDeadline, ttl time.Duration) error {
	gap, term := deadlineCheckInterval(demoteDeadline), "deadline-check interval"
	if call := storeCallTimeout(ttl, demoteDeadline); call > gap {
		gap, term = call, "store-call timeout"
	}
	if demoteDeadline+gap >= ttl {
		return fmt.Errorf("lease demote deadline (%s) plus the %s (%s) must stay strictly under the takeover TTL (%s), because a partitioned holder can demote that late and a standby may acquire at the TTL; raise TF_LEASE_TTL_SEC or lower TF_LEASE_DEMOTE_SEC", demoteDeadline, term, gap, ttl)
	}
	return nil
}

// storeCallTimeout bounds a single DB call inside tick. tick and
// checkDemoteDeadline share Run's one goroutine, so an UNBOUNDED store call
// (a silently black-holed connection — packets vanish, no RST, so the driver
// never returns on its own) would block the whole select and stop the
// deadline check entirely, letting a partitioned holder keep the lease past
// TTL while a standby acquires (double-brain). Bounded, a hung Renew returns
// within this budget and tick checks the deadline as soon as it does, so the
// call delays the self-demote by at most one timeout; demoteMarginError
// counts that delay against the TTL. Sized at half the demote-vs-ttl margin
// and no more than demoteDeadline/3 — but never below a 1s floor, which
// takes PRECEDENCE for a very tight demote deadline (where demoteDeadline/3
// would fall under 1s) so a healthy-but-slow renewal isn't false-failed.
func (m *Manager) storeCallTimeout() time.Duration {
	return storeCallTimeout(m.ttl, m.demoteDeadline)
}

func storeCallTimeout(ttl, demoteDeadline time.Duration) time.Duration {
	d := (ttl - demoteDeadline) / 2
	if cap := demoteDeadline / 3; d > cap {
		d = cap
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Run drives the election loop until ctx is cancelled. On cancellation, a
// current holder demotes (best-effort — the process is shutting down
// either way) before returning. Row bootstrap (EnsureRow) is NOT a
// separate one-time step here — see tick, which retries it on every
// not-holder attempt so a transient failure on the very first call can't
// strand this manager never acquiring.
func (m *Manager) Run(ctx context.Context) {
	renewTicker := time.NewTicker(m.renewInterval)
	defer renewTicker.Stop()
	deadlineTicker := time.NewTicker(deadlineCheckInterval(m.demoteDeadline))
	defer deadlineTicker.Stop()

	m.tick(ctx) // attempt acquisition/renewal immediately at boot, not after the first interval
	for {
		select {
		case <-ctx.Done():
			m.demoteIfHolder("shutdown")
			return
		case <-renewTicker.C:
			m.tick(ctx)
		case <-deadlineTicker.C:
			m.checkDemoteDeadline()
		}
	}
}

// tick does one DB round-trip: renew if holder, else attempt acquisition.
// A holder also checks the monotonic self-demote deadline before and after
// its Renew; between renew ticks, checkDemoteDeadline runs on its own faster
// ticker in Run.
func (m *Manager) tick(ctx context.Context) {
	if m.fenced() {
		// A fenced instance must never hold, and never try to acquire —
		// it's provably unhealthy (superseded registry identity). If it's
		// currently holding, that's the same hazard as a partition:
		// demote now.
		m.demoteIfHolder("identity fenced")
		return
	}

	if m.IsHolder() {
		// Check the deadline on both sides of the Renew rather than leaving
		// it to the deadline ticker alone. When renewInterval is no longer
		// than storeCallTimeout, a hung Renew returns with both tickers
		// ready, and Run's select picks one at random: the deadline ticker
		// can lose several picks in a row, each one costing another hung
		// Renew. Checking here limits the delay to one call whatever the
		// select picks.
		if m.checkDemoteDeadline() {
			return
		}
		// Stamp the ISSUE time, not the post-return time. lastSuccessfulRenewal
		// feeds only time.Since (a local monotonic elapsed) — it is never
		// compared against the DB clock — so it just needs to be a conservative
		// LOWER bound on when this renewal took effect. The UPDATE executes
		// after we issue it, so measuring elapsed from issue-time can only
		// over-count, never under-count. Stamping post-return instead would
		// push the baseline later by the round-trip latency, letting a slow-
		// but-successful renewal make this holder look fresher than it is and
		// erode the demote-before-takeover margin.
		issuedAt := time.Now()
		rctx, cancel := context.WithTimeout(ctx, m.storeCallTimeout())
		renewed, err := m.store.Renew(rctx, m.name, m.holderID, m.currentTerm())
		cancel()
		if err != nil {
			leaseLog.Warn("lease renewal failed (transient?)", "name", m.name, "error", err)
			m.checkDemoteDeadline()
			return
		}
		if !renewed {
			// Definite signal: another instance's TryAcquire already won
			// the row. Demote on the spot rather than waiting for the
			// monotonic deadline.
			m.demote("renewal lost — another instance has taken the lease")
			return
		}
		m.mu.Lock()
		m.lastSuccessfulRenewal = issuedAt
		m.mu.Unlock()
		return
	}

	// Not holder: make sure the row exists before attempting acquisition.
	// Retried on EVERY tick here — not just once at Run's boot — because
	// EnsureRow is a cheap, idempotent INSERT ... ON CONFLICT DO NOTHING,
	// and skipping the retry is a real stuck state: if this is the first
	// pod ever to boot against a fresh leases table and its one EnsureRow
	// attempt hits a transient error, the row never gets created, and
	// TryAcquire's UPDATE against a nonexistent row matches zero rows —
	// returning (acquired=false, err=nil), indistinguishable from
	// "someone else's lease is fresh". Without retrying EnsureRow, this
	// manager would spin on that false signal forever, never acquiring
	// and never logging anything more alarming than "not acquired" (not
	// even an error) — silently stuck.
	//
	// TryAcquire always still runs below regardless of this call's
	// outcome: on the overwhelmingly common path (the row already exists
	// because some pod's EnsureRow succeeded before) this failure is
	// irrelevant to it, and on the empty-table path it just means another
	// harmless "not acquired" this tick, retried again next tick.
	ensureCtx, ensureCancel := context.WithTimeout(ctx, m.storeCallTimeout())
	ensureErr := m.store.EnsureRow(ensureCtx, m.name)
	ensureCancel()
	if ensureErr != nil {
		leaseLog.Warn("ensure lease row failed; retrying next tick", "name", m.name, "error", ensureErr)
	}

	acquireCtx, acquireCancel := context.WithTimeout(ctx, m.storeCallTimeout())
	// Stamped before the call for the same reason the Renew path stamps its
	// issue time: the acquisition sets renewed_at when it executes, after
	// this, so a deadline measured from here can only expire early, never
	// late.
	issuedAt := time.Now()
	term, acquired, err := m.store.TryAcquire(acquireCtx, m.name, m.holderID, m.ttl)
	acquireCancel()
	if err != nil {
		leaseLog.Warn("lease acquisition attempt failed", "name", m.name, "error", err)
		return
	}
	if acquired {
		m.promote(term, issuedAt)
		return
	}
	m.refreshObservedStatus(ctx)
}

// checkDemoteDeadline self-demotes a holder whose last successful renewal
// is older than demoteDeadline — the guard against a silent partition
// (Renew erroring or hanging every tick rather than explicitly losing).
// Right after promotion the baseline is the winning TryAcquire, so a holder
// with no renewals yet is not stale. Reports whether it demoted.
func (m *Manager) checkDemoteDeadline() bool {
	if !m.IsHolder() || m.sinceLastSuccess() < m.demoteDeadline {
		return false
	}
	m.demote(fmt.Sprintf("no successful renewal within the demote deadline (%s)", m.demoteDeadline))
	return true
}

func (m *Manager) currentTerm() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status.Term
}

func (m *Manager) sinceLastSuccess() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return time.Since(m.lastSuccessfulRenewal)
}

// promote records the acquisition and runs onAcquire. issuedAt is when the
// winning TryAcquire was issued, no later than the commit that set the
// row's renewed_at, so it serves as the first renewal baseline.
func (m *Manager) promote(term int64, issuedAt time.Time) {
	m.mu.Lock()
	m.status = Status{Name: m.name, HolderID: m.holderID, Term: term, IsHolder: true}
	m.lastSuccessfulRenewal = issuedAt
	m.mu.Unlock()
	leaseLog.Info("lease acquired", "name", m.name, "holder", m.holderID, "term", term)
	if m.onAcquire == nil {
		return
	}
	start := time.Now()
	m.onAcquire(term)
	m.warnIfCallbackAteDeadlineMargin("onAcquire", term, time.Since(start))
}

// warnIfCallbackAteDeadlineMargin logs when a synchronous onAcquire/
// onDemote call took long enough to threaten the demote-deadline
// guarantee — see the onAcquire/onDemote field doc for why these run
// synchronously on Run's own goroutine, and therefore why their duration
// directly competes with the renewal cadence that keeps a genuinely
// healthy holder from self-demoting. Pure observability: never changes
// behavior, never itself demotes anything.
func (m *Manager) warnIfCallbackAteDeadlineMargin(which string, term int64, elapsed time.Duration) {
	switch {
	case elapsed >= m.demoteDeadline:
		leaseLog.Error(which+" ran longer than the demote deadline; nothing renewed while it was running — the next deadline check may self-demote a perfectly healthy holder. This callback MUST stay non-blocking.",
			"name", m.name, "term", term, "elapsed", elapsed, "demote_deadline", m.demoteDeadline)
	case elapsed >= m.demoteDeadline/2:
		leaseLog.Warn(which+" is eating into the demote-deadline margin",
			"name", m.name, "term", term, "elapsed", elapsed, "demote_deadline", m.demoteDeadline)
	}
}

func (m *Manager) demoteIfHolder(reason string) {
	if m.IsHolder() {
		m.demote(reason)
	}
}

func (m *Manager) demote(reason string) {
	m.mu.Lock()
	wasHolder := m.status.IsHolder
	term := m.status.Term
	m.status.IsHolder = false
	m.mu.Unlock()
	if !wasHolder {
		return
	}
	leaseLog.Warn("lease lost; demoting", "name", m.name, "term", term, "reason", reason)
	if m.onDemote == nil {
		return
	}
	start := time.Now()
	m.onDemote(reason)
	// A slow onDemote doesn't risk a spurious self-demotion the way a
	// slow onAcquire does (this instance already reports IsHolder=false
	// before onDemote runs) — it only delays this goroutine's next
	// acquisition attempt. Timed anyway for symmetry and because a slow
	// stopBrain is still worth knowing about.
	m.warnIfCallbackAteDeadlineMargin("onDemote", term, time.Since(start))
}

// refreshObservedStatus keeps a standby's Status()'s HolderID/Term fresh
// (for /readyz) by reading the row directly, since a standby has no other
// signal for "who holds it right now". Best-effort — a read failure just
// leaves the last-known values in place.
func (m *Manager) refreshObservedStatus(ctx context.Context) {
	readCtx, cancel := context.WithTimeout(ctx, m.storeCallTimeout())
	holderID, term, err := m.store.Read(readCtx, m.name)
	cancel()
	if err != nil {
		return
	}
	m.mu.Lock()
	if !m.status.IsHolder {
		m.status.HolderID = holderID
		m.status.Term = term
	}
	m.mu.Unlock()
}
