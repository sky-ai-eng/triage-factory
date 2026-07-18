// Host-side handlers for the `triagefactory exec slack ...` verbs (TFAC-596):
// the Slack provider's sidecar-half verb handler. It closes over NO db.Stores
// and NO secret store — it reaches its org-bound policy (channel authz,
// workspace resolution) by RELAYING through the runtime to the
// orchestrator-side ops (exec_provider_ops.go), and its bot token by SELECTING
// from the run's sealed bundle (exec_provider.go). That is what lets the same
// handler run in the orchestrator (all/local) AND in the capless per-run
// credential sidecar, where the exec-verb parser now lives.
//
// Because the handler holds nothing host-specific, all three registrations
// (handler, brain-side credential resolver, orchestrator-side policy ops)
// happen from init() with no stores — so the "slack" namespace is present in
// BOTH the server process (where the policy ops execute) and the sidecar
// process (where the handler runs and only relays). A CLI process links this
// package too, so init() runs there; the entitlement gate (checked via the
// runtime) still refuses it, since a CLI process's entitlements provider is the
// everything-off Static default.
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
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// slackExecHandler dispatches every `exec slack <verb>` call, keyed by
// method name (the CLI's method string mirrors the verb: "send", "edit",
// "react", "read_thread", "read_channel", "download").
type slackExecHandler struct {
	client *http.Client
}

func init() {
	registerSlackExec()
}

// registerSlackExec registers the Slack provider — the sidecar-half verb
// handler, the brain-side credential resolver, and the orchestrator-side policy
// ops. All stores-free, so this runs from init() (see the package doc for why
// init(), not install()).
func registerSlackExec() {
	h := &slackExecHandler{client: slackHTTPClient}
	agenthost.RegisterExtension("slack", entitlements.FeatureSlack, h.handle)
	agenthost.RegisterProviderCredential("slack", slackProviderCredential)
	registerSlackProviderOps()
}

func (h *slackExecHandler) handle(ctx context.Context, rt agenthost.ExtensionRuntime, method string, args json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "send":
		return dispatchSlackExec(args, func(a slackSendArgs) (slackSendResult, error) { return h.send(ctx, rt, a) })
	case "edit":
		return dispatchSlackExec(args, func(a slackEditArgs) (slackEditResult, error) { return h.edit(ctx, rt, a) })
	case "react":
		return dispatchSlackExec(args, func(a slackReactArgs) (slackReactResult, error) { return h.react(ctx, rt, a) })
	case "read_thread":
		return dispatchSlackExec(args, func(a slackReadThreadArgs) ([]slackMessageView, error) { return h.readThread(ctx, rt, a) })
	case "read_channel":
		return dispatchSlackExec(args, func(a slackReadChannelArgs) ([]slackMessageView, error) { return h.readChannel(ctx, rt, a) })
	case "download":
		return dispatchSlackExec(args, func(a slackDownloadArgs) (slackDownloadResult, error) { return h.download(ctx, rt, a) })
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

// --- authorization + workspace/token resolution (all relayed) ---
//
// Every store touch below is a RELAY to the orchestrator-side policy op
// (exec_provider_ops.go): the handler holds no db.Stores. The orchestrator
// answers with an authorization result or a workspace IDENTITY (never a
// token), binding the run's org from its own RunInfo. The bot TOKEN is then
// SELECTED locally from the run's sealed bundle — so no secret crosses back.

// authorizeChannel is the stage-1 gate: the run's team must track channelID.
// The op errors when it doesn't, so a nil return means authorized.
func (h *slackExecHandler) authorizeChannel(ctx context.Context, rt agenthost.ExtensionRuntime, channelID string) error {
	return rt.Relay(ctx, "slack", opAuthorizeChannel, slackChannelArg{Channel: channelID}, nil)
}

// resolveWorkspaceIdentity relays the workspace-identity decision for
// channelID — the shared first step behind resolveToken (which only needs it
// to select a token) and send's root-post bookkeeping (which also needs it
// to address opRecordThreadRoot's permalink dispatch at the right workspace).
func (h *slackExecHandler) resolveWorkspaceIdentity(ctx context.Context, rt agenthost.ExtensionRuntime, channelID string) (slackWorkspaceIdentity, error) {
	var ws slackWorkspaceIdentity
	if err := rt.Relay(ctx, "slack", opResolveWorkspace, slackChannelArg{Channel: channelID}, &ws); err != nil {
		return slackWorkspaceIdentity{}, err
	}
	return ws, nil
}

// resolveToken resolves which bot token to act as for channelID: relay the
// workspace-identity decision, then select that (workspace, app)'s token from
// the sealed bundle.
func (h *slackExecHandler) resolveToken(ctx context.Context, rt agenthost.ExtensionRuntime, channelID string) (string, error) {
	ws, err := h.resolveWorkspaceIdentity(ctx, rt, channelID)
	if err != nil {
		return "", err
	}
	return selectBotToken(ctx, rt, ws)
}

// resolveTokenForDownload resolves a bot token for `download`, which carries no
// channel: relay the download workspace-identity decision, then select.
func (h *slackExecHandler) resolveTokenForDownload(ctx context.Context, rt agenthost.ExtensionRuntime) (string, error) {
	var ws slackWorkspaceIdentity
	if err := rt.Relay(ctx, "slack", opResolveWorkspaceForDownload, struct{}{}, &ws); err != nil {
		return "", err
	}
	return selectBotToken(ctx, rt, ws)
}

// authorizeFileChannels is download's gate, run after files.info resolves the
// file's real channel membership. The op errors when none is tracked.
func (h *slackExecHandler) authorizeFileChannels(ctx context.Context, rt agenthost.ExtensionRuntime, channelIDs []string) error {
	return rt.Relay(ctx, "slack", opAuthorizeFileChannels, slackFileChannelsArg{Channels: channelIDs}, nil)
}

// selectBotToken picks the bot token for a resolved (workspace, app) identity
// from the run's sealed bundle — in-process, never asking the orchestrator for
// a secret. A pair outside the sealed (== authorizable) set is an error,
// symmetric with a channel the team doesn't track.
func selectBotToken(ctx context.Context, rt agenthost.ExtensionRuntime, ws slackWorkspaceIdentity) (string, error) {
	raw, err := rt.ProviderCredential(ctx, "slack")
	if err != nil {
		return "", fmt.Errorf("slack: %w", err)
	}
	var creds slackBundleCreds
	if err := json.Unmarshal(raw, &creds); err != nil {
		return "", fmt.Errorf("slack: parse sealed credentials: %w", err)
	}
	token, ok := creds.tokenFor(ws.WorkspaceID, ws.APIAppID)
	if !ok {
		return "", fmt.Errorf("slack: no bot token for workspace %s (app %s) in this run's sealed set", ws.WorkspaceID, ws.APIAppID)
	}
	return token, nil
}

// --- send / edit / react ---

func (h *slackExecHandler) send(ctx context.Context, rt agenthost.ExtensionRuntime, a slackSendArgs) (slackSendResult, error) {
	if a.Channel == "" {
		return slackSendResult{}, fmt.Errorf("slack: --channel is required")
	}
	if a.Body == "" && a.AttachBase64 == "" {
		return slackSendResult{}, fmt.Errorf("slack: send needs --body/--body-file or --attach-file")
	}
	if err := h.authorizeChannel(ctx, rt, a.Channel); err != nil {
		return slackSendResult{}, err
	}
	ws, err := h.resolveWorkspaceIdentity(ctx, rt, a.Channel)
	if err != nil {
		return slackSendResult{}, err
	}
	token, err := selectBotToken(ctx, rt, ws)
	if err != nil {
		return slackSendResult{}, err
	}

	var ts string
	var attachErr error
	if a.Body != "" {
		ts, err = slackChatPostMessage(ctx, h.client, token, slackMessageParams{
			Channel: a.Channel, ThreadTS: a.ThreadTS, Text: a.Body, MarkdownBody: a.Body,
		})
		if err != nil {
			return slackSendResult{}, fmt.Errorf("slack: post message: %w", err)
		}
		if a.AttachBase64 != "" {
			// Never a reply's ts (Slack's files.completeUploadExternal docs warn
			// against it) — the parent thread's root when replying, or ts itself
			// when this message IS the root (a.ThreadTS == "").
			attachThreadTS := a.ThreadTS
			if attachThreadTS == "" {
				attachThreadTS = ts
			}
			if uerr := h.uploadAttachment(ctx, token, a.Channel, attachThreadTS, a); uerr != nil {
				// The message already posted — record it below like any other
				// successful send, then surface the attach failure loudly rather
				// than returning early and leaving a real post unrecorded.
				attachErr = fmt.Errorf("slack: message posted (ts=%s) but file attach failed: %w", ts, uerr)
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
	var threadRootErr error
	if a.ThreadTS == "" {
		// This send IS the thread root: mint/find its entity as kind="thread"
		// before recordMessage's generic touched-entity resolution below sees
		// the same (channel, ts) key — awaited so it lands first, since
		// FindOrCreate never rewrites kind on an already-known row.
		threadRootErr = h.recordThreadRoot(ctx, rt, ws, a.Channel, ts, sendTitleText(a))
	}
	h.recordMessage(ctx, rt, domain.ActionSlackMessagePosted, a.Channel, rootTS, ts, h.permalinkBestEffort(ctx, token, a.Channel, ts))
	switch {
	case attachErr != nil && threadRootErr != nil:
		return slackSendResult{Channel: a.Channel, TS: ts}, fmt.Errorf("%w; %w", attachErr, threadRootErr)
	case attachErr != nil:
		return slackSendResult{Channel: a.Channel, TS: ts}, attachErr
	case threadRootErr != nil:
		return slackSendResult{Channel: a.Channel, TS: ts}, threadRootErr
	default:
		return slackSendResult{Channel: a.Channel, TS: ts}, nil
	}
}

// sendTitleText picks the text a channel-root send's thread-root entity is
// titled from: the message body, falling back to the attachment's name for
// a file-only send (no --body) — mentionTitle("") would otherwise leave a
// freshly-minted kind="thread" entity's title blank.
func sendTitleText(a slackSendArgs) string {
	if a.Body != "" {
		return a.Body
	}
	if a.AttachName != "" {
		return a.AttachName
	}
	return "attachment"
}

func (h *slackExecHandler) edit(ctx context.Context, rt agenthost.ExtensionRuntime, a slackEditArgs) (slackEditResult, error) {
	if a.Channel == "" || a.TS == "" {
		return slackEditResult{}, fmt.Errorf("slack: --channel and --ts are required")
	}
	if a.Body == "" {
		return slackEditResult{}, fmt.Errorf("slack: --body/--body-file is required")
	}
	if err := h.authorizeChannel(ctx, rt, a.Channel); err != nil {
		return slackEditResult{}, err
	}
	token, err := h.resolveToken(ctx, rt, a.Channel)
	if err != nil {
		return slackEditResult{}, err
	}
	if err := slackChatUpdate(ctx, h.client, token, slackMessageParams{
		Channel: a.Channel, ThreadTS: a.TS, Text: a.Body, MarkdownBody: a.Body,
	}); err != nil {
		return slackEditResult{}, fmt.Errorf("slack: update message: %w", err)
	}

	rootTS := h.resolveMessageRootTS(ctx, token, a.Channel, a.TS)
	h.recordMessage(ctx, rt, domain.ActionSlackMessageEdited, a.Channel, rootTS, a.TS, h.permalinkBestEffort(ctx, token, a.Channel, a.TS))
	return slackEditResult{Channel: a.Channel, TS: a.TS}, nil
}

func (h *slackExecHandler) react(ctx context.Context, rt agenthost.ExtensionRuntime, a slackReactArgs) (slackReactResult, error) {
	if a.Channel == "" || a.TS == "" || a.Emoji == "" {
		return slackReactResult{}, fmt.Errorf("slack: --channel, --ts, and --emoji are required")
	}
	if err := h.authorizeChannel(ctx, rt, a.Channel); err != nil {
		return slackReactResult{}, err
	}
	token, err := h.resolveToken(ctx, rt, a.Channel)
	if err != nil {
		return slackReactResult{}, err
	}
	if err := slackReactionsAdd(ctx, h.client, token, a.Channel, a.TS, a.Emoji); err != nil {
		return slackReactResult{}, fmt.Errorf("slack: add reaction: %w", err)
	}

	rootTS := h.resolveMessageRootTS(ctx, token, a.Channel, a.TS)
	detail, _ := json.Marshal(map[string]string{"emoji": a.Emoji})
	rt.Record(ctx, nil, &domain.ExternalAction{
		Provider:   domain.ArtifactProviderSlack,
		Action:     domain.ActionSlackReactionAdded,
		Target:     domain.SlackSourceID(a.Channel, rootTS),
		ExternalID: a.TS,
		Credential: domain.CredentialSlackBot,
		DetailJSON: string(detail),
	})
	return slackReactResult{}, nil
}

// recordThreadRootMaxAttempts / recordThreadRootRetryDelay bound
// recordThreadRoot's retries. Unlike most best-effort writes in this
// package, a dropped call here isn't a "the next poll/touch fixes it" gap:
// FindOrCreate never revisits kind once a row exists, so if this relay never
// lands before recordMessage's generic touched-entity resolver
// (cmd/exec/agenthost/record.go) creates the same entity with its
// kind="message" default, the thread is mis-tagged for good. A few quick
// retries absorb a transient relay/DB hiccup — the common case — rather than
// gambling the engagement signal on a single round trip.
const recordThreadRootMaxAttempts = 3
const recordThreadRootRetryDelay = 200 * time.Millisecond

// recordThreadRoot relays opRecordThreadRoot for a channel-root send (no
// --thread-ts): the orchestrator idempotently finds-or-creates the thread's
// entity as kind="thread" and, only on first creation, dispatches permalink
// resolution. Awaited (not fire-and-forget) so it lands before recordMessage
// runs next — see send()'s call site. Retries on failure (see the constants'
// doc); if every attempt fails, the error is returned (send() surfaces it
// alongside the already-successful post, mirroring attachErr) rather than
// silently swallowed — a silent failure here has a permanent consequence, so
// it must be loud instead.
func (h *slackExecHandler) recordThreadRoot(ctx context.Context, rt agenthost.ExtensionRuntime, ws slackWorkspaceIdentity, channel, ts, text string) error {
	args := slackThreadRootArg{WorkspaceID: ws.WorkspaceID, APIAppID: ws.APIAppID, Channel: channel, TS: ts, Text: text}
	var err error
retry:
	for attempt := 1; attempt <= recordThreadRootMaxAttempts; attempt++ {
		if err = rt.Relay(ctx, "slack", opRecordThreadRoot, args, nil); err == nil {
			return nil
		}
		if attempt < recordThreadRootMaxAttempts {
			// A labeled break: bare "break" here would only exit the select,
			// leaving the for loop to immediately retry against an already-
			// cancelled ctx instead of giving up right away.
			select {
			case <-ctx.Done():
				err = ctx.Err()
				break retry
			case <-time.After(recordThreadRootRetryDelay):
			}
		}
	}
	slackLog.Error("record thread-root entity failed after retries; thread will be mis-tagged kind=message instead of kind=thread",
		"channel", channel, "ts", ts, "attempts", recordThreadRootMaxAttempts, "error", err)
	return fmt.Errorf("slack: record thread-root entity failed after %d attempts (this thread will show as a mid-thread summons, not an engaged root): %w",
		recordThreadRootMaxAttempts, err)
}

// recordMessage records the send/edit matrix: an `artifacts` row keyed
// channel+ts (the message's own ts — send and a later edit share the same
// key, upserting in place per TFAC-596's matrix) with Target set to the
// thread's SlackSourceID (rootTS), plus the paired `external_actions` audit
// row under action.
func (h *slackExecHandler) recordMessage(ctx context.Context, rt agenthost.ExtensionRuntime, action, channel, rootTS, ts, permalink string) {
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
	rt.Record(ctx, a, act)
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

// resolveMessageRootTS best-effort resolves ts's thread root via a SINGLE
// conversations.replies request (slackConversationsRepliesPage, not the
// full-pagination slackConversationsReplies): Slack accepts either a
// thread's root ts or any reply's ts in the `ts` param and returns the
// thread's messages with the root first, on every page regardless of the
// thread's total reply count — conversations.history (channel-level
// messages only) can never see this, since a reply isn't a channel-level
// message. One page is enough since only msgs[0] is ever read; following
// the cursor would cost one HTTP request per reply in a long thread for no
// benefit. Falls back to ts itself (a channel-root message with no thread,
// or a lookup failure) rather than erroring — an edit/react's recording is
// best-effort and must not fail the already-applied Slack write over a
// cosmetic Target grouping.
func (h *slackExecHandler) resolveMessageRootTS(ctx context.Context, token, channel, ts string) string {
	msgs, _, _, err := slackConversationsRepliesPage(ctx, h.client, token, channel, ts, 1, "")
	if err != nil || len(msgs) == 0 {
		return ts
	}
	return msgs[0].TS
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

func (h *slackExecHandler) readThread(ctx context.Context, rt agenthost.ExtensionRuntime, a slackReadThreadArgs) ([]slackMessageView, error) {
	if a.Channel == "" || a.TS == "" {
		return nil, fmt.Errorf("slack: --channel and --ts are required")
	}
	if err := h.authorizeChannel(ctx, rt, a.Channel); err != nil {
		return nil, err
	}
	token, err := h.resolveToken(ctx, rt, a.Channel)
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

func (h *slackExecHandler) readChannel(ctx context.Context, rt agenthost.ExtensionRuntime, a slackReadChannelArgs) ([]slackMessageView, error) {
	if a.Channel == "" {
		return nil, fmt.Errorf("slack: --channel is required")
	}
	if err := h.authorizeChannel(ctx, rt, a.Channel); err != nil {
		return nil, err
	}
	token, err := h.resolveToken(ctx, rt, a.Channel)
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

func (h *slackExecHandler) download(ctx context.Context, rt agenthost.ExtensionRuntime, a slackDownloadArgs) (slackDownloadResult, error) {
	if a.FileID == "" {
		return slackDownloadResult{}, fmt.Errorf("slack: --id is required")
	}
	token, err := h.resolveTokenForDownload(ctx, rt)
	if err != nil {
		return slackDownloadResult{}, err
	}

	fi, err := slackFilesInfo(ctx, h.client, token, a.FileID)
	if err != nil {
		return slackDownloadResult{}, fmt.Errorf("slack: look up file: %w", err)
	}
	// Authorize against the file's OWN channel membership, not the run's
	// context — resolveTokenForDownload only picked a bot identity, it never
	// checked a channel (download has none to check upfront). Every other verb
	// authorizes before making any Slack call; download can only do so after
	// this lookup answers, since a bare file id doesn't name a channel on its
	// own.
	if err := h.authorizeFileChannels(ctx, rt, fi.Channels); err != nil {
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
