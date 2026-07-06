package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// teamChannelStore is the Postgres impl of slackstore.TeamChannelStore.
//
//   - app: ListForTeam + ReplaceForTeam. RLS gates the team-row write by
//     team admin (team_slack_channels_insert/_delete — there is no
//     app-pool UPDATE grant/policy; ReplaceForTeam is insert-then-prune
//     only, see its doc).
//   - admin: TracksChannelSystem, PrimaryTeamForChannelSystem,
//     ListTrackersForOrgSystem, ReconcilePrimariesSystem, SetPrimarySystem —
//     claims-free cross-team reads/writes the routing hooks (a sibling
//     leaf) and the primary-lifecycle operations need.
type teamChannelStore struct {
	app   db.Execer
	admin db.Execer
}

func newTeamChannelStore(app, admin db.Execer) slackstore.TeamChannelStore {
	return &teamChannelStore{app: app, admin: admin}
}

var _ slackstore.TeamChannelStore = (*teamChannelStore)(nil)

const teamChannelColumns = `org_id::text, team_id::text, channel_id, is_primary, created_at`

func scanTeamChannel(sc rowScanner) (slackstore.TeamChannel, error) {
	var tc slackstore.TeamChannel
	if err := sc.Scan(&tc.OrgID, &tc.TeamID, &tc.ChannelID, &tc.IsPrimary, &tc.CreatedAt); err != nil {
		return slackstore.TeamChannel{}, err
	}
	return tc, nil
}

func (s *teamChannelStore) ListForTeam(ctx context.Context, orgID, teamID string) ([]slackstore.TeamChannel, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+teamChannelColumns+`
		FROM team_slack_channels
		WHERE org_id = $1 AND team_id = $2
		ORDER BY channel_id ASC
	`, orgID, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team_slack_channels: %w", err)
	}
	defer rows.Close()

	out := []slackstore.TeamChannel{}
	for rows.Next() {
		tc, err := scanTeamChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan team_slack_channels: %w", err)
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// ReplaceForTeam is insert-then-prune, mirroring
// sqlite/team_github_repos.go's ReplaceForTeam shape: ON CONFLICT DO
// NOTHING preserves an existing row's created_at AND is_primary untouched;
// a wholly new channel_id always inserts is_primary=false, never true — the
// store-layer half of the "is_primary is never written from the app pool"
// contract (see the TeamChannelStore doc). Runs in one transaction so the
// insert pass and the prune can't leave a partial mid-save state.
func (s *teamChannelStore) ReplaceForTeam(ctx context.Context, orgID, teamID string, channelIDs []string) error {
	unique := normalizeChannelIDs(channelIDs)
	return inTx(ctx, s.app, func(q db.Execer) error {
		for _, ch := range unique {
			if _, err := q.ExecContext(ctx, `
				INSERT INTO team_slack_channels (org_id, team_id, channel_id, is_primary)
				VALUES ($1, $2, $3, false)
				ON CONFLICT (team_id, channel_id) DO NOTHING
			`, orgID, teamID, ch); err != nil {
				return fmt.Errorf("insert team_slack_channels[%s]: %w", ch, err)
			}
		}

		if len(unique) == 0 {
			if _, err := q.ExecContext(ctx, `
				DELETE FROM team_slack_channels WHERE org_id = $1 AND team_id = $2
			`, orgID, teamID); err != nil {
				return fmt.Errorf("clear team_slack_channels: %w", err)
			}
			return nil
		}

		if _, err := q.ExecContext(ctx, `
			DELETE FROM team_slack_channels
			WHERE org_id = $1 AND team_id = $2 AND NOT (channel_id = ANY($3))
		`, orgID, teamID, unique); err != nil {
			return fmt.Errorf("prune team_slack_channels: %w", err)
		}
		return nil
	})
}

// normalizeChannelIDs drops blanks and de-duplicates, preserving first-seen
// order (channel IDs need no casing normalization — unlike GitHub owner/repo,
// Slack's are case-sensitive as issued).
func normalizeChannelIDs(channelIDs []string) []string {
	seen := make(map[string]bool, len(channelIDs))
	out := make([]string, 0, len(channelIDs))
	for _, ch := range channelIDs {
		if ch == "" || seen[ch] {
			continue
		}
		seen[ch] = true
		out = append(out, ch)
	}
	return out
}

func (s *teamChannelStore) TracksChannelSystem(ctx context.Context, orgID, teamID, channelID string) (bool, error) {
	var ok bool
	if err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM team_slack_channels WHERE org_id = $1 AND team_id = $2 AND channel_id = $3
		)
	`, orgID, teamID, channelID).Scan(&ok); err != nil {
		return false, fmt.Errorf("tracks channel team_slack_channels: %w", err)
	}
	return ok, nil
}

func (s *teamChannelStore) PrimaryTeamForChannelSystem(ctx context.Context, orgID, channelID string) (string, error) {
	var teamID string
	err := s.admin.QueryRowContext(ctx, `
		SELECT team_id::text FROM team_slack_channels
		WHERE org_id = $1 AND channel_id = $2 AND is_primary
	`, orgID, channelID).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("primary team for channel team_slack_channels: %w", err)
	}
	return teamID, nil
}

func (s *teamChannelStore) ListTrackersForOrgSystem(ctx context.Context, orgID string) (map[string][]slackstore.TeamChannel, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+teamChannelColumns+`
		FROM team_slack_channels
		WHERE org_id = $1
		ORDER BY channel_id ASC, created_at ASC, team_id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list team_slack_channels trackers: %w", err)
	}
	defer rows.Close()

	out := map[string][]slackstore.TeamChannel{}
	for rows.Next() {
		tc, err := scanTeamChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan team_slack_channels trackers: %w", err)
		}
		out[tc.ChannelID] = append(out[tc.ChannelID], tc)
	}
	return out, rows.Err()
}

// ReconcilePrimariesSystem is a single atomic UPDATE ... FROM: the inner
// DISTINCT ON picks, per channel_id, the oldest tracker (created_at ASC,
// team_id ASC tiebreak) among channels that have at least one tracker but
// no row with is_primary yet — a channel that already has a primary never
// appears in that set (the NOT EXISTS), so promoting the picked row can
// never collide with the one-primary-per-channel partial unique index.
// One statement is enough for atomicity here; no inTx wrapper needed.
func (s *teamChannelStore) ReconcilePrimariesSystem(ctx context.Context, orgID string, channelIDs []string) error {
	if len(channelIDs) == 0 {
		return nil
	}
	if _, err := s.admin.ExecContext(ctx, `
		UPDATE team_slack_channels tsc
		SET is_primary = true
		FROM (
			SELECT DISTINCT ON (t.channel_id) t.team_id, t.channel_id
			FROM team_slack_channels t
			WHERE t.org_id = $1
			  AND t.channel_id = ANY($2)
			  AND NOT EXISTS (
			      SELECT 1 FROM team_slack_channels p
			      WHERE p.org_id = t.org_id AND p.channel_id = t.channel_id AND p.is_primary
			  )
			ORDER BY t.channel_id, t.created_at ASC, t.team_id ASC
		) promoted
		WHERE tsc.org_id = $1 AND tsc.channel_id = promoted.channel_id AND tsc.team_id = promoted.team_id
	`, orgID, channelIDs); err != nil {
		return fmt.Errorf("reconcile team_slack_channels primaries: %w", err)
	}
	return nil
}

// SetPrimarySystem runs check-then-clear-then-set in one transaction: the
// existence check happens inside the same tx as the writes (not a
// pre-check race) so a concurrent SetPrimarySystem for the same channel
// serializes normally on Postgres's row locking, and clearing the old
// primary before setting the new one never trips the partial unique index
// (at most one row is_primary=true at any statement boundary).
//
// The final UPDATE's RowsAffected is checked deliberately: under READ
// COMMITTED, each statement in this tx sees the latest committed data at
// the moment IT runs, not a snapshot from the transaction's start. If
// teamID's tracking row is concurrently deleted (e.g. a racing
// ReplaceForTeam untracks the channel) after the EXISTS check above but
// before this UPDATE runs, the UPDATE matches zero rows — without this
// check the call would return nil having already cleared the old primary,
// silently leaving the channel with NO primary at all. Treating that as
// ErrTeamNotTrackingChannel instead rolls back the whole transaction
// (inTx's deferred Rollback fires because this returns non-nil before
// reaching Commit), which undoes the clear too and restores the prior
// primary.
func (s *teamChannelStore) SetPrimarySystem(ctx context.Context, orgID, channelID, teamID string) error {
	return inTx(ctx, s.admin, func(q db.Execer) error {
		var tracks bool
		if err := q.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM team_slack_channels WHERE org_id = $1 AND channel_id = $2 AND team_id = $3
			)
		`, orgID, channelID, teamID).Scan(&tracks); err != nil {
			return fmt.Errorf("check team tracks channel: %w", err)
		}
		if !tracks {
			return slackstore.ErrTeamNotTrackingChannel
		}

		if _, err := q.ExecContext(ctx, `
			UPDATE team_slack_channels SET is_primary = false
			WHERE org_id = $1 AND channel_id = $2 AND is_primary
		`, orgID, channelID); err != nil {
			return fmt.Errorf("clear current primary team_slack_channels: %w", err)
		}
		res, err := q.ExecContext(ctx, `
			UPDATE team_slack_channels SET is_primary = true
			WHERE org_id = $1 AND channel_id = $2 AND team_id = $3
		`, orgID, channelID, teamID)
		if err != nil {
			return fmt.Errorf("set new primary team_slack_channels: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected set new primary team_slack_channels: %w", err)
		}
		if n == 0 {
			return slackstore.ErrTeamNotTrackingChannel
		}
		return nil
	})
}
