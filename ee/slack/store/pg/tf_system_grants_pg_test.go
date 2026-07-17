package pg_test

import (
	"context"
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTfSystem_SlackProviderReads pins that the executor's tf_system pool can
// serve the Slack provider policy ops (ee/slack/exec_provider_ops.go) — the
// orchestrator half the sidecar relays `exec slack` calls to. The core
// pgtest.TestTfSystem_ExecutorSurfaceConformance can't reach these (it doesn't
// import EE), so a missing tf_system grant on a Slack table only surfaces here.
// Each read below fails SQLSTATE 42501 if its grant is absent — add it to the
// tf_system section of the pg baseline.
//
// Seeding runs through a superuser-backed store so the fixtures never depend on
// the grants under test; the reads then run through a store whose admin half is
// tf_system, exactly as a TF_ROLE=executor process wires in internal/app.
func TestTfSystem_SlackProviderReads(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "slack-tfsystem")

	const (
		workspaceID = "T0TFSYS01"
		channelID   = "C0TFSYS01"
	)
	apiAppID := defaultAppID(workspaceID)

	seed := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	mustWithUser(t, seed, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).Workspaces.Upsert(ctx, testWorkspace(orgID, workspaceID))
	})
	if err := slackstore.FromStores(seed).Channels.EnsureSystem(ctx, orgID, workspaceID, channelID, "general"); err != nil {
		t.Fatalf("seed slack_channels: %v", err)
	}
	mustWithUser(t, seed, ctx, orgID, userID, func(tx db.TxStores) error {
		return slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(ctx, orgID, teamID, []string{channelID})
	})

	// The executor's read path: admin half is tf_system.
	sys := slackstore.FromStores(pgstore.New(h.SystemDB, h.AppDB, pgtest.SecretKey))

	if _, err := sys.TeamChannels.TracksChannelSystem(ctx, orgID, teamID, channelID); err != nil {
		t.Errorf("TeamChannels.TracksChannelSystem (team_slack_channels grant): %v", err)
	}
	if _, err := sys.Channels.GetSystem(ctx, orgID, channelID); err != nil {
		t.Errorf("Channels.GetSystem (slack_channels grant): %v", err)
	}
	if _, err := sys.Workspaces.GetByWorkspaceAppSystem(ctx, workspaceID, apiAppID); err != nil {
		t.Errorf("Workspaces.GetByWorkspaceAppSystem (org_slack_workspaces grant): %v", err)
	}
	if _, err := sys.Workspaces.ListAllSystem(ctx); err != nil {
		t.Errorf("Workspaces.ListAllSystem (org_slack_workspaces grant): %v", err)
	}
}
