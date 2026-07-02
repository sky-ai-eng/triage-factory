// Package lite holds the SQLite stub implementation of the Enterprise
// Edition Slack workspace store and registers it for the "sqlite" dialect.
// Slack workspace connect is a multi-mode concept: local mode is N=1 and
// never registers a workspace, so every method returns
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

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func init() {
	db.RegisterStoreExtension("sqlite", slackstore.ExtKey, func(app, admin db.Execer) any {
		return &slackstore.Bundle{
			Workspaces: newWorkspaceStore(app, admin),
			Identities: newIdentityStore(admin),
			Deliveries: newDeliveryStore(),
		}
	})
}

type workspaceStore struct{ q db.Execer }

func newWorkspaceStore(q, _ db.Execer) slackstore.WorkspaceStore {
	return &workspaceStore{q: q}
}

var _ slackstore.WorkspaceStore = (*workspaceStore)(nil)

func (s *workspaceStore) Upsert(context.Context, slackstore.Workspace) error {
	return db.ErrNotApplicableInLocal
}

func (s *workspaceStore) ListForOrg(context.Context, string) ([]slackstore.Workspace, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *workspaceStore) Get(context.Context, string, string) (*slackstore.Workspace, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *workspaceStore) Delete(context.Context, string, string) error {
	return db.ErrNotApplicableInLocal
}

func (s *workspaceStore) ListAllSystem(context.Context) ([]slackstore.Workspace, error) {
	return nil, db.ErrNotApplicableInLocal
}

func (s *workspaceStore) GetByWorkspaceIDSystem(context.Context, string) (*slackstore.Workspace, error) {
	return nil, db.ErrNotApplicableInLocal
}

type deliveryStore struct{}

func newDeliveryStore() slackstore.DeliveryStore { return &deliveryStore{} }

var _ slackstore.DeliveryStore = (*deliveryStore)(nil)

func (s *deliveryStore) MarkDeliveredSystem(context.Context, string, string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}
