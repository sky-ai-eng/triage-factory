package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// pollReadinessStore is the Postgres impl of db.PollReadinessStore. Wired
// against the ADMIN (BYPASSRLS) pool in postgres.New: poll_readiness is a
// system table (RLS deny-by-default, REVOKEd from the app roles) — callers
// already hold an authorized orgID (session claims resolved by the
// handler, or the poller's own system context), so there is no per-row RLS
// policy to gate a browsable surface with, same posture as instances.
type pollReadinessStore struct{ admin queryer }

func newPollReadinessStore(admin queryer) db.PollReadinessStore {
	return &pollReadinessStore{admin: admin}
}

var _ db.PollReadinessStore = (*pollReadinessStore)(nil)

func (s *pollReadinessStore) MarkRestarted(ctx context.Context, orgID, source string) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, restarted_at, last_poll_at, announce_pending)
		VALUES ($1, $2, now(), NULL, false)
		ON CONFLICT (org_id, source) DO UPDATE SET
			restarted_at = EXCLUDED.restarted_at,
			last_poll_at = NULL
	`, orgID, source)
	return err
}

func (s *pollReadinessStore) MarkPollComplete(ctx context.Context, orgID, source string, startedAt time.Time) error {
	var startedAtParam any
	if !startedAt.IsZero() {
		startedAtParam = startedAt
	}
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, last_poll_at)
		VALUES ($1, $2, now())
		ON CONFLICT (org_id, source) DO UPDATE SET
			last_poll_at = EXCLUDED.last_poll_at
		WHERE $3::timestamptz IS NULL
		   OR poll_readiness.restarted_at IS NULL
		   OR $3::timestamptz >= poll_readiness.restarted_at
	`, orgID, source, startedAtParam)
	return err
}

func (s *pollReadinessStore) Ready(ctx context.Context, orgID, source string) (bool, error) {
	var restartedAt, lastPollAt sql.NullTime
	err := s.admin.QueryRowContext(ctx, `
		SELECT restarted_at, last_poll_at FROM poll_readiness WHERE org_id = $1 AND source = $2
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
	res, err := s.admin.ExecContext(ctx, `
		UPDATE poll_readiness SET announce_pending = false
		WHERE org_id = $1 AND source = $2 AND announce_pending = true
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
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO poll_readiness (org_id, source, announce_pending)
		VALUES ($1, $2, true)
		ON CONFLICT (org_id, source) DO UPDATE SET announce_pending = true
	`, orgID, source)
	return err
}

func (s *pollReadinessStore) LastPollTimes(ctx context.Context, orgID string) (map[string]time.Time, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT source, last_poll_at FROM poll_readiness
		WHERE org_id = $1 AND last_poll_at IS NOT NULL
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

// pgConnectionCols is the column list every connection read here projects,
// shared by the point read and the write's RETURNING so the two shapes cannot
// drift.
const pgConnectionCols = `org_id, source, connection_state, connection_changed_at, connection_failure_class`

func (s *pollReadinessStore) RecordConnection(ctx context.Context, orgID, source string, state db.ConnectionState, failureClass string) (stored, previous db.ConnectionStatus, err error) {
	if err := db.ValidateConnection(state, failureClass); err != nil {
		return db.ConnectionStatus{}, db.ConnectionStatus{}, err
	}
	// The class describes a connection that is down and nothing else, so it is
	// derived from the state here rather than trusted from the caller.
	var class any
	if state == db.ConnectionDown {
		class = failureClass
	}
	err = inTx(ctx, s.admin, func(q queryer) error {
		// FOR UPDATE holds an existing row until commit, so two overlapping
		// writers (a demoted holder's last cycle beside its successor's first)
		// read the previous state one after the other and report a change once.
		var rerr error
		if previous, rerr = connectionStatus(ctx, q, orgID, source, true); rerr != nil {
			return rerr
		}
		// An unknown state carries no start time: it is the absence of one.
		stored, rerr = scanConnectionStatus(q.QueryRowContext(ctx, `
			INSERT INTO poll_readiness (org_id, source, connection_state, connection_changed_at, connection_failure_class)
			VALUES ($1, $2, $3, CASE WHEN $3::text = 'unknown' THEN NULL ELSE now() END, $4)
			ON CONFLICT (org_id, source) DO UPDATE SET
				connection_changed_at = CASE
					WHEN poll_readiness.connection_state = EXCLUDED.connection_state
						THEN poll_readiness.connection_changed_at
					ELSE EXCLUDED.connection_changed_at
				END,
				connection_state = EXCLUDED.connection_state,
				connection_failure_class = EXCLUDED.connection_failure_class
			RETURNING `+pgConnectionCols,
			orgID, source, string(state), class))
		return rerr
	})
	if err != nil {
		return db.ConnectionStatus{}, db.ConnectionStatus{}, err
	}
	return stored, previous, nil
}

func (s *pollReadinessStore) Connection(ctx context.Context, orgID, source string) (db.ConnectionStatus, error) {
	return connectionStatus(ctx, s.admin, orgID, source, false)
}

func (s *pollReadinessStore) ListConnectionStatuses(ctx context.Context) ([]db.ConnectionStatus, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT pr.org_id, pr.source, pr.connection_state, pr.connection_changed_at, pr.connection_failure_class
		FROM poll_readiness pr
		JOIN orgs o ON o.id::text = pr.org_id
		WHERE pr.connection_state <> 'unknown'
		  AND o.deleted_at IS NULL
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
// read of the row it is about to replace, which locks it. An absent row is the
// unknown state.
func connectionStatus(ctx context.Context, q queryer, orgID, source string, forUpdate bool) (db.ConnectionStatus, error) {
	query := `SELECT ` + pgConnectionCols + ` FROM poll_readiness WHERE org_id = $1 AND source = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	st, err := scanConnectionStatus(q.QueryRowContext(ctx, query, orgID, source))
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
