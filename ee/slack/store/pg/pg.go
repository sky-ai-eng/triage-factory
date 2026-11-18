// Package pg holds the Postgres implementation of the Enterprise Edition
// Slack stores and registers it with core's store-extension
// registry for the "postgres" dialect — same app/admin pool split and RLS
// posture as the rest of the Postgres data layer, mirroring ee/sso/store/pg.
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func init() {
	db.RegisterStoreExtension("postgres", slackstore.ExtKey, func(app, admin db.Execer) any {
		return &slackstore.Bundle{
			Connections:  newConnectionStore(app, admin),
			Identities:   newIdentityStore(admin),
			Deliveries:   newDeliveryStore(admin),
			Channels:     newChannelRegistryStore(app, admin),
			TeamChannels: newTeamChannelStore(app, admin),
		}
	})
}

// rowScanner is the read shape shared by *sql.Row and *sql.Rows, so
// scanConnection serves both the single-row reads and the iterating lists
// without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}

// connectionStore is the Postgres impl of slackstore.ConnectionStore.
//
//   - app: Upsert, ListForOrg, Get, Delete, LockApp. Request-handler
//     reads/writes gated by the org_slack_connections_* RLS policies.
//   - admin: the ...System methods, for claims-free callers.
type connectionStore struct {
	app   db.Execer
	admin db.Execer
}

func newConnectionStore(app, admin db.Execer) slackstore.ConnectionStore {
	return &connectionStore{app: app, admin: admin}
}

var _ slackstore.ConnectionStore = (*connectionStore)(nil)

// oneActiveIndex is the partial unique index that allows one active
// connection per (org, workspace).
const oneActiveIndex = "org_slack_connections_one_active"

const connectionColumns = `
	workspace_id, api_app_id, org_id::text, source, active, workspace_name, app_name,
	COALESCE(enterprise_id, ''), COALESCE(transport, ''), bot_user_id, bot_token_ref,
	COALESCE(signing_secret_ref, ''), COALESCE(app_token_ref, ''),
	COALESCE(registered_by_user_id::text, ''), created_at, updated_at
`

func scanConnection(sc rowScanner) (slackstore.Connection, error) {
	var c slackstore.Connection
	if err := sc.Scan(
		&c.WorkspaceID, &c.APIAppID, &c.OrgID, &c.Source, &c.Active, &c.WorkspaceName, &c.AppName,
		&c.EnterpriseID, &c.Transport, &c.BotUserID, &c.BotTokenRef,
		&c.SigningSecretRef, &c.AppTokenRef,
		&c.RegisteredByUserID, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return slackstore.Connection{}, err
	}
	return c, nil
}

func scanConnections(rows *sql.Rows) ([]slackstore.Connection, error) {
	defer rows.Close()
	out := []slackstore.Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("scan org_slack_connections: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Upsert tries an UPDATE scoped to (workspace_id, api_app_id, org_id) first —
// a re-submit for a (workspace, app) pair this org already owns, which writes
// the credential and display columns and leaves active and source as stored.
// If that matches no row (the pair is wholly new, or it belongs to a
// different org), it falls through to a plain INSERT. A different org's
// (workspace_id, api_app_id) pair then hits the table's PRIMARY KEY head-on and
// returns a real unique-violation error — no pre-check race window, matching
// the sso_domains claim pattern (the constraint is the authority, the handler
// translates the error). An insert that would make a second active connection
// in the org's workspace returns slackstore.ErrActiveConnectionExists.
//
// Nullable columns are written NULL when c carries "" — NULLIF turns an empty
// string into a NULL column rather than a literal ”.
func (s *connectionStore) Upsert(ctx context.Context, c slackstore.Connection) (slackstore.Connection, error) {
	got, err := scanConnection(s.app.QueryRowContext(ctx, `
		UPDATE org_slack_connections SET
			workspace_name = $4,
			app_name = $5,
			enterprise_id = NULLIF($6, ''),
			transport = NULLIF($7, ''),
			bot_user_id = $8,
			bot_token_ref = $9,
			signing_secret_ref = NULLIF($10, ''),
			app_token_ref = NULLIF($11, ''),
			registered_by_user_id = NULLIF($12, '')::uuid
		WHERE workspace_id = $1 AND api_app_id = $2 AND org_id = $3
		RETURNING `+connectionColumns,
		c.WorkspaceID, c.APIAppID, c.OrgID, c.WorkspaceName, c.AppName, c.EnterpriseID, c.Transport,
		c.BotUserID, c.BotTokenRef, c.SigningSecretRef, c.AppTokenRef, c.RegisteredByUserID))
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return slackstore.Connection{}, fmt.Errorf("update org_slack_connections: %w", err)
	}
	got, err = scanConnection(s.app.QueryRowContext(ctx, `
		INSERT INTO org_slack_connections (
			workspace_id, api_app_id, org_id, source, active, workspace_name, app_name, enterprise_id,
			transport, bot_user_id, bot_token_ref, signing_secret_ref, app_token_ref, registered_by_user_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''),
			NULLIF($9, ''), $10, $11, NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, '')::uuid
		)
		RETURNING `+connectionColumns,
		c.WorkspaceID, c.APIAppID, c.OrgID, c.Source, c.Active, c.WorkspaceName, c.AppName, c.EnterpriseID,
		c.Transport, c.BotUserID, c.BotTokenRef, c.SigningSecretRef, c.AppTokenRef, c.RegisteredByUserID))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == oneActiveIndex {
			return slackstore.Connection{}, slackstore.ErrActiveConnectionExists
		}
		return slackstore.Connection{}, fmt.Errorf("insert org_slack_connections: %w", err)
	}
	return got, nil
}

// ListForOrg counts in its own statement, on the same transaction as the page
// when the caller runs it in one, so the total and the page describe one set.
func (s *connectionStore) ListForOrg(ctx context.Context, orgID string, opts db.ListOpts) ([]slackstore.Connection, int, error) {
	var total int
	if err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM org_slack_connections WHERE org_id = $1
	`, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count org_slack_connections: %w", err)
	}
	if opts.CountOnly {
		return []slackstore.Connection{}, total, nil
	}
	conns, err := s.listForOrg(ctx, s.app, orgID, opts)
	if err != nil {
		return nil, 0, err
	}
	return conns, total, nil
}

func (s *connectionStore) ListForOrgSystem(ctx context.Context, orgID string) ([]slackstore.Connection, error) {
	return s.listForOrg(ctx, s.admin, orgID, db.Unwindowed)
}

func (s *connectionStore) listForOrg(ctx context.Context, q db.Execer, orgID string, opts db.ListOpts) ([]slackstore.Connection, error) {
	query := `
		SELECT ` + connectionColumns + `
		FROM org_slack_connections
		WHERE org_id = $1
		ORDER BY workspace_name ASC, workspace_id ASC, api_app_id ASC`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list org_slack_connections: %w", err)
	}
	return scanConnections(rows)
}

func (s *connectionStore) Get(ctx context.Context, orgID, workspaceID, apiAppID string) (*slackstore.Connection, error) {
	c, err := scanConnection(s.app.QueryRowContext(ctx, `
		SELECT `+connectionColumns+`
		FROM org_slack_connections
		WHERE workspace_id = $1 AND api_app_id = $2 AND org_id = $3
	`, workspaceID, apiAppID, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_slack_connections: %w", err)
	}
	return &c, nil
}

func (s *connectionStore) Delete(ctx context.Context, orgID, workspaceID, apiAppID string) error {
	if _, err := s.app.ExecContext(ctx, `
		DELETE FROM org_slack_connections WHERE workspace_id = $1 AND api_app_id = $2 AND org_id = $3
	`, workspaceID, apiAppID, orgID); err != nil {
		return fmt.Errorf("delete org_slack_connections: %w", err)
	}
	return nil
}

// DeleteSystem's updated_at condition holds against a concurrent connect: the
// connect's UPDATE holds the row's lock, and once it commits Postgres
// re-checks the condition against the row it wrote, whose updated_at the
// trigger moved.
func (s *connectionStore) DeleteSystem(ctx context.Context, orgID, workspaceID, apiAppID string, updatedAt time.Time) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM org_slack_connections
		WHERE workspace_id = $1 AND api_app_id = $2 AND org_id = $3 AND updated_at = $4
	`, workspaceID, apiAppID, orgID, updatedAt)
	if err != nil {
		return false, fmt.Errorf("delete org_slack_connections (system): %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete org_slack_connections (system): rows affected: %w", err)
	}
	return n > 0, nil
}

func (s *connectionStore) ListAllSystem(ctx context.Context) ([]slackstore.Connection, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+connectionColumns+`
		FROM org_slack_connections
		ORDER BY org_id ASC, workspace_id ASC, api_app_id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all org_slack_connections: %w", err)
	}
	return scanConnections(rows)
}

func (s *connectionStore) ActiveForWorkspaceSystem(ctx context.Context, orgID, workspaceID string) (*slackstore.Connection, error) {
	c, err := scanConnection(s.admin.QueryRowContext(ctx, `
		SELECT `+connectionColumns+`
		FROM org_slack_connections
		WHERE org_id = $1 AND workspace_id = $2 AND active
	`, orgID, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active org_slack_connections: %w", err)
	}
	return &c, nil
}

func (s *connectionStore) GetByWorkspaceAppSystem(ctx context.Context, workspaceID, apiAppID string) (*slackstore.Connection, error) {
	c, err := scanConnection(s.admin.QueryRowContext(ctx, `
		SELECT `+connectionColumns+`
		FROM org_slack_connections
		WHERE workspace_id = $1 AND api_app_id = $2
	`, workspaceID, apiAppID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_slack_connections by workspace+app id: %w", err)
	}
	return &c, nil
}

// LockApp runs on the app pool — the SAME connection/transaction the caller
// will go on to call AppBoundToOtherOrgSystem and Upsert against — because a
// Postgres advisory lock's blocking behavior only coordinates callers that
// contend for the identical key; it does nothing on its own unless every
// writer actually calls it first. pg_advisory_xact_lock ties the lock's
// lifetime to the current transaction, so it needs no paired unlock: it
// releases automatically on commit or rollback.
//
// hashtextextended (64-bit), not hashtext (32-bit): every other advisory
// lock in this codebase uses the 64-bit form — team_github_repos.go's
// per-org repo-reconcile lock, auth_handlers.go's per-identity/per-email
// login locks — precisely so distinct keys don't collide onto the same
// lock, whether from a birthday-paradox clash within one domain or a
// coincidental clash across two unrelated ones. Salt 2: distinct from
// auth_handlers.go's 0 (email) and 1 (auth_user_id), and from
// team_github_repos.go's 0 (org_id) — this app-id lock domain shares the
// same global pg_advisory_xact_lock(bigint) keyspace as all of them, so a
// distinct salt keeps a Slack api_app_id from ever landing on the same key
// as an unrelated org id or email hashed under a reused salt.
func (s *connectionStore) LockApp(ctx context.Context, apiAppID string) error {
	if _, err := s.app.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 2))`, apiAppID); err != nil {
		return fmt.Errorf("lock app invariant: %w", err)
	}
	return nil
}

// AppBoundToOtherOrgSystem runs on the admin pool (bypasses RLS) since the
// question — "does ANY org other than mine already own this api_app_id" —
// is by definition unanswerable from an RLS-scoped view that only shows the
// caller's own rows.
func (s *connectionStore) AppBoundToOtherOrgSystem(ctx context.Context, apiAppID, orgID string) (bool, error) {
	var exists bool
	if err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM org_slack_connections
			WHERE api_app_id = $1 AND org_id != $2 AND source = 'org'
		)
	`, apiAppID, orgID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check org_slack_connections app-single-org invariant: %w", err)
	}
	return exists, nil
}

// deliveryStore is the Postgres impl of slackstore.DeliveryStore. Admin pool
// only: the pre-auth webhook receiver (and the future Socket Mode client)
// have no request claims for RLS to gate on.
type deliveryStore struct {
	admin db.Execer
}

func newDeliveryStore(admin db.Execer) slackstore.DeliveryStore {
	return &deliveryStore{admin: admin}
}

var _ slackstore.DeliveryStore = (*deliveryStore)(nil)

// deliveryPruneAge is how long a delivery row is retained before the
// opportunistic prune sweeps it — comfortably past Slack's own retry
// window, with margin for a slow/backed-up receiver.
const deliveryPruneAge = 72 * time.Hour

// MarkDeliveredSystem inserts (apiAppID, eventID) with ON CONFLICT DO
// NOTHING; fresh reports whether the row was newly inserted (RowsAffected
// > 0) versus an already-seen duplicate. The conflict arm waits on an
// uncommitted insert of the same key, which is what serializes two
// deliveries of one event across their transactions.
func (s *deliveryStore) MarkDeliveredSystem(ctx context.Context, apiAppID, eventID string) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		INSERT INTO slack_event_deliveries (api_app_id, event_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, apiAppID, eventID)
	if err != nil {
		return false, fmt.Errorf("insert slack_event_deliveries: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected slack_event_deliveries: %w", err)
	}
	return n > 0, nil
}

// PruneDeliveredSystem — see the interface doc. A missed prune only means
// the table grows a little until the next one, so callers log its error and
// move on.
func (s *deliveryStore) PruneDeliveredSystem(ctx context.Context) error {
	if _, err := s.admin.ExecContext(ctx, `
		DELETE FROM slack_event_deliveries WHERE received_at < $1
	`, time.Now().Add(-deliveryPruneAge)); err != nil {
		return fmt.Errorf("prune slack_event_deliveries: %w", err)
	}
	return nil
}
