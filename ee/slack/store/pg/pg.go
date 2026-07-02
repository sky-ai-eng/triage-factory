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

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func init() {
	db.RegisterStoreExtension("postgres", slackstore.ExtKey, func(app, admin db.Execer) any {
		return &slackstore.Bundle{
			Workspaces: newWorkspaceStore(app, admin),
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
	workspace_id, org_id::text, workspace_name, COALESCE(enterprise_id, ''), transport,
	bot_user_id, bot_token_ref, COALESCE(signing_secret_ref, ''), COALESCE(app_token_ref, ''),
	COALESCE(registered_by_user_id::text, ''), created_at, updated_at
`

func scanWorkspace(sc rowScanner) (slackstore.Workspace, error) {
	var w slackstore.Workspace
	if err := sc.Scan(
		&w.WorkspaceID, &w.OrgID, &w.WorkspaceName, &w.EnterpriseID, &w.Transport,
		&w.BotUserID, &w.BotTokenRef, &w.SigningSecretRef, &w.AppTokenRef,
		&w.RegisteredByUserID, &w.CreatedAt, &w.UpdatedAt,
	); err != nil {
		return slackstore.Workspace{}, err
	}
	return w, nil
}

// Upsert writes ws as the full desired end-state. It tries an UPDATE scoped
// to (workspace_id, org_id) first — a re-submit for a workspace this org
// already owns; if that affects zero rows (either the workspace_id is
// wholly new, or it belongs to a different org), it falls through to a
// plain INSERT. A different org's workspace_id then hits the table's
// PRIMARY KEY head-on and returns a real unique-violation error — no
// pre-check race window, matching the sso_domains claim pattern (the
// constraint is the authority, the handler translates the error).
//
// Nullable columns (enterprise_id, signing_secret_ref, app_token_ref,
// registered_by_user_id) are written NULL when ws carries "" — NULLIF turns
// an empty string request into a NULL column rather than a literal ”.
func (s *workspaceStore) Upsert(ctx context.Context, ws slackstore.Workspace) error {
	res, err := s.app.ExecContext(ctx, `
		UPDATE org_slack_workspaces SET
			workspace_name = $3,
			enterprise_id = NULLIF($4, ''),
			transport = $5,
			bot_user_id = $6,
			bot_token_ref = $7,
			signing_secret_ref = NULLIF($8, ''),
			app_token_ref = NULLIF($9, ''),
			registered_by_user_id = NULLIF($10, '')::uuid
		WHERE workspace_id = $1 AND org_id = $2
	`, ws.WorkspaceID, ws.OrgID, ws.WorkspaceName, ws.EnterpriseID, ws.Transport,
		ws.BotUserID, ws.BotTokenRef, ws.SigningSecretRef, ws.AppTokenRef, ws.RegisteredByUserID)
	if err != nil {
		return fmt.Errorf("update org_slack_workspace: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO org_slack_workspaces (
			workspace_id, org_id, workspace_name, enterprise_id, transport,
			bot_user_id, bot_token_ref, signing_secret_ref, app_token_ref, registered_by_user_id
		) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, '')::uuid)
	`, ws.WorkspaceID, ws.OrgID, ws.WorkspaceName, ws.EnterpriseID, ws.Transport,
		ws.BotUserID, ws.BotTokenRef, ws.SigningSecretRef, ws.AppTokenRef, ws.RegisteredByUserID); err != nil {
		return fmt.Errorf("insert org_slack_workspace: %w", err)
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
			return nil, fmt.Errorf("scan org_slack_workspace: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *workspaceStore) Get(ctx context.Context, orgID, workspaceID string) (*slackstore.Workspace, error) {
	w, err := scanWorkspace(s.app.QueryRowContext(ctx, `
		SELECT `+workspaceColumns+`
		FROM org_slack_workspaces
		WHERE workspace_id = $1 AND org_id = $2
	`, workspaceID, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_slack_workspace: %w", err)
	}
	return &w, nil
}

func (s *workspaceStore) Delete(ctx context.Context, orgID, workspaceID string) error {
	if _, err := s.app.ExecContext(ctx, `
		DELETE FROM org_slack_workspaces WHERE workspace_id = $1 AND org_id = $2
	`, workspaceID, orgID); err != nil {
		return fmt.Errorf("delete org_slack_workspace: %w", err)
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
			return nil, fmt.Errorf("scan org_slack_workspace: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
