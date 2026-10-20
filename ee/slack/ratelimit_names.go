package slack

import (
	"context"
	"errors"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/ratelimits"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// init names Slack's rate-limit credentials for the readings route, the same
// registration shape as the event-source probe beside it: core holds no Slack
// symbol, and the names live in the workspace store here.
func init() {
	ratelimits.RegisterNamer(upstream.Slack, rateLimitNames)
}

// rateLimitNames names each bot token's credential (slackBotCaller) by the
// workspace it is installed in. An app-level token (slackAppCaller) belongs to
// the app rather than to any one workspace, so it is left unnamed rather than
// given the name of a workspace it is not in.
func rateLimitNames(ctx context.Context, tx db.TxStores, orgID string) (map[string]string, error) {
	bundle := slackstore.FromTx(tx)
	if bundle == nil {
		return nil, errors.New("slack store bundle absent from the transaction: the slack store extension is not registered")
	}
	workspaces, err := bundle.Workspaces.ListForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(workspaces))
	for _, ws := range workspaces {
		if ws.WorkspaceName != "" {
			out[slackBotCaller(orgID, ws.WorkspaceID, ws.APIAppID).credential] = ws.WorkspaceName
		}
	}
	return out, nil
}
