package lite

import (
	"context"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// identityStore is the SQLite stub of slackstore.SlackIdentityStore. Slack
// identity capture is a multi-mode concept (it resolves against
// public.user_identities, which doesn't exist in local mode's SQLite
// schema): local mode never runs the ingest pipeline that would call this,
// so every method returns db.ErrNotApplicableInLocal. Mirrors workspaceStore.
type identityStore struct{ q db.Execer }

func newIdentityStore(q db.Execer) slackstore.SlackIdentityStore {
	return &identityStore{q: q}
}

var _ slackstore.SlackIdentityStore = (*identityStore)(nil)

func (s *identityStore) LookupSystem(context.Context, string, string) (*slackstore.SlackIdentity, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *identityStore) UpsertResolvedSystem(context.Context, string, string, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *identityStore) MarkAttemptSystem(context.Context, string, string, string) error {
	return db.ErrNotApplicableInLocal
}
