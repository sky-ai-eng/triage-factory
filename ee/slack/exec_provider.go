package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// This file is the Slack provider's credential half: the brain-side resolver
// that seals the conversation's authorizable Slack bot tokens into the per-claim bundle,
// and the keyed-set shape the sidecar-side handler selects from. It is
// provider-owned (the shape crosses core only as opaque JSON), so core never
// sees a Slack credential symbol.
//
// The unifying insight (same as GitHub's per-repo token set): a run's Slack
// credential is not one token but a SET keyed by (workspace, app), resolved at
// provision time to exactly the workspaces of the team's tracked channels —
// which IS the authorizable set authorizeChannel already enforces, so sealing
// those and no others loses nothing. The secret rides in the sealed bundle;
// the runtime SELECTION (channel -> workspace) is non-secret policy the
// handler relays for, and the orchestrator answers with a workspace identity,
// never a token.

// slackBundleCreds is the Slack provider's sealed keyed set: one bot token per
// (workspaceID, apiAppID) the run may act as. Marshaled opaque into
// bundle.Providers["slack"].
type slackBundleCreds struct {
	Workspaces []slackWorkspaceToken `json:"workspaces,omitempty"`
}

// slackWorkspaceToken is one member of the keyed set — the bot token for a
// (workspace, app) pair, selected in-sidecar by the same (workspaceID,
// apiAppID) the resolve_workspace relay returns for a channel.
type slackWorkspaceToken struct {
	WorkspaceID string `json:"workspace_id"`
	APIAppID    string `json:"api_app_id"`
	BotToken    string `json:"bot_token"`
}

// tokenFor selects the bot token for (workspaceID, apiAppID) from the sealed
// set, in-sidecar, never asking the orchestrator for a secret. ok=false when
// the pair is outside the sealed (== authorizable) set — the handler surfaces
// that as an authorization failure, symmetric with a channel the team doesn't
// track.
func (c slackBundleCreds) tokenFor(workspaceID, apiAppID string) (string, bool) {
	for _, w := range c.Workspaces {
		if w.WorkspaceID == workspaceID && w.APIAppID == apiAppID {
			return w.BotToken, w.BotToken != ""
		}
	}
	return "", false
}

// slackProviderCredential is the brain-side resolver registered under the
// "slack" namespace: it seals a bot token for every (workspace, app) that owns
// a channel the conversation's team tracks. Returns (nil, nil) when the team tracks no
// Slack channels or the org has none connected (a GitHub/Jira-only run's
// bundle then carries no Slack section). Runs only on the brain, against the
// secret-bearing stores — the sidecar never reaches a secret store.
func slackProviderCredential(ctx context.Context, stores db.Stores, scope agenthost.ProvisionScope) (json.RawMessage, error) {
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		// EE not linked, or the Slack store isn't registered (local mode) —
		// nothing to seal.
		return nil, nil
	}

	trackers, err := bundle.TeamChannels.ListTrackersForOrgSystem(ctx, scope.OrgID)
	if err != nil {
		if errors.Is(err, db.ErrNotApplicableInLocal) {
			return nil, nil
		}
		return nil, fmt.Errorf("list channel trackers: %w", err)
	}

	// The channels THIS team tracks — the authorizable set authorizeChannel
	// gates on, so their workspaces are exactly what may be sealed.
	trackedChannels := map[string]struct{}{}
	for channelID, tcs := range trackers {
		for _, tc := range tcs {
			if tc.TeamID == scope.TeamID {
				trackedChannels[channelID] = struct{}{}
				break
			}
		}
	}
	if len(trackedChannels) == 0 {
		return nil, nil
	}

	// The distinct workspaces those channels live in.
	workspaceIDs := map[string]struct{}{}
	for channelID := range trackedChannels {
		ch, err := bundle.Channels.GetSystem(ctx, scope.OrgID, channelID)
		if err != nil {
			return nil, fmt.Errorf("look up channel %s: %w", channelID, err)
		}
		if ch != nil && ch.WorkspaceID != "" {
			workspaceIDs[ch.WorkspaceID] = struct{}{}
		}
	}
	if len(workspaceIDs) == 0 {
		return nil, nil
	}

	all, err := bundle.Workspaces.ListAllSystem(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	var creds slackBundleCreds
	for _, ws := range all {
		if ws.OrgID != scope.OrgID {
			continue
		}
		if _, ok := workspaceIDs[ws.WorkspaceID]; !ok {
			continue
		}
		token, err := stores.Secrets.GetSystem(ctx, scope.OrgID, ws.BotTokenRef)
		if err != nil {
			return nil, fmt.Errorf("read bot token for workspace %s: %w", ws.WorkspaceID, err)
		}
		if token == "" {
			continue
		}
		creds.Workspaces = append(creds.Workspaces, slackWorkspaceToken{
			WorkspaceID: ws.WorkspaceID,
			APIAppID:    ws.APIAppID,
			BotToken:    token,
		})
	}
	if len(creds.Workspaces) == 0 {
		return nil, nil
	}
	// Deterministic order so a given input seals byte-stably.
	sort.Slice(creds.Workspaces, func(i, j int) bool {
		if creds.Workspaces[i].WorkspaceID != creds.Workspaces[j].WorkspaceID {
			return creds.Workspaces[i].WorkspaceID < creds.Workspaces[j].WorkspaceID
		}
		return creds.Workspaces[i].APIAppID < creds.Workspaces[j].APIAppID
	})
	return json.Marshal(creds)
}
