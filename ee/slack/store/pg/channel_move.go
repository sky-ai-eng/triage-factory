package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// channelMoveLockSalt is the hashtextextended salt for the per-org channel
// move lock ("CHID"). See the salt registry in internal/server/advisorylock.go.
const channelMoveLockSalt int64 = 0x43484944

// MoveSystem — see the interface doc. The per-org advisory lock serializes
// two deliveries of the same change (each app the org has in the channel
// receives its own); the second finds nothing left under oldID.
//
// The rows live in tables core owns as well as in the Slack ones, and they
// move in one transaction because a half-moved channel routes a thread's
// messages to an entity no team tracks. The rules for the core rows are
// Slack's own — a thread key is a channel id and a timestamp, a Slack
// artifact's key is the message's key — so they are written here rather than
// as a source-agnostic method on the entity store.
func (s *channelRegistryStore) MoveSystem(ctx context.Context, orgID, oldID, newID string) (slackstore.ChannelMove, error) {
	if oldID == "" || newID == "" {
		return slackstore.ChannelMove{}, errors.New("move slack channel: empty channel id")
	}
	if oldID == newID {
		return slackstore.ChannelMove{}, nil
	}
	var out slackstore.ChannelMove
	err := inTx(ctx, s.admin, func(q db.Execer) error {
		if _, err := q.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, $2))`, orgID, channelMoveLockSalt); err != nil {
			return fmt.Errorf("lock channel move: %w", err)
		}
		if err := recordChannelIDChange(ctx, q, orgID, oldID, newID); err != nil {
			return err
		}
		if err := moveRegistryRow(ctx, q, orgID, oldID, newID); err != nil {
			return err
		}
		var err error
		if out.Trackers, err = moveTrackingRows(ctx, q, orgID, oldID, newID); err != nil {
			return err
		}
		if out.Entities, out.Superseded, err = moveThreadEntities(ctx, q, orgID, oldID, newID); err != nil {
			return err
		}
		if out.Artifacts, err = moveArtifacts(ctx, q, orgID, oldID, newID); err != nil {
			return err
		}
		if out.Actions, err = moveActionPointers(ctx, q, orgID, oldID, newID); err != nil {
			return err
		}
		if out.Handlers, err = moveHandlerFilters(ctx, q, orgID, oldID, newID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return slackstore.ChannelMove{}, err
	}
	return out, nil
}

// recordChannelIDChange records oldID → newID, keeping every recorded change
// one hop from the id the channel has now. A record of newID having moved to
// oldID is dropped first: newID is current again, and re-pointing that record
// would map an id onto itself.
func recordChannelIDChange(ctx context.Context, q db.Execer, orgID, oldID, newID string) error {
	if _, err := q.ExecContext(ctx, `
		DELETE FROM slack_channel_id_changes
		WHERE org_id = $1 AND old_channel_id = $2 AND new_channel_id = $3
	`, orgID, newID, oldID); err != nil {
		return fmt.Errorf("drop reversed slack channel id change: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE slack_channel_id_changes SET new_channel_id = $3
		WHERE org_id = $1 AND new_channel_id = $2
	`, orgID, oldID, newID); err != nil {
		return fmt.Errorf("re-point earlier slack channel id changes: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO slack_channel_id_changes (org_id, old_channel_id, new_channel_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (org_id, old_channel_id) DO UPDATE SET
			new_channel_id = EXCLUDED.new_channel_id,
			changed_at     = EXCLUDED.changed_at
		WHERE slack_channel_id_changes.new_channel_id <> EXCLUDED.new_channel_id
	`, orgID, oldID, newID); err != nil {
		return fmt.Errorf("record slack channel id change: %w", err)
	}
	return nil
}

// moveRegistryRow moves the slack_channels row, merging it into the row newID
// already has when a message under the new id was sighted first. The newID
// row's workspace is the more recent sighting and is kept; its name is kept
// when it has one.
func moveRegistryRow(ctx context.Context, q db.Execer, orgID, oldID, newID string) error {
	if _, err := q.ExecContext(ctx, `
		INSERT INTO slack_channels (org_id, channel_id, workspace_id, name, name_resolved_at, first_seen_at, last_mention_at)
		SELECT org_id, $3, workspace_id, name, name_resolved_at, first_seen_at, last_mention_at
		FROM slack_channels
		WHERE org_id = $1 AND channel_id = $2
		ON CONFLICT (org_id, channel_id) DO UPDATE SET
			first_seen_at    = LEAST(slack_channels.first_seen_at, EXCLUDED.first_seen_at),
			last_mention_at  = GREATEST(slack_channels.last_mention_at, EXCLUDED.last_mention_at),
			name             = CASE WHEN slack_channels.name <> '' THEN slack_channels.name ELSE EXCLUDED.name END,
			name_resolved_at = CASE WHEN slack_channels.name <> '' THEN slack_channels.name_resolved_at ELSE EXCLUDED.name_resolved_at END
	`, orgID, oldID, newID); err != nil {
		return fmt.Errorf("move slack_channels row: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		DELETE FROM slack_channels WHERE org_id = $1 AND channel_id = $2
	`, orgID, oldID); err != nil {
		return fmt.Errorf("drop moved slack_channels row: %w", err)
	}
	return nil
}

// moveTrackingRows moves team_slack_channels. The statements are ordered so
// the one-primary index never sees two primaries for the new id: a primary
// the new id gained is demoted first, when the old id has one.
func moveTrackingRows(ctx context.Context, q db.Execer, orgID, oldID, newID string) (int, error) {
	var trackers int
	if err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM team_slack_channels WHERE org_id = $1 AND channel_id = $2
	`, orgID, oldID).Scan(&trackers); err != nil {
		return 0, fmt.Errorf("count team_slack_channels to move: %w", err)
	}
	if trackers == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE team_slack_channels SET is_primary = false
		WHERE org_id = $1 AND channel_id = $3 AND is_primary
		  AND EXISTS (
		      SELECT 1 FROM team_slack_channels
		      WHERE org_id = $1 AND channel_id = $2 AND is_primary
		  )
	`, orgID, oldID, newID); err != nil {
		return 0, fmt.Errorf("demote new channel id's primary: %w", err)
	}
	// A team tracking both ids keeps its new-id row, carrying over the old
	// row's primary and its created_at, which primary succession orders on.
	if _, err := q.ExecContext(ctx, `
		UPDATE team_slack_channels n SET
			is_primary = n.is_primary OR o.is_primary,
			created_at = LEAST(n.created_at, o.created_at)
		FROM team_slack_channels o
		WHERE n.org_id = $1 AND n.channel_id = $3
		  AND o.org_id = $1 AND o.channel_id = $2 AND o.team_id = n.team_id
	`, orgID, oldID, newID); err != nil {
		return 0, fmt.Errorf("merge team_slack_channels tracking both ids: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		DELETE FROM team_slack_channels o
		WHERE o.org_id = $1 AND o.channel_id = $2
		  AND EXISTS (
		      SELECT 1 FROM team_slack_channels n
		      WHERE n.org_id = $1 AND n.channel_id = $3 AND n.team_id = o.team_id
		  )
	`, orgID, oldID, newID); err != nil {
		return 0, fmt.Errorf("drop merged team_slack_channels rows: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE team_slack_channels SET channel_id = $3 WHERE org_id = $1 AND channel_id = $2
	`, orgID, oldID, newID); err != nil {
		return 0, fmt.Errorf("move team_slack_channels rows: %w", err)
	}
	return trackers, nil
}

// moveThreadEntities rekeys every Slack entity under oldID, in any state.
// Only an active row can collide (keys are unique among active rows), and
// only with an active row the new id minted first — which is closed so the
// original can take the key.
func moveThreadEntities(ctx context.Context, q db.Execer, orgID, oldID, newID string) (int, []string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, source_id, COALESCE(url, ''), state FROM entities
		WHERE org_id = $1 AND source = 'slack' AND scope = $2 AND starts_with(source_id, $3)
		ORDER BY id
		FOR UPDATE
	`, orgID, domain.SlackScope, oldID+"/")
	if err != nil {
		return 0, nil, fmt.Errorf("list slack entities to move: %w", err)
	}
	type held struct{ id, key, url, state string }
	var found []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.key, &h.url, &h.state); err != nil {
			rows.Close()
			return 0, nil, err
		}
		found = append(found, h)
	}
	if err := rows.Close(); err != nil {
		return 0, nil, err
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	var superseded []string
	for _, h := range found {
		newKey, ok := slackstore.MoveThreadKey(h.key, oldID, newID)
		if !ok {
			continue
		}
		if h.state == "active" {
			var holder string
			err := q.QueryRowContext(ctx, `
				UPDATE entities SET state = 'closed', closed_at = now()
				WHERE org_id = $1 AND source = 'slack' AND scope = $2 AND source_id = $3
				  AND state = 'active' AND id <> $4
				RETURNING id
			`, orgID, domain.SlackScope, newKey, h.id).Scan(&holder)
			switch {
			case err == nil:
				superseded = append(superseded, holder)
			case !errors.Is(err, sql.ErrNoRows):
				return 0, nil, fmt.Errorf("close slack entity minted under %s: %w", newKey, err)
			}
		}
		url := h.url
		if moved, ok := slackstore.MovePermalink(h.url, oldID, newID); ok {
			url = moved
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE entities SET source_id = $1, url = $2 WHERE org_id = $3 AND id = $4
		`, newKey, url, orgID, h.id); err != nil {
			return 0, nil, fmt.Errorf("move slack entity %s -> %s: %w", h.key, newKey, err)
		}
	}
	return len(found), superseded, nil
}

// moveArtifacts rewrites the Slack artifacts naming the channel: the target
// (the thread's key), the dedup key (the message's key) and the permalink. A
// dedup key the new id already has — the same message recorded again under
// the new id before the change arrived — stays where it is rather than
// colliding; its target and link still move.
func moveArtifacts(ctx context.Context, q db.Execer, orgID, oldID, newID string) (int, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, dedup_key, target, COALESCE(url, '') FROM artifacts
		WHERE org_id = $1 AND provider = $2
		  AND (starts_with(target, $3) OR strpos(dedup_key, ':' || $3) > 0)
		ORDER BY id
		FOR UPDATE
	`, orgID, domain.ArtifactProviderSlack, oldID+"/")
	if err != nil {
		return 0, fmt.Errorf("list slack artifacts to move: %w", err)
	}
	type held struct{ id, key, target, url string }
	var found []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.key, &h.target, &h.url); err != nil {
			rows.Close()
			return 0, err
		}
		found = append(found, h)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	moved := 0
	for _, h := range found {
		target, targetMoved := slackstore.MoveThreadKey(h.target, oldID, newID)
		key, keyMoved := slackstore.MoveArtifactKey(h.key, oldID, newID)
		url, urlMoved := slackstore.MovePermalink(h.url, oldID, newID)
		if keyMoved {
			var taken bool
			if err := q.QueryRowContext(ctx, `
				SELECT EXISTS (SELECT 1 FROM artifacts WHERE org_id = $1 AND dedup_key = $2)
			`, orgID, key).Scan(&taken); err != nil {
				return 0, fmt.Errorf("check moved artifact key %s: %w", key, err)
			}
			if taken {
				key, keyMoved = h.key, false
			}
		}
		if !targetMoved && !keyMoved && !urlMoved {
			continue
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE artifacts SET target = $1, dedup_key = $2, url = NULLIF($3, ''), updated_at = now()
			WHERE org_id = $4 AND id = $5
		`, target, key, url, orgID, h.id); err != nil {
			return 0, fmt.Errorf("move slack artifact %s: %w", h.id, err)
		}
		moved++
	}
	return moved, nil
}

// moveActionPointers moves the audit ledger's pointer for Slack actions whose
// link points into the channel's archive. Only current_url is written; the
// pointer's current value is the rewrite base, so successive moves chain.
func moveActionPointers(ctx context.Context, q db.Execer, orgID, oldID, newID string) (int, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, COALESCE(current_url, url) FROM external_actions
		WHERE org_id = $1 AND provider = $2
		  AND strpos(COALESCE(current_url, url, ''), '/archives/' || $3) > 0
		ORDER BY id
	`, orgID, domain.ArtifactProviderSlack, oldID)
	if err != nil {
		return 0, fmt.Errorf("list slack external actions to move: %w", err)
	}
	type pending struct{ id, url string }
	var updates []pending
	for rows.Next() {
		var id, u string
		if err := rows.Scan(&id, &u); err != nil {
			rows.Close()
			return 0, err
		}
		if moved, ok := slackstore.MovePermalink(u, oldID, newID); ok {
			updates = append(updates, pending{id: id, url: moved})
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, u := range updates {
		if _, err := q.ExecContext(ctx, `
			UPDATE external_actions SET current_url = $1 WHERE org_id = $2 AND id = $3
		`, u.url, orgID, u.id); err != nil {
			return 0, fmt.Errorf("move slack external action pointer %s: %w", u.id, err)
		}
	}
	return len(updates), nil
}

// moveHandlerFilters rewrites the channel_in filter of every slack:message
// handler naming oldID. Only that key changes; every other predicate field is
// carried through as stored.
func moveHandlerFilters(ctx context.Context, q db.Execer, orgID, oldID, newID string) (int, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, scope_predicate_json::text FROM event_handlers
		WHERE org_id = $1 AND event_type = $2
		  AND scope_predicate_json->'channel_in' @> jsonb_build_array($3::text)
		ORDER BY id
		FOR UPDATE
	`, orgID, domain.EventSlackMessage, oldID)
	if err != nil {
		return 0, fmt.Errorf("list slack handlers to move: %w", err)
	}
	type pending struct{ id, predicate string }
	var updates []pending
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		predicate, ok, err := moveChannelInPredicate(raw, oldID, newID)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("slack handler %s: %w", id, err)
		}
		if ok {
			updates = append(updates, pending{id: id, predicate: predicate})
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, u := range updates {
		if _, err := q.ExecContext(ctx, `
			UPDATE event_handlers SET scope_predicate_json = $1::jsonb WHERE org_id = $2 AND id = $3
		`, u.predicate, orgID, u.id); err != nil {
			return 0, fmt.Errorf("move slack handler %s channel filter: %w", u.id, err)
		}
	}
	return len(updates), nil
}

// moveChannelInPredicate rewrites the channel_in list of one stored predicate.
// The predicate is decoded field by field rather than into the predicate
// type, so a field this package does not know is written back untouched.
func moveChannelInPredicate(raw, oldID, newID string) (string, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return "", false, fmt.Errorf("decode predicate: %w", err)
	}
	var channels []string
	if err := json.Unmarshal(fields["channel_in"], &channels); err != nil {
		return "", false, fmt.Errorf("decode channel_in: %w", err)
	}
	moved, ok := slackstore.MoveChannelFilter(channels, oldID, newID)
	if !ok {
		return "", false, nil
	}
	encoded, err := json.Marshal(moved)
	if err != nil {
		return "", false, err
	}
	fields["channel_in"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

// CurrentIDSystem — see the interface doc. Every recorded change points at
// the id the channel has now, so one read answers.
func (s *channelRegistryStore) CurrentIDSystem(ctx context.Context, orgID, channelID string) (string, error) {
	var current string
	err := s.admin.QueryRowContext(ctx, `
		SELECT new_channel_id FROM slack_channel_id_changes
		WHERE org_id = $1 AND old_channel_id = $2
	`, orgID, channelID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return channelID, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve slack channel id %s: %w", channelID, err)
	}
	return current, nil
}
