package lite

import (
	"context"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// teamChannelStore is the SQLite stub of slackstore.TeamChannelStore. Team
// channel tracking is a multi-mode concept (team<->channel binds only
// matter once more than one team can track Slack channels); local mode
// never runs the Slack routing this backs, so every method returns
// db.ErrNotApplicableInLocal. Mirrors workspaceStore.
type teamChannelStore struct{ q db.Execer }

func newTeamChannelStore(q, _ db.Execer) slackstore.TeamChannelStore {
	return &teamChannelStore{q: q}
}

var _ slackstore.TeamChannelStore = (*teamChannelStore)(nil)

func (s *teamChannelStore) ListForTeam(context.Context, string, string) ([]slackstore.TeamChannel, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *teamChannelStore) ReplaceForTeam(context.Context, string, string, []string) error {
	return db.ErrNotApplicableInLocal
}

func (s *teamChannelStore) TracksChannelSystem(context.Context, string, string, string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}

func (s *teamChannelStore) PrimaryTeamForChannelSystem(context.Context, string, string) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (s *teamChannelStore) ListTrackersForOrgSystem(context.Context, string) (map[string][]slackstore.TeamChannel, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *teamChannelStore) ReconcilePrimariesSystem(context.Context, string, []string) error {
	return db.ErrNotApplicableInLocal
}

func (s *teamChannelStore) SetPrimarySystem(context.Context, string, string, string) error {
	return db.ErrNotApplicableInLocal
}
