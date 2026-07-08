// Host-side handlers for the `triagefactory exec slack ...` verbs (TFAC-596):
// the entitlement-gated ExtensionHandler registered via
// agenthost.RegisterExtension. Registered from install() (NOT init()) so it
// can close over the server's db.Stores — the registry itself is a
// process-global map safe to write here because ee.Install() runs during
// server boot, before the agenthost daemon serves any run. The side effect
// is intended: a local-mode CLI process never reaches install(), so the
// "slack" namespace is never registered there — these verbs are structurally
// multi-only, same posture as every other Slack surface (workspaces are
// Postgres-only).
package slack

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// slackExecHandler dispatches every `exec slack <verb>` call, keyed by
// method name (the CLI's method string mirrors the verb: "send", "edit",
// "react", "read_thread", "read_channel", "download").
type slackExecHandler struct {
	stores db.Stores
	client *http.Client
}

// registerSlackExec registers the "slack" extension namespace against
// stores. Called once from install() — see the package doc above.
func registerSlackExec(stores db.Stores) {
	h := &slackExecHandler{stores: stores, client: slackHTTPClient}
	agenthost.RegisterExtension("slack", entitlements.FeatureSlack, h.handle)
}

func (h *slackExecHandler) handle(ctx context.Context, info agenthost.RunInfo, method string, args json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "send":
		return dispatchSlackExec(args, func(a slackSendArgs) (slackSendResult, error) { return h.send(ctx, info, a) })
	case "edit":
		return dispatchSlackExec(args, func(a slackEditArgs) (slackEditResult, error) { return h.edit(ctx, info, a) })
	case "react":
		return dispatchSlackExec(args, func(a slackReactArgs) (slackReactResult, error) { return h.react(ctx, info, a) })
	case "read_thread":
		return dispatchSlackExec(args, func(a slackReadThreadArgs) ([]slackMessageView, error) { return h.readThread(ctx, info, a) })
	case "read_channel":
		return dispatchSlackExec(args, func(a slackReadChannelArgs) ([]slackMessageView, error) { return h.readChannel(ctx, info, a) })
	case "download":
		return dispatchSlackExec(args, func(a slackDownloadArgs) (slackDownloadResult, error) { return h.download(ctx, info, a) })
	default:
		return nil, fmt.Errorf("slack: unknown method %q", method)
	}
}

// dispatchSlackExec unmarshals args into A, calls fn, and marshals the
// result — the generic decode/encode boilerplate every verb's handle case
// shares.
func dispatchSlackExec[A any, R any](args json.RawMessage, fn func(A) (R, error)) (json.RawMessage, error) {
	var a A
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("slack: invalid args: %w", err)
		}
	}
	r, err := fn(a)
	if err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// slackReactResult is react's (empty) success shape — react has nothing to
// report back beyond "it worked", but every verb marshals SOMETHING so the
// CLI's printJSON always has valid JSON to print.
type slackReactResult struct{}

// --- authorization ---

// authorizeChannel is the stage-1 gate every verb runs before touching
// Slack: the run's team must track channelID (mirrors exec workspace add's
// team-tracked-repo gate). A mention-triggered run's owning team is always a
// tracker of its own channel by construction, so this passes trivially for
// the common case; it's load-bearing for a GitHub/Jira-triggered run
// reaching into a channel its team doesn't track.
func (h *slackExecHandler) authorizeChannel(ctx context.Context, info agenthost.RunInfo, channelID string) error {
	bundle := slackstore.FromStores(h.stores)
	if bundle == nil {
		return fmt.Errorf("slack: not available")
	}
	tracks, err := bundle.TeamChannels.TracksChannelSystem(ctx, info.OrgID, info.TeamID, channelID)
	if err != nil {
		return fmt.Errorf("slack: check channel authorization: %w", err)
	}
	if !tracks {
		return fmt.Errorf("slack: this team does not track channel %s", channelID)
	}
	return nil
}

// --- workspace / token resolution ---

// resolveWorkspaceAndToken resolves the (workspace, bot token) pair to act
// as for channelID. Channel registry unknown → error. Otherwise: if this
// run's task is a slack:mention task whose event metadata names this SAME
// channel, that metadata's (workspace_id, api_app_id) is authoritative —
// it's literally which bot identity received the mention. Otherwise, every
// connected workspace matching the channel's registered WorkspaceID is
// listed: exactly one → use it; more than one (two apps installed into the
// same workspace) → refuse rather than guess which bot identity speaks.
func (h *slackExecHandler) resolveWorkspaceAndToken(ctx context.Context, info agenthost.RunInfo, channelID string) (slackstore.Workspace, string, error) {
	bundle := slackstore.FromStores(h.stores)
	if bundle == nil {
		return slackstore.Workspace{}, "", fmt.Errorf("slack: not available")
	}
	channel, err := bundle.Channels.GetSystem(ctx, info.OrgID, channelID)
	if err != nil {
		return slackstore.Workspace{}, "", fmt.Errorf("slack: look up channel %s: %w", channelID, err)
	}
	if channel == nil {
		return slackstore.Workspace{}, "", fmt.Errorf("slack: channel %s is not visible to Triage Factory", channelID)
	}

	if ws, metaChannel, ok, err := h.workspaceFromRunTaskMetadata(ctx, info); err != nil {
		return slackstore.Workspace{}, "", err
	} else if ok && metaChannel == channelID {
		token, err := h.botToken(ctx, info, ws)
		return ws, token, err
	}

	workspaces, err := h.orgWorkspaces(ctx, info.OrgID)
	if err != nil {
		return slackstore.Workspace{}, "", err
	}
	var matches []slackstore.Workspace
	for _, ws := range workspaces {
		if ws.WorkspaceID == channel.WorkspaceID {
			matches = append(matches, ws)
		}
	}
	switch len(matches) {
	case 0:
		return slackstore.Workspace{}, "", fmt.Errorf("slack: no connected app for workspace %s (channel %s)", channel.WorkspaceID, channelID)
	case 1:
		token, err := h.botToken(ctx, info, matches[0])
		return matches[0], token, err
	default:
		return slackstore.Workspace{}, "", fmt.Errorf(
			"slack: workspace %s has %d connected apps in this org; cannot determine which bot identity to act as for channel %s",
			channel.WorkspaceID, len(matches), channelID)
	}
}

// resolveWorkspaceForDownload resolves a bot token for `download`, which
// carries no --channel (a file id alone doesn't name one). It prefers this
// run's own mention-task metadata (the thread context the run was spawned
// from, if any); absent that, it falls back to the org's sole connected
// workspace, refusing when the org has none or more than one (no channel to
// disambiguate against, so no guessing). This resolves ONLY which bot
// identity to speak as — it does NOT authorize the download; a token here is
// just enough credential to look up the file's own metadata. The actual
// authorization gate is authorizeFileChannels, run against the file's real
// channel membership once files.info answers (see download) — this two-step
// split exists because, unlike every other verb, download has no --channel
// upfront to gate on before making any Slack call at all.
func (h *slackExecHandler) resolveWorkspaceForDownload(ctx context.Context, info agenthost.RunInfo) (string, error) {
	if ws, _, ok, err := h.workspaceFromRunTaskMetadata(ctx, info); err != nil {
		return "", err
	} else if ok {
		return h.botToken(ctx, info, ws)
	}

	workspaces, err := h.orgWorkspaces(ctx, info.OrgID)
	if err != nil {
		return "", err
	}
	if len(workspaces) != 1 {
		return "", fmt.Errorf(
			"slack: cannot determine which connected workspace to download from (this run has no Slack thread context and the org has %d connected workspaces)",
			len(workspaces))
	}
	return h.botToken(ctx, info, workspaces[0])
}

// authorizeFileChannels is download's authorization gate — run only after
// files.info resolves the file's real channel membership, since download
// carries no --channel upfront to check via authorizeChannel like every
// other verb. Refuses when channelIDs is empty (a file shared nowhere
// channel-scoped — e.g. only ever DM'd — has nothing to authorize against,
// so there's no basis to allow it) or when the team tracks none of them;
// passes as soon as ANY one of the file's channels is tracked, since a file
// shared into several channels only needs one legitimate path to it.
func (h *slackExecHandler) authorizeFileChannels(ctx context.Context, info agenthost.RunInfo, channelIDs []string) error {
	if len(channelIDs) == 0 {
		return fmt.Errorf("slack: this file isn't shared into any channel this team could be authorized against")
	}
	bundle := slackstore.FromStores(h.stores)
	if bundle == nil {
		return fmt.Errorf("slack: not available")
	}
	for _, channelID := range channelIDs {
		tracks, err := bundle.TeamChannels.TracksChannelSystem(ctx, info.OrgID, info.TeamID, channelID)
		if err != nil {
			return fmt.Errorf("slack: check channel authorization: %w", err)
		}
		if tracks {
			return nil
		}
	}
	return fmt.Errorf("slack: this team does not track any channel this file is shared into")
}

// workspaceFromRunTaskMetadata resolves the run's own Slack context — its
// task, if a slack:mention task, and that mention's event metadata — via the
// documented chain: AgentRuns.GetSystem -> Task -> PrimaryEventID ->
// Events.GetMetadataSystem. ok=false with a nil error covers every "this run
// genuinely has no Slack thread context" case (a GitHub/Jira-triggered run, a
// run with no task, a task that isn't slack:mention, no metadata recorded) —
// none of those are failures, they just mean the caller falls back to its
// own resolution path. A non-nil error means a real lookup failed (a
// transient store error) — the caller must NOT treat that the same as "no
// context": silently falling through to the org-wide fallback on a masked
// failure risks resolving a different (but not ambiguous, so not refused)
// connected app than the one that actually owns this run's thread, i.e.
// replying as the wrong bot identity. So every store call below propagates
// its error; only a genuine "not found" result maps to ok=false, nil.
func (h *slackExecHandler) workspaceFromRunTaskMetadata(ctx context.Context, info agenthost.RunInfo) (ws slackstore.Workspace, channel string, ok bool, err error) {
	run, err := h.stores.AgentRuns.GetSystem(ctx, info.OrgID, info.RunID)
	if err != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: load run: %w", err)
	}
	if run == nil || run.TaskID == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	task, err := h.stores.Tasks.GetSystem(ctx, info.OrgID, run.TaskID)
	if err != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: load task: %w", err)
	}
	if task == nil || task.EventType != domain.EventSlackMention || task.PrimaryEventID == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	metaJSON, err := h.stores.Events.GetMetadataSystem(ctx, info.OrgID, task.PrimaryEventID)
	if err != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: load event metadata: %w", err)
	}
	if metaJSON == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	var meta SlackMentionMetadata
	if jsonErr := json.Unmarshal([]byte(metaJSON), &meta); jsonErr != nil {
		return slackstore.Workspace{}, "", false, fmt.Errorf("slack: parse event metadata: %w", jsonErr)
	}
	if meta.WorkspaceID == "" {
		return slackstore.Workspace{}, "", false, nil
	}
	bundle := slackstore.FromStores(h.stores)
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

// botToken reads ws's bot token from the secret store.
func (h *slackExecHandler) botToken(ctx context.Context, info agenthost.RunInfo, ws slackstore.Workspace) (string, error) {
	token, err := h.stores.Secrets.GetSystem(ctx, info.OrgID, ws.BotTokenRef)
	if err != nil {
		return "", fmt.Errorf("slack: read bot token: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("slack: no bot token configured for workspace %s", ws.WorkspaceID)
	}
	return token, nil
}

// orgWorkspaces lists orgID's connected Slack workspaces. Exec verbs run with
// no request-JWT-claims context (an event-triggered run has none at all, and
// a manual run's synthetic claims aren't threaded through the extension
// seam), so this goes through the admin-pool ListAllSystem (every org) rather
// than the app-pool, RLS-gated ListForOrg the HTTP handlers use — filtered
// down to orgID here.
func (h *slackExecHandler) orgWorkspaces(ctx context.Context, orgID string) ([]slackstore.Workspace, error) {
	bundle := slackstore.FromStores(h.stores)
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

// --- send / edit / react ---

func (h *slackExecHandler) send(ctx context.Context, info agenthost.RunInfo, a slackSendArgs) (slackSendResult, error) {
	if a.Channel == "" {
		return slackSendResult{}, fmt.Errorf("slack: --channel is required")
	}
	if a.Body == "" && a.AttachBase64 == "" {
		return slackSendResult{}, fmt.Errorf("slack: send needs --body/--body-file or --attach-file")
	}
	if err := h.authorizeChannel(ctx, info, a.Channel); err != nil {
		return slackSendResult{}, err
	}
	_, token, err := h.resolveWorkspaceAndToken(ctx, info, a.Channel)
	if err != nil {
		return slackSendResult{}, err
	}

	var ts string
	if a.Body != "" {
		ts, err = slackChatPostMessage(ctx, h.client, token, slackMessageParams{
			Channel: a.Channel, ThreadTS: a.ThreadTS, Text: a.Body, MarkdownBody: a.Body,
		})
		if err != nil {
			return slackSendResult{}, fmt.Errorf("slack: post message: %w", err)
		}
		if a.AttachBase64 != "" {
			if uerr := h.uploadAttachment(ctx, token, a.Channel, ts, a); uerr != nil {
				// The message already posted — surface the attach failure but keep
				// the ts the agent needs for any follow-up (edit/react), rather than
				// discarding a message that already landed.
				return slackSendResult{Channel: a.Channel, TS: ts},
					fmt.Errorf("slack: message posted (ts=%s) but file attach failed: %w", ts, uerr)
			}
		}
	} else {
		decoded, name, derr := decodeAttachment(a.AttachName, a.AttachBase64)
		if derr != nil {
			return slackSendResult{}, derr
		}
		fileID, uerr := slackFilesUpload(ctx, h.client, token, slackFileUploadParams{
			Filename: name, Length: len(decoded), Channel: a.Channel, ThreadTS: a.ThreadTS, Body: bytes.NewReader(decoded),
		})
		if uerr != nil {
			return slackSendResult{}, fmt.Errorf("slack: upload file: %w", uerr)
		}
		ts, err = h.findRecentMessageTSForFile(ctx, token, a.Channel, a.ThreadTS, fileID)
		if err != nil {
			return slackSendResult{}, fmt.Errorf("slack: file uploaded but could not resolve its message ts: %w", err)
		}
	}

	rootTS := a.ThreadTS
	if rootTS == "" {
		rootTS = ts
	}
	h.recordMessage(ctx, info, domain.ActionSlackMessagePosted, a.Channel, rootTS, ts, h.permalinkBestEffort(ctx, token, a.Channel, ts))
	return slackSendResult{Channel: a.Channel, TS: ts}, nil
}

func (h *slackExecHandler) edit(ctx context.Context, info agenthost.RunInfo, a slackEditArgs) (slackEditResult, error) {
	if a.Channel == "" || a.TS == "" {
		return slackEditResult{}, fmt.Errorf("slack: --channel and --ts are required")
	}
	if a.Body == "" {
		return slackEditResult{}, fmt.Errorf("slack: --body/--body-file is required")
	}
	if err := h.authorizeChannel(ctx, info, a.Channel); err != nil {
		return slackEditResult{}, err
	}
	_, token, err := h.resolveWorkspaceAndToken(ctx, info, a.Channel)
	if err != nil {
		return slackEditResult{}, err
	}
	if err := slackChatUpdate(ctx, h.client, token, slackMessageParams{
		Channel: a.Channel, ThreadTS: a.TS, Text: a.Body, MarkdownBody: a.Body,
	}); err != nil {
		return slackEditResult{}, fmt.Errorf("slack: update message: %w", err)
	}

	rootTS := h.resolveMessageRootTS(ctx, token, a.Channel, a.TS)
	h.recordMessage(ctx, info, domain.ActionSlackMessageEdited, a.Channel, rootTS, a.TS, h.permalinkBestEffort(ctx, token, a.Channel, a.TS))
	return slackEditResult{Channel: a.Channel, TS: a.TS}, nil
}

func (h *slackExecHandler) react(ctx context.Context, info agenthost.RunInfo, a slackReactArgs) (slackReactResult, error) {
	if a.Channel == "" || a.TS == "" || a.Emoji == "" {
		return slackReactResult{}, fmt.Errorf("slack: --channel, --ts, and --emoji are required")
	}
	if err := h.authorizeChannel(ctx, info, a.Channel); err != nil {
		return slackReactResult{}, err
	}
	_, token, err := h.resolveWorkspaceAndToken(ctx, info, a.Channel)
	if err != nil {
		return slackReactResult{}, err
	}
	if err := slackReactionsAdd(ctx, h.client, token, a.Channel, a.TS, a.Emoji); err != nil {
		return slackReactResult{}, fmt.Errorf("slack: add reaction: %w", err)
	}

	rootTS := h.resolveMessageRootTS(ctx, token, a.Channel, a.TS)
	detail, _ := json.Marshal(map[string]string{"emoji": a.Emoji})
	agenthost.RecordExternalWrite(ctx, h.stores, info, nil, &domain.ExternalAction{
		Provider:   domain.ArtifactProviderSlack,
		Action:     domain.ActionSlackReactionAdded,
		Target:     domain.SlackSourceID(a.Channel, rootTS),
		ExternalID: a.TS,
		Credential: domain.CredentialSlackBot,
		DetailJSON: string(detail),
	})
	return slackReactResult{}, nil
}

// recordMessage records the send/edit matrix: an `artifacts` row keyed
// channel+ts (the message's own ts — send and a later edit share the same
// key, upserting in place per TFAC-596's matrix) with Target set to the
// thread's SlackSourceID (rootTS), plus the paired `external_actions` audit
// row under action.
func (h *slackExecHandler) recordMessage(ctx context.Context, info agenthost.RunInfo, action, channel, rootTS, ts, permalink string) {
	target := domain.SlackSourceID(channel, rootTS)
	a := &domain.Artifact{
		Provider:   domain.ArtifactProviderSlack,
		Kind:       domain.ArtifactKindMessage,
		Target:     target,
		ExternalID: ts,
		URL:        permalink,
		State:      domain.ArtifactStateMessagePosted,
		DedupKey:   domain.ArtifactDedupKey(domain.ArtifactProviderSlack, domain.ArtifactKindMessage, channel+"/"+ts, ""),
	}
	act := &domain.ExternalAction{
		Provider:   domain.ArtifactProviderSlack,
		Action:     action,
		Target:     target,
		ExternalID: ts,
		URL:        permalink,
		Credential: domain.CredentialSlackBot,
	}
	agenthost.RecordExternalWrite(ctx, h.stores, info, a, act)
}

// permalinkBestEffort resolves ts's shareable link; "" on any failure — a
// permalink is a nice-to-have on the artifact row, never load-bearing.
func (h *slackExecHandler) permalinkBestEffort(ctx context.Context, token, channel, ts string) string {
	link, err := slackChatGetPermalink(ctx, h.client, token, channel, ts)
	if err != nil {
		return ""
	}
	return link
}

// resolveMessageRootTS best-effort resolves ts's thread root: a bounded
// conversations.history lookup (Latest=ts, Inclusive, Limit=1) reads the
// message's own thread_ts field, if any. Falls back to ts itself (a
// channel-root message, or a lookup failure) rather than erroring — an edit/
// react's recording is best-effort and must not fail the already-applied
// Slack write over a cosmetic Target grouping.
func (h *slackExecHandler) resolveMessageRootTS(ctx context.Context, token, channel, ts string) string {
	msgs, err := slackConversationsHistory(ctx, h.client, token, slackConversationsHistoryParams{
		Channel: channel, Latest: ts, Inclusive: true, Limit: 1,
	})
	if err != nil || len(msgs) == 0 {
		return ts
	}
	if msgs[0].ThreadTS != "" {
		return msgs[0].ThreadTS
	}
	return ts
}

// findRecentMessageTSForFile locates the ts of the message a just-completed
// file upload landed as, by scanning a bounded recent window (thread replies
// if threadTS is set, else channel history) for a message carrying fileID
// among its files. Slack's v2 upload flow doesn't return the sharing
// message's ts directly, so this is the best available signal.
func (h *slackExecHandler) findRecentMessageTSForFile(ctx context.Context, token, channel, threadTS, fileID string) (string, error) {
	const scanWindow = 20
	var msgs []slackMessage
	var err error
	if threadTS != "" {
		msgs, _, err = slackConversationsReplies(ctx, h.client, token, channel, threadTS, scanWindow)
	} else {
		msgs, err = slackConversationsHistory(ctx, h.client, token, slackConversationsHistoryParams{Channel: channel, Limit: scanWindow})
	}
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		for _, f := range m.Files {
			if f.ID == fileID {
				return m.TS, nil
			}
		}
	}
	return "", fmt.Errorf("could not locate the uploaded file's message among the %d most recent", scanWindow)
}

// decodeAttachment base64-decodes an attachment payload, defaulting its name
// and enforcing slackExecMaxFileBytes.
func decodeAttachment(name, base64Body string) (decoded []byte, resolvedName string, err error) {
	decoded, err = base64.StdEncoding.DecodeString(base64Body)
	if err != nil {
		return nil, "", fmt.Errorf("slack: decode attachment: %w", err)
	}
	if len(decoded) > slackExecMaxFileBytes {
		return nil, "", fmt.Errorf("slack: attachment is %d bytes, exceeds the %d-byte cap", len(decoded), slackExecMaxFileBytes)
	}
	if name == "" {
		name = "attachment"
	}
	return decoded, name, nil
}

// uploadAttachment decodes a.AttachBase64 and uploads it into channel,
// threaded under threadTS (the just-posted message's own ts) — a reply
// carrying the file rather than a separate top-level share.
func (h *slackExecHandler) uploadAttachment(ctx context.Context, token, channel, threadTS string, a slackSendArgs) error {
	decoded, name, err := decodeAttachment(a.AttachName, a.AttachBase64)
	if err != nil {
		return err
	}
	_, err = slackFilesUpload(ctx, h.client, token, slackFileUploadParams{
		Filename: name, Length: len(decoded), Channel: channel, ThreadTS: threadTS, Body: bytes.NewReader(decoded),
	})
	return err
}

// --- read thread / read channel ---

func (h *slackExecHandler) readThread(ctx context.Context, info agenthost.RunInfo, a slackReadThreadArgs) ([]slackMessageView, error) {
	if a.Channel == "" || a.TS == "" {
		return nil, fmt.Errorf("slack: --channel and --ts are required")
	}
	if err := h.authorizeChannel(ctx, info, a.Channel); err != nil {
		return nil, err
	}
	_, token, err := h.resolveWorkspaceAndToken(ctx, info, a.Channel)
	if err != nil {
		return nil, err
	}
	msgs, _, err := slackConversationsReplies(ctx, h.client, token, a.Channel, a.TS, a.Limit)
	if err != nil {
		return nil, fmt.Errorf("slack: read thread: %w", err)
	}
	return h.viewMessages(ctx, token, msgs), nil
}

// defaultReadChannelLimit bounds a plain (non-anchored) `read channel` call
// when the agent passes no --limit — enough recent context to be useful
// without an unbounded fetch.
const defaultReadChannelLimit = 20

func (h *slackExecHandler) readChannel(ctx context.Context, info agenthost.RunInfo, a slackReadChannelArgs) ([]slackMessageView, error) {
	if a.Channel == "" {
		return nil, fmt.Errorf("slack: --channel is required")
	}
	if err := h.authorizeChannel(ctx, info, a.Channel); err != nil {
		return nil, err
	}
	_, token, err := h.resolveWorkspaceAndToken(ctx, info, a.Channel)
	if err != nil {
		return nil, err
	}

	var msgs []slackMessage
	if a.TS != "" && (a.NumPrior > 0 || a.NumFollowing > 0) {
		msgs, err = h.readChannelAnchored(ctx, token, a)
	} else {
		limit := a.Limit
		if limit <= 0 {
			limit = defaultReadChannelLimit
		}
		msgs, err = slackConversationsHistory(ctx, h.client, token, slackConversationsHistoryParams{Channel: a.Channel, Limit: limit})
	}
	if err != nil {
		return nil, fmt.Errorf("slack: read channel: %w", err)
	}
	sortMessagesByTS(msgs)
	return h.viewMessages(ctx, token, msgs), nil
}

// readChannelAnchored builds the N-prior/N-following window around a.TS: up
// to two bounded conversations.history calls (before/after) plus the anchor
// message itself, rather than one unbounded fetch — mirrors
// slackConversationsHistory's own "no internal pagination loop" posture.
func (h *slackExecHandler) readChannelAnchored(ctx context.Context, token string, a slackReadChannelArgs) ([]slackMessage, error) {
	var out []slackMessage
	if a.NumPrior > 0 {
		before, err := slackConversationsHistory(ctx, h.client, token, slackConversationsHistoryParams{
			Channel: a.Channel, Latest: a.TS, Inclusive: false, Limit: a.NumPrior,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, before...)
	}
	anchor, err := slackConversationsHistory(ctx, h.client, token, slackConversationsHistoryParams{
		Channel: a.Channel, Latest: a.TS, Inclusive: true, Limit: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve anchor message: %w", err)
	}
	out = append(out, anchor...)
	if a.NumFollowing > 0 {
		after, err := slackConversationsHistory(ctx, h.client, token, slackConversationsHistoryParams{
			Channel: a.Channel, Oldest: a.TS, Inclusive: false, Limit: a.NumFollowing,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, after...)
	}
	return out, nil
}

// sortMessagesByTS orders messages oldest-first (conversations.history
// returns newest-first) so a read verb's output reads top-to-bottom like the
// thread/channel itself. Compares ts as (seconds, fractional-nanoseconds)
// integer pairs rather than a parsed float64: a Slack ts's 10-digit seconds
// + 6-digit fraction is 16 significant digits, right at float64's ~15-17
// digit precision ceiling, so two close-together messages could silently
// misorder if compared as floats.
func sortMessagesByTS(msgs []slackMessage) {
	sort.Slice(msgs, func(i, j int) bool {
		si, fi := parseSlackTSParts(msgs[i].TS)
		sj, fj := parseSlackTSParts(msgs[j].TS)
		if si != sj {
			return si < sj
		}
		return fi < fj
	})
}

// parseSlackTSParts splits a Slack ts ("1355517523.000005") into its integer
// seconds and a fixed-width (9-digit, nanosecond-scale) fractional part —
// mirrors ingest.go's parseSlackTS zero-padding convention so the two stay
// consistent. An unparsable ts yields (0, 0) — sorts first, same "unknown"
// treatment parseSlackTS gives a bad timestamp elsewhere in this package.
func parseSlackTSParts(ts string) (seconds, fracNanos int64) {
	secStr, fracStr, _ := strings.Cut(ts, ".")
	seconds, _ = strconv.ParseInt(secStr, 10, 64)
	if fracStr == "" {
		return seconds, 0
	}
	for len(fracStr) < 9 {
		fracStr += "0"
	}
	fracNanos, _ = strconv.ParseInt(fracStr[:9], 10, 64)
	return seconds, fracNanos
}

// viewMessages converts the API-level slackMessage slice to the CLI-facing
// slackMessageView slice, best-effort resolving each sender's display name
// via slackUsersInfo — cached per invocation (per read call) so a thread
// with many messages from the same few people doesn't re-resolve the same
// user repeatedly. A name-resolution failure never fails the read: the
// message is still returned with SenderName left empty.
func (h *slackExecHandler) viewMessages(ctx context.Context, token string, msgs []slackMessage) []slackMessageView {
	names := map[string]string{}
	out := make([]slackMessageView, 0, len(msgs))
	for _, m := range msgs {
		senderID := m.User
		if senderID == "" {
			senderID = m.BotID
		}
		senderName := ""
		if m.User != "" {
			if cached, ok := names[m.User]; ok {
				senderName = cached
			} else {
				info, err := slackUsersInfo(ctx, h.client, token, m.User)
				if err == nil && info != nil {
					senderName = nonEmpty(info.DisplayName, info.RealName)
				}
				names[m.User] = senderName
			}
		}
		view := slackMessageView{TS: m.TS, ThreadTS: m.ThreadTS, SenderID: senderID, SenderName: senderName, Text: m.Text}
		for _, f := range m.Files {
			view.Files = append(view.Files, slackFileRefView(f))
		}
		out = append(out, view)
	}
	return out
}

// --- download ---

func (h *slackExecHandler) download(ctx context.Context, info agenthost.RunInfo, a slackDownloadArgs) (slackDownloadResult, error) {
	if a.FileID == "" {
		return slackDownloadResult{}, fmt.Errorf("slack: --id is required")
	}
	token, err := h.resolveWorkspaceForDownload(ctx, info)
	if err != nil {
		return slackDownloadResult{}, err
	}

	fi, err := slackFilesInfo(ctx, h.client, token, a.FileID)
	if err != nil {
		return slackDownloadResult{}, fmt.Errorf("slack: look up file: %w", err)
	}
	// Authorize against the file's OWN channel membership, not the run's
	// context — resolveWorkspaceForDownload only picked a bot identity, it
	// never checked a channel (download has none to check upfront). Every
	// other verb authorizes before making any Slack call; download can only
	// do so after this lookup answers, since a bare file id doesn't name a
	// channel on its own.
	if err := h.authorizeFileChannels(ctx, info, fi.Channels); err != nil {
		return slackDownloadResult{}, err
	}
	if fi.Size > slackExecMaxFileBytes {
		return slackDownloadResult{}, fmt.Errorf("slack: file is %d bytes, exceeds the %d-byte download cap", fi.Size, slackExecMaxFileBytes)
	}
	var buf bytes.Buffer
	if err := slackFileDownload(ctx, h.client, token, fi.URLPrivate, &buf); err != nil {
		return slackDownloadResult{}, fmt.Errorf("slack: download file: %w", err)
	}
	return slackDownloadResult{Name: fi.Name, Base64: base64.StdEncoding.EncodeToString(buf.Bytes())}, nil
}
