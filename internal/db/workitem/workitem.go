// Package workitem is the shared contract for durable, leased work rows.
//
// A work item is a database row recording an obligation. A lease gives one
// worker temporary ownership of that row; each acquisition returns a Receipt
// carrying a new generation number, and every subsequent write must present
// that receipt. A delayed worker therefore cannot write after another worker
// has taken over, even when both carry the same instance identity.
//
// The package owns the shared column vocabulary, the SQL shapes for both
// dialects, and the typed outcomes. It never reads or writes a kind's own
// columns except through the allowlist a Kind declares, and it runs no
// goroutine, ticker or sweeper: recovery happens on the next Claim, which is
// what makes the contract testable without a scheduler.
//
// Adopting tables own their schema, their payload columns, and the indexes
// IndexDDL renders for them.
package workitem

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Dialect names the SQL dialect a Kind's table lives in. The zero value is
// unset and fails Validate, so a Kind literal that forgets the field cannot
// silently address the wrong dialect's syntax.
type Dialect int

const (
	DialectUnset Dialect = iota
	Postgres
	SQLite
)

func (d Dialect) String() string {
	switch d {
	case Postgres:
		return "postgres"
	case SQLite:
		return "sqlite"
	default:
		return "unset"
	}
}

// Strategy is how a kind makes repeated execution of its unit safe. It is
// declared once per kind and enforced: Complete refuses a FencedReplay kind
// and MarkDone refuses a SingleTx one, so the declaration is load-bearing
// rather than documentation.
type Strategy int

const (
	StrategyUnset Strategy = iota
	// SingleTx commits the domain mutation and the item's completion in one
	// transaction, through Complete.
	SingleTx
	// FencedReplay uses several transactions, each carrying the kind's own
	// domain fence, and completes the item with MarkDone.
	FencedReplay
)

func (s Strategy) String() string {
	switch s {
	case SingleTx:
		return "single_tx"
	case FencedReplay:
		return "fenced_replay"
	default:
		return "unset"
	}
}

// UniqueMode is how long an admitted unique_key stays reserved. The zero
// value reserves nothing, which is the mode that needs no index.
type UniqueMode int

const (
	// UniqueNone admits every row. A kind in this mode has no uniqueness
	// index, so Admit refuses a non-empty unique key rather than storing one
	// that dedups nothing.
	UniqueNone UniqueMode = iota
	// UniqueForever reserves the key across every row, terminal ones included.
	UniqueForever
	// UniqueWhileUnsettled reserves the key while the row is ready, leased or
	// parked. Parked work keeps holding its key, which is why a parked row is
	// redriven or superseded rather than left for a replacement to race.
	UniqueWhileUnsettled
)

// Outcome is the typed verdict on one attempt. It is what parking reasons and
// metrics key on, so it is a closed vocabulary rather than free text.
type Outcome string

const (
	OutcomeTransient       Outcome = "transient"
	OutcomeDependencyDown  Outcome = "dependency_down"
	OutcomePoisonSuspected Outcome = "poison_suspected"
	OutcomeDeadline        Outcome = "deadline"
	OutcomePermanent       Outcome = "permanent"
)

func (o Outcome) valid() bool {
	switch o {
	case OutcomeTransient, OutcomeDependencyDown, OutcomePoisonSuspected, OutcomeDeadline, OutcomePermanent:
		return true
	}
	return false
}

// Stored last_outcome values the package writes itself, outside the typed
// attempt outcomes above. They are listed here because they are the vocabulary
// an operator surface reads, and because Claim's budget park deliberately
// keeps whatever outcome preceded it rather than overwriting the failure that
// spent the budget.
const (
	outcomeDone            = "done"
	outcomeCancelled       = "cancelled"
	outcomeDeferred        = "deferred"
	outcomeRedriven        = "redriven"
	outcomeSuperseded      = "superseded"
	outcomeBudgetExhausted = "budget_exhausted"
)

// BackoffSpec places a failed attempt's retry. A zero value means every
// default: 5s base, 5m cap, ±25% jitter.
type BackoffSpec struct {
	Base   time.Duration
	Cap    time.Duration
	Jitter float64
}

// Default backoff values, applied field by field to a zero BackoffSpec.
const (
	DefaultBackoffBase   = 5 * time.Second
	DefaultBackoffCap    = 5 * time.Minute
	DefaultBackoffJitter = 0.25
)

func (b BackoffSpec) resolved() BackoffSpec {
	if b.Base <= 0 {
		b.Base = DefaultBackoffBase
	}
	if b.Cap <= 0 {
		b.Cap = DefaultBackoffCap
	}
	if b.Jitter == 0 {
		b.Jitter = DefaultBackoffJitter
	}
	return b
}

// Policy is a kind's timing and budget. Every field's zero value means its
// documented default, and defaults derive from the resolved Lease so a kind
// that only shortens the lease still gets a coherent renewal interval.
type Policy struct {
	// MaxAttempts is the attempt budget copied onto each row at admission.
	MaxAttempts int
	// Lease is how long an acquisition owns the row before it is reclaimable.
	Lease time.Duration
	// RenewEvery is how often a holder should renew. The package never runs a
	// timer; it validates the value and leaves the ticker to the worker.
	RenewEvery time.Duration
	// UnitDeadline is the ctx timeout a worker applies to one unit. Same deal:
	// validated here, applied by the worker, so the ordering rule that it stay
	// strictly inside the lease is checked in one place.
	UnitDeadline time.Duration
	Backoff      BackoffSpec
	// Fairness interleaves Claim across orgs by fewest currently leased rows.
	// False is FIFO by id.
	Fairness bool
}

// Default policy values.
const (
	DefaultMaxAttempts = 5
	DefaultLease       = 60 * time.Second
)

func (p Policy) resolved() Policy {
	if p.MaxAttempts == 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.Lease <= 0 {
		p.Lease = DefaultLease
	}
	if p.RenewEvery <= 0 {
		p.RenewEvery = p.Lease / 3
	}
	if p.UnitDeadline <= 0 {
		p.UnitDeadline = p.Lease / 2
	}
	p.Backoff = p.Backoff.resolved()
	return p
}

// Kind describes one adopting table to the package. It is a value, held by the
// adopting store and passed to every call; nothing here is stateful.
type Kind struct {
	// Table is the adopting table's name. It is interpolated into SQL, so it
	// is validated as a bare identifier.
	Table   string
	Dialect Dialect
	Policy  Policy
	Unique  UniqueMode
	// Strategy is the completion discipline. Complete and MarkDone each refuse
	// the other's kind.
	Strategy Strategy
	// Columns is the kind's own columns, and the allowlist Admit validates its
	// cols map against. A name outside it is an error, never interpolated.
	Columns []string
	// Frozen is the subset of Columns read into Receipt.Frozen at claim, where
	// the value is fixed for that generation whatever the row does afterwards.
	Frozen []string
}

// identRE is the shape a table or column name must have to be interpolated.
// Everything the package interpolates passes through it; everything else is a
// bind parameter.
var identRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// maxIdentLen leaves room for the longest index suffix IndexDDL appends inside
// Postgres's 63-byte identifier limit.
const maxIdentLen = 45

// blockColumns is the shared column vocabulary. The package's SQL references
// only these and the table name, which is what lets one set of builders serve
// every kind.
var blockColumns = []string{
	"id", "org_id", "status", "attempt", "max_attempts", "next_attempt_at",
	"lease_generation", "lease_owner", "lease_epoch", "leased_at", "lease_expires_at",
	"cancel_requested_at", "cancel_requested_by", "cancel_reason",
	"last_error", "last_outcome", "unique_key", "superseded_by",
	"first_enqueued_at", "created_at", "done_at",
}

func isBlockColumn(name string) bool {
	for _, c := range blockColumns {
		if c == name {
			return true
		}
	}
	return false
}

// Validate reports whether the Kind can be used. It checks the resolved policy
// rather than the literal one, so a zero Policy is legal and means defaults.
func (k Kind) Validate() error {
	if k.Table == "" {
		return errors.New("workitem: kind has no table")
	}
	if !identRE.MatchString(k.Table) || len(k.Table) > maxIdentLen {
		return fmt.Errorf("workitem: table %q is not a bare lower-case identifier of at most %d characters", k.Table, maxIdentLen)
	}
	if k.Dialect != Postgres && k.Dialect != SQLite {
		return fmt.Errorf("workitem: kind %s has dialect %s", k.Table, k.Dialect)
	}
	if k.Strategy != SingleTx && k.Strategy != FencedReplay {
		return fmt.Errorf("workitem: kind %s declares no execution strategy", k.Table)
	}
	switch k.Unique {
	case UniqueNone, UniqueForever, UniqueWhileUnsettled:
	default:
		return fmt.Errorf("workitem: kind %s has unknown unique mode %d", k.Table, k.Unique)
	}

	seen := make(map[string]bool, len(k.Columns))
	for _, c := range k.Columns {
		if !identRE.MatchString(c) {
			return fmt.Errorf("workitem: kind %s column %q is not a bare lower-case identifier", k.Table, c)
		}
		if isBlockColumn(c) {
			return fmt.Errorf("workitem: kind %s declares %q, which is a shared block column the package owns", k.Table, c)
		}
		if seen[c] {
			return fmt.Errorf("workitem: kind %s declares column %q twice", k.Table, c)
		}
		seen[c] = true
	}
	for _, f := range k.Frozen {
		if !seen[f] {
			return fmt.Errorf("workitem: kind %s freezes %q, which is not one of its declared columns", k.Table, f)
		}
	}

	p := k.Policy.resolved()
	if p.MaxAttempts < 1 {
		return fmt.Errorf("workitem: kind %s has MaxAttempts %d", k.Table, p.MaxAttempts)
	}
	if p.UnitDeadline >= p.Lease {
		return fmt.Errorf("workitem: kind %s has UnitDeadline %s at or beyond its %s lease", k.Table, p.UnitDeadline, p.Lease)
	}
	if p.RenewEvery >= p.Lease {
		return fmt.Errorf("workitem: kind %s has RenewEvery %s at or beyond its %s lease", k.Table, p.RenewEvery, p.Lease)
	}
	if p.Backoff.Jitter < 0 || p.Backoff.Jitter >= 1 {
		return fmt.Errorf("workitem: kind %s has backoff jitter %v outside [0,1)", k.Table, p.Backoff.Jitter)
	}
	return nil
}

// Owner identifies the process taking a lease. It is provenance: the write
// fence is the generation, never the owner.
type Owner struct {
	ID    string
	Epoch int64
}

// Receipt is the holder's authority over one acquisition. Every holder
// operation presents it, and a stale generation matches no rows.
type Receipt struct {
	ItemID          int64
	OrgID           string
	LeaseGeneration int64
	LeaseExpiresAt  time.Time
	Attempt         int
	UniqueKey       string
	// Frozen holds the kind's Frozen columns as read in the claim statement.
	// They are fixed for this generation; the row may move underneath.
	Frozen map[string]any
}

// ClaimResult is one Claim call's work. Claimed is in claim order — the pick's
// own ordering, so a fairness-ordered claim hands back a fairness-ordered
// batch. Settled rows yield no receipt and are counted instead.
type ClaimResult struct {
	Claimed   []Receipt
	Cancelled int
	Parked    int
}

// Depths is the queue's shape right now, for a metrics reader. Ready counts
// only rows eligible now; the rest of status='ready' is Deferred, so the two
// partition it rather than overlapping.
type Depths struct {
	Ready    int
	Leased   int
	Parked   int
	Deferred int
	// OldestReadyAge and OldestDeferredAge measure from first_enqueued_at —
	// the original enqueue, preserved across a redrive — so a redriven row
	// reports the age of the obligation rather than the age of the retry.
	// Zero when there is no such row.
	OldestReadyAge    time.Duration
	OldestDeferredAge time.Duration
}

var (
	// ErrLeaseLost is every guard failure: wrong generation, no longer leased,
	// or an expired lease. The package writes nothing else for that item.
	ErrLeaseLost = errors.New("workitem: lease lost")
	// ErrCancelled means a holder operation found a cancellation request and
	// settled the item instead of applying its own disposition.
	ErrCancelled = errors.New("workitem: cancelled")
	// ErrDeferRefused means the deferral predicate was false. No refund, and
	// the row is still leased — the holder must Requeue or let the lease lapse.
	ErrDeferRefused = errors.New("workitem: defer refused")
	// ErrNotCancellable means no ready or leased row without an existing
	// request matched.
	ErrNotCancellable = errors.New("workitem: not cancellable")
	// ErrNotParked means no parked row matched. A parked row with a pending
	// cancellation request is deliberately not redrivable; it is supersedable.
	ErrNotParked = errors.New("workitem: not parked")
	// ErrAdmissionRaced means a SQLite admission conflicted with a row that was
	// settled before its id could be read back — reachable only on a DBTX that
	// is neither a pool nor a transaction, since either of those bounds the two
	// statements. The insert is safe to retry.
	ErrAdmissionRaced = errors.New("workitem: admission raced a settling row")
)

// quoteLiteral renders a package-owned constant as a SQL string literal. It is
// only ever handed values from the closed vocabularies above, and it refuses a
// quote rather than escaping one, because a value needing an escape here would
// mean a caller's string reached a place only constants belong.
func quoteLiteral(s string) string {
	if strings.ContainsAny(s, "'\\") {
		panic("workitem: literal " + s + " is not a package constant")
	}
	return "'" + s + "'"
}
