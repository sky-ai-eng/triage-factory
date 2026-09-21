package workitem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Item is one row's shared block, as an operator surface renders it. Kind
// columns are not here: a kind describes its own rows in its own words, and
// this package never reads them except through the allowlist a Kind declares.
type Item struct {
	ID     int64
	OrgID  string
	Status string

	Attempt       int
	MaxAttempts   int
	NextAttemptAt *time.Time

	LeaseGeneration int64
	LeaseOwner      string
	LeaseEpoch      *int64
	LeasedAt        *time.Time
	LeaseExpiresAt  *time.Time

	CancelRequestedAt *time.Time
	CancelRequestedBy string
	CancelReason      string

	LastError    string
	LastOutcome  string
	UniqueKey    string
	SupersededBy *int64

	FirstEnqueuedAt time.Time
	CreatedAt       time.Time
	DoneAt          *time.Time
}

// StatusDeferred is the one list filter that is not a stored status: the
// ready rows whose retry time has not come, the same partition Measure
// reports as Deferred. It is accepted by List beside the five stored values.
const StatusDeferred = "deferred"

// ListStatuses is every value List accepts as a status filter, for a caller
// validating input before it reaches the store.
var ListStatuses = []string{StatusReady, StatusLeased, StatusDone, StatusParked, StatusCancelled, StatusDeferred}

// itemColumns is the block projection every Item read answers with, so a row
// read one way is identical to the same row read the other.
const itemColumns = "id, org_id, status, attempt, max_attempts, next_attempt_at," +
	" lease_generation, COALESCE(lease_owner, ''), lease_epoch, leased_at, lease_expires_at," +
	" cancel_requested_at, COALESCE(cancel_requested_by, ''), COALESCE(cancel_reason, '')," +
	" COALESCE(last_error, ''), COALESCE(last_outcome, ''), COALESCE(unique_key, ''), superseded_by," +
	" first_enqueued_at, created_at, done_at"

// List returns one page of an org's rows, newest first, optionally narrowed
// to one status, plus the unpaged total under the same filter.
//
// id DESC is a total order (id is monotonic per insert), so the pages
// partition the result set. A page whose rows moved since the count ran is a
// page with fewer rows than the total implies, which is normal for a table an
// operator reads and then acts on.
//
// status "" is every status. "ready" is the ripe ready rows and "deferred"
// the ready rows whose retry time is still ahead — the two partition
// status='ready' exactly as Measure's depths do, so a list filter and a depth
// gauge describe the same rows.
func List(ctx context.Context, q DBTX, k Kind, orgID, status string, limit, offset int) ([]Item, int, error) {
	if err := k.Validate(); err != nil {
		return nil, 0, err
	}
	if orgID == "" {
		return nil, 0, fmt.Errorf("workitem: %s list has no org", k.Table)
	}
	if limit < 0 || offset < 0 {
		return nil, 0, fmt.Errorf("workitem: %s list window %d/%d is negative", k.Table, limit, offset)
	}

	// The table is aliased t because the status predicates, and the claim
	// filter inside them, are written over that alias.
	a := newArgs(k.Dialect)
	where := " WHERE t.org_id = " + a.bind(orgID)
	pred, err := k.statusPredicate(status)
	if err != nil {
		return nil, 0, err
	}
	if pred != "" {
		where += " AND " + pred
	}

	var total int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+k.Table+" t"+where, a.vals...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("workitem: count %s: %w", k.Table, err)
	}
	if limit == 0 {
		return []Item{}, total, nil
	}

	stmt := "SELECT " + itemColumns + " FROM " + k.Table + " t" + where +
		" ORDER BY t.id DESC LIMIT " + a.bind(limit) + " OFFSET " + a.bind(offset)
	rows, err := q.QueryContext(ctx, stmt, a.vals...)
	if err != nil {
		return nil, 0, fmt.Errorf("workitem: list %s: %w", k.Table, err)
	}
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("workitem: list %s: %w", k.Table, err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("workitem: list %s: %w", k.Table, err)
	}
	return out, total, nil
}

// Get returns one of the org's rows by id, or (nil, nil) when the org has no
// such row: a miss is an answer here, not a fault.
func Get(ctx context.Context, q DBTX, k Kind, orgID string, id int64) (*Item, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	if orgID == "" {
		return nil, fmt.Errorf("workitem: %s get has no org", k.Table)
	}
	a := newArgs(k.Dialect)
	stmt := "SELECT " + itemColumns + " FROM " + k.Table +
		" WHERE org_id = " + a.bind(orgID) + " AND id = " + a.bind(id)
	it, err := scanItem(q.QueryRowContext(ctx, stmt, a.vals...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workitem: get %s row %d: %w", k.Table, id, err)
	}
	return &it, nil
}

// statusPredicate renders a List status filter, "" for every status. A value
// outside the vocabulary is an error rather than an empty page: a filter that
// silently matched nothing would read as "nothing there".
func (k Kind) statusPredicate(status string) (string, error) {
	switch status {
	case "":
		return "", nil
	case StatusReady:
		return k.ripePredicate(), nil
	case StatusDeferred:
		return k.deferredPredicate(), nil
	case StatusLeased, StatusDone, StatusParked, StatusCancelled:
		return "t.status = " + quoteLiteral(status), nil
	default:
		return "", fmt.Errorf("workitem: %s list has unknown status %q", k.Table, status)
	}
}

// scanItem reads one itemColumns row. It takes the narrow Scan interface so
// *sql.Row and *sql.Rows share it and the two reads cannot drift.
func scanItem(row interface{ Scan(...any) error }) (Item, error) {
	var (
		it                                                      Item
		nextAt, leasedAt, expiresAt, cancelAt, firstAt, created dbTime
		doneAt                                                  dbTime
		leaseEpoch, supersededBy                                sql.NullInt64
	)
	err := row.Scan(
		&it.ID, &it.OrgID, &it.Status, &it.Attempt, &it.MaxAttempts, &nextAt,
		&it.LeaseGeneration, &it.LeaseOwner, &leaseEpoch, &leasedAt, &expiresAt,
		&cancelAt, &it.CancelRequestedBy, &it.CancelReason,
		&it.LastError, &it.LastOutcome, &it.UniqueKey, &supersededBy,
		&firstAt, &created, &doneAt,
	)
	if err != nil {
		return Item{}, err
	}
	it.NextAttemptAt = nextAt.ptr()
	it.LeasedAt = leasedAt.ptr()
	it.LeaseExpiresAt = expiresAt.ptr()
	it.CancelRequestedAt = cancelAt.ptr()
	it.FirstEnqueuedAt = firstAt.Time
	it.CreatedAt = created.Time
	it.DoneAt = doneAt.ptr()
	it.LeaseEpoch = nullInt64Ptr(leaseEpoch)
	it.SupersededBy = nullInt64Ptr(supersededBy)
	return it, nil
}

func (d dbTime) ptr() *time.Time {
	if !d.Valid {
		return nil
	}
	t := d.Time
	return &t
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
