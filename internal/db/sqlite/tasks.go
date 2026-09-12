package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// taskStore is the SQLite impl of db.TaskStore. SQL bodies are moved
// verbatim from the pre-D2 internal/db/tasks.go; the only behavioral
// change is the orgID assertion at each method entry. Local mode is
// single-tenant by design — the orgID column on the SQLite tasks row
// defaults to LocalDefaultOrgID so we don't need to thread it through
// every UPDATE/INSERT, but rejecting an unexpected orgID at the entry
// point is the safety net for a caller that's confused about which
// mode it's in.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl, but SQLite has one connection — both
// arguments collapse onto the same queryer. The `...System` admin-
// pool variants are thin wrappers around the non-System methods.
type taskStore struct{ q queryer }

func newTaskStore(q, _ queryer) db.TaskStore { return &taskStore{q: q} }

var _ db.TaskStore = (*taskStore)(nil)

// --- Lookup ---

func (s *taskStore) Get(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var t domain.Task
	err := scanTaskFromRow(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.id = ?
	`, taskID), &t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// sqliteTaskTeamFilter mirrors pgTaskTeamFilter: it narrows a tasks
// query (alias t) to a *set* of teams — owning team_id OR a task_teams
// visibility row — appending the ids twice (SQLite binds ? positionally,
// so the two IN-lists each need their own copy). Empty teamIDs is a
// no-op. The fragment is always emitted last in the WHERE (before ORDER
// BY), so appending the args keeps them in placeholder order. Local mode
// is N=1 (one team) so this is effectively inert, but it keeps the SQLite
// store conformant with the interface contract.
func sqliteTaskTeamFilter(teamIDs []string, args []any) (string, []any) {
	if len(teamIDs) == 0 {
		return "", args
	}
	ph := strings.TrimRight(strings.Repeat("?, ", len(teamIDs)), ", ")
	for _, id := range teamIDs { // first IN (t.team_id)
		args = append(args, id)
	}
	for _, id := range teamIDs { // second IN (task_teams)
		args = append(args, id)
	}
	return fmt.Sprintf(" AND (t.team_id IN (%s) OR EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = t.id AND tt.team_id IN (%s)))", ph, ph), args
}

// sqliteTaskRuleOrderJoin attaches each task's matching event_handler rule
// sort_order (the lowest, when several rules cover the event type) so the list
// ordering can put "CI is broken" above "someone asked for a review" without
// the caller knowing the rule set. LEFT JOIN: a task whose event type no
// enabled rule covers still lists, ordered by the COALESCE default. The
// derived table is org-scoped so another org's rules can't influence ordering.
const sqliteTaskRuleOrderJoin = `
	LEFT JOIN (
		SELECT org_id, event_type, MIN(sort_order) AS sort_order
		FROM event_handlers
		WHERE enabled = 1 AND kind = 'rule'
		GROUP BY org_id, event_type
	) tr ON t.event_type = tr.event_type AND t.org_id = tr.org_id`

// sqliteTaskAttentionTier orders the OPEN rows of a lane by whose move it is —
// the first preference term on the In Progress and In Review lanes, so the card
// waiting on a human is on page one rather than wherever its priority put it.
// It is a subquery and no join, so it is absent from the count query by
// construction; the page projects it as well as orders by it, because a keyset
// cursor resumes from the tuple's values and this is one of them.
//
// A closed row takes a constant tier, on the same `closed_at IS NOT NULL` the
// partition above tests, and that is load-bearing rather than tidy. This term
// is read BEFORE the recency term, so without the guard every read spanning
// both partitions — an unfiltered one, or any explicit status set mixing open
// rows with terminal ones — would order its closed tail by unfinished business
// instead of by recency: a stale closure still holding a draft pull request
// climbing over a later one that left nothing behind. Whether a lane carries
// the tier at all is then only a question of cost
// (db.TaskListFilter.OrdersByAttention), never of whether the tail is safe.
//
// The constant is 2 because that is what a closed row honestly is on this
// scale: not anybody's move. Any constant would tie them; this one does not
// need a reader to know it is a sentinel.
//
// Snoozed rows are deliberately NOT guarded the same way. They are segregated
// by their own partition above, and inside that group the tier displaces
// nothing a reader is owed — a deferred set falls straight through to rule
// order and priority, which is the same preference class the tier belongs to.
// The closed tail is the only group with a recency contract to protect.
//
// The tiers, and the reason each is where it is:
//
//	0  needs you   — the task holds a conversation matching the per-conversation
//	                 needs-you predicate: an unanswered permission prompt, or a
//	                 conversation that is not live and still holds an unresolved
//	                 artifact.
//	1  failed      — its newest conversation died. Nothing is coming; a human
//	                 decides what happens next, but there is no question
//	                 waiting on them, so it sits below tier 0.
//	2  in flight   — an agent is working, and a task with no conversation at
//	                 all: neither is anybody's move, and a row that has not
//	                 started must not sort below finished work.
//	3  completed   — its newest conversation concluded. Done reading, last.
//
// Tier 0 is `sqliteConversationAttentionSQL` — the SAME predicate the
// conversations list's `attention` filter and the rail's `needs` count read,
// lifted to a per-task EXISTS. A second definition of "needs you" would let a
// lane disagree with the rail counted above it.
//
// The status halves read the DISPLAY status, not the stored column: the card a
// human is looking at reads the display ladder, and a conversation mid-claim
// carries no stored status at all. `failed` / `completed` mirror
// domain.StatusFailed / domain.StatusCompleted, which SQL cannot import; the
// dual-dialect conformance suite is what holds the two literals to them.
//
// "Newest" is (started_at DESC, id) — the same ordering ConversationStore.List
// groups a task's conversations by, so the conversation this reads is the one
// the board renders on the card. A task with no conversation yields SQL NULL,
// which no WHEN matches: the ELSE is what puts it in tier 2 rather than
// needing its own arm.
const sqliteTaskAttentionTier = `CASE
	         WHEN t.closed_at IS NOT NULL THEN 2
	         WHEN EXISTS (SELECT 1 FROM conversations r
	                      WHERE r.task_id = t.id AND ` + sqliteConversationAttentionSQL + `)
	              THEN 0
	         ELSE CASE (SELECT ` + sqliteDisplayStatusSQL + `
	                    FROM conversations r
	                    WHERE r.task_id = t.id
	                    ORDER BY r.started_at DESC, r.id
	                    LIMIT 1)
	                WHEN 'failed'    THEN 1
	                WHEN 'completed' THEN 3
	                ELSE 2
	              END
	       END`

// sqliteTaskClaimantJoin resolves the name the claimee sort orders on. Both
// joins are on a primary key, so neither can drop or duplicate a row; they
// are added only for that sort so every other query keeps the plan it had.
const sqliteTaskClaimantJoin = `
	LEFT JOIN agents ca ON ca.id = t.claimed_by_agent_id
	LEFT JOIN users cu ON cu.id = t.claimed_by_user_id`

// taskSortTerm is one term of a task list's total order.
//
// expr is what the ORDER BY orders on. bind and conv are how a cursor value —
// one term of db.TaskSortKey's dialect-neutral rendering — is put back into
// the shape that expression yields, so the keyset comparison lands where the
// ORDER BY put the row: bind wraps the placeholder in whatever the expression
// does to its column (`{}` stands in for the placeholder), and conv decides
// the Go value the driver binds. They live on one struct because an ORDER BY
// and a comparison that disagree are a page that silently skips rows.
//
// conv is what SQLite needs rather than a cast. Its comparisons rank by
// storage class before value, so a cursor bound as text sorts above every
// integer in a column rather than among them; and a timestamp must reach the
// column's own TEXT layout to compare against it, which the driver does for a
// time.Time and no date function does without rounding to the second.
//
// target, when set, names where this term's value scans on the returned row.
// It is set for exactly the terms whose value lives on no column the task read
// already carries — the matching rule's sort_order, the claimant's name, the
// attention tier — and the page adds the term's expr to its SELECT for each.
// A caller mints its next cursor from the row it was handed, so a term whose
// value the row cannot show is a position it cannot resume from.
type taskSortTerm struct {
	expr   string
	bind   string
	conv   func(string) (any, error)
	desc   bool
	target func(*domain.Task) any
}

// sqliteTaskKeyBind is the bind for a term whose expression reads its column
// bare — most of them; conv is what does the work.
const sqliteTaskKeyBind = "{}"

func sqliteTaskKeyInt(s string) (any, error) {
	return strconv.ParseInt(s, 10, 64)
}

func sqliteTaskKeyReal(s string) (any, error) {
	return strconv.ParseFloat(s, 64)
}

// sqliteTaskKeyTime binds a cursor timestamp as a time.Time, which the driver
// renders in exactly the layout the column holds (see db.SQLiteTimeFormatParam
// — every task timestamp is written through it, none takes the column
// default). The alternative, putting both sides through datetime(), rounds to
// the second: SQLite's date functions cannot express the sub-second ordering
// the raw column already has.
func sqliteTaskKeyTime(s string) (any, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, err
	}
	return t.UTC(), nil
}

// sqliteTaskKeyTimeOrEmpty is the conv for a term over a nullable timestamp,
// where the empty string is the row's NULL — the term's own COALESCE maps the
// column to that same empty string. A term over a NOT NULL column takes
// sqliteTaskKeyTime instead, so an empty value there is refused rather than
// silently compared as text against a timestamp.
func sqliteTaskKeyTimeOrEmpty(s string) (any, error) {
	if s == "" {
		return "", nil
	}
	return sqliteTaskKeyTime(s)
}

// sqliteTaskUnclaimedExpr is the claimee sort's leading term: unclaimed rows
// sort last in BOTH directions, so the flag is its own always-ascending term
// ahead of the name rather than part of it.
const sqliteTaskUnclaimedExpr = `CASE WHEN t.claimed_by_agent_id IS NULL AND t.claimed_by_user_id IS NULL THEN 1 ELSE 0 END`

// sqliteTaskListTerms renders the joins and the ordered terms for a filter's
// sort — the default order when it names no key, otherwise the reader's key
// between the lane structure and the id tiebreaker. The request's sort never
// reaches the SQL as text: the key switches to a constant fragment and the
// direction to a bool, so an ORDER BY is assembled from this file's own
// strings whatever the request said.
//
// The whole default order, read top to bottom as "what does the reader want to
// see first":
//
//  1. live before snoozed — a deferred entry never jumps above pickable work,
//     however high its priority. SQLite evaluates the comparison as 0/1, so
//     false (live) sorts first; Postgres orders false before true identically.
//  2. open before closed. Inert on a single-lane query (every row ties), so
//     the queue keeps the ordering it had — and it never compares a NULL
//     closed_at against a non-NULL one, which is what keeps the two dialects'
//     NULL-ordering defaults from diverging at the recency term below.
//  3. the attention tier over the open rows of the lanes that carry it
//     (sqliteTaskAttentionTier) — a closed row is not tiered, so the term is
//     inert inside the partition below and the recency term keeps it.
//  4. newest-closed first — the Done column's recency, inert everywhere else.
//  5. rule sort_order, then priority, then id — the queue's own ordering.
//
// Terms 1-3 are structure rather than preference, so they stay in front of a
// reader's key: a snoozed row must not climb over live work because it sorts
// first by title, and a run parked on a human still leads its lane. Terms 4
// and 5 are what a sort_key replaces.
//
// One term wraps a column the ORDER BY could read bare: closed_at is COALESCEd
// to a sentinel because a NULL breaks a keyset chain — every comparison
// against it is NULL, so every arm below the partition evaluates false, and
// the page resumes at nothing. The sentinel is order-inert because the
// partition above it already separates open rows from closed ones, so all the
// rows it applies to are in one lane and tie there anyway.
func sqliteTaskListTerms(f db.TaskListFilter) (joins string, terms []taskSortTerm) {
	desc := f.SortDir != db.TaskSortDirAsc
	terms = []taskSortTerm{
		{expr: "(t.status = 'snoozed')", bind: sqliteTaskKeyBind, conv: sqliteTaskKeyInt},
		{expr: "(t.closed_at IS NOT NULL)", bind: sqliteTaskKeyBind, conv: sqliteTaskKeyInt},
	}
	if f.OrdersByAttention() {
		terms = append(terms, taskSortTerm{
			expr: sqliteTaskAttentionTier, bind: sqliteTaskKeyBind, conv: sqliteTaskKeyInt,
			target: func(t *domain.Task) any { return &t.ListAttentionTier }})
	}
	switch f.SortKey {
	case db.TaskSortTitle:
		// COALESCE so a title-less entity sorts as empty rather than as
		// NULL, whose position differs between the dialects by direction.
		terms = append(terms, taskSortTerm{
			expr: "LOWER(COALESCE(e.title, ''))", bind: "LOWER({})", desc: desc})
	case db.TaskSortCreated:
		terms = append(terms, taskSortTerm{
			expr: "t.created_at", bind: sqliteTaskKeyBind, conv: sqliteTaskKeyTime, desc: desc})
	case db.TaskSortEventType:
		terms = append(terms, taskSortTerm{
			expr: "t.event_type", bind: sqliteTaskKeyBind, desc: desc})
	case db.TaskSortClaimee:
		joins = sqliteTaskClaimantJoin
		terms = append(terms,
			taskSortTerm{expr: sqliteTaskUnclaimedExpr, bind: sqliteTaskKeyBind, conv: sqliteTaskKeyInt},
			taskSortTerm{
				expr: "COALESCE(ca.display_name, cu.display_name, '')", bind: sqliteTaskKeyBind, desc: desc,
				target: func(t *domain.Task) any { return &t.ListClaimeeName }})
	default:
		// No key, or one no vocabulary defines — the HTTP layer refuses the
		// latter, so reaching it means a caller built the filter directly.
		// The default order is the honest answer either way; inventing SQL
		// for an unknown key is not.
		joins = sqliteTaskRuleOrderJoin
		terms = append(terms,
			taskSortTerm{expr: "COALESCE(t.closed_at, '')", bind: sqliteTaskKeyBind, conv: sqliteTaskKeyTimeOrEmpty, desc: true},
			taskSortTerm{
				expr: "COALESCE(tr.sort_order, 999)", bind: sqliteTaskKeyBind, conv: sqliteTaskKeyInt,
				target: func(t *domain.Task) any { return &t.ListSortOrder }},
			taskSortTerm{expr: "COALESCE(t.priority_score, 0.5)", bind: sqliteTaskKeyBind, conv: sqliteTaskKeyReal, desc: true})
	}
	// The id tiebreaker is what makes the order total, and a total order is
	// what makes a page a window rather than a sample: without it, rows tying
	// on every other term are dropped and repeated across pages by offset and
	// keyset alike.
	return joins, append(terms, taskSortTerm{expr: "t.id", bind: sqliteTaskKeyBind})
}

// sqliteTaskOrderBy renders the terms as the query's ORDER BY.
func sqliteTaskOrderBy(terms []taskSortTerm) string {
	parts := make([]string, len(terms))
	for i, term := range terms {
		dir := " ASC"
		if term.desc {
			dir = " DESC"
		}
		parts[i] = term.expr + dir
	}
	return `
	ORDER BY ` + strings.Join(parts, `,
	         `)
}

// sqliteTaskKeysetWhere renders the comparison that resumes a page after the
// row whose order values are `after` — the WHERE half of keyset paging,
// appended to the page query's WHERE body (never the count query's: the total
// is the whole filtered set, not what is left of it).
//
// A mixed-direction tuple can't use one row comparison, so this is the
// lexicographic chain spelled out: past the first term, or tied on it and past
// the second, or tied on both and past the third, … Each comparison takes its
// own term's direction and each bound value goes through its term's own
// normalization, so the chain cuts the result set at exactly the point the
// ORDER BY put that row.
//
// It nests rather than flattening into one OR-list of AND-chains. The two
// forms are equivalent — the nested one distributes into the flat one — but
// nesting mentions each term's expression exactly twice instead of once per
// arm below it, and the attention tier's expression is a correlated subquery
// per row. A flat chain would run it up to four times where this runs it
// twice.
func sqliteTaskKeysetWhere(terms []taskSortTerm, after []string, args []any) (string, []any, error) {
	if len(after) != len(terms) {
		return "", nil, db.ErrBadPageCursor
	}
	bind := func(term taskSortTerm, value string) (string, error) {
		v, err := sqliteTaskKeyValue(term, value)
		if err != nil {
			return "", err
		}
		args = append(args, v)
		return strings.Replace(term.bind, "{}", "?", 1), nil
	}
	// Descends the terms in order, which is also the order the placeholders
	// appear in — SQLite binds `?` by position, so a chain assembled in any
	// other order would read its cursor values shuffled.
	var chain func(i int) (string, error)
	chain = func(i int) (string, error) {
		op := " > "
		if terms[i].desc {
			op = " < "
		}
		past, err := bind(terms[i], after[i])
		if err != nil {
			return "", err
		}
		clause := terms[i].expr + op + past
		if i == len(terms)-1 {
			return clause, nil
		}
		tied, err := bind(terms[i], after[i])
		if err != nil {
			return "", err
		}
		rest, err := chain(i + 1)
		if err != nil {
			return "", err
		}
		return clause + " OR (" + terms[i].expr + " = " + tied + " AND (" + rest + "))", nil
	}
	body, err := chain(0)
	if err != nil {
		return "", nil, err
	}
	return ` AND (` + body + `)`, args, nil
}

// sqliteTaskKeyValue converts one cursor value for its term. A value that
// doesn't parse is db.ErrBadPageCursor rather than a driver error: it can only
// come from a token this build didn't mint, which is a caller fault.
func sqliteTaskKeyValue(term taskSortTerm, value string) (any, error) {
	if term.conv == nil {
		return value, nil
	}
	v, err := term.conv(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", db.ErrBadPageCursor, err)
	}
	return v, nil
}

// sqliteTaskListWhere renders db.TaskListFilter as a WHERE body (no leading
// WHERE) plus its args in placeholder order. "1=1" for the empty filter keeps
// every caller's SQL one shape.
func sqliteTaskListWhere(f db.TaskListFilter) (string, []any) {
	clauses := []string{"1=1"}
	var args []any

	if len(f.Statuses) > 0 {
		var arms []string
		var lifecycle []string
		for _, s := range f.Statuses {
			if s == db.TaskListStatusClaimed {
				// The claim axis, not a lifecycle status — see
				// db.TaskListStatusClaimed. Bot claims count: they surface the
				// window between a delegate stamp and the run's first
				// transition, plus spawn-failure rows that stay claimed-queued
				// until someone retries.
				arms = append(arms, "(t.status = 'queued' AND (t.claimed_by_user_id IS NOT NULL OR t.claimed_by_agent_id IS NOT NULL))")
				continue
			}
			lifecycle = append(lifecycle, s)
		}
		if len(lifecycle) > 0 {
			ph := strings.TrimRight(strings.Repeat("?, ", len(lifecycle)), ", ")
			arms = append(arms, fmt.Sprintf("t.status IN (%s)", ph))
			for _, s := range lifecycle {
				args = append(args, s)
			}
		}
		clauses = append(clauses, "("+strings.Join(arms, " OR ")+")")
	}
	if f.OnlyUnclaimed {
		clauses = append(clauses, "t.claimed_by_agent_id IS NULL AND t.claimed_by_user_id IS NULL")
	}
	if !f.IncludeSnoozed {
		clauses = append(clauses, "(t.snooze_until IS NULL OR t.snooze_until <= datetime('now'))")
	}
	if f.ClosedSince != nil {
		// Terminal rows only: a closed_at window says nothing about an open
		// task, so narrowing one on it would silently empty a mixed query.
		//
		// datetime() on both sides so the two on-disk shapes — the schema's
		// CURRENT_TIMESTAMP default and a Go-bound time.Time — compare as
		// instants rather than as text of differing widths.
		clauses = append(clauses, "(t.status NOT IN ('done', 'dismissed') OR (t.closed_at IS NOT NULL AND datetime(t.closed_at) >= datetime(?)))")
		args = append(args, f.ClosedSince.UTC().Format("2006-01-02 15:04:05"))
	}
	if f.CreatedSince != nil {
		// datetime() on both sides for the same two-shapes reason as
		// ClosedSince above.
		clauses = append(clauses, "datetime(t.created_at) >= datetime(?)")
		args = append(args, f.CreatedSince.UTC().Format("2006-01-02 15:04:05"))
	}
	if f.CreatedBefore != nil {
		clauses = append(clauses, "datetime(t.created_at) <= datetime(?)")
		args = append(args, f.CreatedBefore.UTC().Format("2006-01-02 15:04:05"))
	}
	if len(f.EventTypes) > 0 {
		ph := strings.TrimRight(strings.Repeat("?, ", len(f.EventTypes)), ", ")
		clauses = append(clauses, fmt.Sprintf("t.event_type IN (%s)", ph))
		for _, et := range f.EventTypes {
			args = append(args, et)
		}
	}
	if term := strings.TrimSpace(f.Search); term != "" {
		// The four fields the card shows, matched as a literal substring:
		// LikeEscape neutralizes the wildcards, so a needle holding '%'
		// matches a percent sign instead of every row. A NULL ai_summary
		// simply fails its arm — no COALESCE needed for an OR.
		pattern := "%" + db.LikeEscape(strings.ToLower(term)) + "%"
		clauses = append(clauses, `(LOWER(e.title) LIKE ? ESCAPE '\'
		            OR LOWER(e.source_id) LIKE ? ESCAPE '\'
		            OR LOWER(t.ai_summary) LIKE ? ESCAPE '\'
		            OR LOWER(t.event_type) LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern, pattern, pattern)
	}
	if len(f.Sources) > 0 {
		ph := strings.TrimRight(strings.Repeat("?, ", len(f.Sources)), ", ")
		clauses = append(clauses, fmt.Sprintf("e.source IN (%s)", ph))
		for _, src := range f.Sources {
			args = append(args, src)
		}
	}
	teamClause, args := sqliteTaskTeamFilter(f.TeamIDs, args)
	// sqliteTaskTeamFilter renders as " AND (...)" for direct concatenation
	// onto a WHERE body; it is always last, so its args stay in placeholder
	// order behind the ones above.
	return strings.Join(clauses, " AND ") + teamClause, args
}

func (s *taskStore) List(ctx context.Context, orgID string, f db.TaskListFilter, opts db.ListOpts) ([]domain.Task, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	where, args := sqliteTaskListWhere(f)

	// The total runs the same filters on the same connection as the page, so
	// a caller's "showing 50 of 213" can't be assembled from two different
	// snapshots. It carries no ordering at all — neither the rule-order join
	// (a LEFT JOIN of a grouped derived table referenced only by the ORDER BY,
	// so it can neither drop nor duplicate a row) nor the attention tier's
	// per-row subqueries. Counting is not ordering, and both would only make
	// it slower.
	var total int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Task{}, total, nil
	}

	joins, terms := sqliteTaskListTerms(f)
	cols, targets := sqliteTaskListProjection(terms)
	pageArgs := append([]any{}, args...)
	keyset := ""
	if len(opts.After) > 0 {
		// The keyset replaces OFFSET rather than joining it: a cursor names a
		// position in the order, and skipping rows past it would be counting
		// the same position twice.
		var err error
		if keyset, pageArgs, err = sqliteTaskKeysetWhere(terms, opts.After, pageArgs); err != nil {
			return nil, 0, err
		}
	}
	query := `
		SELECT ` + cols + `
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id` + joins + `
		WHERE ` + where + keyset + sqliteTaskOrderBy(terms)
	if opts.Limit > 0 {
		query += `
		LIMIT ?`
		pageArgs = append(pageArgs, opts.Limit)
		if len(opts.After) == 0 {
			query += ` OFFSET ?`
			pageArgs = append(pageArgs, opts.Offset)
		}
	}
	tasks, err := queryListedTasksCtx(ctx, s.q, targets, query, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	return tasks, total, nil
}

// FacetEventTypes renders the same WHERE List does — the facet counts the
// rows the lane lists, and a second WHERE is how the two drift apart. The
// rule-order join and the sort joins are absent for the same reason the
// count query leaves them out: nothing here orders by a rule's sort_order or
// a claimant's name, and neither join can change which rows are counted.
func (s *taskStore) FacetEventTypes(ctx context.Context, orgID string, f db.TaskListFilter) ([]db.Facet, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	where, args := sqliteTaskListWhere(f)
	rows, err := s.q.QueryContext(ctx, `
		SELECT t.event_type, COUNT(*)
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE `+where+`
		GROUP BY t.event_type
		ORDER BY t.event_type ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.ScanFacets(rows)
}

func (s *taskStore) FindActiveByEntityAndType(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return queryTasksCtx(ctx, s.q, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.event_type = ? AND t.status NOT IN ('done', 'dismissed')
	`, entityID, eventType)
}

// FindActiveByEntityAndTypeSystem mirrors FindActiveByEntityAndType.
// The tracker consumes this through the admin pool in
// Postgres; SQLite has one connection, so this delegates straight
// through with the same assertLocalOrg gate.
func (s *taskStore) FindActiveByEntityAndTypeSystem(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error) {
	return s.FindActiveByEntityAndType(ctx, orgID, entityID, eventType)
}

// --- Admin-pool variants ---
//
// All `...System` methods below delegate straight through to their
// non-System counterparts. SQLite has one connection so the pool
// distinction doesn't exist; the wrappers are kept for signature
// parity with Postgres. The router consumes these from its eventbus
// subscriber goroutine.

func (s *taskStore) GetSystem(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
	return s.Get(ctx, orgID, taskID)
}

func (s *taskStore) FindActiveByEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.Task, error) {
	return s.FindActiveByEntity(ctx, orgID, entityID)
}

func (s *taskStore) FindOrCreateAtSystem(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	return s.FindOrCreateAt(ctx, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt)
}

// FindOrCreateAtUnlessEntityActiveSystem mirrors the plain check-then-act
// the router used to do inline (FindActiveByEntitySystem, then
// conditionally FindOrCreateAtSystem) before TFAC-579 moved it behind a
// real lock on Postgres. SQLite/local is single-connection N=1 — there's no
// concurrent writer for a lock to exclude — so this exists for interface
// conformance with the Postgres impl rather than a correctness fix here.
func (s *taskStore) FindOrCreateAtUnlessEntityActiveSystem(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (task *domain.Task, created, suppressed bool, err error) {
	active, err := s.FindActiveByEntitySystem(ctx, orgID, entityID)
	if err != nil {
		return nil, false, false, err
	}
	if len(active) > 0 {
		return nil, false, true, nil
	}
	task, created, err = s.FindOrCreateAtSystem(ctx, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt)
	return task, created, false, err
}

func (s *taskStore) SetVisibilityTeamsSystem(ctx context.Context, orgID, taskID string, teamIDs []string) error {
	return s.SetVisibilityTeams(ctx, orgID, taskID, teamIDs)
}

func (s *taskStore) VisibilityTeamsSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return s.VisibilityTeams(ctx, orgID, taskID)
}

func (s *taskStore) SetOwnerTeamSystem(ctx context.Context, orgID, taskID, teamID string) (domain.Task, error) {
	return s.SetOwnerTeam(ctx, orgID, taskID, teamID)
}

func (s *taskStore) BumpSystem(ctx context.Context, orgID, taskID, eventID string) (domain.Task, error) {
	return s.Bump(ctx, orgID, taskID, eventID)
}

func (s *taskStore) CloseSystem(ctx context.Context, orgID, taskID, closeReason, closeEventType string) (domain.Task, error) {
	return s.Close(ctx, orgID, taskID, closeReason, closeEventType)
}

func (s *taskStore) SetStatusSystem(ctx context.Context, orgID, taskID, status string) (domain.Task, error) {
	return s.SetStatus(ctx, orgID, taskID, status)
}

func (s *taskStore) RecordEventSystem(ctx context.Context, orgID, taskID, eventID, kind string) error {
	return s.RecordEvent(ctx, orgID, taskID, eventID, kind)
}

func (s *taskStore) CountConsecutiveFailedConversationsSystem(ctx context.Context, orgID, entityID, promptID string) (int, error) {
	return s.CountConsecutiveFailedConversations(ctx, orgID, entityID, promptID)
}

func (s *taskStore) StampAgentClaimIfUnclaimedSystem(ctx context.Context, orgID, taskID, agentID, actingTeamID string) (bool, error) {
	return s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, actingTeamID)
}

func (s *taskStore) OwnerTeamForLatestTaskInTypesSystem(ctx context.Context, orgID, entityID string, eventTypes []string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	if len(eventTypes) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(eventTypes))
	args := make([]any, 0, len(eventTypes)+1)
	args = append(args, entityID)
	for i, et := range eventTypes {
		placeholders[i] = "?"
		args = append(args, et)
	}
	var teamID string
	err := s.q.QueryRowContext(ctx, `
		SELECT team_id
		FROM tasks
		WHERE entity_id = ?
		  AND team_id IS NOT NULL
		  AND event_type IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY created_at DESC
		LIMIT 1
	`, args...).Scan(&teamID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return teamID, nil
}

func (s *taskStore) FindActiveByEntity(ctx context.Context, orgID, entityID string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return queryTasksCtx(ctx, s.q, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.status NOT IN ('done', 'dismissed')
	`, entityID)
}

// listActiveRefsChunkSize is the chunk applied to the IN clause when
// fanning out across many entities, kept conservatively below SQLite's
// historical bound-variable limit. Same shape RecentEventsByEntity
// uses.
const listActiveRefsChunkSize = 500

func (s *taskStore) ListActiveRefsForEntities(ctx context.Context, orgID string, entityIDs []string, _ []string) ([]domain.PendingTaskRef, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// teamIDs ignored: N=1 local mode has one team, so the factory's team
	// filter has nothing to narrow here (the frontend never renders it
	// below 2 teams). Mirrors the Queued/Entities asymmetry.
	if len(entityIDs) == 0 {
		return nil, nil
	}
	out := make([]domain.PendingTaskRef, 0, len(entityIDs))
	for start := 0; start < len(entityIDs); start += listActiveRefsChunkSize {
		end := start + listActiveRefsChunkSize
		if end > len(entityIDs) {
			end = len(entityIDs)
		}
		chunk := entityIDs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := s.q.QueryContext(ctx, `
			SELECT id, entity_id, event_type, dedup_key
			FROM tasks
			WHERE entity_id IN (`+strings.Join(placeholders, ",")+`)
				AND status NOT IN ('done', 'dismissed')
			ORDER BY entity_id, event_type, created_at DESC, rowid DESC
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ref domain.PendingTaskRef
			if err := rows.Scan(&ref.ID, &ref.EntityID, &ref.EventType, &ref.DedupKey); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, ref)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *taskStore) EntityIDsWithActiveTasks(ctx context.Context, orgID, source string) (map[string]struct{}, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT t.entity_id
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE e.source = ? AND t.status NOT IN ('done', 'dismissed')
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// --- Lifecycle ---

func (s *taskStore) FindOrCreate(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64) (*domain.Task, bool, error) {
	return s.FindOrCreateAt(ctx, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, time.Now().UTC())
}

func (s *taskStore) FindOrCreateAt(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, false, err
	}
	// teamID is the owning/attributed team stamped on a new row. Empty is
	// allowed and means "unresolved owner" — author-centric routing couldn't
	// pick a single team — and is inserted as NULL. A NULL-team task is still
	// visible via task_teams and gates auto-delegation off; it consolidates
	// to one team on the first human claim.
	var teamBind any
	if teamID != "" {
		teamBind = teamID
	}
	// Try to find an existing active task first. Identity is
	// (entity, event_type, dedup_key) — team is not part of it, so a
	// situation already tasked by another team's rule returns that
	// task here rather than spawning a duplicate.
	var existing domain.Task
	err := scanTaskFromRow(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.event_type = ? AND t.dedup_key = ?
			AND t.status NOT IN ('done', 'dismissed')
		LIMIT 1
	`, entityID, eventType, dedupKey), &existing)
	if err == nil {
		return &existing, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}

	// Create new task. The partial unique index on
	// (entity_id, event_type, dedup_key) WHERE status NOT IN
	// ('done', 'dismissed') is the race backstop: a concurrent
	// goroutine that races past the SELECT will get rejected on
	// INSERT, and we re-read to return the winner's row.
	id := uuid.New().String()
	// team_id is the owning/attributed team; 'team' is the canonical
	// visibility. The broader visibility set is written separately via
	// SetVisibilityTeams.
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at,
		                   team_id, visibility)
		VALUES (?, ?, ?, ?, ?, 'queued', ?, 'pending', ?, ?, 'team')
	`, id, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt.UTC(), teamBind)
	if err != nil {
		var raced domain.Task
		err2 := scanTaskFromRow(s.q.QueryRowContext(ctx, `
			SELECT `+sqliteTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE t.entity_id = ? AND t.event_type = ? AND t.dedup_key = ?
				AND t.status NOT IN ('done', 'dismissed')
			LIMIT 1
		`, entityID, eventType, dedupKey), &raced)
		if err2 == nil {
			return &raced, false, nil
		}
		return nil, false, err
	}

	task, err := s.Get(ctx, orgID, id)
	if err != nil {
		return nil, false, err
	}
	return task, true, nil
}

func (s *taskStore) Bump(ctx context.Context, orgID, taskID, eventID string) (domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Task{}, err
	}
	var t domain.Task
	return scanTaskBareRow(s.q.QueryRowContext(ctx, `
		UPDATE tasks
		SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
		    snooze_until = CASE WHEN status = 'snoozed' THEN NULL ELSE snooze_until END
		WHERE id = ?
		RETURNING `+sqliteTaskBareColumns,
		taskID), &t)
}

func (s *taskStore) Close(ctx context.Context, orgID, taskID, closeReason, closeEventType string) (domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Task{}, err
	}
	t, err := closeTaskRow(ctx, s.q, taskID, closeReason, closeEventType)
	if err != nil {
		return domain.Task{}, err
	}
	if t == nil {
		return domain.Task{}, db.ErrNoSuchTask
	}
	return *t, nil
}

// closeTaskRow is Close's body, reporting the closed row via RETURNING or
// nil when the state-guarded WHERE matched nothing — a missing id, or a task
// already terminal (the same guard that makes a replayed close a no-op).
// CloseWithConversationCancelIntentSystem shares it so both close paths agree
// on what "did this land" means, and on the SAME connection this call's
// caller is already inside a transaction on.
func closeTaskRow(ctx context.Context, q queryer, taskID, closeReason, closeEventType string) (*domain.Task, error) {
	var cet *string
	if closeEventType != "" {
		cet = &closeEventType
	}
	var t domain.Task
	row, err := scanTaskBareRow(q.QueryRowContext(ctx, `
		UPDATE tasks SET status = 'done', close_reason = ?, close_event_type = ?,
		                 closed_at = ?
		WHERE id = ? AND status NOT IN ('done', 'dismissed')
		RETURNING `+sqliteTaskBareColumns,
		closeReason, cet, time.Now().UTC(), taskID), &t)
	if err == db.ErrNoSuchTask {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// scanActiveConversationIDs drains the task's non-terminal conversation ids and closes
// the cursor before returning.
func scanActiveConversationIDs(ctx context.Context, q queryer, taskID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE task_id = ?
		  AND (status IS NULL OR status NOT IN (`+conversationTerminalStatusesSQL+`))
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *taskStore) CloseWithConversationCancelIntentSystem(ctx context.Context, orgID, taskID, closeReason, closeEventType, closingEventID string) (bool, []string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, nil, err
	}
	var (
		closed          bool
		conversationIDs []string
	)
	err := inTx(ctx, s.q, func(q queryer) error {
		t, err := closeTaskRow(ctx, q, taskID, closeReason, closeEventType)
		if err != nil {
			return fmt.Errorf("close task: %w", err)
		}
		closed = t != nil

		if closingEventID != "" {
			if _, err := q.ExecContext(ctx, `
				INSERT OR IGNORE INTO task_events (task_id, event_id, kind, created_at)
				VALUES (?, ?, 'closed', ?)
			`, taskID, closingEventID, time.Now().UTC()); err != nil {
				return fmt.Errorf("record close audit: %w", err)
			}
		}
		if !closed {
			return nil
		}

		// Drained and closed before the UPDATE below rather than on a defer:
		// both ride the one connection this tx holds, and an open cursor is
		// the kind of thing a driver is entitled to refuse to write around.
		if conversationIDs, err = scanActiveConversationIDs(ctx, q, taskID); err != nil {
			return fmt.Errorf("list active conversations: %w", err)
		}

		// `status = 'running' AND cancel_requested = 0` is
		// RequestRunCancelSystem's guard verbatim — a blueprint that already
		// finished is not a blueprint this close is entitled to call off. The
		// Postgres twin carries the model; this is the same predicate in the
		// other dialect.
		if _, err := q.ExecContext(ctx, `
			UPDATE blueprint_runs SET cancel_requested = 1
			WHERE status = 'running'
			  AND cancel_requested = 0
			  AND id IN (
			      SELECT c.blueprint_run_id FROM conversations c
			      WHERE c.task_id = ?
			        AND c.blueprint_run_id IS NOT NULL
			        AND (c.status IS NULL OR c.status NOT IN (`+conversationTerminalStatusesSQL+`))
			  )
		`, taskID); err != nil {
			return fmt.Errorf("stamp run cancel intent: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return closed, conversationIDs, nil
}

func (s *taskStore) SetStatus(ctx context.Context, orgID, taskID, status string) (domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Task{}, err
	}
	var t domain.Task
	return scanTaskBareRow(s.q.QueryRowContext(ctx, `
		UPDATE tasks SET status = ? WHERE id = ?
		RETURNING `+sqliteTaskBareColumns,
		status, taskID), &t)
}

func (s *taskStore) AdvanceStatusForUser(ctx context.Context, orgID, taskID, userID, newStatus string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if newStatus != "in_progress" && newStatus != "in_review" {
		return false, nil
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET status = ?
		 WHERE id = ?
		   AND claimed_by_user_id = ?
		   AND status IN ('queued', 'in_progress', 'in_review')
	`, newStatus, taskID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) RecordEvent(ctx context.Context, orgID, taskID, eventID, kind string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO task_events (task_id, event_id, kind, created_at)
		VALUES (?, ?, ?, ?)
	`, taskID, eventID, kind, time.Now().UTC())
	return err
}

// MarkEventInjectedSystem flips the timeline row AND stamps the task's
// agent claim in one transaction — see db.AgentClaimStamp for why the two
// writes are inseparable. A stamp refusal (user owns it, bot already owns
// it, task terminal) is not an error and leaves the mark committed.
func (s *taskStore) MarkEventInjectedSystem(ctx context.Context, orgID, taskID, eventID string, claim db.AgentClaimStamp) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	claimed := false
	err := inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx, `
			UPDATE task_events SET kind = 'injected' WHERE task_id = ? AND event_id = ?
		`, taskID, eventID); err != nil {
			return err
		}
		if claim.AgentID == "" {
			return nil
		}
		var err error
		claimed, err = stampAgentClaimIfUnclaimed(ctx, q, taskID, claim.AgentID, claim.ActingTeamID)
		return err
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

func (s *taskStore) SetVisibilityTeams(ctx context.Context, orgID, taskID string, teamIDs []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	for _, teamID := range teamIDs {
		if teamID == "" {
			continue
		}
		if _, err := s.q.ExecContext(ctx, `
			INSERT OR IGNORE INTO task_teams (task_id, team_id) VALUES (?, ?)
		`, taskID, teamID); err != nil {
			return err
		}
	}
	return nil
}

func (s *taskStore) VisibilityTeams(ctx context.Context, orgID, taskID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT team_id FROM task_teams WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// sqliteActingTeamExpr derives the owning team for a user-initiated
// claim: a team in the task's visibility set (task_teams) that the
// claimer belongs to. It prefers the current owner team when the
// claimer is a member of it — honoring "keep the owner when the choice
// is ambiguous" — and otherwise picks the lowest-id qualifying team so
// the result is deterministic. The existing owner team_id is kept only
// when the intersection is empty. The result is always a team the
// claimer belongs to, which the RLS update check requires. Bind order
// is (taskID, userID).
const sqliteActingTeamExpr = `COALESCE(
		(SELECT tt.team_id
		   FROM task_teams tt
		   JOIN memberships m ON m.team_id = tt.team_id
		  WHERE tt.task_id = ? AND m.user_id = ?
		  ORDER BY (tt.team_id = tasks.team_id) DESC, tt.team_id ASC
		  LIMIT 1),
		team_id)`

func (s *taskStore) SetOwnerTeam(ctx context.Context, orgID, taskID, teamID string) (domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Task{}, err
	}
	// Empty teamID keeps the stored team_id (COALESCE/NULLIF, the same idiom
	// stampAgentClaimIfUnclaimed uses below) rather than skipping the
	// statement — the row still has to exist for the write to answer
	// anything, so a bogus id reports ErrNoSuchTask on this path too instead
	// of the prior silent no-op.
	var t domain.Task
	return scanTaskBareRow(s.q.QueryRowContext(ctx, `
		UPDATE tasks SET team_id = COALESCE(NULLIF(?, ''), team_id) WHERE id = ?
		RETURNING `+sqliteTaskBareColumns,
		teamID, taskID), &t)
}

// --- Claim mutations ---

func (s *taskStore) SetClaimedByAgent(ctx context.Context, orgID, taskID, agentID string) (domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Task{}, err
	}
	var claimedByAgentID any = agentID
	if agentID == "" {
		claimedByAgentID = nil
	}
	var t domain.Task
	return scanTaskBareRow(s.q.QueryRowContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       claimed_by_user_id  = NULL
		 WHERE id = ?
		RETURNING `+sqliteTaskBareColumns,
		claimedByAgentID, taskID), &t)
}

func (s *taskStore) SetClaimedByUser(ctx context.Context, orgID, taskID, userID string) (domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Task{}, err
	}
	var claimedByUserID any = userID
	if userID == "" {
		// Empty string is the domain's NULL convention. Passing it raw
		// would violate the users(id) FK on the next read.
		claimedByUserID = nil
	}
	var t domain.Task
	return scanTaskBareRow(s.q.QueryRowContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id  = ?,
		       claimed_by_agent_id = NULL
		 WHERE id = ?
		RETURNING `+sqliteTaskBareColumns,
		claimedByUserID, taskID), &t)
}

func (s *taskStore) StampAgentClaimIfUnclaimed(ctx context.Context, orgID, taskID, agentID, actingTeamID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	return stampAgentClaimIfUnclaimed(ctx, s.q, taskID, agentID, actingTeamID)
}

// stampAgentClaimIfUnclaimed is the guarded claim write itself, taking the
// queryer so the commitment-coupled callers (the fenced run insert, the
// pending-firing enqueue, the injection mark) can run it on their own
// transaction rather than as a second, separately-failing write. Mirrors
// the Postgres helper of the same name.
func stampAgentClaimIfUnclaimed(ctx context.Context, q queryer, taskID, agentID, actingTeamID string) (bool, error) {
	if agentID == "" {
		return false, fmt.Errorf("StampAgentClaimIfUnclaimed: empty agentID")
	}
	// actingTeamID is the firing trigger's team; on a successful claim
	// it consolidates the card to that owning team. Empty leaves
	// team_id unchanged.
	res, err := q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       team_id = COALESCE(NULLIF(?, ''), team_id),
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND claimed_by_user_id IS NULL
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != ?)
		   AND status NOT IN ('done', 'dismissed')
	`, agentID, actingTeamID, taskID, agentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) HandoffAgentClaim(ctx context.Context, orgID, taskID, agentID, userID string) (db.HandoffResult, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return db.HandoffRefused, err
	}
	if agentID == "" {
		return db.HandoffRefused, fmt.Errorf("HandoffAgentClaim: empty agentID")
	}
	if userID == "" {
		return db.HandoffRefused, fmt.Errorf("HandoffAgentClaim: empty userID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       claimed_by_user_id  = NULL,
		       team_id = `+sqliteActingTeamExpr+`,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND (claimed_by_user_id  IS NULL OR claimed_by_user_id  = ?)
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != ?)
		   AND status NOT IN ('done', 'dismissed')
	`, agentID, taskID, userID, taskID, userID, agentID)
	if err != nil {
		return db.HandoffRefused, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.HandoffRefused, err
	}
	if n > 0 {
		return db.HandoffChanged, nil
	}
	// 0 rows — re-read to figure out which guard tripped. Terminal-status
	// check takes precedence over the no-op check so a sticky-bot-claim
	// on a closed task doesn't fall through to HandoffNoOp and let the
	// caller proceed past a row that mustn't be reopened.
	var curUser, curAgent sql.NullString
	var curStatus string
	err = s.q.QueryRowContext(ctx,
		`SELECT claimed_by_user_id, claimed_by_agent_id, status FROM tasks WHERE id = ?`,
		taskID,
	).Scan(&curUser, &curAgent, &curStatus)
	if err == sql.ErrNoRows {
		return db.HandoffRefused, nil
	}
	if err != nil {
		return db.HandoffRefused, err
	}
	if curStatus == "done" || curStatus == "dismissed" {
		return db.HandoffRefused, nil
	}
	if curAgent.Valid && curAgent.String == agentID {
		return db.HandoffNoOp, nil
	}
	return db.HandoffRefused, nil
}

func (s *taskStore) ResolveClaimTeam(ctx context.Context, orgID, taskID, userID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	// N=1: the local user isn't enrolled via memberships, so the
	// visibility-set subquery is empty and this collapses to the task's
	// current team_id — the sole local team in practice. Mirrors the
	// Postgres derivation shape for interface parity.
	var team string
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(
		         (SELECT tt.team_id
		            FROM task_teams tt
		            JOIN memberships m ON m.team_id = tt.team_id
		           WHERE tt.task_id = ? AND m.user_id = ?
		           ORDER BY (tt.team_id = t.team_id) DESC, tt.team_id ASC LIMIT 1),
		         t.team_id)
		  FROM tasks t
		 WHERE t.id = ?
	`, taskID, userID, taskID).Scan(&team)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve claim team: %w", err)
	}
	return team, nil
}

func (s *taskStore) TakeoverClaimFromAgent(ctx context.Context, orgID, taskID, userID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if userID == "" {
		return false, fmt.Errorf("TakeoverClaimFromAgent: empty userID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id  = ?,
		       claimed_by_agent_id = NULL,
		       team_id = `+sqliteActingTeamExpr+`,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND claimed_by_agent_id IS NOT NULL
		   AND claimed_by_user_id  IS NULL
		   AND status NOT IN ('done', 'dismissed')
	`, userID, taskID, userID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) ClaimQueuedForUser(ctx context.Context, orgID, taskID, userID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if userID == "" {
		return false, fmt.Errorf("ClaimQueuedForUser: empty userID is not a valid claimant")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id = ?,
		       team_id = `+sqliteActingTeamExpr+`,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND status IN ('queued', 'snoozed')
		   AND claimed_by_user_id  IS NULL
		   AND claimed_by_agent_id IS NULL
	`, userID, taskID, userID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) ReassignClaimToUser(ctx context.Context, orgID, taskID, fromUserID, toUserID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	return reassignClaimToUserSQLite(ctx, s.q, taskID, fromUserID, toUserID)
}

func (s *taskStore) ReassignClaimToUserSystem(ctx context.Context, orgID, taskID, fromUserID, toUserID string) (bool, error) {
	return s.ReassignClaimToUser(ctx, orgID, taskID, fromUserID, toUserID)
}

// reassignClaimToUserSQLite is the shared body for ReassignClaimToUser and
// its (trivial, single-connection) ReassignClaimToUserSystem wrapper. Besides
// the claim CAS every other claim mutation does, it bakes the target-team-
// membership guard directly into the WHERE clause — an EXISTS against
// memberships requiring toUserID to belong to a team associated with the
// task (its task_teams visibility set or its current team_id) — atomically
// with the CAS itself, mirroring the Postgres impl. Local mode is N=1 in
// practice (no real cross-user memberships), but the guard keeps the SQLite
// store conformant with the interface contract.
func reassignClaimToUserSQLite(ctx context.Context, q queryer, taskID, fromUserID, toUserID string) (bool, error) {
	if fromUserID == "" {
		return false, fmt.Errorf("ReassignClaimToUser: empty fromUserID")
	}
	if toUserID == "" {
		return false, fmt.Errorf("ReassignClaimToUser: empty toUserID")
	}
	res, err := q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id = ?,
		       team_id = `+sqliteActingTeamExpr+`,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND claimed_by_user_id = ?
		   AND status NOT IN ('done', 'dismissed')
		   AND EXISTS (
		         SELECT 1 FROM memberships m2
		          WHERE m2.user_id = ?
		            AND (
		              m2.team_id IN (SELECT team_id FROM task_teams WHERE task_id = ?)
		              OR m2.team_id = (SELECT team_id FROM tasks WHERE id = ?)
		            )
		       )
	`, toUserID, taskID, toUserID, taskID, fromUserID, toUserID, taskID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- Breaker ---

func (s *taskStore) CountConsecutiveFailedConversations(ctx context.Context, orgID, entityID, promptID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var count int
	err := s.q.QueryRowContext(ctx, `
		WITH recent AS (
			SELECT
				CASE
					WHEN r.blueprint_run_id IS NULL THEN 'leaf'
					ELSE 'blueprint'
				END AS kind,
				r.blueprint_run_id,
				COALESCE(cr.status, r.status) AS status,
				COALESCE(cr.started_at, r.started_at) AS started_at,
				ROW_NUMBER() OVER (
					PARTITION BY COALESCE(r.blueprint_run_id, r.id)
					ORDER BY r.started_at ASC
				) AS step_rank
			FROM conversations r
			JOIN tasks t ON r.task_id = t.id
			LEFT JOIN blueprint_runs cr ON cr.id = r.blueprint_run_id
			WHERE t.entity_id = ?
				AND (
					(r.blueprint_run_id IS NULL AND r.prompt_id = ?)
					OR (cr.blueprint_id = ?)
				)
				AND r.trigger_type = 'event'
		),
		dedup AS (
			SELECT status, started_at
			FROM recent
			WHERE step_rank = 1
			ORDER BY started_at DESC
			LIMIT 20
		)
		SELECT COUNT(*)
		FROM dedup
		WHERE status IN ('failed', 'aborted')
			AND started_at > (
				SELECT COALESCE(MAX(started_at), '1970-01-01')
				FROM dedup WHERE status = 'completed'
			)
	`, entityID, promptID, promptID).Scan(&count)
	return count, err
}

// --- Internal helpers ---

// taskMemoryOwedRowSQL / taskMemoryPendingSQL / taskMemoryAttemptSQL are the
// Postgres twins' predicate in the other dialect — see internal/db/postgres/
// tasks.go for what "owing" means, why the subagent rows are excluded, and why
// the attempt subquery settles which conversation it speaks for before it
// reads an attempt rather than ranking the two together. Same words, same
// index (idx_conversations_task_ended), and the same two readers: this file's
// task read and the claim scan in conversation_queue.go.
func taskMemoryOwedRowSQL(orgExpr, taskExpr string) string {
	return `owed.org_id = ` + orgExpr + ` AND owed.task_id = ` + taskExpr + `
		  AND owed.ended_at IS NOT NULL AND owed.parent_conversation_id IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM conversation_memory mem
		      WHERE mem.conversation_id = owed.id)`
}

func taskMemoryPendingSQL(orgExpr, taskExpr string) string {
	return `EXISTS (SELECT 1 FROM conversations owed WHERE ` + taskMemoryOwedRowSQL(orgExpr, taskExpr) + `)`
}

func taskMemoryAttemptSQL(orgExpr, taskExpr, col string) string {
	return `(SELECT att.` + col + `
		FROM conversation_memory_attempts att
		WHERE att.conversation_id = (
		    SELECT owed.id FROM conversations owed
		    WHERE ` + taskMemoryOwedRowSQL(orgExpr, taskExpr) + `
		    ORDER BY owed.ended_at DESC, owed.id DESC
		    LIMIT 1)
		ORDER BY att.started_at DESC, att.id DESC
		LIMIT 1)`
}

// sqliteTaskColumnsWithEntity is the canonical column list for every
// task query that feeds scanTask. Lives on TaskStore because it owns
// the surface; ScoreStore's UnscoredTasks references this via
// the same-package import.
//
// A var rather than a const because the memory-pending tail is built from the
// shared predicate helpers above — the point of which is that the claim scan
// and this list cannot disagree about what "pending" means.
var sqliteTaskColumnsWithEntity = `
	t.id, t.entity_id, t.event_type, t.dedup_key, t.primary_event_id,
	t.team_id,
	t.status, t.priority_score, t.ai_summary, t.autonomy_suitability,
	t.priority_reasoning, t.scoring_status, t.severity, t.relevance_reason,
	t.source_status, t.snooze_until, t.close_reason, t.close_event_type,
	t.closed_at, t.created_at,
	t.claimed_by_agent_id, t.claimed_by_user_id,
	COALESCE(e.title, ''), COALESCE(e.url, ''), e.source_id, e.source, e.kind,
	-- Guard json_extract so malformed or empty legacy snapshots do not fail
	-- the entire task query.
	COALESCE(
		CASE
			WHEN json_valid(NULLIF(e.snapshot_json, ''))
				THEN json_extract(NULLIF(e.snapshot_json, ''), '$.open_subtask_count')
			ELSE NULL
		END,
		0
	),
	-- Slack thread message count: the messages addressed to the bot on this
	-- entity. Gated on source so only Slack tasks pay the correlated count.
	-- (event_type, entity_id) seeks idx_events_type_entity; SQLite entities
	-- carry no org_id column (local mode is single-tenant), and that index is
	-- not org-prefixed, so no org predicate is added or needed here.
	CASE
		WHEN e.source = 'slack' THEN (
			SELECT COUNT(*) FROM events ev
			WHERE ev.event_type = 'slack:message' AND ev.entity_id = t.entity_id
		)
		ELSE 0
	END,
	-- The memory the task is still waiting on, and the newest try at
	-- producing it. The attempt columns are NULL whenever memory_pending is
	-- false, by construction rather than by a guard: they read the same owing
	-- conversation the flag does, and there is none.
	` + taskMemoryPendingSQL("t.org_id", "t.id") + `,
	COALESCE(` + taskMemoryAttemptSQL("t.org_id", "t.id", "outcome") + `, ''),
	COALESCE(` + taskMemoryAttemptSQL("t.org_id", "t.id", "error_kind") + `, ''),
	COALESCE(` + taskMemoryAttemptSQL("t.org_id", "t.id", "error_message") + `, ''),
	` + taskMemoryAttemptSQL("t.org_id", "t.id", "started_at")

// sqliteTaskListProjection is the SELECT list and the scan targets for one
// List page: the canonical columns every task read projects, plus this order's
// own ordering values — the ones the terms carry a target for, because they
// live on no column the canonical list has. The two are built together and in
// term order, so a column can't be added without somewhere to scan it.
func sqliteTaskListProjection(terms []taskSortTerm) (string, []func(*domain.Task) any) {
	cols := sqliteTaskColumnsWithEntity
	var targets []func(*domain.Task) any
	for _, term := range terms {
		if term.target == nil {
			continue
		}
		cols += `,
	` + term.expr
		targets = append(targets, term.target)
	}
	return cols, targets
}

// sqliteTaskBareColumns is tasks' own columns — no entity join — in the
// order taskScanState.bareTargets expects. It is the leading, identical
// prefix of sqliteTaskColumnsWithEntity (kept that way so the two column
// lists cannot drift apart) and is what every converted write's RETURNING
// projects: see the returned-row shape note on db.TaskStore.
const sqliteTaskBareColumns = `
	id, entity_id, event_type, dedup_key, primary_event_id,
	team_id,
	status, priority_score, ai_summary, autonomy_suitability,
	priority_reasoning, scoring_status, severity, relevance_reason,
	source_status, snooze_until, close_reason, close_event_type,
	closed_at, created_at,
	claimed_by_agent_id, claimed_by_user_id`

// taskScanState holds the NullX intermediates for one row of
// sqliteTaskColumnsWithEntity. Keeping the helper here means
// TaskStore's scan path is right next to the column list — the
// dependency runs in one direction (queries → scan) and lives in one
// file.
type taskScanState struct {
	teamID                             sql.NullString
	priorityScore, autonomySuitability sql.NullFloat64
	aiSummary, priorityReasoning       sql.NullString
	severity, relevanceReason          sql.NullString
	sourceStatus, scoringStatus        sql.NullString
	closeReason, closeEventType        sql.NullString
	snoozeUntil, closedAt              sql.NullTime
	claimedByAgentID, claimedByUserID  sql.NullString

	// The memory-pending tail. memoryAttemptStartedAt is the one nullable of
	// the four: its validity IS "an attempt exists", which is why the three
	// text columns can be COALESCEd to empty in SQL without losing the
	// difference between a running attempt (no outcome yet) and no attempt at
	// all. It scans as text and parses via parseDBDatetime — a DATETIME
	// column loses its declared type inside a scalar subselect, the same
	// detour claimed_at takes on the conversation read.
	memoryAttemptOutcome      string
	memoryAttemptErrorKind    string
	memoryAttemptErrorMessage string
	memoryAttemptStartedAt    sql.NullString
}

// bareTargets is the scan-target list for sqliteTaskBareColumns — the
// tasks-table columns only, none of the entity-join fields. targets below
// extends it with those.
func (s *taskScanState) bareTargets(t *domain.Task) []any {
	return []any{
		&t.ID, &t.EntityID, &t.EventType, &t.DedupKey, &t.PrimaryEventID,
		&s.teamID,
		&t.Status, &s.priorityScore, &s.aiSummary, &s.autonomySuitability,
		&s.priorityReasoning, &s.scoringStatus, &s.severity, &s.relevanceReason,
		&s.sourceStatus, &s.snoozeUntil, &s.closeReason, &s.closeEventType,
		&s.closedAt, &t.CreatedAt,
		&s.claimedByAgentID, &s.claimedByUserID,
	}
}

func (s *taskScanState) targets(t *domain.Task) []any {
	return append(s.bareTargets(t),
		&t.Title, &t.SourceURL, &t.EntitySourceID, &t.EntitySource, &t.EntityKind,
		&t.OpenSubtaskCount, &t.SlackMessageCount,
		&t.MemoryPending,
		&s.memoryAttemptOutcome, &s.memoryAttemptErrorKind,
		&s.memoryAttemptErrorMessage, &s.memoryAttemptStartedAt,
	)
}

// listTargets extends targets with the ordering values sqliteTaskListProjection
// appended for this order's terms. Each is NOT NULL by the expression that
// produced it, so none needs a NullX intermediate.
func (s *taskScanState) listTargets(t *domain.Task, extras []func(*domain.Task) any) []any {
	out := s.targets(t)
	for _, extra := range extras {
		out = append(out, extra(t))
	}
	return out
}

// finalize moves the NullX intermediates onto the task. It returns an error
// for the one value it has to parse rather than merely copy: the memory
// attempt's start, which arrives as text (see the field's note).
func (s *taskScanState) finalize(t *domain.Task) error {
	if s.teamID.Valid {
		v := s.teamID.String
		t.TeamID = &v
	}
	if s.priorityScore.Valid {
		t.PriorityScore = &s.priorityScore.Float64
	}
	if s.autonomySuitability.Valid {
		t.AutonomySuitability = &s.autonomySuitability.Float64
	}
	t.AISummary = s.aiSummary.String
	t.PriorityReasoning = s.priorityReasoning.String
	t.Severity = s.severity.String
	t.RelevanceReason = s.relevanceReason.String
	t.SourceStatus = s.sourceStatus.String
	t.ScoringStatus = s.scoringStatus.String
	t.CloseReason = s.closeReason.String
	t.CloseEventType = s.closeEventType.String
	if s.snoozeUntil.Valid {
		t.SnoozeUntil = &s.snoozeUntil.Time
	}
	if s.closedAt.Valid {
		t.ClosedAt = &s.closedAt.Time
	}
	t.ClaimedByAgentID = s.claimedByAgentID.String
	t.ClaimedByUserID = s.claimedByUserID.String
	if s.memoryAttemptStartedAt.Valid {
		startedAt, err := parseDBDatetime(s.memoryAttemptStartedAt.String)
		if err != nil {
			return fmt.Errorf("parse memory attempt started_at %q: %w", s.memoryAttemptStartedAt.String, err)
		}
		t.MemoryAttempt = &domain.MemoryAttemptSummary{
			Outcome:      domain.MemoryAttemptOutcome(s.memoryAttemptOutcome),
			ErrorKind:    domain.MemoryAttemptErrorKind(s.memoryAttemptErrorKind),
			ErrorMessage: s.memoryAttemptErrorMessage,
			StartedAt:    startedAt.UTC(),
		}
	}
	return nil
}

func scanTaskFields(rows *sql.Rows, t *domain.Task) error {
	var s taskScanState
	if err := rows.Scan(s.targets(t)...); err != nil {
		return err
	}
	return s.finalize(t)
}

func scanTaskFromRow(row *sql.Row, t *domain.Task) error {
	var s taskScanState
	if err := row.Scan(s.targets(t)...); err != nil {
		return err
	}
	return s.finalize(t)
}

// scanTaskBareRow decodes a sqliteTaskBareColumns row — an id-keyed write's
// UPDATE ... RETURNING. No row scanned means the write's WHERE clause
// matched nothing (a missing id, or a state guard the row's current status
// didn't satisfy), which is db.ErrNoSuchTask.
func scanTaskBareRow(row *sql.Row, t *domain.Task) (domain.Task, error) {
	var s taskScanState
	if err := row.Scan(s.bareTargets(t)...); err != nil {
		if err == sql.ErrNoRows {
			return domain.Task{}, db.ErrNoSuchTask
		}
		return domain.Task{}, err
	}
	if err := s.finalize(t); err != nil {
		return domain.Task{}, err
	}
	return *t, nil
}

// queryListedTasksCtx is queryTasksCtx for a List page, whose projection
// carries this order's own ordering values after the canonical columns.
func queryListedTasksCtx(ctx context.Context, q queryer, extras []func(*domain.Task) any, query string, args ...any) ([]domain.Task, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		var st taskScanState
		if err := rows.Scan(st.listTargets(&t, extras)...); err != nil {
			return nil, err
		}
		if err := st.finalize(&t); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func queryTasksCtx(ctx context.Context, q queryer, query string, args ...any) ([]domain.Task, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		if err := scanTaskFields(rows, &t); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
