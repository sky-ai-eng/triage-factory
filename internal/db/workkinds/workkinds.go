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

// The pending-firings kind's identity on the work-kind registry, declared
// beside the Kind for the same reason the event queue's is. A firing unit is
// a handful of store reads and one fenced insert, so a minute of ready
// backlog means the worker is not draining.
const (
	PendingFiringsName                 = "pending_firings"
	PendingFiringsLabel                = "Queued auto-delegations"
	PendingFiringsOldestReadyObjective = 60 * time.Second
	// PendingFiringDeferTaskBusy is the deferral reason a firing records
	// while its task holds a live conversation.
	PendingFiringDeferTaskBusy = "task_busy"
)

// PendingFirings is the router's per-task auto-delegation queue as a work
// kind: one row per (task, trigger) intent that could not fire when its
// event arrived because the task was busy.
//
// FencedReplay, on two named domain fences: (1) the blueprint-run insert is
// fenced on blueprint_runs (triggering_event_id, trigger_id), so a replay of
// a firing whose run committed returns delegate.ErrAlreadyFired and the row
// is skipped with that reason; (2) the one-active-run-per-task index refuses
// a second live run, returning delegate.ErrTaskBusy, which the worker turns
// into a deferral. A firing's only domain write is that insert, one
// transaction, so a replay either finds it committed or repeats it whole.
//
// UniqueWhileUnsettled keyed on (task, trigger): a parked firing holds its
// key, so a later event for the same pair collapses onto the parked row
// rather than minting a new one, and an operator redrives or cancels it.
//
// The claim filter is the per-task gate as the claim query applies it: a
// firing is claimable only while its task holds no live top-level
// conversation. A ready row behind a busy task is deferred, not ready, and
// becomes ripe when the task frees with no write of anyone's.
//
// MaxAttempts 5 with the package's default backoff: a fault that survives
// retries at roughly 5s, 10s, 20s and 40s is one a person has to look at.
// Lease and deadline are the event queue's, for the same reason — the unit
// is database writes, and spawner.Delegate is a pure enqueue. Fairness
// interleaves at batch granularity, which is why the worker claims ten at a
// time. Frozen is empty: the kind's columns are immutable after admission
// except skip_reason and fired_run_id, which only the terminal write sets,
// so the store reads them by id after Claim returns.
func PendingFirings(d workitem.Dialect) workitem.Kind {
	return workitem.Kind{
		Table:   "pending_firings",
		Dialect: d,
		Policy: workitem.Policy{
			MaxAttempts:  5,
			Lease:        60 * time.Second,
			RenewEvery:   20 * time.Second,
			UnitDeadline: 30 * time.Second,
			Fairness:     true,
		},
		Unique:      workitem.UniqueWhileUnsettled,
		Strategy:    workitem.FencedReplay,
		Columns:     []string{"entity_id", "task_id", "trigger_id", "triggering_event_id", "skip_reason", "fired_run_id"},
		ClaimFilter: PendingFiringsClaimFilter(d),
		Observer:    workmetrics.Observe(PendingFiringsName),
	}
}

// PendingFiringKey is the unique key one firing is admitted under: one per
// (task, trigger) while one is unsettled. The SQLite migration spells the
// same key in SQL for the rows it carries over, and a test holds the two to
// the same text.
func PendingFiringKey(taskID, triggerID string) string { return taskID + ":" + triggerID }

// PendingFiringsClaimFilter is the per-task gate as the claim query applies
// it: a firing is claimable only while its task holds no live top-level
// conversation. It is the conversation store's live-conversation predicate
// applied to the row's task, and each dialect package tests that the two
// keep answering the same question. Postgres binds the org as well because
// its conversations table is org-wide; SQLite is one org.
func PendingFiringsClaimFilter(d workitem.Dialect) string {
	org := ""
	if d == workitem.Postgres {
		org = "r.org_id = t.org_id AND "
	}
	return "NOT EXISTS (SELECT 1 FROM conversations r WHERE " + org +
		"r.task_id = t.task_id AND r.ended_at IS NULL AND r.parent_conversation_id IS NULL" +
		" AND (r.status IS NULL OR r.status NOT IN ('completed','failed')))"
}

// The score re-evaluation kind's identity on the work-kind registry, declared
// beside the Kind for the same reason the others are. A re-evaluation unit
// is a handful of store reads and one small transaction, so a minute of
// ready backlog means the worker is not draining.
const (
	TaskReDeriveName                 = "task_rederive_queue"
	TaskReDeriveLabel                = "Score re-evaluations"
	TaskReDeriveOldestReadyObjective = 60 * time.Second
	// TaskReDeriveDeferScoreMoved is the deferral reason a re-evaluation
	// records when a newer score landed after it was claimed.
	TaskReDeriveDeferScoreMoved = "score_moved"
)

// TaskReDerive is the post-scoring re-evaluation of a task's deferred
// triggers as a work kind: one row per task whose scores have landed and
// whose min_autonomy_suitability triggers have not yet been evaluated
// against them.
//
// SingleTx: the evaluation's effect is a pending_firings admission and the
// task's claim stamp, which is exactly what a completion closure can hold.
// The firing itself happens later, under the firings kind's own contract.
//
// Frozen requested_revision: the score write raises the row's
// requested_revision in the transaction that writes the scores, and the
// claim freezes the value into the receipt. Completion compares the frozen
// value with the row's current one under the row lock — equal commits the
// evaluation, higher commits nothing and defers — so an evaluation can never
// mark a newer score's obligation done on the strength of an older read.
// task_id is immutable after admission, so the store reads it by id after
// the claim, as the event queue reads its columns.
//
// UniqueWhileUnsettled keyed on the task id itself: a score landing while a
// row is ready, leased or parked raises that row in place instead of
// minting another, and a score landing after the row is done admits a fresh
// ready one.
//
// Timing and budget are the event queue's, for the same reason: the unit is
// reads and one small transaction, so a 30s deadline is an order of
// magnitude past a slow unit and every unit finishes inside the 60s lease
// by construction. Nothing renews on a timer; RenewEvery is declared
// because Validate requires a coherent value. Backoff is the package
// default, so a read that keeps failing retries at roughly 5s, 10s, 20s,
// 40s and then parks. Fairness interleaves at batch granularity, which is
// why the worker claims ten at a time.
func TaskReDerive(d workitem.Dialect) workitem.Kind {
	return workitem.Kind{
		Table:   "task_rederive_queue",
		Dialect: d,
		Policy: workitem.Policy{
			MaxAttempts:  5,
			Lease:        60 * time.Second,
			RenewEvery:   20 * time.Second,
			UnitDeadline: 30 * time.Second,
			Fairness:     true,
		},
		Unique:   workitem.UniqueWhileUnsettled,
		Strategy: workitem.SingleTx,
		Columns:  []string{"task_id", "requested_revision"},
		Frozen:   []string{"requested_revision"},
		Observer: workmetrics.Observe(TaskReDeriveName),
	}
}
