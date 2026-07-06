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

// channelRegistryStore is the Postgres impl of slackstore.ChannelRegistryStore.
//
//   - app: ListForOrg — the member-visible read (slack_channels_select RLS
//     gates on org membership; there is no app-pool write policy at all,
//     see the migration's RLS notes).
//   - admin: every write (UpsertSightingSystem, EnsureSystem,
//     SetNameSystem) and GetSystem — ingest sightings, name resolution, and
//     the channels API's ensure/get are all system-initiated, claims-free
//     callers.
type channelRegistryStore struct {
	app   db.Execer
	admin db.Execer
}

func newChannelRegistryStore(app, admin db.Execer) slackstore.ChannelRegistryStore {
	return &channelRegistryStore{app: app, admin: admin}
}

var _ slackstore.ChannelRegistryStore = (*channelRegistryStore)(nil)

const channelColumns = `
	org_id::text, channel_id, workspace_id, name, name_resolved_at, first_seen_at, last_mention_at
`

func scanChannel(sc rowScanner) (slackstore.Channel, error) {
	var c slackstore.Channel
	var nameResolvedAt, lastMentionAt sql.NullTime
	if err := sc.Scan(
		&c.OrgID, &c.ChannelID, &c.WorkspaceID, &c.Name, &nameResolvedAt, &c.FirstSeenAt, &lastMentionAt,
	); err != nil {
		return slackstore.Channel{}, err
	}
	if nameResolvedAt.Valid {
		c.NameResolvedAt = &nameResolvedAt.Time
	}
	if lastMentionAt.Valid {
		c.LastMentionAt = &lastMentionAt.Time
	}
	return c, nil
}

// UpsertSightingSystem tries the INSERT first (mirrors DeliveryStore.
// MarkDeliveredSystem's ON CONFLICT DO NOTHING + RowsAffected shape):
// RowsAffected > 0 means this sighting created the row, so created=true and
// there is nothing left to do. Otherwise a row already existed and a second
// statement bumps workspace_id + last_mention_at in place, leaving
// first_seen_at and name untouched.
func (s *channelRegistryStore) UpsertSightingSystem(ctx context.Context, orgID, workspaceID, channelID string, at time.Time) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		INSERT INTO slack_channels (org_id, channel_id, workspace_id, first_seen_at, last_mention_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (org_id, channel_id) DO NOTHING
	`, orgID, channelID, workspaceID, at)
	if err != nil {
		return false, fmt.Errorf("insert slack_channels sighting: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected slack_channels sighting: %w", err)
	}
	if n > 0 {
		return true, nil
	}

	if _, err := s.admin.ExecContext(ctx, `
		UPDATE slack_channels SET workspace_id = $3, last_mention_at = $4
		WHERE org_id = $1 AND channel_id = $2
	`, orgID, channelID, workspaceID, at); err != nil {
		return false, fmt.Errorf("bump slack_channels sighting: %w", err)
	}
	return false, nil
}

// EnsureSystem inserts a skeleton row when absent, deliberately leaving
// first_seen_at/last_mention_at at their INSERT defaults/NULL rather than
// stamping a mention that never happened. On conflict it refreshes
// workspace_id/name only when the caller supplied a non-empty value,
// preserving whatever a prior sighting or ensure already established.
func (s *channelRegistryStore) EnsureSystem(ctx context.Context, orgID, workspaceID, channelID, name string) error {
	if _, err := s.admin.ExecContext(ctx, `
		INSERT INTO slack_channels (org_id, channel_id, workspace_id, name, name_resolved_at)
		VALUES ($1, $2, $3, $4, CASE WHEN $4 <> '' THEN now() END)
		ON CONFLICT (org_id, channel_id) DO UPDATE SET
			workspace_id     = CASE WHEN EXCLUDED.workspace_id <> '' THEN EXCLUDED.workspace_id ELSE slack_channels.workspace_id END,
			name             = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE slack_channels.name END,
			name_resolved_at = CASE WHEN EXCLUDED.name <> '' THEN now() ELSE slack_channels.name_resolved_at END
	`, orgID, channelID, workspaceID, name); err != nil {
		return fmt.Errorf("ensure slack_channels: %w", err)
	}
	return nil
}

func (s *channelRegistryStore) SetNameSystem(ctx context.Context, orgID, channelID, name string) error {
	if _, err := s.admin.ExecContext(ctx, `
		UPDATE slack_channels SET name = $3, name_resolved_at = now()
		WHERE org_id = $1 AND channel_id = $2
	`, orgID, channelID, name); err != nil {
		return fmt.Errorf("set name slack_channels: %w", err)
	}
	return nil
}

func (s *channelRegistryStore) GetSystem(ctx context.Context, orgID, channelID string) (*slackstore.Channel, error) {
	c, err := scanChannel(s.admin.QueryRowContext(ctx, `
		SELECT `+channelColumns+`
		FROM slack_channels
		WHERE org_id = $1 AND channel_id = $2
	`, orgID, channelID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get slack_channels: %w", err)
	}
	return &c, nil
}

func (s *channelRegistryStore) ListForOrg(ctx context.Context, orgID string) ([]slackstore.Channel, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+channelColumns+`
		FROM slack_channels
		WHERE org_id = $1
		ORDER BY last_mention_at DESC NULLS LAST, first_seen_at DESC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list slack_channels: %w", err)
	}
	defer rows.Close()

	out := []slackstore.Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan slack_channels: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
