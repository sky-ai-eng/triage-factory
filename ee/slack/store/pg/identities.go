package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// identityStore is the Postgres impl of slackstore.SlackIdentityStore.
// Admin-pool only (BYPASSRLS): every method is a `...System` call — capture
// is system-initiated at ingest, in a detached goroutine with no request
// claims context. See the interface doc and the table's RLS notes.
type identityStore struct {
	admin db.Execer
}

func newIdentityStore(admin db.Execer) slackstore.SlackIdentityStore {
	return &identityStore{admin: admin}
}

var _ slackstore.SlackIdentityStore = (*identityStore)(nil)

func (s *identityStore) LookupSystem(ctx context.Context, workspaceID, slackUserID string) (*slackstore.SlackIdentity, error) {
	var id slackstore.SlackIdentity
	var userID, displayName sql.NullString
	err := s.admin.QueryRowContext(ctx, `
		SELECT workspace_id, slack_user_id, user_id::text, slack_display_name, source, last_attempt_at
		FROM user_slack_identities
		WHERE workspace_id = $1 AND slack_user_id = $2
	`, workspaceID, slackUserID).Scan(
		&id.WorkspaceID, &id.SlackUserID, &userID, &displayName, &id.Source, &id.LastAttemptAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup user_slack_identities: %w", err)
	}
	if userID.Valid {
		v := userID.String
		id.UserID = &v
	}
	id.SlackDisplayName = displayName.String
	return &id, nil
}

// UpsertResolvedSystem always writes source='auto_email' — the only writer
// today. The future Sign-in-with-Slack OIDC leaf resolves through a
// different, self-service (app-pool) write path, not this admin-pool method.
func (s *identityStore) UpsertResolvedSystem(ctx context.Context, workspaceID, slackUserID, userID, displayName string) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO user_slack_identities
			(workspace_id, slack_user_id, user_id, slack_display_name, source, last_attempt_at, created_at, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), 'auto_email', now(), now(), now())
		ON CONFLICT (workspace_id, slack_user_id) DO UPDATE SET
			user_id             = EXCLUDED.user_id,
			slack_display_name  = EXCLUDED.slack_display_name,
			source              = EXCLUDED.source,
			last_attempt_at     = now(),
			updated_at          = now()
	`, workspaceID, slackUserID, userID, displayName)
	if err != nil {
		return fmt.Errorf("upsert resolved user_slack_identities: %w", err)
	}
	return nil
}

// MarkAttemptSystem's ON CONFLICT WHERE guard is the whole contract here: it
// only ever touches a row that is still unresolved (user_id IS NULL), so a
// negative-cache write racing behind (or arriving after) a resolution can
// never clobber it — the UPDATE simply matches zero rows in that case.
func (s *identityStore) MarkAttemptSystem(ctx context.Context, workspaceID, slackUserID, displayName string) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO user_slack_identities
			(workspace_id, slack_user_id, user_id, slack_display_name, source, last_attempt_at, created_at, updated_at)
		VALUES ($1, $2, NULL, NULLIF($3, ''), 'auto_email', now(), now(), now())
		ON CONFLICT (workspace_id, slack_user_id) DO UPDATE SET
			last_attempt_at    = now(),
			slack_display_name = EXCLUDED.slack_display_name
		WHERE user_slack_identities.user_id IS NULL
	`, workspaceID, slackUserID, displayName)
	if err != nil {
		return fmt.Errorf("mark attempt user_slack_identities: %w", err)
	}
	return nil
}
