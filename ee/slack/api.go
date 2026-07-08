package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// slackAPIBase is the Slack Web API base. A var (not a const) so tests can
// point it at a fake server; production never overrides it. No per-org
// override exists — unlike GitHub/Jira, Slack is a single fixed host.
var slackAPIBase = "https://slack.com/api"

// slackHTTPClient bounds every Slack API call so a hung upstream can't wedge
// a connect request. Slack is a third-party network hop, so this is the
// upper bound for the abnormal case — mirrors ssoHTTPClient's rationale for
// the in-network GoTrue admin call, just with a shorter budget since these
// calls run inline in a user-facing request.
var slackHTTPClient = &http.Client{Timeout: 15 * time.Second}

// authTestResult is the subset of Slack's auth.test response the connect
// handler needs: the workspace identity (team_id/team), the bot's own user
// id (so ingest can later detect its own mentions), the bot's own bot id
// (the first leg of the api_app_id derivation chain — see slackBotsInfo),
// and the Enterprise Grid id when present.
type authTestResult struct {
	TeamID       string
	Team         string
	UserID       string
	BotID        string
	EnterpriseID string
}

// slackAuthTest validates botToken against Slack's auth.test and returns
// the workspace identity it resolves to. This is the ONLY way the connect
// handler learns a workspace's id — the admin never types it. A non-2xx
// HTTP response or a Slack-level {"ok":false} both surface as an error
// carrying Slack's own message where available; an "ok":true response
// missing team_id or bot_id also errors rather than returning a
// partially-populated result — bot_id in particular feeds straight into
// slackBotsInfo, so a silently-empty value here would surface later as a
// confusing bots.info failure instead of a clear one at its actual source.
func slackAuthTest(ctx context.Context, client *http.Client, botToken string) (*authTestResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/auth.test", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var out struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error"`
		TeamID       string `json:"team_id"`
		Team         string `json:"team"`
		UserID       string `json:"user_id"`
		BotID        string `json:"bot_id"`
		EnterpriseID string `json:"enterprise_id"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("slack auth.test: %s", nonEmpty(out.Error, "not ok"))
	}
	if out.TeamID == "" {
		return nil, fmt.Errorf("slack auth.test: response carried no team_id")
	}
	if out.BotID == "" {
		return nil, fmt.Errorf("slack auth.test: response carried no bot_id")
	}
	return &authTestResult{
		TeamID:       out.TeamID,
		Team:         out.Team,
		UserID:       out.UserID,
		BotID:        out.BotID,
		EnterpriseID: out.EnterpriseID,
	}, nil
}

// botsInfoResult is the subset of Slack's bots.info response the connect
// handler needs: the app id that owns this bot — the key material for the
// app-single-org invariant.
type botsInfoResult struct {
	AppID string
}

// slackBotsInfo looks up a bot's owning app id via bots.info — the second
// leg of the connect handler's app-id derivation chain (auth.test -> bot_id
// -> slackBotsInfo -> app_id). Runs under the users:read scope already in
// the shipped manifest (confirmed against Slack's current docs: bots.info
// is users:read-scoped despite the "bots" name). A non-2xx HTTP response or
// a Slack-level {"ok":false} both surface as an error; the connect handler
// treats any error here as fatal (400, connect refused) — the app id is now
// key material, not an optional enrichment, so there is no fallback.
func slackBotsInfo(ctx context.Context, client *http.Client, botToken, botID string) (*botsInfoResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		slackAPIBase+"/bots.info?bot="+url.QueryEscape(botID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Bot   struct {
			AppID string `json:"app_id"`
		} `json:"bot"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("slack bots.info: %s", nonEmpty(out.Error, "not ok"))
	}
	if out.Bot.AppID == "" {
		return nil, fmt.Errorf("slack bots.info: response carried no app_id")
	}
	return &botsInfoResult{AppID: out.Bot.AppID}, nil
}

// slackOpenConnection validates appToken (an app-level xapp- token) via
// apps.connections.open — used at connect time purely as a credential
// check: a successful call returns a wss:// URL good for one connection,
// which this leaf discards (the socket connection manager that actually
// dials one, socket_conn.go, calls slackConnectionsOpen directly instead).
func slackOpenConnection(ctx context.Context, client *http.Client, appToken string) error {
	_, err := slackConnectionsOpen(ctx, client, appToken)
	return err
}

// slackAuthErrorCodes is the set of apps.connections.open error codes that
// mean the app token itself is bad (revoked, wrong scope, disabled
// workspace) rather than a transient upstream issue — retrying on the
// normal backoff schedule would just hammer Slack with a token that will
// never succeed. Sourced from Slack's documented auth-related error codes.
var slackAuthErrorCodes = map[string]bool{
	"invalid_auth":     true,
	"not_authed":       true,
	"account_inactive": true,
	"token_revoked":    true,
	"missing_scope":    true,
}

// slackAuthError wraps an apps.connections.open failure whose error code is
// in slackAuthErrorCodes — the signal the socket connection loop's backoff
// (socket_conn.go's run) uses to jump straight to the cap and report
// stateAuthFailed instead of the ordinary backing-off status.
type slackAuthError struct{ code string }

func (e *slackAuthError) Error() string { return "slack: " + e.code }

// isSlackAuthError reports whether err wraps a slackAuthError.
func isSlackAuthError(err error) bool {
	var authErr *slackAuthError
	return errors.As(err, &authErr)
}

// slackConnectionsOpen calls apps.connections.open and returns the
// short-lived, single-use wss:// URL it mints for one connection — the
// Socket Mode handshake's first call. A fresh call is required per
// (re)connect; the returned URL is never reused.
func slackConnectionsOpen(ctx context.Context, client *http.Client, appToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/apps.connections.open", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appToken)

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		URL   string `json:"url"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return "", err
	}
	if !out.OK {
		if slackAuthErrorCodes[out.Error] {
			return "", &slackAuthError{code: nonEmpty(out.Error, "not ok")}
		}
		return "", fmt.Errorf("slack apps.connections.open: %s", nonEmpty(out.Error, "not ok"))
	}
	if out.URL == "" {
		return "", fmt.Errorf("slack apps.connections.open: response carried no url")
	}
	return out.URL, nil
}

// usersInfoResult is the subset of Slack's users.info response the identity
// resolver (TFAC-531) needs: the profile email (the verified-email match
// key), a display name for the audit-rendering column, and the
// is_bot/deleted flags that short-circuit resolution for senders that can
// never map to a human TF user.
type usersInfoResult struct {
	RealName    string
	DisplayName string
	Email       string
	IsBot       bool
	Deleted     bool
}

// slackUsersInfo looks up a Slack user's profile via users.info — the
// identity resolver's only network call. Requires the users:read.email
// scope (already in the shipped manifest, TFAC-529). A non-2xx HTTP
// response or a Slack-level {"ok":false} both surface as an error; the
// resolver treats any error here as transient (it writes nothing, so the
// next mention retries — see ee/slack/identity.go).
func slackUsersInfo(ctx context.Context, client *http.Client, botToken, slackUserID string) (*usersInfoResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		slackAPIBase+"/users.info?user="+url.QueryEscape(slackUserID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			IsBot   bool `json:"is_bot"`
			Deleted bool `json:"deleted"`
			Profile struct {
				Email       string `json:"email"`
				RealName    string `json:"real_name"`
				DisplayName string `json:"display_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("slack users.info: %s", nonEmpty(out.Error, "not ok"))
	}
	return &usersInfoResult{
		RealName:    out.User.Profile.RealName,
		DisplayName: out.User.Profile.DisplayName,
		Email:       out.User.Profile.Email,
		IsBot:       out.User.IsBot,
		Deleted:     out.User.Deleted,
	}, nil
}

// conversationsInfoResult is the subset of Slack's conversations.info
// response the channel registry (TFAC-541/542) needs: the channel's
// current display name.
type conversationsInfoResult struct {
	Name string
}

// slackConversationsInfo looks up a channel's display name via
// conversations.info — the channel name resolver's only network call
// (channels.go). Requires the channels:read/groups:read scopes (already in
// the shipped manifest, #561). A non-2xx HTTP response or a Slack-level
// {"ok":false} both surface as an error; the caller treats any error here
// as transient — it writes nothing, so the next sighting or stale-name
// sweep retries (see ee/slack/channels.go).
func slackConversationsInfo(ctx context.Context, client *http.Client, botToken, channelID string) (*conversationsInfoResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		slackAPIBase+"/conversations.info?channel="+url.QueryEscape(channelID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("slack conversations.info: %s", nonEmpty(out.Error, "not ok"))
	}
	return &conversationsInfoResult{Name: out.Channel.Name}, nil
}

// slackRetryAfterCap bounds how long doSlackJSON will wait on a 429's
// Retry-After header before retrying — Slack's own advertised wait is
// honored up to this ceiling so a single misbehaving upstream response
// can't stall a caller far past what's tolerable inline (every wrapper
// flows through here, including ones called on a user-facing request
// path).
const slackRetryAfterCap = 10 * time.Second

// slackRetryAfterDefault is the wait used when a 429 response carries no
// (or an unparsable) Retry-After header — Slack always sends one on a real
// rate limit, so this only guards against a malformed upstream response,
// not the common case.
const slackRetryAfterDefault = 1 * time.Second

// slackRateLimitError wraps a doSlackJSON call that hit a second
// consecutive 429 even after the single Retry-After-honoring retry.
// Distinguishes a sustained rate limit from every other non-2xx failure
// doSlackJSON otherwise returns as a plain error — callers that want to
// special-case sustained throttling (vs. treating it like any other
// failure) can type-assert via isSlackRateLimitError.
type slackRateLimitError struct{ retryAfter time.Duration }

func (e *slackRateLimitError) Error() string {
	return fmt.Sprintf("slack api: rate limited (retry-after %s)", e.retryAfter)
}

// isSlackRateLimitError reports whether err wraps a slackRateLimitError.
func isSlackRateLimitError(err error) bool {
	var e *slackRateLimitError
	return errors.As(err, &e)
}

// doSlackJSON executes req and decodes the JSON body into out. Slack
// answers 200 even for most application-level failures (the {"ok":false}
// convention) — a non-2xx here means something more fundamental (rate
// limit, upstream outage), so it's surfaced with the raw status and a
// capped body rather than attempting to parse it as the {ok,...} shape.
//
// A 429 is a special case: doSlackJSON honors the Retry-After header
// (capped at slackRetryAfterCap, respecting ctx cancellation) and retries
// the request exactly once. A second consecutive 429 returns a typed
// *slackRateLimitError rather than looping further — there is no general
// retry loop here.
func doSlackJSON(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	resp, body, err := doSlackRequest(client, req)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		wait := slackRetryAfterDuration(resp.Header.Get("Retry-After"))
		if err := slackSleep(ctx, wait); err != nil {
			return err
		}
		retryReq, err := cloneSlackRequest(ctx, req)
		if err != nil {
			return fmt.Errorf("slack api: rebuild request for retry: %w", err)
		}
		resp, body, err = doSlackRequest(client, retryReq)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return &slackRateLimitError{retryAfter: slackRetryAfterDuration(resp.Header.Get("Retry-After"))}
		}
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack api: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("slack api: decode response: %w", err)
	}
	return nil
}

// doSlackRequest executes req and reads its (capped) body, wrapping a
// transport-level failure the same way the original inline doSlackJSON
// body did. Shared by doSlackJSON's initial attempt and its single 429
// retry.
func doSlackRequest(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("slack api request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp, body, nil
}

// slackRetryAfterDuration parses a Retry-After header value (Slack always
// sends whole seconds) and caps it at slackRetryAfterCap. An empty or
// unparsable header falls back to slackRetryAfterDefault rather than
// retrying immediately or not at all.
func slackRetryAfterDuration(header string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || secs < 0 {
		return slackRetryAfterDefault
	}
	d := time.Duration(secs) * time.Second
	if d > slackRetryAfterCap {
		return slackRetryAfterCap
	}
	return d
}

// slackSleep waits out d, or returns ctx's error immediately if ctx is
// canceled first — the "respecting ctx cancellation" half of the 429
// retry contract, so a caller with a short-lived context isn't held
// hostage by Slack's advertised wait.
func slackSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cloneSlackRequest rebuilds orig for the single 429 retry. orig's Body
// (if any) was already drained by the first doSlackRequest call, so this
// re-derives it from GetBody — the standard-library-populated replay hook
// every request built by this file gets automatically, since every body
// here originates from a bytes.Reader (see slackConversationsJoin and the
// Part 1 wrappers) rather than an arbitrary one-shot io.Reader.
func cloneSlackRequest(ctx context.Context, orig *http.Request) (*http.Request, error) {
	clone := orig.Clone(ctx)
	if orig.GetBody != nil {
		body, err := orig.GetBody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
	}
	return clone, nil
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// slackConversation is one channel returned by conversations.list or
// users.conversations — the shape the channels API (channels_handler.go)
// needs for the live Slack candidate merge (TFAC-543).
type slackConversation struct {
	ID        string
	Name      string
	IsPrivate bool
}

// slackConversationsListCap bounds how many channels a single
// conversations.list/users.conversations pagination loop collects,
// regardless of how many pages Slack offers — a defensive cap (ticket's
// "paginate to a sane cap ~1000 channels") against an org with an unbounded
// channel count turning the channels-list request into an unbounded loop.
// A var (not a const), mirroring slackAPIBase, so a test can lower it to
// exercise the truncation path without generating a thousand fake channels.
var slackConversationsListCap = 1000

// slackConversationsList enumerates the workspace's public channel universe
// via conversations.list (types=public_channel, exclude_archived=true),
// paginating until Slack stops returning a next_cursor or the cap is hit.
// truncated=true means the cap was hit before Slack ran out of pages — the
// returned list is a prefix, not the whole universe (see
// slackConversationsListCap).
func slackConversationsList(ctx context.Context, client *http.Client, botToken string) (channels []slackConversation, truncated bool, err error) {
	return slackConversationsPaginate(ctx, client, botToken, "conversations.list", "types=public_channel&exclude_archived=true&limit=200")
}

// slackUsersConversations enumerates the bot's own channel memberships
// (public + private) via users.conversations — the source of BotIsMember /
// IsPrivate for the live candidate merge. truncated has the same meaning as
// slackConversationsList's.
func slackUsersConversations(ctx context.Context, client *http.Client, botToken string) (channels []slackConversation, truncated bool, err error) {
	return slackConversationsPaginate(ctx, client, botToken, "users.conversations", "types=public_channel,private_channel&exclude_archived=true&limit=200")
}

// slackConversationsPaginate is the shared pagination loop for
// conversations.list and users.conversations — both return the same
// {ok, channels: [{id,name,is_private}], response_metadata: {next_cursor}}
// shape. A non-2xx HTTP response or a Slack-level {"ok":false} both surface
// as an error; the caller (liveSlackCandidates) treats any error here as
// transient and drops just this workspace's live candidates. truncated=true
// means slackConversationsListCap was hit while Slack still had more pages
// (a non-empty next_cursor) — the caller must not treat the returned list as
// exhaustive; it surfaces a warning rather than silently under-reporting.
func slackConversationsPaginate(ctx context.Context, client *http.Client, botToken, method, query string) (out []slackConversation, truncated bool, err error) {
	cursor := ""
	for {
		reqURL := slackAPIBase + "/" + method + "?" + query
		if cursor != "" {
			reqURL += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Authorization", "Bearer "+botToken)

		var resp struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			Channels []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				IsPrivate bool   `json:"is_private"`
			} `json:"channels"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := doSlackJSON(ctx, client, req, &resp); err != nil {
			return nil, false, err
		}
		if !resp.OK {
			return nil, false, fmt.Errorf("slack %s: %s", method, nonEmpty(resp.Error, "not ok"))
		}
		for _, c := range resp.Channels {
			out = append(out, slackConversation{ID: c.ID, Name: c.Name, IsPrivate: c.IsPrivate})
			if len(out) >= slackConversationsListCap {
				return out, resp.ResponseMetadata.NextCursor != "", nil
			}
		}
		cursor = resp.ResponseMetadata.NextCursor
		if cursor == "" {
			return out, false, nil
		}
	}
}

// slackJoinPrivateChannelCodes are the conversations.join error codes Slack
// returns when the target turns out to be a private channel the bot isn't
// already in — there is no API-level "request to join" for a private
// channel, only an existing member's /invite, so these classify as
// invite_required rather than the generic join_failed (channels_handler.go's
// ensureAndAutoJoin). "channel_not_found" covers the common case: the bot
// has no visibility at all into a private channel it isn't a member of, so
// Slack answers as if the channel doesn't exist rather than "forbidden."
var slackJoinPrivateChannelCodes = map[string]bool{
	"channel_not_found":                     true,
	"method_not_supported_for_channel_type": true,
}

// slackJoinPrivateChannelError wraps a conversations.join failure whose
// error code is in slackJoinPrivateChannelCodes.
type slackJoinPrivateChannelError struct{ code string }

func (e *slackJoinPrivateChannelError) Error() string { return "slack: " + e.code }

// isSlackJoinPrivateChannelError reports whether err wraps a
// slackJoinPrivateChannelError.
func isSlackJoinPrivateChannelError(err error) bool {
	var e *slackJoinPrivateChannelError
	return errors.As(err, &e)
}

// slackConversationsJoin has the bot join channelID via conversations.join —
// the auto-join step for a newly tracked channel (channels_handler.go's
// ensureAndAutoJoin) whose live-candidate view didn't already show the bot
// as a member. Requires the channels:join scope (in the shipped manifest
// since #561). A private-channel failure surfaces as
// *slackJoinPrivateChannelError (see isSlackJoinPrivateChannelError); any
// other non-ok response is a plain error (join_failed).
func slackConversationsJoin(ctx context.Context, client *http.Client, botToken, channelID string) error {
	form := url.Values{"channel": {channelID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/conversations.join",
		bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := doSlackJSON(ctx, client, req, &resp); err != nil {
		return err
	}
	if !resp.OK {
		if slackJoinPrivateChannelCodes[resp.Error] {
			return &slackJoinPrivateChannelError{code: nonEmpty(resp.Error, "not ok")}
		}
		return fmt.Errorf("slack conversations.join: %s", nonEmpty(resp.Error, "not ok"))
	}
	return nil
}

// --- Part 1 (TFAC-595): outbound wrappers ---
//
// Everything below is the Slack agent runtime's outbound surface: post/
// update a message, react, read thread/history context, resolve a
// permalink, drive the assistant status indicator, and move files in both
// directions. Every wrapper still builds on doSlackJSON (api.go's
// doSlackJSON/doSlackRequest), so the 429-retry contract applies uniformly
// — the two exceptions are slackFileDownload and slackUploadFileBytes,
// which move raw bytes rather than a {ok,...} JSON envelope and so talk to
// the HTTP client directly (see each function's doc).

// slackMessageParams is the shared param shape for slackChatPostMessage and
// slackChatUpdate — see each function's doc for how ThreadTS's wire meaning
// differs between the two (thread-reply target vs. the ts of the message
// being edited).
type slackMessageParams struct {
	Channel      string
	ThreadTS     string
	Text         string
	MarkdownBody string
}

// slackMarkdownBodyMaxRunes is Block Kit's documented character cap for a
// "markdown" block. slackChatPostMessage/slackChatUpdate return a typed
// *slackMarkdownTooLongError rather than silently truncating an over-long
// agent-composed body — the caller decides how to shorten it.
const slackMarkdownBodyMaxRunes = 12000

// slackMarkdownTooLongError is slackChatPostMessage/slackChatUpdate's typed
// error for a MarkdownBody exceeding slackMarkdownBodyMaxRunes.
type slackMarkdownTooLongError struct{ length int }

func (e *slackMarkdownTooLongError) Error() string {
	return fmt.Sprintf("slack: markdown body is %d runes, exceeds the %d-rune cap", e.length, slackMarkdownBodyMaxRunes)
}

// isSlackMarkdownTooLongError reports whether err wraps a
// slackMarkdownTooLongError.
func isSlackMarkdownTooLongError(err error) bool {
	var e *slackMarkdownTooLongError
	return errors.As(err, &e)
}

// slackMarkdownBlock is Block Kit's "markdown" block type — Slack renders
// real Markdown from it (including tables), unlike the legacy mrkdwn text
// object. The shape both slackChatPostMessage and slackChatUpdate send
// alongside a plain-text fallback.
type slackMarkdownBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// slackMarkdownBlocksFor validates markdown and wraps it as a single-
// element []slackMarkdownBlock, or returns (nil, nil) when markdown is
// empty — the plain-text-only case neither wrapper should attach blocks
// for.
func slackMarkdownBlocksFor(markdown string) ([]slackMarkdownBlock, error) {
	if markdown == "" {
		return nil, nil
	}
	if n := len([]rune(markdown)); n > slackMarkdownBodyMaxRunes {
		return nil, &slackMarkdownTooLongError{length: n}
	}
	return []slackMarkdownBlock{{Type: "markdown", Text: markdown}}, nil
}

// slackPostJSON POSTs a JSON-encoded payload to slackAPIBase+"/"+method —
// the transport chat.postMessage/chat.update/files.completeUploadExternal
// need to express nested structures (blocks, the files array) that the
// form-urlencoded idiom (slackConversationsJoin, slackReactionsAdd) can't.
// Flows through doSlackJSON like every other wrapper, so the 429 retry
// applies uniformly; the JSON body is built from a bytes.Reader, so
// doSlackJSON's retry clone (cloneSlackRequest) can always replay it via
// the standard library's auto-populated GetBody.
func slackPostJSON(ctx context.Context, client *http.Client, botToken, method string, payload, out any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack api: marshal %s payload: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/"+method, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return doSlackJSON(ctx, client, req, out)
}

// slackChatPostMessage posts a new message via chat.postMessage, returning
// its ts (the id a later slackChatUpdate/slackReactionsAdd call needs).
// params.ThreadTS, when set, is the optional thread root to reply into
// (chat.postMessage's own "thread_ts"). When params.MarkdownBody is set it
// is sent as a single {type:"markdown"} block alongside params.Text as the
// plain-text notification fallback — blocks are never sent alone, so a
// screen reader or notification surface still has something to render.
func slackChatPostMessage(ctx context.Context, client *http.Client, botToken string, params slackMessageParams) (ts string, err error) {
	blocks, err := slackMarkdownBlocksFor(params.MarkdownBody)
	if err != nil {
		return "", err
	}
	body := map[string]any{"channel": params.Channel, "text": params.Text}
	if params.ThreadTS != "" {
		body["thread_ts"] = params.ThreadTS
	}
	if blocks != nil {
		body["blocks"] = blocks
	}

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	if err := slackPostJSON(ctx, client, botToken, "chat.postMessage", body, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("slack chat.postMessage: %s", nonEmpty(out.Error, "not ok"))
	}
	return out.TS, nil
}

// slackChatUpdate edits an existing message via chat.update. params.ThreadTS
// here carries the ts of the message BEING EDITED (chat.update's own "ts"
// parameter) — unlike slackChatPostMessage, where the same struct field is
// an optional thread-reply target. Slack's chat.update has a destructive
// quirk: sending text without blocks REMOVES any blocks the message already
// had. This wrapper always sends text, and sends blocks whenever
// params.MarkdownBody is set; a caller that wants to preserve a message's
// markdown formatting across an edit MUST re-supply MarkdownBody — this
// wrapper does not fetch-and-preserve the prior body on the caller's
// behalf.
func slackChatUpdate(ctx context.Context, client *http.Client, botToken string, params slackMessageParams) error {
	blocks, err := slackMarkdownBlocksFor(params.MarkdownBody)
	if err != nil {
		return err
	}
	body := map[string]any{"channel": params.Channel, "ts": params.ThreadTS, "text": params.Text}
	if blocks != nil {
		body["blocks"] = blocks
	}

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := slackPostJSON(ctx, client, botToken, "chat.update", body, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack chat.update: %s", nonEmpty(out.Error, "not ok"))
	}
	return nil
}

// slackReactionsAdd adds an emoji reaction via reactions.add. Idempotent:
// Slack's own "already_reacted" error means the reaction is already there —
// exactly the caller's desired end state — so it's treated as success
// rather than surfaced as an error.
func slackReactionsAdd(ctx context.Context, client *http.Client, botToken, channel, ts, name string) error {
	form := url.Values{"channel": {channel}, "timestamp": {ts}, "name": {name}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/reactions.add",
		bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := doSlackJSON(ctx, client, req, &resp); err != nil {
		return err
	}
	if !resp.OK && resp.Error != "already_reacted" {
		return fmt.Errorf("slack reactions.add: %s", nonEmpty(resp.Error, "not ok"))
	}
	return nil
}

// slackMessage is one message returned by conversations.replies or
// conversations.history — the shared shape both need for thread/history
// display (agent context).
type slackMessage struct {
	User     string
	BotID    string
	Text     string
	TS       string
	ThreadTS string
}

// slackConversationsRepliesCap bounds how many messages a single
// slackConversationsReplies pagination loop collects, mirroring
// slackConversationsListCap's defensive-cap rationale — an unexpectedly
// long thread must not turn one call into an unbounded loop. A var so a
// test can lower it.
var slackConversationsRepliesCap = 1000

// slackConversationsReplies returns a thread's messages (the root message
// first) via conversations.replies, paginating by cursor until Slack stops
// returning a next_cursor or slackConversationsRepliesCap is hit.
// truncated=true means the cap was hit before Slack ran out of pages — the
// same contract as slackConversationsList's truncated. limit, when > 0, is
// passed through as the per-page size Slack should return.
func slackConversationsReplies(ctx context.Context, client *http.Client, botToken, channel, ts string, limit int) (messages []slackMessage, truncated bool, err error) {
	cursor := ""
	for {
		q := url.Values{"channel": {channel}, "ts": {ts}}
		if limit > 0 {
			q.Set("limit", strconv.Itoa(limit))
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, slackAPIBase+"/conversations.replies?"+q.Encode(), nil)
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Authorization", "Bearer "+botToken)

		var resp struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			Messages []struct {
				User     string `json:"user"`
				BotID    string `json:"bot_id"`
				Text     string `json:"text"`
				TS       string `json:"ts"`
				ThreadTS string `json:"thread_ts"`
			} `json:"messages"`
			HasMore          bool `json:"has_more"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := doSlackJSON(ctx, client, req, &resp); err != nil {
			return nil, false, err
		}
		if !resp.OK {
			return nil, false, fmt.Errorf("slack conversations.replies: %s", nonEmpty(resp.Error, "not ok"))
		}
		for _, m := range resp.Messages {
			messages = append(messages, slackMessage{User: m.User, BotID: m.BotID, Text: m.Text, TS: m.TS, ThreadTS: m.ThreadTS})
			if len(messages) >= slackConversationsRepliesCap {
				return messages, resp.ResponseMetadata.NextCursor != "" || resp.HasMore, nil
			}
		}
		cursor = resp.ResponseMetadata.NextCursor
		if cursor == "" {
			return messages, false, nil
		}
	}
}

// slackConversationsHistoryParams bounds ONE conversations.history call.
// Unlike slackConversationsReplies, this wrapper never loops internally —
// see slackConversationsHistory's doc for why.
type slackConversationsHistoryParams struct {
	Channel   string
	Latest    string
	Oldest    string
	Inclusive bool
	Limit     int
}

// slackConversationsHistory returns one bounded page of channel history via
// conversations.history. Deliberately NOT a pagination loop like
// slackConversationsReplies: a caller building N-before/N-after context
// around an anchor ts makes two separate bounded calls instead — Latest=
// anchor (Inclusive=false) for the messages just before it, Oldest=anchor
// (Inclusive=false) for the messages just after — rather than this wrapper
// walking cursors on its own.
func slackConversationsHistory(ctx context.Context, client *http.Client, botToken string, params slackConversationsHistoryParams) ([]slackMessage, error) {
	q := url.Values{"channel": {params.Channel}}
	if params.Latest != "" {
		q.Set("latest", params.Latest)
	}
	if params.Oldest != "" {
		q.Set("oldest", params.Oldest)
	}
	if params.Inclusive {
		q.Set("inclusive", "true")
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slackAPIBase+"/conversations.history?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var resp struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages []struct {
			User     string `json:"user"`
			BotID    string `json:"bot_id"`
			Text     string `json:"text"`
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
		} `json:"messages"`
	}
	if err := doSlackJSON(ctx, client, req, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack conversations.history: %s", nonEmpty(resp.Error, "not ok"))
	}
	out := make([]slackMessage, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		out = append(out, slackMessage{User: m.User, BotID: m.BotID, Text: m.Text, TS: m.TS, ThreadTS: m.ThreadTS})
	}
	return out, nil
}

// slackChatGetPermalink resolves a message's shareable URL via
// chat.getPermalink. No extra scopes needed beyond what's already granted.
func slackChatGetPermalink(ctx context.Context, client *http.Client, botToken, channel, messageTS string) (string, error) {
	q := url.Values{"channel": {channel}, "message_ts": {messageTS}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slackAPIBase+"/chat.getPermalink?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var out struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		Permalink string `json:"permalink"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("slack chat.getPermalink: %s", nonEmpty(out.Error, "not ok"))
	}
	return out.Permalink, nil
}

// slackAssistantLoadingMessagesMax caps how many rotating loading strings
// slackAssistantSetStatus sends — Slack documents a 10-message limit for
// assistant.threads.setStatus's loading_messages.
const slackAssistantLoadingMessagesMax = 10

// slackAssistantSetStatus sets (or, when status == "", clears) the
// assistant thinking-status indicator via assistant.threads.setStatus.
// Works under chat:write alone — no assistant:write grant needed (Slack
// changelog 2026-03-05 widened the accepted scopes). loadingMessages beyond
// slackAssistantLoadingMessagesMax is truncated; status is always sent
// (even empty), matching Slack's own "empty status clears it" contract.
// loading_messages goes over the wire as a single comma-joined value, not a
// JSON array — this endpoint is form-urlencoded, not one of the JSON-body
// wrappers above.
func slackAssistantSetStatus(ctx context.Context, client *http.Client, botToken, channel, threadTS, status string, loadingMessages []string) error {
	if len(loadingMessages) > slackAssistantLoadingMessagesMax {
		loadingMessages = loadingMessages[:slackAssistantLoadingMessagesMax]
	}
	form := url.Values{"channel_id": {channel}, "thread_ts": {threadTS}, "status": {status}}
	if len(loadingMessages) > 0 {
		form.Set("loading_messages", strings.Join(loadingMessages, ","))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/assistant.threads.setStatus",
		bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := doSlackJSON(ctx, client, req, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("slack assistant.threads.setStatus: %s", nonEmpty(resp.Error, "not ok"))
	}
	return nil
}

// slackFileInfoResult is the subset of files.info's response the file
// download/context flow needs: enough to name the file and fetch its
// bytes.
type slackFileInfoResult struct {
	ID         string
	Name       string
	Mimetype   string
	Size       int64
	URLPrivate string
}

// slackFilesInfo looks up a file's metadata via files.info — the
// prerequisite for slackFileDownload, which needs URLPrivate.
func slackFilesInfo(ctx context.Context, client *http.Client, botToken, fileID string) (*slackFileInfoResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		slackAPIBase+"/files.info?file="+url.QueryEscape(fileID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		File  struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Mimetype   string `json:"mimetype"`
			Size       int64  `json:"size"`
			URLPrivate string `json:"url_private"`
		} `json:"file"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("slack files.info: %s", nonEmpty(out.Error, "not ok"))
	}
	return &slackFileInfoResult{
		ID: out.File.ID, Name: out.File.Name, Mimetype: out.File.Mimetype,
		Size: out.File.Size, URLPrivate: out.File.URLPrivate,
	}, nil
}

// slackFileDownload streams urlPrivate's bytes (slackFilesInfo's
// URLPrivate) to w, using botToken as a Bearer credential — url_private is
// only fetchable with the workspace's own bot token, never anonymously.
// Streams directly from the response body rather than buffering the whole
// file in memory, so a large attachment can't blow up process memory. Does
// NOT flow through doSlackJSON (this isn't a slack.com/api {ok,...}
// endpoint, and the response body is opaque file bytes, not JSON) — no 429
// retry here, matching the rest of this function's plain-HTTP posture.
func slackFileDownload(ctx context.Context, client *http.Client, botToken, urlPrivate string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlPrivate, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack file download request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("slack file download: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("slack file download: copy response body: %w", err)
	}
	return nil
}

// slackUploadedFile is one entry of files.completeUploadExternal's "files"
// array: the file id slackGetUploadURLExternal reserved, with an optional
// display title.
type slackUploadedFile struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// slackGetUploadURLExternal is the first leg of the v2 upload flow. Slack
// retired the legacy files.upload (newly-created apps lost access
// 2024-05-16; the method stopped working entirely 2025-11-12 per Slack's
// changelog), so this — files.getUploadURLExternal +
// files.completeUploadExternal — is the only current path. It reserves a
// short-lived upload destination sized for a file of length bytes.
func slackGetUploadURLExternal(ctx context.Context, client *http.Client, botToken, filename string, length int) (uploadURL, fileID string, err error) {
	form := url.Values{"filename": {filename}, "length": {strconv.Itoa(length)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+"/files.getUploadURLExternal",
		bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var out struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := doSlackJSON(ctx, client, req, &out); err != nil {
		return "", "", err
	}
	if !out.OK {
		return "", "", fmt.Errorf("slack files.getUploadURLExternal: %s", nonEmpty(out.Error, "not ok"))
	}
	return out.UploadURL, out.FileID, nil
}

// slackUploadFileBytes POSTs r's contents to uploadURL — the destination
// slackGetUploadURLExternal reserved. Not a slack.com/api method: the URL
// itself is a one-time credential, so no Authorization header and no
// doSlackJSON envelope decode (Slack's response here isn't the {ok,...}
// shape). Streams from r rather than buffering, mirroring
// slackFileDownload's rationale in the opposite direction.
func slackUploadFileBytes(ctx context.Context, client *http.Client, uploadURL string, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack file upload request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("slack file upload: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// slackCompleteUploadExternal finalizes files previously reserved via
// slackGetUploadURLExternal and uploaded via slackUploadFileBytes, sharing
// them into channel (in-thread when threadTS is set) — the second leg of
// the v2 upload flow.
func slackCompleteUploadExternal(ctx context.Context, client *http.Client, botToken, channel, threadTS string, files []slackUploadedFile) error {
	body := map[string]any{"files": files}
	if channel != "" {
		body["channel_id"] = channel
	}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := slackPostJSON(ctx, client, botToken, "files.completeUploadExternal", body, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack files.completeUploadExternal: %s", nonEmpty(out.Error, "not ok"))
	}
	return nil
}

// slackFileUploadParams bundles one file's upload through the full v2
// flow — see slackFilesUpload.
type slackFileUploadParams struct {
	Filename string
	Length   int
	Title    string
	Channel  string
	ThreadTS string
	Body     io.Reader
}

// slackFilesUpload performs the full v2 upload flow for one file: reserve
// (slackGetUploadURLExternal), upload the bytes (slackUploadFileBytes),
// complete (slackCompleteUploadExternal) — landing it in Channel, in-thread
// when ThreadTS is set. Returns the file id Slack assigned.
func slackFilesUpload(ctx context.Context, client *http.Client, botToken string, params slackFileUploadParams) (fileID string, err error) {
	uploadURL, fileID, err := slackGetUploadURLExternal(ctx, client, botToken, params.Filename, params.Length)
	if err != nil {
		return "", err
	}
	if err := slackUploadFileBytes(ctx, client, uploadURL, params.Body); err != nil {
		return "", err
	}
	if err := slackCompleteUploadExternal(ctx, client, botToken, params.Channel, params.ThreadTS,
		[]slackUploadedFile{{ID: fileID, Title: params.Title}}); err != nil {
		return "", err
	}
	return fileID, nil
}
