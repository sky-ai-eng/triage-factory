// Package lite holds the SQLite stub implementation of the Enterprise
// Edition Slack stores and registers it for the "sqlite" dialect.
// Connecting Slack is a multi-mode concept: local mode is N=1 and
// never registers a connection, so every method returns
// db.ErrNotApplicableInLocal. The stub exists only so the Slack bundle is
// fully wired in both modes — no local caller ever reaches it (the Slack
// routes gate on the `slack` entitlement, which a community/unlicensed
// local build never carries). Mirrors ee/sso/store/lite.
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
package lite

import (
	"context"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func init() {
	db.RegisterStoreExtension("sqlite", slackstore.ExtKey, func(app, admin db.Execer) any {
		return &slackstore.Bundle{
			Connections:  newConnectionStore(app, admin),
			Identities:   newIdentityStore(admin),
			Deliveries:   newDeliveryStore(),
			Channels:     newChannelRegistryStore(app, admin),
			TeamChannels: newTeamChannelStore(app, admin),
		}
	})
}

type connectionStore struct{ q db.Execer }

func newConnectionStore(q, _ db.Execer) slackstore.ConnectionStore {
	return &connectionStore{q: q}
}

var _ slackstore.ConnectionStore = (*connectionStore)(nil)

func (s *connectionStore) Upsert(context.Context, slackstore.Connection) (slackstore.Connection, error) {
	return slackstore.Connection{}, db.ErrNotApplicableInLocal
}

func (s *connectionStore) ListForOrg(context.Context, string) ([]slackstore.Connection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *connectionStore) Get(context.Context, string, string, string) (*slackstore.Connection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *connectionStore) Delete(context.Context, string, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *connectionStore) DeleteSystem(context.Context, string, string, string, time.Time) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}

func (s *connectionStore) ListAllSystem(context.Context) ([]slackstore.Connection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *connectionStore) ListForOrgSystem(context.Context, string) ([]slackstore.Connection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *connectionStore) ActiveForWorkspaceSystem(context.Context, string, string) (*slackstore.Connection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *connectionStore) GetByWorkspaceAppSystem(context.Context, string, string) (*slackstore.Connection, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *connectionStore) LockApp(context.Context, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *connectionStore) AppBoundToOtherOrgSystem(context.Context, string, string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}

type deliveryStore struct{}

func newDeliveryStore() slackstore.DeliveryStore { return &deliveryStore{} }

var _ slackstore.DeliveryStore = (*deliveryStore)(nil)

func (s *deliveryStore) MarkDeliveredSystem(context.Context, string, string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}

func (s *deliveryStore) PruneDeliveredSystem(context.Context) error {
	return db.ErrNotApplicableInLocal
}
