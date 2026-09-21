package workitem

// Observer is told about every disposition the package commits, after the
// commit. It is how a metrics surface learns what the queue did without the
// package emitting anything itself: adopting a work kind installs an observer
// on its Kind, and the package stays out of the metrics pipeline.
//
// Every method is called with the org the row belongs to. None may block or
// fail, and a nil Observer on a Kind is a no-op everywhere.
//
// Rules the package keeps:
//
//   - A call happens after the write is durable as far as this package can
//     see. For an operation that opens its own transaction (Claim, Complete,
//     Defer) that is after the commit; a round of Claim that fails reports
//     nothing. For an operation run on the caller's DBTX the package cannot
//     see the commit, so it reports when the statement returns and the caller
//     owns the boundary — a disposition the caller then rolls back has been
//     reported anyway.
//   - Claim reports once per committed round per org with that org's counts,
//     because a cross-org claim's round mixes tenants and every call names one
//     org. Each row the round parked for a spent budget is also reported as
//     Parked with reason "budget_exhausted", and each cancellation it settled
//     as Cancelled; the counts on Claimed are the round's shape, not a second
//     count of either.
//   - A guard miss reports LeaseLost with the op that missed and nothing
//     else. Complete or MarkDone finding a cancellation request reports
//     Cancelled, never Completed.
//   - Park passes its reason through as the label, so callers pass constants.
type Observer interface {
	// Claimed is one committed claim round's shape for one org: rows leased,
	// of which reclaimed from an expired lease, and rows settled instead of
	// leased because they were cancelled or had spent their budget.
	Claimed(orgID string, leased, reclaimed, cancelled, parked int)
	// Completed is a Complete or MarkDone that flipped the row done.
	Completed(orgID string)
	// Requeued is a Requeue that returned the row to ready under outcome.
	Requeued(orgID string, outcome Outcome)
	// Parked is a row leaving circulation: a Requeue that parked (reason is
	// the outcome), a Park (reason is its argument), or a claim-time budget
	// park (reason is "budget_exhausted").
	Parked(orgID, reason string)
	// Deferred is a Defer that refunded the attempt and returned the row to
	// ready.
	Deferred(orgID string)
	// Cancelled is a cancellation request settled, at claim or by a holder.
	Cancelled(orgID string)
	// LeaseLost is a holder verb whose guard missed. op is the verb:
	// complete, mark_done, requeue, park, defer or renew.
	LeaseLost(orgID, op string)
	// Redriven is an operator redrive that moved a parked row.
	Redriven(orgID string)
	// Superseded is an operator supersede that settled a parked row.
	Superseded(orgID string)
}

// The op names LeaseLost reports. They are the verbs as an operator reads
// them, a closed vocabulary because they become a metric label.
const (
	OpComplete = "complete"
	OpMarkDone = "mark_done"
	OpRequeue  = "requeue"
	OpPark     = "park"
	OpDefer    = "defer"
	OpRenew    = "renew"
)

// ReasonBudgetExhausted is the Parked reason for a claim-time park of a row
// whose budget was already spent. It is the same string the row's
// last_outcome falls back to when no attempt ever reported one.
const ReasonBudgetExhausted = outcomeBudgetExhausted

// observe returns the Kind's observer, or a no-op when none is installed, so
// every call site reports unconditionally.
func (k Kind) observe() Observer {
	if k.Observer == nil {
		return noopObserver{}
	}
	return k.Observer
}

type noopObserver struct{}

func (noopObserver) Claimed(string, int, int, int, int) {}
func (noopObserver) Completed(string)                   {}
func (noopObserver) Requeued(string, Outcome)           {}
func (noopObserver) Parked(string, string)              {}
func (noopObserver) Deferred(string)                    {}
func (noopObserver) Cancelled(string)                   {}
func (noopObserver) LeaseLost(string, string)           {}
func (noopObserver) Redriven(string)                    {}
func (noopObserver) Superseded(string)                  {}
