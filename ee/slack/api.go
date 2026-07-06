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

// doSlackJSON executes req and decodes the JSON body into out. Slack
// answers 200 even for most application-level failures (the {"ok":false}
// convention) — a non-2xx here means something more fundamental (rate
// limit, upstream outage), so it's surfaced with the raw status and a
// capped body rather than attempting to parse it as the {ok,...} shape.
func doSlackJSON(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack api request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack api: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("slack api: decode response: %w", err)
	}
	return nil
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
const slackConversationsListCap = 1000

// slackConversationsList enumerates the workspace's public channel universe
// via conversations.list (types=public_channel, exclude_archived=true),
// paginating until Slack stops returning a next_cursor or the cap is hit.
func slackConversationsList(ctx context.Context, client *http.Client, botToken string) ([]slackConversation, error) {
	return slackConversationsPaginate(ctx, client, botToken, "conversations.list", "types=public_channel&exclude_archived=true&limit=200")
}

// slackUsersConversations enumerates the bot's own channel memberships
// (public + private) via users.conversations — the source of BotIsMember /
// IsPrivate for the live candidate merge.
func slackUsersConversations(ctx context.Context, client *http.Client, botToken string) ([]slackConversation, error) {
	return slackConversationsPaginate(ctx, client, botToken, "users.conversations", "types=public_channel,private_channel&exclude_archived=true&limit=200")
}

// slackConversationsPaginate is the shared pagination loop for
// conversations.list and users.conversations — both return the same
// {ok, channels: [{id,name,is_private}], response_metadata: {next_cursor}}
// shape. A non-2xx HTTP response or a Slack-level {"ok":false} both surface
// as an error; the caller (liveSlackCandidates) treats any error here as
// transient and drops just this workspace's live candidates.
func slackConversationsPaginate(ctx context.Context, client *http.Client, botToken, method, query string) ([]slackConversation, error) {
	var out []slackConversation
	cursor := ""
	for {
		reqURL := slackAPIBase + "/" + method + "?" + query
		if cursor != "" {
			reqURL += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
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
			return nil, err
		}
		if !resp.OK {
			return nil, fmt.Errorf("slack %s: %s", method, nonEmpty(resp.Error, "not ok"))
		}
		for _, c := range resp.Channels {
			out = append(out, slackConversation{ID: c.ID, Name: c.Name, IsPrivate: c.IsPrivate})
			if len(out) >= slackConversationsListCap {
				return out, nil
			}
		}
		cursor = resp.ResponseMetadata.NextCursor
		if cursor == "" {
			return out, nil
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
