// Package pg holds the Postgres implementation of the Enterprise Edition
// Slack workspace store and registers it with core's store-extension
// registry for the "postgres" dialect — same app/admin pool split and RLS
// posture as the rest of the Postgres data layer, mirroring ee/sso/store/pg.
//
// Licensed under the Enterprise Edition License (see ee/LICENSE).
package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func init() {
	db.RegisterStoreExtension("postgres", slackstore.ExtKey, func(app, admin db.Execer) any {
		return &slackstore.Bundle{
			Workspaces: newWorkspaceStore(app, admin),
			Identities: newIdentityStore(admin),
			Deliveries: newDeliveryStore(admin),
		}
	})
}

// rowScanner is the read shape shared by *sql.Row and *sql.Rows, so
// scanWorkspace serves both the single-row Get and the iterating
// ListForOrg/ListAllSystem without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}

// workspaceStore is the Postgres impl of slackstore.WorkspaceStore.
//
//   - app: Upsert, ListForOrg, Get, Delete. Request-handler reads/writes
//     gated by the org_slack_workspaces_* RLS policies.
//   - admin: ListAllSystem. The future socket connection manager enumerates
//     every configured workspace across every org at boot — no per-request
//     claims to gate on.
type workspaceStore struct {
	app   db.Execer
	admin db.Execer
}

func newWorkspaceStore(app, admin db.Execer) slackstore.WorkspaceStore {
	return &workspaceStore{app: app, admin: admin}
}

var _ slackstore.WorkspaceStore = (*workspaceStore)(nil)

const workspaceColumns = `
	workspace_id, api_app_id, org_id::text, workspace_name, COALESCE(enterprise_id, ''), transport,
	bot_user_id, bot_token_ref, COALESCE(signing_secret_ref, ''), COALESCE(app_token_ref, ''),
	COALESCE(registered_by_user_id::text, ''), created_at, updated_at
`

func scanWorkspace(sc rowScanner) (slackstore.Workspace, error) {
	var w slackstore.Workspace
	if err := sc.Scan(
		&w.WorkspaceID, &w.APIAppID, &w.OrgID, &w.WorkspaceName, &w.EnterpriseID, &w.Transport,
		&w.BotUserID, &w.BotTokenRef, &w.SigningSecretRef, &w.AppTokenRef,
		&w.RegisteredByUserID, &w.CreatedAt, &w.UpdatedAt,
	); err != nil {
		return slackstore.Workspace{}, err
	}
	return w, nil
}

// Upsert writes ws as the full desired end-state. It tries an UPDATE scoped
// to (workspace_id, api_app_id, org_id) first — a re-submit for a
// (workspace, app) pair this org already owns; if that affects zero rows
// (either the (workspace_id, api_app_id) pair is wholly new, or it belongs
// to a different org), it falls through to a plain INSERT. A different
// org's (workspace_id, api_app_id) pair then hits the table's PRIMARY KEY
// head-on and returns a real unique-violation error — no pre-check race
// window, matching the sso_domains claim pattern (the constraint is the
// authority, the handler translates the error). Callers MUST have already
// called LockApp and checked AppBoundToOtherOrgSystem before calling
// Upsert — this PK alone does not catch a DIFFERENT workspace_id already
// carrying ws.APIAppID under another org, since api_app_id isn't unique on
// its own (see the interface doc).
//
// Nullable columns (enterprise_id, signing_secret_ref, app_token_ref,
// registered_by_user_id) are written NULL when ws carries "" — NULLIF turns
// an empty string request into a NULL column rather than a literal ”.
func (s *workspaceStore) Upsert(ctx context.Context, ws slackstore.Workspace) error {
	res, err := s.app.ExecContext(ctx, `
		UPDATE org_slack_workspaces SET
			workspace_name = $4,
			enterprise_id = NULLIF($5, ''),
			transport = $6,
			bot_user_id = $7,
			bot_token_ref = $8,
			signing_secret_ref = NULLIF($9, ''),
			app_token_ref = NULLIF($10, ''),
			registered_by_user_id = NULLIF($11, '')::uuid
		WHERE workspace_id = $1 AND api_app_id = $2 AND org_id = $3
	`, ws.WorkspaceID, ws.APIAppID, ws.OrgID, ws.WorkspaceName, ws.EnterpriseID, ws.Transport,
		ws.BotUserID, ws.BotTokenRef, ws.SigningSecretRef, ws.AppTokenRef, ws.RegisteredByUserID)
	if err != nil {
		return fmt.Errorf("update org_slack_workspaces: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO org_slack_workspaces (
			workspace_id, api_app_id, org_id, workspace_name, enterprise_id, transport,
			bot_user_id, bot_token_ref, signing_secret_ref, app_token_ref, registered_by_user_id
		) VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, '')::uuid)
	`, ws.WorkspaceID, ws.APIAppID, ws.OrgID, ws.WorkspaceName, ws.EnterpriseID, ws.Transport,
		ws.BotUserID, ws.BotTokenRef, ws.SigningSecretRef, ws.AppTokenRef, ws.RegisteredByUserID); err != nil {
		return fmt.Errorf("insert org_slack_workspaces: %w", err)
	}
	return nil
}

func (s *workspaceStore) ListForOrg(ctx context.Context, orgID string) ([]slackstore.Workspace, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+workspaceColumns+`
		FROM org_slack_workspaces
		WHERE org_id = $1
		ORDER BY created_at DESC, workspace_id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list org_slack_workspaces: %w", err)
	}
	defer rows.Close()

	out := []slackstore.Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("scan org_slack_workspaces: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *workspaceStore) Get(ctx context.Context, orgID, workspaceID, apiAppID string) (*slackstore.Workspace, error) {
	w, err := scanWorkspace(s.app.QueryRowContext(ctx, `
		SELECT `+workspaceColumns+`
		FROM org_slack_workspaces
		WHERE workspace_id = $1 AND api_app_id = $2 AND org_id = $3
	`, workspaceID, apiAppID, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_slack_workspaces: %w", err)
	}
	return &w, nil
}

func (s *workspaceStore) Delete(ctx context.Context, orgID, workspaceID, apiAppID string) error {
	if _, err := s.app.ExecContext(ctx, `
		DELETE FROM org_slack_workspaces WHERE workspace_id = $1 AND api_app_id = $2 AND org_id = $3
	`, workspaceID, apiAppID, orgID); err != nil {
		return fmt.Errorf("delete org_slack_workspaces: %w", err)
	}
	return nil
}

func (s *workspaceStore) ListAllSystem(ctx context.Context) ([]slackstore.Workspace, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+workspaceColumns+`
		FROM org_slack_workspaces
		ORDER BY org_id ASC, workspace_id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all org_slack_workspaces: %w", err)
	}
	defer rows.Close()

	out := []slackstore.Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("scan org_slack_workspaces: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *workspaceStore) GetByWorkspaceAppSystem(ctx context.Context, workspaceID, apiAppID string) (*slackstore.Workspace, error) {
	w, err := scanWorkspace(s.admin.QueryRowContext(ctx, `
		SELECT `+workspaceColumns+`
		FROM org_slack_workspaces
		WHERE workspace_id = $1 AND api_app_id = $2
	`, workspaceID, apiAppID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_slack_workspaces by workspace+app id: %w", err)
	}
	return &w, nil
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
func (s *workspaceStore) LockApp(ctx context.Context, apiAppID string) error {
	if _, err := s.app.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 2))`, apiAppID); err != nil {
		return fmt.Errorf("lock app invariant: %w", err)
	}
	return nil
}

// AppBoundToOtherOrgSystem runs on the admin pool (bypasses RLS) since the
// question — "does ANY org other than mine already own this api_app_id" —
// is by definition unanswerable from an RLS-scoped view that only shows the
// caller's own rows.
func (s *workspaceStore) AppBoundToOtherOrgSystem(ctx context.Context, apiAppID, orgID string) (bool, error) {
	var exists bool
	if err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM org_slack_workspaces WHERE api_app_id = $1 AND org_id != $2
		)
	`, apiAppID, orgID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check org_slack_workspaces app-single-org invariant: %w", err)
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
// > 0) versus an already-seen duplicate. The prune runs after, best-effort:
// its error (if any) is swallowed rather than returned — a missed prune
// just means the table grows a little until the next successful call, never
// a reason to fail the delivery this call is trying to dedup.
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
	fresh := n > 0

	_, _ = s.admin.ExecContext(ctx, `
		DELETE FROM slack_event_deliveries WHERE received_at < $1
	`, time.Now().Add(-deliveryPruneAge))

	return fresh, nil
}
