package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// pollReadinessStore is the SQLite impl of db.PollReadinessStore. SQLite is
// N=1 (one process, one org via runmode.LocalDefaultOrgID), so this is a
// storage move from the old in-memory Server/announcer fields, not a
// behavior change.
type pollReadinessStore struct{ q queryer }

func newPollReadinessStore(q queryer) db.PollReadinessStore {
	return &pollReadinessStore{q: q}
}

var _ db.PollReadinessStore = (*pollReadinessStore)(nil)

func (s *pollReadinessStore) MarkRestarted(ctx context.Context, orgID, source string) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, restarted_at, last_poll_at, announce_pending)
		VALUES (?, ?, ?, NULL, 0)
		ON CONFLICT (org_id, source) DO UPDATE SET
			restarted_at = excluded.restarted_at,
			last_poll_at = NULL
	`, orgID, source, time.Now().UTC())
	return err
}

func (s *pollReadinessStore) MarkPollComplete(ctx context.Context, orgID, source string, startedAt time.Time) error {
	var startedAtParam any
	if !startedAt.IsZero() {
		startedAtParam = startedAt.UTC()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, last_poll_at)
		VALUES (?, ?, ?)
		ON CONFLICT (org_id, source) DO UPDATE SET
			last_poll_at = excluded.last_poll_at
		WHERE ? IS NULL OR poll_readiness.restarted_at IS NULL OR ? >= poll_readiness.restarted_at
	`, orgID, source, time.Now().UTC(), startedAtParam, startedAtParam)
	return err
}

func (s *pollReadinessStore) Ready(ctx context.Context, orgID, source string) (bool, error) {
	var restartedAt, lastPollAt sql.NullTime
	err := s.q.QueryRowContext(ctx, `
		SELECT restarted_at, last_poll_at FROM poll_readiness WHERE org_id = ? AND source = ?
	`, orgID, source).Scan(&restartedAt, &lastPollAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !lastPollAt.Valid {
		return false, nil
	}
	if !restartedAt.Valid {
		return true, nil
	}
	return lastPollAt.Time.After(restartedAt.Time), nil
}

func (s *pollReadinessStore) TakeAnnouncePending(ctx context.Context, orgID, source string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE poll_readiness SET announce_pending = 0
		WHERE org_id = ? AND source = ? AND announce_pending = 1
	`, orgID, source)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *pollReadinessStore) SetAnnouncePending(ctx context.Context, orgID, source string) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, announce_pending)
		VALUES (?, ?, 1)
		ON CONFLICT (org_id, source) DO UPDATE SET announce_pending = 1
	`, orgID, source)
	return err
}

func (s *pollReadinessStore) LastPollTimes(ctx context.Context, orgID string) (map[string]time.Time, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT source, last_poll_at FROM poll_readiness
		WHERE org_id = ? AND last_poll_at IS NOT NULL
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var source string
		var at time.Time
		if err := rows.Scan(&source, &at); err != nil {
			return nil, err
		}
		out[source] = at.UTC()
	}
	return out, rows.Err()
}

// sqliteConnectionCols is the column list every connection read here
// projects, shared by the point read and the write's RETURNING so the two
// shapes cannot drift.
const sqliteConnectionCols = `org_id, source, connection_state, connection_changed_at, connection_failure_class`

func (s *pollReadinessStore) RecordConnection(ctx context.Context, orgID, source string, state db.ConnectionState, failureClass string) (stored, previous db.ConnectionStatus, err error) {
	if err := db.ValidateConnection(state, failureClass); err != nil {
		return db.ConnectionStatus{}, db.ConnectionStatus{}, err
	}
	// The class describes a connection that is down and nothing else, so it is
	// derived from the state here rather than trusted from the caller. An
	// unknown state carries no start time: it is the absence of one.
	var class, changedAt any
	if state == db.ConnectionDown {
		class = failureClass
	}
	if state != db.ConnectionUnknown {
		changedAt = time.Now().UTC()
	}
	// The IMMEDIATE transaction takes the write lock at BEGIN, so the read
	// below already excludes a concurrent writer.
	err = inTx(ctx, s.q, func(q queryer) error {
		var rerr error
		if previous, rerr = connectionStatus(ctx, q, orgID, source); rerr != nil {
			return rerr
		}
		stored, rerr = scanConnectionStatus(q.QueryRowContext(ctx, `
			INSERT INTO poll_readiness (org_id, source, connection_state, connection_changed_at, connection_failure_class)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (org_id, source) DO UPDATE SET
				connection_changed_at = CASE
					WHEN poll_readiness.connection_state = excluded.connection_state
						THEN poll_readiness.connection_changed_at
					ELSE excluded.connection_changed_at
				END,
				connection_state = excluded.connection_state,
				connection_failure_class = excluded.connection_failure_class
			RETURNING `+sqliteConnectionCols,
			orgID, source, string(state), changedAt, class))
		return rerr
	})
	if err != nil {
		return db.ConnectionStatus{}, db.ConnectionStatus{}, err
	}
	return stored, previous, nil
}

func (s *pollReadinessStore) Connection(ctx context.Context, orgID, source string) (db.ConnectionStatus, error) {
	return connectionStatus(ctx, s.q, orgID, source)
}

func (s *pollReadinessStore) ListConnectionStatuses(ctx context.Context) ([]db.ConnectionStatus, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT pr.org_id, pr.source, pr.connection_state, pr.connection_changed_at, pr.connection_failure_class
		FROM poll_readiness pr
		JOIN orgs o ON o.id = pr.org_id
		WHERE pr.connection_state <> 'unknown'
		ORDER BY pr.org_id, pr.source
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []db.ConnectionStatus{}
	for rows.Next() {
		st, err := scanConnectionStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// connectionStatus is the point read behind Connection and RecordConnection's
// read of the row it is about to replace. An absent row is the unknown state.
func connectionStatus(ctx context.Context, q queryer, orgID, source string) (db.ConnectionStatus, error) {
	st, err := scanConnectionStatus(q.QueryRowContext(ctx,
		`SELECT `+sqliteConnectionCols+` FROM poll_readiness WHERE org_id = ? AND source = ?`,
		orgID, source))
	if errors.Is(err, sql.ErrNoRows) {
		return db.ConnectionStatus{OrgID: orgID, Source: source, State: db.ConnectionUnknown}, nil
	}
	return st, err
}

func scanConnectionStatus(row interface{ Scan(...any) error }) (db.ConnectionStatus, error) {
	var (
		st        db.ConnectionStatus
		state     string
		changedAt sql.NullTime
		class     sql.NullString
	)
	if err := row.Scan(&st.OrgID, &st.Source, &state, &changedAt, &class); err != nil {
		return db.ConnectionStatus{}, err
	}
	st.State = db.ConnectionState(state)
	if changedAt.Valid {
		at := changedAt.Time.UTC()
		st.ChangedAt = &at
	}
	st.FailureClass = class.String
	return st, nil
}
