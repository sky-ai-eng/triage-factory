package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// channelRegistryStore is the Postgres impl of slackstore.ChannelRegistryStore.
//
//   - app: ListForOrg — the member-visible read (slack_channels_select RLS
//     gates on org membership; there is no app-pool write policy at all,
//     see the migration's RLS notes).
//   - admin: every write (UpsertSightingSystem, EnsureSystem, SetNameSystem,
//     FindOrCreateThreadSystem, MoveSystem) and ResolveSystem — ingest
//     sightings, name resolution, thread creation and the channels API's
//     ensure/resolve are all system-initiated, claims-free callers.
type channelRegistryStore struct {
	app   db.Execer
	admin db.Execer
}

func newChannelRegistryStore(app, admin db.Execer) slackstore.ChannelRegistryStore {
	return &channelRegistryStore{app: app, admin: admin}
}

var _ slackstore.ChannelRegistryStore = (*channelRegistryStore)(nil)

const channelColumns = `
	id::text, org_id::text, channel_id, workspace_id, name, name_resolved_at, first_seen_at, last_mention_at
`

func scanChannel(sc rowScanner) (slackstore.Channel, error) {
	var c slackstore.Channel
	var nameResolvedAt, lastMentionAt sql.NullTime
	if err := sc.Scan(
		&c.ID, &c.OrgID, &c.ChannelID, &c.WorkspaceID, &c.Name, &nameResolvedAt, &c.FirstSeenAt, &lastMentionAt,
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

// currentChannelIDSQL resolves the channel id bound at the given placeholder
// to the id the channel has now, inline. Every recorded change points at the
// channel's current id (MoveSystem keeps it so), so one hop answers. The org
// is always $1.
func currentChannelIDSQL(placeholder string) string {
	return `COALESCE((SELECT x.new_channel_id FROM slack_channel_id_changes x
		WHERE x.org_id = $1 AND x.old_channel_id = ` + placeholder + `), ` + placeholder + `)`
}

// lockChannelWrites takes the per-org channel move lock in shared mode, for a
// write that resolves a channel id and writes rows spelling it: a move holds
// the lock exclusively, so it waits for the write to commit, and the write
// waits for a move in progress and then reads the change it recorded.
func lockChannelWrites(ctx context.Context, q db.Execer, orgID string) error {
	if _, err := q.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1, $2))`, orgID, channelMoveLockSalt); err != nil {
		return fmt.Errorf("lock channel move: %w", err)
	}
	return nil
}

// UpsertSightingSystem is one statement under the shared move lock: the
// insert arm records the first sight, the conflict arm bumps workspace_id +
// last_mention_at in place, leaving first_seen_at and name untouched.
func (s *channelRegistryStore) UpsertSightingSystem(ctx context.Context, orgID, workspaceID, channelID string, at time.Time) (slackstore.Channel, error) {
	var c slackstore.Channel
	err := inTx(ctx, s.admin, func(q db.Execer) error {
		if err := lockChannelWrites(ctx, q, orgID); err != nil {
			return err
		}
		var err error
		c, err = scanChannel(q.QueryRowContext(ctx, `
			INSERT INTO slack_channels (org_id, channel_id, workspace_id, first_seen_at, last_mention_at)
			VALUES ($1, `+currentChannelIDSQL("$2")+`, $3, $4, $4)
			ON CONFLICT (org_id, channel_id) DO UPDATE SET
				workspace_id    = EXCLUDED.workspace_id,
				last_mention_at = EXCLUDED.last_mention_at
			RETURNING `+channelColumns,
			orgID, channelID, workspaceID, at))
		return err
	})
	if err != nil {
		return slackstore.Channel{}, fmt.Errorf("upsert slack_channels sighting: %w", err)
	}
	return c, nil
}

// EnsureSystem inserts a skeleton row when absent, deliberately leaving
// first_seen_at/last_mention_at at their INSERT defaults/NULL rather than
// stamping a mention that never happened. On conflict it refreshes
// workspace_id/name only when the caller supplied a non-empty value,
// preserving whatever a prior sighting or ensure already established.
func (s *channelRegistryStore) EnsureSystem(ctx context.Context, orgID, workspaceID, channelID, name string) (slackstore.Channel, error) {
	var c slackstore.Channel
	err := inTx(ctx, s.admin, func(q db.Execer) error {
		if err := lockChannelWrites(ctx, q, orgID); err != nil {
			return err
		}
		var err error
		c, err = scanChannel(q.QueryRowContext(ctx, `
			INSERT INTO slack_channels (org_id, channel_id, workspace_id, name, name_resolved_at)
			VALUES ($1, `+currentChannelIDSQL("$2")+`, $3, $4, CASE WHEN $4 <> '' THEN now() END)
			ON CONFLICT (org_id, channel_id) DO UPDATE SET
				workspace_id     = CASE WHEN EXCLUDED.workspace_id <> '' THEN EXCLUDED.workspace_id ELSE slack_channels.workspace_id END,
				name             = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE slack_channels.name END,
				name_resolved_at = CASE WHEN EXCLUDED.name <> '' THEN now() ELSE slack_channels.name_resolved_at END
			RETURNING `+channelColumns,
			orgID, channelID, workspaceID, name))
		return err
	})
	if err != nil {
		return slackstore.Channel{}, fmt.Errorf("ensure slack_channels: %w", err)
	}
	return c, nil
}

func (s *channelRegistryStore) SetNameSystem(ctx context.Context, orgID, id, name string) error {
	if _, err := s.admin.ExecContext(ctx, `
		UPDATE slack_channels SET name = $3, name_resolved_at = now()
		WHERE org_id = $1 AND id = $2
	`, orgID, id, name); err != nil {
		return fmt.Errorf("set name slack_channels: %w", err)
	}
	return nil
}

func (s *channelRegistryStore) ResolveSystem(ctx context.Context, orgID, channelID string) (*slackstore.Channel, error) {
	c, err := scanChannel(s.admin.QueryRowContext(ctx, `
		SELECT `+channelColumns+`
		FROM slack_channels
		WHERE org_id = $1 AND channel_id = `+currentChannelIDSQL("$2"),
		orgID, channelID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve slack_channels: %w", err)
	}
	return &c, nil
}

func (s *channelRegistryStore) GetSystem(ctx context.Context, orgID, id string) (*slackstore.Channel, error) {
	c, err := scanChannel(s.admin.QueryRowContext(ctx, `
		SELECT `+channelColumns+`
		FROM slack_channels
		WHERE org_id = $1 AND id = $2
	`, orgID, id))
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
		ORDER BY last_mention_at DESC NULLS LAST, first_seen_at DESC, id
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

// threadKeyOrder is the key lookup's preference among entities sharing a
// thread's key — the active one (there is at most one), otherwise the most
// recently closed — the order db.EntityStore's key lookup reads in.
const threadKeyOrder = `ORDER BY (state = 'active') DESC, closed_at DESC NULLS LAST, created_at DESC, id DESC`

// FindOrCreateThreadSystem — see the interface doc. It writes the entities
// row itself because the row references the channel's registry row, which
// only this store resolves; the insert is the entity store's (DO NOTHING on a
// lost race, then a re-read decides what the loser sees).
func (s *channelRegistryStore) FindOrCreateThreadSystem(ctx context.Context, orgID, channelID, threadTS, kind, title, url string) (slackstore.Thread, error) {
	if threadTS == "" {
		return slackstore.Thread{}, errors.New("find or create slack thread: empty thread ts")
	}
	var out slackstore.Thread
	err := inTx(ctx, s.admin, func(q db.Execer) error {
		if err := lockChannelWrites(ctx, q, orgID); err != nil {
			return err
		}
		channel, err := scanChannel(q.QueryRowContext(ctx, `
			SELECT `+channelColumns+`
			FROM slack_channels
			WHERE org_id = $1 AND channel_id = `+currentChannelIDSQL("$2"),
			orgID, channelID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", slackstore.ErrChannelNotRegistered, channelID)
		}
		if err != nil {
			return fmt.Errorf("resolve slack channel %s: %w", channelID, err)
		}
		out.Channel = channel
		key := domain.SlackSourceID(channel.ChannelID, threadTS)
		find := func() (string, error) {
			var found string
			err := q.QueryRowContext(ctx, `
				SELECT id FROM entities
				WHERE org_id = $1 AND source = 'slack' AND scope = $2 AND source_id = $3
				`+threadKeyOrder+` LIMIT 1
			`, orgID, domain.SlackScope, key).Scan(&found)
			if errors.Is(err, sql.ErrNoRows) {
				return "", nil
			}
			return found, err
		}
		existing, err := find()
		if err != nil {
			return fmt.Errorf("find slack thread %s: %w", key, err)
		}
		if existing != "" {
			out.EntityID = existing
			return nil
		}

		newID := uuid.New().String()
		now := time.Now().UTC()
		res, err := q.ExecContext(ctx, `
			INSERT INTO entities (id, org_id, source, scope, source_id, kind, title, url, state, created_at, last_polled_at, slack_channel_id)
			VALUES ($1, $2, 'slack', $3, $4, $5, $6, $7, 'active', $8, $8, $9)
			ON CONFLICT DO NOTHING
		`, newID, orgID, domain.SlackScope, key, kind, title, url, now, channel.ID)
		if err != nil {
			return fmt.Errorf("insert slack thread %s: %w", key, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			out.EntityID, out.Created = newID, true
			return nil
		}
		if existing, err = find(); err != nil {
			return fmt.Errorf("find slack thread %s: %w", key, err)
		}
		if existing == "" {
			return fmt.Errorf("%w: slack %s in %s", db.ErrEntityKeyOccupied, key, domain.SlackScope)
		}
		out.EntityID = existing
		return nil
	})
	if err != nil {
		return slackstore.Thread{}, err
	}
	return out, nil
}
