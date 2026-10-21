package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// channelMoveLockSalt is the hashtextextended salt for the per-org channel
// move lock ("CHID"). See the salt registry in internal/server/advisorylock.go.
const channelMoveLockSalt int64 = 0x43484944

// MoveSystem — see the interface doc. The per-org advisory lock serializes
// two deliveries of the same change (each app the org has in the channel
// receives its own), and holds off the writes that spell a channel id
// (lockChannelWrites) until the move commits.
//
// References to the channel hold its registry row and need nothing: a thread
// is found by its channel's row and root ts, and a Slack artifact's dedup key
// names the row and the message's ts. What moves is the row's channel_id and
// the display values that spell it, which live in tables core owns as well as
// in the Slack ones; they move in the same transaction, so no display value
// spells an id the channel no longer has. The rules for the core rows
// are Slack's own — a thread key is a channel id and a timestamp — so they are
// written here rather than as a source-agnostic method on the entity store.
func (s *channelRegistryStore) MoveSystem(ctx context.Context, orgID, oldID, newID string) (slackstore.ChannelMove, error) {
	if oldID == "" || newID == "" {
		return slackstore.ChannelMove{}, errors.New("move slack channel: empty channel id")
	}
	if oldID == newID {
		return slackstore.ChannelMove{To: newID}, nil
	}
	var out slackstore.ChannelMove
	// The artifacts rewritten, by id: the absorb rekeys some, and the target
	// and permalink move can reach the same ones.
	artifacts := map[string]bool{}
	err := inTx(ctx, s.admin, func(q db.Execer) error {
		if _, err := q.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, $2))`, orgID, channelMoveLockSalt); err != nil {
			return fmt.Errorf("lock channel move: %w", err)
		}
		to, err := recordChannelIDChange(ctx, q, orgID, oldID, newID)
		if err != nil {
			return err
		}
		out.To = to
		channel, err := registryRowFor(ctx, q, orgID, oldID)
		if err != nil {
			return err
		}
		if channel != "" {
			absorbed, err := registryRowFor(ctx, q, orgID, to)
			if err != nil {
				return err
			}
			if absorbed != "" {
				out.Absorbed = absorbed
				if err := absorbRegistryRow(ctx, q, orgID, channel, absorbed, &out, artifacts); err != nil {
					return err
				}
			}
			if _, err := q.ExecContext(ctx, `
				UPDATE slack_channels SET channel_id = $3 WHERE org_id = $1 AND id = $2
			`, orgID, channel, to); err != nil {
				return fmt.Errorf("move slack_channels row: %w", err)
			}
			if out.Entities, err = moveThreadKeys(ctx, q, orgID, channel, oldID, to); err != nil {
				return err
			}
		}
		if err := moveArtifacts(ctx, q, orgID, channel, oldID, to, artifacts); err != nil {
			return err
		}
		out.Artifacts = len(artifacts)
		if out.Actions, err = moveActionPointers(ctx, q, orgID, oldID, to); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return slackstore.ChannelMove{}, err
	}
	return out, nil
}

// recordChannelIDChange records oldID's change and returns the id its rows
// move to (the cases are on the interface doc). Every recorded change keeps
// naming the id the channel has now: changes recorded onto oldID are
// re-pointed at the returned id, and a reversal drops newID's record first,
// so the re-pointing never maps an id onto itself.
func recordChannelIDChange(ctx context.Context, q db.Execer, orgID, oldID, newID string) (string, error) {
	if to, changed, err := recordedChange(ctx, q, orgID, oldID); err != nil || changed {
		return to, err
	}
	target := newID
	to, changed, err := recordedChange(ctx, q, orgID, newID)
	switch {
	case err != nil:
		return "", err
	case changed && to == oldID:
		if _, err := q.ExecContext(ctx, `
			DELETE FROM slack_channel_id_changes WHERE org_id = $1 AND old_channel_id = $2
		`, orgID, newID); err != nil {
			return "", fmt.Errorf("drop reversed slack channel id change: %w", err)
		}
	case changed:
		target = to
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE slack_channel_id_changes SET new_channel_id = $3
		WHERE org_id = $1 AND new_channel_id = $2
	`, orgID, oldID, target); err != nil {
		return "", fmt.Errorf("re-point earlier slack channel id changes: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO slack_channel_id_changes (org_id, old_channel_id, new_channel_id)
		VALUES ($1, $2, $3)
	`, orgID, oldID, target); err != nil {
		return "", fmt.Errorf("record slack channel id change: %w", err)
	}
	return target, nil
}

// recordedChange returns the id a recorded change moved channelID to, and
// whether one is recorded.
func recordedChange(ctx context.Context, q db.Execer, orgID, channelID string) (string, bool, error) {
	var to string
	err := q.QueryRowContext(ctx, `
		SELECT new_channel_id FROM slack_channel_id_changes
		WHERE org_id = $1 AND old_channel_id = $2
	`, orgID, channelID).Scan(&to)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read slack channel id change for %s: %w", channelID, err)
	}
	return to, true, nil
}

// registryRowFor returns the id of the slack_channels row Slack's channelID
// names, locked for the move, or "" when TF has none.
func registryRowFor(ctx context.Context, q db.Execer, orgID, channelID string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id::text FROM slack_channels WHERE org_id = $1 AND channel_id = $2 FOR UPDATE
	`, orgID, channelID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read slack_channels row for %s: %w", channelID, err)
	}
	return id, nil
}

// absorbRegistryRow merges the row the new id already had (a message under
// the new id was sighted, or a team tracked it, before the change arrived)
// into the channel's row, moves every reference to it onto the channel's row,
// and deletes it. The absorbed row's workspace is the more recent sighting and
// is kept when it has one; so is its name. It records what it moved on out,
// and the artifacts it rekeyed in artifacts.
func absorbRegistryRow(ctx context.Context, q db.Execer, orgID, channel, absorbed string, out *slackstore.ChannelMove, artifacts map[string]bool) error {
	if _, err := q.ExecContext(ctx, `
		UPDATE slack_channels c SET
			first_seen_at    = LEAST(c.first_seen_at, a.first_seen_at),
			last_mention_at  = GREATEST(c.last_mention_at, a.last_mention_at),
			workspace_id     = CASE WHEN a.workspace_id <> '' THEN a.workspace_id ELSE c.workspace_id END,
			name             = CASE WHEN a.name <> '' THEN a.name ELSE c.name END,
			name_resolved_at = CASE WHEN a.name <> '' THEN a.name_resolved_at ELSE c.name_resolved_at END
		FROM slack_channels a
		WHERE c.org_id = $1 AND c.id = $2 AND a.org_id = $1 AND a.id = $3
	`, orgID, channel, absorbed); err != nil {
		return fmt.Errorf("merge absorbed slack_channels row: %w", err)
	}
	var err error
	if out.Trackers, err = absorbTrackingRows(ctx, q, orgID, channel, absorbed); err != nil {
		return err
	}
	if out.Superseded, err = supersedeDuplicateThreads(ctx, q, orgID, channel, absorbed); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE entities SET slack_channel_id = $2 WHERE org_id = $1 AND slack_channel_id = $3
	`, orgID, channel, absorbed); err != nil {
		return fmt.Errorf("move absorbed slack channel's entities: %w", err)
	}
	if err := rekeyAbsorbedArtifacts(ctx, q, orgID, channel, absorbed, artifacts); err != nil {
		return err
	}
	if out.Handlers, out.SkippedHandlers, err = moveHandlerFilters(ctx, q, orgID, absorbed, channel); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `
		DELETE FROM slack_channels WHERE org_id = $1 AND id = $2
	`, orgID, absorbed); err != nil {
		return fmt.Errorf("drop absorbed slack_channels row: %w", err)
	}
	return nil
}

// supersedeDuplicateThreads closes, for each thread that both the channel's
// row and the absorbed row have an active entity for, the newer of the two:
// the older is the thread's original. A tie keeps the channel's. It runs
// before the absorbed row's entities move onto the channel's, so a thread
// never has two active entities. Returns the ids it closed, sorted.
func supersedeDuplicateThreads(ctx context.Context, q db.Execer, orgID, channel, absorbed string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		UPDATE entities e SET state = 'closed', closed_at = now()
		FROM entities c, entities a
		WHERE c.org_id = $1 AND c.slack_channel_id = $2 AND c.state = 'active'
		  AND a.org_id = $1 AND a.slack_channel_id = $3 AND a.state = 'active'
		  AND a.slack_thread_ts = c.slack_thread_ts
		  AND e.org_id = $1
		  AND e.id = CASE WHEN a.created_at < c.created_at THEN c.id ELSE a.id END
		RETURNING e.id::text
	`, orgID, channel, absorbed)
	if err != nil {
		return nil, fmt.Errorf("close superseded slack thread entities: %w", err)
	}
	defer rows.Close()
	var closed []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		closed = append(closed, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(closed)
	return closed, nil
}

// rekeyAbsorbedArtifacts rewrites the dedup keys of the Slack artifacts that
// name the absorbed registry row to name the channel's row, whose messages
// they are from now on: a later write about the same message resolves to the
// channel's row and keys on it. A key the channel's row already holds — the
// same message recorded under both rows before the change arrived — is left
// as it is, since the two rows were two before the move.
func rekeyAbsorbedArtifacts(ctx context.Context, q db.Execer, orgID, channel, absorbed string, artifacts map[string]bool) error {
	rows, err := q.QueryContext(ctx, `
		SELECT id, dedup_key FROM artifacts
		WHERE org_id = $1 AND provider = $2 AND scope = $3
		  AND strpos(dedup_key, ':' || $4 || '/') > 0
		ORDER BY id
		FOR UPDATE
	`, orgID, domain.ArtifactProviderSlack, domain.SlackScope, absorbed)
	if err != nil {
		return fmt.Errorf("list slack artifacts keyed on the absorbed channel row: %w", err)
	}
	type held struct{ id, key string }
	var found []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.key); err != nil {
			rows.Close()
			return err
		}
		found = append(found, h)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range found {
		key, ok := slackstore.MoveArtifactKey(h.key, absorbed, channel)
		if !ok {
			continue
		}
		var taken bool
		if err := q.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM artifacts WHERE org_id = $1 AND scope = $3 AND dedup_key = $2)
		`, orgID, key, domain.SlackScope).Scan(&taken); err != nil {
			return fmt.Errorf("check rekeyed artifact key %s: %w", key, err)
		}
		if taken {
			continue
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE artifacts SET dedup_key = $1, updated_at = now() WHERE org_id = $2 AND id = $3
		`, key, orgID, h.id); err != nil {
			return fmt.Errorf("rekey slack artifact %s: %w", h.id, err)
		}
		artifacts[h.id] = true
	}
	return nil
}

// absorbTrackingRows moves the absorbed row's team_slack_channels onto the
// channel's. The statements are ordered so the one-primary index never sees
// two primaries for the channel: a primary the absorbed row gained is demoted
// first, when the channel has one.
func absorbTrackingRows(ctx context.Context, q db.Execer, orgID, channel, absorbed string) (int, error) {
	var trackers int
	if err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM team_slack_channels WHERE org_id = $1 AND slack_channel_id = $2
	`, orgID, absorbed).Scan(&trackers); err != nil {
		return 0, fmt.Errorf("count team_slack_channels to absorb: %w", err)
	}
	if trackers == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE team_slack_channels SET is_primary = false
		WHERE org_id = $1 AND slack_channel_id = $3 AND is_primary
		  AND EXISTS (
		      SELECT 1 FROM team_slack_channels
		      WHERE org_id = $1 AND slack_channel_id = $2 AND is_primary
		  )
	`, orgID, channel, absorbed); err != nil {
		return 0, fmt.Errorf("demote absorbed slack channel's primary: %w", err)
	}
	// A team tracking both keeps its row on the channel, carrying over the
	// absorbed row's primary and the older created_at, which primary
	// succession orders on.
	if _, err := q.ExecContext(ctx, `
		UPDATE team_slack_channels c SET
			is_primary = c.is_primary OR a.is_primary,
			created_at = LEAST(c.created_at, a.created_at)
		FROM team_slack_channels a
		WHERE c.org_id = $1 AND c.slack_channel_id = $2
		  AND a.org_id = $1 AND a.slack_channel_id = $3 AND a.team_id = c.team_id
	`, orgID, channel, absorbed); err != nil {
		return 0, fmt.Errorf("merge team_slack_channels tracking both rows: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		DELETE FROM team_slack_channels a
		WHERE a.org_id = $1 AND a.slack_channel_id = $3
		  AND EXISTS (
		      SELECT 1 FROM team_slack_channels c
		      WHERE c.org_id = $1 AND c.slack_channel_id = $2 AND c.team_id = a.team_id
		  )
	`, orgID, channel, absorbed); err != nil {
		return 0, fmt.Errorf("drop merged team_slack_channels rows: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE team_slack_channels SET slack_channel_id = $2 WHERE org_id = $1 AND slack_channel_id = $3
	`, orgID, channel, absorbed); err != nil {
		return 0, fmt.Errorf("move absorbed team_slack_channels rows: %w", err)
	}
	return trackers, nil
}

// moveThreadKeys rewrites the display values of the channel's thread
// entities, in any state, whose key does not spell newID: the key becomes
// "<newID>/<thread ts>" and a permalink into oldID's archive moves into
// newID's. A thread is found by its channel's row and root ts, which the move
// leaves alone, so nothing here is an identity. Returns how many it rewrote.
func moveThreadKeys(ctx context.Context, q db.Execer, orgID, channel, oldID, newID string) (int, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, slack_thread_ts, COALESCE(url, '') FROM entities
		WHERE org_id = $1 AND slack_channel_id = $2 AND source_id <> $3 || '/' || slack_thread_ts
		ORDER BY id
		FOR UPDATE
	`, orgID, channel, newID)
	if err != nil {
		return 0, fmt.Errorf("list slack entities to move: %w", err)
	}
	type held struct{ id, ts, url string }
	var found []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.ts, &h.url); err != nil {
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
	for _, h := range found {
		url := h.url
		if moved, ok := slackstore.MovePermalink(h.url, oldID, newID); ok {
			url = moved
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE entities SET source_id = $1, url = $2 WHERE org_id = $3 AND id = $4
		`, domain.SlackSourceID(newID, h.ts), url, orgID, h.id); err != nil {
			return 0, fmt.Errorf("move slack entity %s: %w", h.id, err)
		}
	}
	return len(found), nil
}

// moveArtifacts rewrites the display values of the Slack artifacts naming the
// channel: the target, which is the key of the thread entity the artifact is
// about (artifacts.entity_id) and takes the key moveThreadKeys gave it, and
// the permalink. An artifact linked to no entity has no key to take; its
// target spells the channel id, and moves with it. The dedup key names the
// channel's registry row and the message's ts, so it does not move. channel
// is the channel's registry row, "" when it has none. Each artifact rewritten
// is added to artifacts.
func moveArtifacts(ctx context.Context, q db.Execer, orgID, channel, oldID, newID string, artifacts map[string]bool) error {
	rows, err := q.QueryContext(ctx, `
		SELECT a.id, a.target, COALESCE(a.url, ''), COALESCE(e.source_id, '')
		  FROM artifacts a
		  LEFT JOIN entities e ON e.org_id = a.org_id AND e.id = a.entity_id
		 WHERE a.org_id = $1 AND a.provider = $2 AND a.scope = $4
		   AND ((a.entity_id IS NULL AND starts_with(a.target, $3 || '/'))
		        OR (e.slack_channel_id = NULLIF($5, '')::uuid AND a.target <> e.source_id)
		        OR strpos(COALESCE(a.url, ''), '/archives/' || $3) > 0)
		 ORDER BY a.id
		 FOR UPDATE OF a
	`, orgID, domain.ArtifactProviderSlack, oldID, domain.SlackScope, channel)
	if err != nil {
		return fmt.Errorf("list slack artifacts to move: %w", err)
	}
	type held struct{ id, target, url, entityKey string }
	var found []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.target, &h.url, &h.entityKey); err != nil {
			rows.Close()
			return err
		}
		found = append(found, h)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, h := range found {
		target, targetMoved := h.entityKey, h.entityKey != "" && h.entityKey != h.target
		if h.entityKey == "" {
			target, targetMoved = slackstore.MoveThreadKey(h.target, oldID, newID)
		}
		url, urlMoved := slackstore.MovePermalink(h.url, oldID, newID)
		if !targetMoved && !urlMoved {
			continue
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE artifacts SET target = $1, url = NULLIF($2, ''), updated_at = now()
			WHERE org_id = $3 AND id = $4
		`, target, url, orgID, h.id); err != nil {
			return fmt.Errorf("move slack artifact %s: %w", h.id, err)
		}
		artifacts[h.id] = true
	}
	return nil
}

// moveActionPointers moves the audit ledger's pointer for Slack actions whose
// link points into the channel's archive. Only current_url is written; the
// pointer's current value is the rewrite base, so successive moves chain.
func moveActionPointers(ctx context.Context, q db.Execer, orgID, oldID, newID string) (int, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, COALESCE(current_url, url) FROM external_actions
		WHERE org_id = $1 AND provider = $2 AND scope = $4
		  AND strpos(COALESCE(current_url, url, ''), '/archives/' || $3) > 0
		ORDER BY id
	`, orgID, domain.ArtifactProviderSlack, oldID, domain.SlackScope)
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
// handler naming the registry row oldRef to name newRef. Only that key
// changes; every other predicate field is carried through as stored. A
// handler whose filter cannot be decoded is skipped and reported rather than
// failing the move: the rest of the channel's rows do not wait on one bad
// row.
func moveHandlerFilters(ctx context.Context, q db.Execer, orgID, oldRef, newRef string) (int, []string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, scope_predicate_json::text FROM event_handlers
		WHERE org_id = $1 AND event_type = $2
		  AND scope_predicate_json->'channel_in' @> jsonb_build_array($3::text)
		ORDER BY id
		FOR UPDATE
	`, orgID, domain.EventSlackMessage, oldRef)
	if err != nil {
		return 0, nil, fmt.Errorf("list slack handlers to move: %w", err)
	}
	type pending struct{ id, predicate string }
	var updates []pending
	var skipped []string
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, nil, err
		}
		predicate, ok, err := moveChannelInPredicate(raw, oldRef, newRef)
		switch {
		case err != nil:
			skipped = append(skipped, id)
		case ok:
			updates = append(updates, pending{id: id, predicate: predicate})
		}
	}
	if err := rows.Close(); err != nil {
		return 0, nil, err
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	for _, u := range updates {
		if _, err := q.ExecContext(ctx, `
			UPDATE event_handlers SET scope_predicate_json = $1::jsonb WHERE org_id = $2 AND id = $3
		`, u.predicate, orgID, u.id); err != nil {
			return 0, nil, fmt.Errorf("move slack handler %s channel filter: %w", u.id, err)
		}
	}
	return len(updates), skipped, nil
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
