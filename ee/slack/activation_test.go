package slack

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
)

// TestWatchedChannelStatuses: a channel the bot is in is member, a public one
// it is not in is will_join, one it cannot see is needs_invite, and one past a
// truncated listing is unknown; a channel nobody watches is not listed, and
// the answer is ordered by name, then channel id.
func TestWatchedChannelStatuses(t *testing.T) {
	watched := []slackstore.Channel{
		{ID: "r1", ChannelID: "C0PUBLIC", Name: "eng-alerts"},
		{ID: "r2", ChannelID: "C0MEMBER", Name: "deploys"},
		{ID: "r3", ChannelID: "G0PRIVATE", Name: "secret"},
		{ID: "r4", ChannelID: "C0NONAME"},
		{ID: "r5", ChannelID: "C0BBB", Name: "deploys"},
	}
	candidates := []slackCandidate{
		{ChannelID: "C0PUBLIC", Name: "eng-alerts"},
		{ChannelID: "C0MEMBER", Name: "deploys", BotIsMember: true},
		{ChannelID: "C0NONAME", Name: "random"},
		{ChannelID: "C0UNWATCHED", Name: "unwatched"},
	}

	got := watchedChannelStatuses(watched, candidates, false, false)
	want := []activationChannel{
		{ChannelID: "C0BBB", Name: "deploys", Status: channelNeedsInvite},
		{ChannelID: "C0MEMBER", Name: "deploys", Status: channelMember},
		{ChannelID: "C0PUBLIC", Name: "eng-alerts", Status: channelWillJoin},
		{ChannelID: "C0NONAME", Name: "random", Status: channelWillJoin},
		{ChannelID: "G0PRIVATE", Name: "secret", Status: channelNeedsInvite},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses =\n%+v\nwant\n%+v", got, want)
	}

	truncated := watchedChannelStatuses(watched, candidates, true, false)
	for _, c := range truncated {
		wantStatus := map[string]string{
			"C0PUBLIC": channelWillJoin, "C0MEMBER": channelMember, "C0NONAME": channelWillJoin,
			"G0PRIVATE": channelUnknown, "C0BBB": channelUnknown,
		}[c.ChannelID]
		if c.Status != wantStatus {
			t.Errorf("truncated: %s = %q; want %q", c.ChannelID, c.Status, wantStatus)
		}
	}

	for _, c := range watchedChannelStatuses(watched, nil, false, true) {
		if c.Status != channelUnknown {
			t.Errorf("stopped: %s = %q; want unknown", c.ChannelID, c.Status)
		}
	}

	if got := watchedChannelStatuses(nil, candidates, false, false); got == nil || len(got) != 0 {
		t.Errorf("no watched channels = %#v; want an empty list", got)
	}
}

// TestActivationFingerprint: the fingerprint moves with either connection's
// updated_at and with the watched set, and not with the order the watched
// channels are read in.
func TestActivationFingerprint(t *testing.T) {
	at := time.Date(2026, 11, 18, 12, 0, 0, 123456000, time.UTC)
	from := &slackstore.Connection{APIAppID: "A0FROM", UpdatedAt: at}
	to := &slackstore.Connection{APIAppID: "A0TO", UpdatedAt: at}
	watched := []slackstore.Channel{{ID: "r1"}, {ID: "r2"}}
	base := activationFingerprint("org", "T1", from, to, watched)

	if got := activationFingerprint("org", "T1", from, to, []slackstore.Channel{{ID: "r2"}, {ID: "r1"}}); got != base {
		t.Errorf("reordered watched set changed the fingerprint")
	}
	if strings.ContainsAny(base, "+/=") {
		t.Errorf("fingerprint %q is not unpadded base64url", base)
	}

	later := at.Add(time.Microsecond)
	for name, got := range map[string]string{
		"from updated_at":  activationFingerprint("org", "T1", &slackstore.Connection{APIAppID: "A0FROM", UpdatedAt: later}, to, watched),
		"to updated_at":    activationFingerprint("org", "T1", from, &slackstore.Connection{APIAppID: "A0TO", UpdatedAt: later}, watched),
		"a channel added":  activationFingerprint("org", "T1", from, to, append(watched, slackstore.Channel{ID: "r3"})),
		"a channel gone":   activationFingerprint("org", "T1", from, to, watched[:1]),
		"no active":        activationFingerprint("org", "T1", nil, to, watched),
		"another org":      activationFingerprint("org2", "T1", from, to, watched),
		"another space":    activationFingerprint("org", "T2", from, to, watched),
		"another from app": activationFingerprint("org", "T1", &slackstore.Connection{APIAppID: "A0OTHER", UpdatedAt: at}, to, watched),
	} {
		if got == base {
			t.Errorf("%s: fingerprint unchanged", name)
		}
	}
}

// TestActivationBlockers: a bot token Slack refuses, with any code but a
// transient one, is token_rejected, one short a scope is missing_scopes naming
// it, and an app-level token Slack refuses is app_token_rejected, beside
// whatever the bot token gets; tokens that pass have no blocker, and a check
// that meets no answer about a token, Slack failing on its own side or pausing
// the workspace included, is an error rather than a blocker.
func TestActivationBlockers(t *testing.T) {
	all := strings.Join(slackBotScopes, ",")
	short := strings.Join(slices.DeleteFunc(slices.Clone(slackBotScopes), func(s string) bool { return s == "files:write" }), ",")
	var openCalls int
	withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/apps.connections.open") {
			openCalls++
			switch bearerToken(r) {
			case "xapp-revoked":
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "token_revoked"})
			case "xapp-no-permission":
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "no_permission"})
			case "xapp-slack-failing":
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "internal_error"})
			default:
				writeSlackFakeJSON(w, map[string]any{"ok": true, "url": "wss://fake.slack.example/link"})
			}
			return
		}
		switch bearerToken(r) {
		case "xoxb-rejected":
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "invalid_auth"})
		case "xoxb-expired":
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "token_expired"})
		case "xoxb-ekm":
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "ekm_access_denied"})
		case "xoxb-migrating":
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "team_added_to_org"})
		case "xoxb-short":
			w.Header().Set("X-OAuth-Scopes", short)
			writeSlackFakeJSON(w, map[string]any{"ok": true, "team_id": "T1", "bot_id": "B1"})
		case "xoxb-slack-failing":
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "internal_error"})
		case "xoxb-down":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.Header().Set("X-OAuth-Scopes", all)
			writeSlackFakeJSON(w, map[string]any{"ok": true, "team_id": "T1", "bot_id": "B1"})
		}
	})
	h := &workspacesHandler{client: slackHTTPClient}
	target := &slackstore.Connection{WorkspaceID: "T1", APIAppID: "A1"}

	for _, tc := range []struct {
		tokens targetTokens
		want   []activationBlocker
	}{
		{targetTokens{bot: "xoxb-ok"}, nil},
		{targetTokens{bot: "xoxb-rejected"}, []activationBlocker{{Code: blockerTokenRejected}}},
		{targetTokens{bot: "xoxb-expired"}, []activationBlocker{{Code: blockerTokenRejected}}},
		{targetTokens{bot: "xoxb-ekm"}, []activationBlocker{{Code: blockerTokenRejected}}},
		{targetTokens{bot: "xoxb-short"}, []activationBlocker{{Code: blockerMissingScopes, Scopes: []string{"files:write"}}}},
		{targetTokens{bot: "xoxb-ok", app: "xapp-ok"}, nil},
		{targetTokens{bot: "xoxb-ok", app: "xapp-revoked"}, []activationBlocker{{Code: blockerAppTokenRejected}}},
		{targetTokens{bot: "xoxb-ok", app: "xapp-no-permission"}, []activationBlocker{{Code: blockerAppTokenRejected}}},
		{targetTokens{bot: "xoxb-rejected", app: "xapp-revoked"}, []activationBlocker{{Code: blockerTokenRejected}, {Code: blockerAppTokenRejected}}},
	} {
		got, err := h.activationBlockers(t.Context(), "org", target, tc.tokens)
		if err != nil {
			t.Errorf("%+v: err = %v", tc.tokens, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: blockers = %+v; want %+v", tc.tokens, got, tc.want)
		}
	}
	for _, tokens := range []targetTokens{
		{bot: "xoxb-slack-failing"},
		{bot: "xoxb-migrating"},
		{bot: "xoxb-down"},
		{bot: "xoxb-ok", app: "xapp-slack-failing"},
	} {
		if got, err := h.activationBlockers(t.Context(), "org", target, tokens); err == nil {
			t.Errorf("%+v: blockers = %+v, err = nil; want Slack unreachable", tokens, got)
		}
	}

	before := openCalls
	if _, err := h.activationBlockers(t.Context(), "org", target, targetTokens{bot: "xoxb-ok"}); err != nil {
		t.Fatalf("bot token only: %v", err)
	}
	if openCalls != before {
		t.Errorf("apps.connections.open called with no app-level token")
	}
}
