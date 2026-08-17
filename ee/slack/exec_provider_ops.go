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
// answer here is an authorization decision, a workspace IDENTITY (never a bot
// token — that rides the sealed bundle; see exec_provider.go), or — for
// opRecordThreadRoot — the entity bookkeeping only the orchestrator's stores
// can do. Identity is bound orchestrator-side from the ConversationInfo the ProviderOp
// receives, so a sidecar cannot steer these at another org's channels.

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
	agenthost.RegisterProviderOp("slack", opRecordThreadRoot, slackOpRecordThreadRoot)
}

// Provider op names — the "slack" namespace's policy ops.
const (
	opAuthorizeChannel            = "authorize_channel"
	opResolveWorkspace            = "resolve_workspace"
	opResolveWorkspaceForDownload = "resolve_workspace_download"
	opAuthorizeFileChannels       = "authorize_file_channels"
	opRecordThreadRoot            = "record_thread_root"
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
	// slackThreadRootArg is opRecordThreadRoot's wire shape: the workspace
	// identity exec_host.go already resolved for the send, plus the
	// channel-root message's own coordinates and text.
	slackThreadRootArg struct {
		WorkspaceID string `json:"workspace_id"`
		APIAppID    string `json:"api_app_id"`
		Channel     string `json:"channel"`
		TS          string `json:"ts"`
		Text        string `json:"text"`
	}
)

// slackOpAuthorizeChannel is the stage-1 gate: does the run's team track the
// channel (mirrors exec workspace add's team-tracked-repo gate).
func slackOpAuthorizeChannel(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo, args json.RawMessage) (any, error) {
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
func slackOpResolveWorkspace(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo, args json.RawMessage) (any, error) {
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

	if ws, metaChannel, ok, err := workspaceFromConversationTaskMetadata(ctx, stores, info); err != nil {
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
func slackOpResolveWorkspaceForDownload(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo, _ json.RawMessage) (any, error) {
	if ws, _, ok, err := workspaceFromConversationTaskMetadata(ctx, stores, info); err != nil {
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
func slackOpAuthorizeFileChannels(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo, args json.RawMessage) (any, error) {
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

// slackOpRecordThreadRoot is exec_host.go send()'s companion for a
// channel-root post (no --thread-ts): the run's own message IS the thread's
// root, so the thread is engaged the same way a root mention engages one at
// ingest (ingest.go's handleEventCallback) — idempotent FindOrCreateSystem
// with kind="thread", titled from the posted text. FindOrCreate never
// rewrites kind on an already-known row, so exec_host.go calls this — and
// awaits it — before the generic touched-entity resolution
// (cmd/exec/agenthost/record.go) that recordMessage triggers next can find/
// create the same (channel, ts) key first and default it to kind="message".
// Only on first creation, dispatches the same best-effort permalink
// resolution the ingest pipeline fires for a root mention.
func slackOpRecordThreadRoot(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo, args json.RawMessage) (any, error) {
	var a slackThreadRootArg
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if a.Channel == "" || a.TS == "" {
		return nil, fmt.Errorf("slack: record_thread_root requires channel and ts")
	}
	sourceID := domain.SlackSourceID(a.Channel, a.TS)
	entity, created, err := stores.Entities.FindOrCreateSystem(ctx, info.OrgID, "slack", sourceID, "thread", mentionTitle(a.Text), "")
	if err != nil {
		return nil, fmt.Errorf("slack: record thread root entity: %w", err)
	}
	if !created {
		return nil, nil
	}
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		return nil, nil
	}
	ws, err := bundle.Workspaces.GetByWorkspaceAppSystem(ctx, a.WorkspaceID, a.APIAppID)
	if err != nil {
		slackLog.Warn("record thread root: resolve workspace for permalink failed", "workspace", a.WorkspaceID, "entity", entity.ID, "error", err)
		return nil, nil
	}
	if ws == nil {
		return nil, nil
	}
	NewPermalinkResolver(stores).dispatch(*ws, entity.ID, a.Channel, a.TS)
	return nil, nil
}

// workspaceFromConversationTaskMetadata resolves the conversation's own Slack context — its task,
// if a slack:message task, and that message's event metadata — via
// Conversations.GetSystem → Task → PrimaryEventID → Events.GetMetadataSystem. Every
// store call propagates its error; only a genuine "not found" maps to (ok=false,
// nil), so a masked failure never silently falls through to the org-wide
// fallback and replies as the wrong bot identity.
func workspaceFromConversationTaskMetadata(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo) (ws slackstore.Workspace, channel string, ok bool, err error) {
	conv, err := stores.Conversations.GetSystem(ctx, info.OrgID, info.ConversationID)
	if err != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: load conversation: %w", err)
	}
	if conv == nil || conv.TaskID == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	task, err := stores.Tasks.GetSystem(ctx, info.OrgID, conv.TaskID)
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
