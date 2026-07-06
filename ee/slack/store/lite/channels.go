package lite

import (
	"context"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// channelRegistryStore is the SQLite stub of slackstore.ChannelRegistryStore.
// The Slack channel registry is a multi-mode concept — it only matters once
// more than one team/org can see the same channel; local mode never runs
// the ingest pipeline that would call this, so every method returns
// db.ErrNotApplicableInLocal. Mirrors workspaceStore.
type channelRegistryStore struct{ q db.Execer }

func newChannelRegistryStore(q, _ db.Execer) slackstore.ChannelRegistryStore {
	return &channelRegistryStore{q: q}
}

var _ slackstore.ChannelRegistryStore = (*channelRegistryStore)(nil)

func (s *channelRegistryStore) UpsertSightingSystem(context.Context, string, string, string, time.Time) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}

func (s *channelRegistryStore) EnsureSystem(context.Context, string, string, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *channelRegistryStore) SetNameSystem(context.Context, string, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *channelRegistryStore) GetSystem(context.Context, string, string) (*slackstore.Channel, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *channelRegistryStore) ListForOrg(context.Context, string) ([]slackstore.Channel, error) {
	return nil, db.ErrNotApplicableInLocal
}
