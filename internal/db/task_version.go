package db

import (
	"errors"
	"slices"
	"strings"
)

// ErrTaskVersion is a person's task write refused because the task changed
// since the caller read it: the version the write names is no longer the
// task's. Nothing was written. The caller re-reads the task and decides again
// against what changed; there is no server-side merge.
var ErrTaskVersion = errors.New("db: the task changed since the caller read it")

// TaskVersionColumns are the task columns a person acts on: the lifecycle,
// both claims, the wake time, the close, and the owning team. A write that
// changes any of them raises tasks.version. Everything else on the row (the
// scores and summaries, the scoring status, a held close, source_status) is
// maintained by the system for display or bookkeeping, and a write to it
// leaves the version where it is, so a scorer pass never makes a person's
// pending write stale.
var TaskVersionColumns = []string{
	"status",
	"claimed_by_user_id",
	"claimed_by_agent_id",
	"snooze_until",
	"closed_at",
	"close_reason",
	"close_event_type",
	"team_id",
}

// TaskAssign is one assignment in a task UPDATE's SET list: the column, and
// the SQL expression it is set to. Expr is evaluated against the row as it
// stood before the statement, as every SET expression is.
type TaskAssign struct {
	Col  string
	Expr string
}

// TaskSetSQL renders a task UPDATE's SET list (without the SET keyword) from
// its assignments, and appends the version term when any of them is to one
// of TaskVersionColumns. Every task writer builds its SET list here, in both
// dialects, which is what keeps the rule in one place: the version rises by
// one exactly when the statement changes one of those columns, compared
// column by column with IS DISTINCT FROM, which both dialects evaluate
// NULL-safely. A write that re-asserts a value the row already holds (a bump
// of a task that isn't snoozed, the in_progress stage of a task already in
// progress) leaves the version alone.
//
// The comparison repeats each expression, so a bind inside one appears twice
// in the statement: Postgres's $N and SQLite's ?N may repeat, a bare ? may not.
//
// The bump is not a trigger because SQLite's RETURNING reports the row as the
// statement wrote it, without a trigger's changes, and every task write's
// returned row has to carry the version it persisted.
func TaskSetSQL(assigns ...TaskAssign) string {
	parts := make([]string, 0, len(assigns)+1)
	var changed []string
	for _, a := range assigns {
		parts = append(parts, a.Col+" = "+a.Expr)
		if slices.Contains(TaskVersionColumns, a.Col) {
			changed = append(changed, a.Col+" IS DISTINCT FROM ("+a.Expr+")")
		}
	}
	if len(changed) > 0 {
		parts = append(parts, "version = version + CASE WHEN "+strings.Join(changed, "\n\t\t\t OR ")+" THEN 1 ELSE 0 END")
	}
	return strings.Join(parts, ",\n\t\t       ")
}

// RequeueTaskAssigns is what returning a task to the queue writes, in both
// dialects: the queued status with no wake time, no claim and the
// source_status the claim recorded with it, no close, and no held close.
// Every value is a literal, so it binds nothing.
var RequeueTaskAssigns = []TaskAssign{
	{Col: "status", Expr: "'queued'"},
	{Col: "snooze_until", Expr: "NULL"},
	{Col: "claimed_by_agent_id", Expr: "NULL"},
	{Col: "claimed_by_user_id", Expr: "NULL"},
	{Col: "source_status", Expr: "NULL"},
	{Col: "closed_at", Expr: "NULL"},
	{Col: "close_reason", Expr: "NULL"},
	{Col: "close_deferred_event_id", Expr: "NULL"},
	{Col: "close_deferred_at", Expr: "NULL"},
}

// ReopenTaskAssigns is what reopening a closed task writes, in both dialects:
// the status its claim implies (a claimed task is in progress, and the queue
// holds only unclaimed ones), no wake time, no close, and no held close. The
// claim, the owning team, and the scores stay as the close left them. Every
// value is read off the row or a literal, so it binds nothing.
var ReopenTaskAssigns = []TaskAssign{
	{Col: "status", Expr: "CASE WHEN claimed_by_agent_id IS NOT NULL OR claimed_by_user_id IS NOT NULL THEN 'in_progress' ELSE 'queued' END"},
	{Col: "snooze_until", Expr: "NULL"},
	{Col: "closed_at", Expr: "NULL"},
	{Col: "close_reason", Expr: "NULL"},
	{Col: "close_event_type", Expr: "NULL"},
	{Col: "close_deferred_event_id", Expr: "NULL"},
	{Col: "close_deferred_at", Expr: "NULL"},
}
