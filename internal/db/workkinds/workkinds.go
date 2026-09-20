// Package workkinds declares the production work kinds — each adopting
// table's workitem.Kind, once, for both dialect packages to share.
//
// The declaration cannot live in internal/db beside the store interfaces,
// because a Kind value there would pull the package's SQL builders into every
// consumer of db; and it cannot live in either dialect package, because the
// other would then have to import its sibling or restate it. Each Kind
// carries its metrics observer, so the store that adopts the table needs no
// wiring to be counted.
package workkinds

import (
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/workmetrics"
)

// The event queue's identity on the work-kind registry: the path segment and
// metric label, the panel heading, and the age its oldest ready row may reach
// before the drain counts as behind. A routing unit is database writes, so a
// minute of ready backlog means the worker is not draining, not that a unit
// is slow. Declared beside the Kind so the observer's label and the handle's
// name cannot disagree.
const (
	EventQueueName                 = "event_queue"
	EventQueueLabel                = "Event routing"
	EventQueueOldestReadyObjective = 60 * time.Second
)

// EventQueue is the router's durable event queue as a work kind.
//
// FencedReplay, on three named domain fences: (1) task mint and bump are
// idempotent under the tasks partial unique index on (entity_id, event_type,
// dedup_key) for active rows; (2) trigger firing is fenced on blueprint_runs
// (triggering_event_id, trigger_id) plus the one-active-run index, with
// ErrTaskBusy deferring onto pending_firings; (3) the close phase is fenced on
// the entity's poll_seq (entity_poll_seq on the row, CloseTerminalSystem's
// guard) for terminating events, and on Tasks.Close being a guarded
// transition that no-ops on an already-closed row for typed closes. The
// events audit row is written at admission, not by the unit, so a replay
// never re-records it.
//
// UniqueWhileUnsettled is keyed only for the close obligation
// (EventQueueCloseOwedKey); ordinary events carry no key, because the
// snapshot diff is their dedup and the queue must never suppress a
// transition. A parked obligation holds the entity's key until an operator
// redrives it.
//
// A routing unit is database writes and enqueues, never an inline provider
// call or an agent process, so a 30s deadline is an order of magnitude past a
// slow unit and every unit finishes inside the 60s lease by construction.
// Nothing renews on a timer; RenewEvery is declared because Validate
// requires a coherent value. Backoff is the package default, so a transient
// fault gets retries at roughly 5s, 10s, 20s, 40s and then a park.
//
// Fairness interleaves at batch granularity under the single sequential
// worker: a batch from one org leaves it with ten leased rows, so the next
// pick prefers another. With a batch of one it would be inert, which is why
// the worker claims ten at a time.
//
// Frozen is empty: the kind's columns are immutable after admission, so the
// store reads them by id after Claim returns rather than through the
// receipt, which sidesteps how each driver scans a uuid into any.
func EventQueue(d workitem.Dialect) workitem.Kind {
	return workitem.Kind{
		Table:   "event_queue",
		Dialect: d,
		Policy: workitem.Policy{
			MaxAttempts:  5,
			Lease:        60 * time.Second,
			RenewEvery:   20 * time.Second,
			UnitDeadline: 30 * time.Second,
			Fairness:     true,
		},
		Unique:   workitem.UniqueWhileUnsettled,
		Strategy: workitem.FencedReplay,
		Columns:  []string{"event_id", "entity_id", "event_type", "traceparent", "entity_poll_seq"},
		Observer: workmetrics.Observe(EventQueueName),
	}
}

// EventQueueCloseOwedKey is the unique key a close obligation is admitted
// under: one per entity while one is unsettled.
func EventQueueCloseOwedKey(entityID string) string {
	return "close_owed:" + entityID
}
