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
// Two independent tickers, deliberately decoupled:
//   - renew ticks every renewInterval and does the DB work (attempt
//     acquisition when not holder, renew when holder).
//   - deadline ticks at a finer grain and ONLY checks the holder's own
//     monotonic clock against demoteDeadline — so a DB partition (Renew
//     erroring, not just returning renewed=false) is caught close to the
//     deadline itself, not up to one whole renewInterval late.
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

	onAcquire func(term int64)
	onDemote  func(reason string)

	mu                    sync.RWMutex
	status                Status
	lastSuccessfulRenewal time.Time
}

// NewManager builds a Manager. Returns an error if demoteDeadline is not
// strictly less than ttl (KnobsFromEnv already enforces this on the env
// path; this is the belt-and-suspenders check for direct construction,
// e.g. from tests). fenced may be nil (treated as "never fenced" — local
// callers that don't wire identity-fence detection get plain lease
// semantics). onAcquire/onDemote may be nil (a Manager with no brain to
// drive, useful for pure election tests).
func NewManager(store Store, name, holderID string, renewInterval, ttl, demoteDeadline time.Duration, fenced func() bool, onAcquire func(term int64), onDemote func(reason string)) (*Manager, error) {
	if demoteDeadline >= ttl {
		return nil, fmt.Errorf("lease demote deadline (%s) must be strictly less than the takeover TTL (%s)", demoteDeadline, ttl)
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
// The monotonic self-demote deadline is NOT checked here — see
// checkDemoteDeadline, driven by its own faster ticker in Run.
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
		renewed, err := m.store.Renew(ctx, m.name, m.holderID, m.currentTerm())
		if err != nil {
			leaseLog.Warn("lease renewal failed (transient?)", "name", m.name, "error", err)
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
		m.lastSuccessfulRenewal = time.Now()
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
	if err := m.store.EnsureRow(ctx, m.name); err != nil {
		leaseLog.Warn("ensure lease row failed; retrying next tick", "name", m.name, "error", err)
	}

	term, acquired, err := m.store.TryAcquire(ctx, m.name, m.holderID, m.ttl)
	if err != nil {
		leaseLog.Warn("lease acquisition attempt failed", "name", m.name, "error", err)
		return
	}
	if acquired {
		m.promote(term)
		return
	}
	m.refreshObservedStatus(ctx)
}

// checkDemoteDeadline self-demotes a holder whose last successful renewal
// is older than demoteDeadline — the guard against a silent partition
// (Renew erroring every tick rather than explicitly losing) rather than
// the ambiguous zero-successful-renewals-yet state right after promotion.
func (m *Manager) checkDemoteDeadline() {
	if !m.IsHolder() {
		return
	}
	if m.sinceLastSuccess() >= m.demoteDeadline {
		m.demote(fmt.Sprintf("no successful renewal within the demote deadline (%s)", m.demoteDeadline))
	}
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

func (m *Manager) promote(term int64) {
	m.mu.Lock()
	m.status = Status{Name: m.name, HolderID: m.holderID, Term: term, IsHolder: true}
	m.lastSuccessfulRenewal = time.Now()
	m.mu.Unlock()
	leaseLog.Info("lease acquired", "name", m.name, "holder", m.holderID, "term", term)
	if m.onAcquire != nil {
		m.onAcquire(term)
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
	if m.onDemote != nil {
		m.onDemote(reason)
	}
}

// refreshObservedStatus keeps a standby's Status()'s HolderID/Term fresh
// (for /readyz) by reading the row directly, since a standby has no other
// signal for "who holds it right now". Best-effort — a read failure just
// leaves the last-known values in place.
func (m *Manager) refreshObservedStatus(ctx context.Context) {
	holderID, term, err := m.store.Read(ctx, m.name)
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
