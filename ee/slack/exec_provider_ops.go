package slack

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// This file is the Slack provider's POLICY half: the org-bound, NON-SECRET ops
// the orchestrator serves against its stores. The sidecar-half handler
// (exec_host.go) relays to these instead of holding db.Stores itself — every
// answer here is an authorization decision or a workspace IDENTITY, never a bot
// token (the token rides the sealed bundle; see exec_provider.go). Identity is
// bound orchestrator-side from the RunInfo the ProviderOp receives, so a
// sidecar cannot steer these at another org's channels.

// registerSlackProviderOps registers the Slack policy ops. Every op is a pure
// function over the (stores, info) the dispatch passes it, so this needs no
// stores at registration and runs from init() — which is what makes the ops
// present in BOTH the server process (where they execute) and, harmlessly, the
// sidecar process (where they are only ever relayed away, never run).
func registerSlackProviderOps() {
	agenthost.RegisterProviderOp("slack", opAuthorizeChannel, slackOpAuthorizeChannel)
	agenthost.RegisterProviderOp("slack", opResolveWorkspace, slackOpResolveWorkspace)
	agenthost.RegisterProviderOp("slack", opResolveWorkspaceForDownload, slackOpResolveWorkspaceForDownload)
	agenthost.RegisterProviderOp("slack", opAuthorizeFileChannels, slackOpAuthorizeFileChannels)
}

// Provider op names — the "slack" namespace's policy ops.
const (
	opAuthorizeChannel            = "authorize_channel"
	opResolveWorkspace            = "resolve_workspace"
	opResolveWorkspaceForDownload = "resolve_workspace_download"
	opAuthorizeFileChannels       = "authorize_file_channels"
)

// Wire payloads for the policy ops. A workspace IDENTITY (workspace_id,
// api_app_id) is the SELECTION key the handler picks a sealed bot token by — it
// is not itself secret.
type (
	slackChannelArg struct {
		Channel string `json:"channel"`
	}
	slackFileChannelsArg struct {
		Channels []string `json:"channels"`
	}
	slackAuthorizedResult struct {
		Authorized bool `json:"authorized"`
	}
	slackWorkspaceIdentity struct {
		WorkspaceID string `json:"workspace_id"`
		APIAppID    string `json:"api_app_id"`
	}
)

// slackOpAuthorizeChannel is the stage-1 gate: does the run's team track the
// channel (mirrors exec workspace add's team-tracked-repo gate).
func slackOpAuthorizeChannel(ctx context.Context, stores db.Stores, info agenthost.RunInfo, args json.RawMessage) (any, error) {
	var a slackChannelArg
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		return nil, fmt.Errorf("slack: not available")
	}
	tracks, err := bundle.TeamChannels.TracksChannelSystem(ctx, info.OrgID, info.TeamID, a.Channel)
	if err != nil {
		return nil, fmt.Errorf("slack: check channel authorization: %w", err)
	}
	if !tracks {
		return nil, fmt.Errorf("slack: this team does not track channel %s", a.Channel)
	}
	return slackAuthorizedResult{Authorized: true}, nil
}

// slackOpResolveWorkspace resolves which (workspace, app) IDENTITY to act as for
// a channel — the non-secret half of the old resolveWorkspaceAndToken. Channel
// registry unknown → error. If this run's task is a slack:message naming this
// SAME channel, that message's (workspace_id, api_app_id) is authoritative;
// otherwise every connected workspace matching the channel's WorkspaceID is
// listed: exactly one → use it; more than one → refuse rather than guess.
func slackOpResolveWorkspace(ctx context.Context, stores db.Stores, info agenthost.RunInfo, args json.RawMessage) (any, error) {
	var a slackChannelArg
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		return nil, fmt.Errorf("slack: not available")
	}
	channel, err := bundle.Channels.GetSystem(ctx, info.OrgID, a.Channel)
	if err != nil {
		return nil, fmt.Errorf("slack: look up channel %s: %w", a.Channel, err)
	}
	if channel == nil {
		return nil, fmt.Errorf("slack: channel %s is not visible to Triage Factory", a.Channel)
	}

	if ws, metaChannel, ok, err := workspaceFromRunTaskMetadata(ctx, stores, info); err != nil {
		return nil, err
	} else if ok && metaChannel == a.Channel {
		return slackWorkspaceIdentity{WorkspaceID: ws.WorkspaceID, APIAppID: ws.APIAppID}, nil
	}

	workspaces, err := orgWorkspaces(ctx, stores, info.OrgID)
	if err != nil {
		return nil, err
	}
	var matches []slackstore.Workspace
	for _, ws := range workspaces {
		if ws.WorkspaceID == channel.WorkspaceID {
			matches = append(matches, ws)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("slack: no connected app for workspace %s (channel %s)", channel.WorkspaceID, a.Channel)
	case 1:
		return slackWorkspaceIdentity{WorkspaceID: matches[0].WorkspaceID, APIAppID: matches[0].APIAppID}, nil
	default:
		return nil, fmt.Errorf(
			"slack: workspace %s has %d connected apps in this org; cannot determine which bot identity to act as for channel %s",
			channel.WorkspaceID, len(matches), a.Channel)
	}
}

// slackOpResolveWorkspaceForDownload resolves which (workspace, app) IDENTITY to
// speak as for `download`, which carries no channel: prefer this run's own
// message-task metadata, else the org's sole connected workspace (refuse on
// zero or ambiguous). This picks an identity ONLY; the file's real channel
// membership is authorized separately (slackOpAuthorizeFileChannels).
func slackOpResolveWorkspaceForDownload(ctx context.Context, stores db.Stores, info agenthost.RunInfo, _ json.RawMessage) (any, error) {
	if ws, _, ok, err := workspaceFromRunTaskMetadata(ctx, stores, info); err != nil {
		return nil, err
	} else if ok {
		return slackWorkspaceIdentity{WorkspaceID: ws.WorkspaceID, APIAppID: ws.APIAppID}, nil
	}
	workspaces, err := orgWorkspaces(ctx, stores, info.OrgID)
	if err != nil {
		return nil, err
	}
	if len(workspaces) != 1 {
		return nil, fmt.Errorf(
			"slack: cannot determine which connected workspace to download from (this run has no Slack thread context and the org has %d connected workspaces)",
			len(workspaces))
	}
	return slackWorkspaceIdentity{WorkspaceID: workspaces[0].WorkspaceID, APIAppID: workspaces[0].APIAppID}, nil
}

// slackOpAuthorizeFileChannels is download's authorization gate, run after
// files.info resolves the file's real channel membership. Refuses when the file
// is shared into no channel this team tracks; passes as soon as ANY one is.
func slackOpAuthorizeFileChannels(ctx context.Context, stores db.Stores, info agenthost.RunInfo, args json.RawMessage) (any, error) {
	var a slackFileChannelsArg
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if len(a.Channels) == 0 {
		return nil, fmt.Errorf("slack: this file isn't shared into any channel this team could be authorized against")
	}
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		return nil, fmt.Errorf("slack: not available")
	}
	for _, channelID := range a.Channels {
		tracks, err := bundle.TeamChannels.TracksChannelSystem(ctx, info.OrgID, info.TeamID, channelID)
		if err != nil {
			return nil, fmt.Errorf("slack: check channel authorization: %w", err)
		}
		if tracks {
			return slackAuthorizedResult{Authorized: true}, nil
		}
	}
	return nil, fmt.Errorf("slack: this team does not track any channel this file is shared into")
}

// workspaceFromRunTaskMetadata resolves the run's own Slack context — its task,
// if a slack:message task, and that message's event metadata — via
// AgentRuns.GetSystem → Task → PrimaryEventID → Events.GetMetadataSystem. Every
// store call propagates its error; only a genuine "not found" maps to (ok=false,
// nil), so a masked failure never silently falls through to the org-wide
// fallback and replies as the wrong bot identity.
func workspaceFromRunTaskMetadata(ctx context.Context, stores db.Stores, info agenthost.RunInfo) (ws slackstore.Workspace, channel string, ok bool, err error) {
	run, err := stores.AgentRuns.GetSystem(ctx, info.OrgID, info.RunID)
	if err != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: load run: %w", err)
	}
	if run == nil || run.TaskID == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	task, err := stores.Tasks.GetSystem(ctx, info.OrgID, run.TaskID)
	if err != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: load task: %w", err)
	}
	if task == nil || task.EventType != domain.EventSlackMessage || task.PrimaryEventID == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	metaJSON, err := stores.Events.GetMetadataSystem(ctx, info.OrgID, task.PrimaryEventID)
	if err != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: load event metadata: %w", err)
	}
	if metaJSON == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	var meta SlackMessageMetadata
	if jsonErr := json.Unmarshal([]byte(metaJSON), &meta); jsonErr != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: parse event metadata: %w", jsonErr)
	}
	if meta.WorkspaceID == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: not available")
	}
	row, err := bundle.Workspaces.GetByWorkspaceAppSystem(ctx, meta.WorkspaceID, meta.APIAppID)
	if err != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: resolve workspace from event metadata: %w", err)
	}
	if row == nil {
		return slackstore.Workspace{}, "", false, nil
	}
	return *row, meta.Channel, true, nil
}

// orgWorkspaces lists orgID's connected Slack workspaces via the admin-pool
// ListAllSystem (exec ops run with no request-JWT claims), filtered to orgID.
func orgWorkspaces(ctx context.Context, stores db.Stores, orgID string) ([]slackstore.Workspace, error) {
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		return nil, fmt.Errorf("slack: not available")
	}
	all, err := bundle.Workspaces.ListAllSystem(ctx)
	if err != nil {
		return nil, fmt.Errorf("slack: list workspaces: %w", err)
	}
	var out []slackstore.Workspace
	for _, ws := range all {
		if ws.OrgID == orgID {
			out = append(out, ws)
		}
	}
	return out, nil
}
