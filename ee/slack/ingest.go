package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// mentionTitleMaxRunes bounds the entity title derived from a mention's
// text — enough to be recognizable in a task list without pulling in the
// whole message body (that stays in the event's metadata_json).
const mentionTitleMaxRunes = 120

// inboundMention is the transport-neutral shape both the Events API
// receiver (this leaf, webhook.go) and the Socket Mode client (the next
// leaf) parse their payload into before calling handleEventCallback —
// Slack's inner "event" object carries the same fields on both transports.
type inboundMention struct {
	Type     string // the inner event's own "type", e.g. "app_mention"
	EventID  string // the outer envelope's Slack event id (Ev…) — the dedup key
	Channel  string
	User     string // the mentioning human's Slack user id
	BotID    string // set when the message itself originated from a bot
	Text     string
	TS       string // the message's own ts
	ThreadTS string // the parent thread's ts; "" for a root-message mention
}

// entityFinder is the narrow slice of db.EntityStore the ingest pipeline
// needs. Declared locally (rather than depending on the full store
// interface) so tests can supply a single-method fake instead of
// implementing db.EntityStore's entire surface; db.EntityStore satisfies
// this structurally, so the production wiring (install.go) needs no
// adapter.
type entityFinder interface {
	FindOrCreateSystem(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error)
}

// ingestPipeline is the transport-neutral core both the Events API receiver
// and (the next leaf) Socket Mode feed through handleEventCallback.
// Constructed once at install with dependencies off ExtensionAPI
// (Stores(), PublishEvent) — see install.go.
type ingestPipeline struct {
	entities   entityFinder
	deliveries slackstore.DeliveryStore
	publish    func(domain.Event)
	// identity resolves the mention's sender to a TF user (TFAC-531),
	// best-effort and detached — see resolveSender's doc. nil-safe: tests
	// and any future caller that doesn't need identity capture simply
	// construct ingestPipeline without it.
	identity *IdentityResolver
	// channels records the sighting registry (slack_channels, TFAC-541);
	// channelName resolves a sighted channel's display name (TFAC-542),
	// best-effort and detached — see captureChannelSighting/
	// resolveChannelName. Both nil-safe for the same reason as identity.
	channels    slackstore.ChannelRegistryStore
	channelName *ChannelResolver
	// permalink resolves a freshly-created thread entity's chat.getPermalink
	// URL into entities.url (TFAC-595), best-effort and detached — see
	// PermalinkResolver.dispatch. Nil-safe for the same reason as identity.
	permalink *PermalinkResolver
	// title recomposes a freshly-created thread entity's title into
	// "<sender> said "<message>" in #<channel>" once the sender + channel
	// names resolve, best-effort and detached — see TitleResolver.dispatch.
	// Nil-safe for the same reason as identity.
	title *TitleResolver
}

// handleEventCallback is the one function both transports call for an
// inbound Slack event. It is deliberately synchronous: one dedup insert,
// one entity find-or-create, one publish — well inside Slack's 3-second
// webhook ack budget. Returns an error only for a genuine failure (store
// error, marshal failure); every "nothing to do" case (wrong event type,
// self/bot mention, duplicate delivery) is a nil-error early return, logged
// at debug — none of those are failures the caller should retry or 5xx on.
func (p *ingestPipeline) handleEventCallback(ctx context.Context, ws slackstore.Workspace, ev inboundMention) error {
	if ev.Type != "app_mention" {
		slackLog.Debug("dropping non-app_mention slack event", "type", ev.Type, "workspace", ws.WorkspaceID)
		return nil
	}
	// EventID feeds the dedup key and Channel/TS feed the entity source_id
	// (domain.SlackSourceID) — an empty value in any of them (a malformed
	// or unexpectedly-shaped payload) would dedup unrelated deliveries
	// together or collide entities across channels, so treat it as
	// malformed and drop rather than deriving a garbage key from it.
	if ev.EventID == "" || ev.Channel == "" || ev.TS == "" {
		slackLog.Debug("dropping malformed app_mention: missing event_id/channel/ts", "workspace", ws.WorkspaceID)
		return nil
	}
	if ev.BotID != "" || (ws.BotUserID != "" && ev.User == ws.BotUserID) {
		slackLog.Debug("dropping self/bot-authored slack mention", "workspace", ws.WorkspaceID)
		return nil
	}

	fresh, err := p.deliveries.MarkDeliveredSystem(ctx, ws.APIAppID, ev.EventID)
	if err != nil {
		return fmt.Errorf("mark slack delivery: %w", err)
	}
	if !fresh {
		slackLog.Debug("dropping duplicate slack delivery", "workspace", ws.WorkspaceID, "app", ws.APIAppID, "event_id", ev.EventID)
		return nil
	}

	occurredAt := parseSlackTS(ev.TS)
	p.captureChannelSighting(ctx, ws, ev.Channel, occurredAt)

	root := ev.ThreadTS
	if root == "" {
		root = ev.TS
	}
	sourceID := domain.SlackSourceID(ev.Channel, root)

	entity, created, err := p.entities.FindOrCreateSystem(ctx, ws.OrgID, "slack", sourceID, "message", mentionTitle(ev.Text), "")
	if err != nil {
		return fmt.Errorf("find or create slack entity: %w", err)
	}
	// Only on create: the thread-root permalink is stable once minted, and
	// repeat mentions on an already-known thread shouldn't re-resolve it.
	if created && p.permalink != nil {
		p.permalink.dispatch(ws, entity.ID, ev.Channel, root)
	}
	// Only on create, for the same reason: the enriched title is written once
	// against the thread-creating mention, not re-derived per re-mention.
	if created && p.title != nil {
		p.title.dispatch(ws, entity.ID, ev.Channel, ev.User, ev.Text)
	}

	metaJSON, err := json.Marshal(SlackMessageMetadata{
		WorkspaceID: ws.WorkspaceID,
		APIAppID:    ws.APIAppID,
		Channel:     ev.Channel,
		TS:          ev.TS,
		ThreadTS:    ev.ThreadTS,
		SenderID:    ev.User,
		Text:        ev.Text,
		EventID:     ev.EventID,
		Mentioned:   true,
	})
	if err != nil {
		return fmt.Errorf("marshal slack message metadata: %w", err)
	}

	p.publish(domain.Event{
		OrgID:        ws.OrgID,
		EventType:    domain.EventSlackMessage,
		EntityID:     &entity.ID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   occurredAt,
	})

	// Best-effort sender identity capture (TFAC-531), detached from the
	// publish above: it must never make a real Slack mention wait on a
	// users.info round-trip, and its own internal timeout bounds the whole
	// chain regardless of this context.Background() being long-lived.
	if p.identity != nil {
		go p.identity.resolveSender(context.Background(), ws, ev.User)
	}

	return nil
}

// captureChannelSighting upserts the sighting registry row (slack_channels,
// TFAC-541) for channelID and, on the first sighting or a stale name,
// dispatches best-effort channel-name resolution — detached from the
// publish path exactly like sender identity capture (resolveSender): a
// registry hiccup or a slow conversations.info round-trip must never
// withhold the publish that follows or the transport's ack. Every failure
// here is logged and swallowed, never returned — see handleEventCallback's
// caller, which always proceeds to publish regardless of this call's
// outcome.
func (p *ingestPipeline) captureChannelSighting(ctx context.Context, ws slackstore.Workspace, channelID string, at time.Time) {
	if p.channels == nil {
		return
	}
	created, err := p.channels.UpsertSightingSystem(ctx, ws.OrgID, ws.WorkspaceID, channelID, at)
	if err != nil {
		slackLog.Warn("channel sighting upsert failed", "workspace", ws.WorkspaceID, "channel", channelID, "error", err)
		return
	}
	needsResolve := created
	if !created {
		// One extra read per non-first mention, only to check staleness —
		// accepted here rather than threading a "last known name state"
		// through UpsertSightingSystem's return value: mention volume is
		// inherently low (one delivery per @mention, not per message), and
		// this keeps the staleness rule (channels.go's channelNameStaleAfter)
		// out of the store layer's write path.
		row, err := p.channels.GetSystem(ctx, ws.OrgID, channelID)
		if err != nil {
			slackLog.Warn("channel sighting: staleness check failed", "workspace", ws.WorkspaceID, "channel", channelID, "error", err)
		} else if row != nil {
			needsResolve = row.NameResolvedAt == nil || time.Since(*row.NameResolvedAt) > channelNameStaleAfter
		}
	}
	if needsResolve && p.channelName != nil {
		p.channelName.dispatch(ws, channelID)
	}
}

// slackEntityRe matches a Slack mrkdwn entity token — the <...> forms Slack
// wraps around user/channel/special mentions and links in a message's raw
// text. The inner content decides the kind: a leading @, #, or ! is a
// mention/special; anything else is a link. Captured and rewritten to a
// human-readable form by humanizeSlackText.
var slackEntityRe = regexp.MustCompile(`<([^>]+)>`)

// mentionTitle derives the entity title from a mention's raw text. Slack
// delivers the body with its markup intact — the triggering @bot is a
// `<@U…>` token, other people are `<@U…|name>`, channels are `<#C…|name>`,
// links are `<url|label>` — so a bare title reads like
// "<@U0BHY927K34> What can you do?". humanizeSlackText turns those tokens
// into their readable form (dropping the leading bot mention, keeping named
// mentions as "@name"/"#name", links as their label), then whitespace is
// collapsed and the result capped at mentionTitleMaxRunes.
func mentionTitle(text string) string {
	return truncateRunes(strings.Join(strings.Fields(humanizeSlackText(text)), " "), mentionTitleMaxRunes)
}

// humanizeSlackText rewrites Slack's mrkdwn entity tokens into plain,
// title-friendly text and unescapes the three characters Slack escapes in
// message bodies (&amp; &lt; &gt;). Entities carrying no display name — a
// bare `<@U123>` or `<#C123>`, whose name lives behind an API lookup this
// synchronous path deliberately never makes — are dropped rather than
// leaking a raw Slack id into the title.
func humanizeSlackText(text string) string {
	out := slackEntityRe.ReplaceAllStringFunc(text, func(tok string) string {
		inner := tok[1 : len(tok)-1] // strip the surrounding < >
		body, label, hasLabel := strings.Cut(inner, "|")
		switch {
		case strings.HasPrefix(body, "@"): // user mention
			if hasLabel {
				return "@" + label
			}
			return "" // bare id (e.g. the triggering @bot) — drop it
		case strings.HasPrefix(body, "#"): // channel mention
			if hasLabel {
				return "#" + label
			}
			return ""
		case strings.HasPrefix(body, "!"): // special mention (@here/@channel/subteam)
			switch body {
			case "!here", "!channel", "!everyone":
				return "@" + body[1:]
			}
			if hasLabel { // <!subteam^S…|name>
				return "@" + label
			}
			return ""
		default: // link: <url|label> or <url>
			if hasLabel {
				return label
			}
			return body
		}
	})
	return htmlUnescapeSlack(out)
}

// htmlUnescapeSlack reverses the three entity escapes Slack applies to
// message text (and only those three — Slack does not escape other HTML
// entities in message bodies). &amp; is last so an already-unescaped &lt;
// isn't produced and then re-read.
func htmlUnescapeSlack(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

// composeMentionTitle builds the enriched entity title the async TitleResolver
// writes once it has resolved the sender's display name and the channel name:
// `Alice said "what can you do?" in #general`. Either name may be empty (its
// lookup failed or was skipped); the frame degrades gracefully — no "said"
// frame without a sender, no "in #…" without a channel. With neither, it
// returns just the humanized message text, identical to the synchronous
// mentionTitle, so a caller can detect "nothing to enrich" and skip the write.
//
// The message text is truncated to whatever budget the fixed frame leaves so
// the "who/where" survives — a long message loses its tail, not its context.
func composeMentionTitle(senderName, channelName, rawText string) string {
	text := strings.Join(strings.Fields(humanizeSlackText(rawText)), " ")

	var prefix, suffix, quote string
	if senderName != "" {
		prefix = senderName + " said "
		quote = `"` // quote the message only inside a "X said …" frame
	}
	if channelName != "" {
		suffix = " in #" + channelName
	}

	budget := mentionTitleMaxRunes - utf8.RuneCountInString(prefix+quote+quote+suffix)
	if budget < 10 {
		budget = 10 // pathological long name/channel — keep a sliver of message
	}
	return truncateRunes(prefix+quote+truncateRunes(text, budget)+quote+suffix, mentionTitleMaxRunes)
}

// truncateRunes caps s at maxRunes codepoints, replacing the last rune with
// an ellipsis when truncation happens — rune-based so a multi-byte
// codepoint is never split.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}

// parseSlackTS converts a Slack timestamp ("1355517523.000005" — seconds
// with a microsecond-resolution fractional suffix) into a time.Time. An
// unparseable ts yields the zero value, matching domain.Event.OccurredAt's
// documented "unknown" contract rather than failing the whole publish over
// a cosmetic timestamp.
func parseSlackTS(ts string) time.Time {
	secStr, fracStr, _ := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var nsec int64
	if fracStr != "" {
		for len(fracStr) < 9 {
			fracStr += "0"
		}
		nsec, err = strconv.ParseInt(fracStr[:9], 10, 64)
		if err != nil {
			return time.Time{}
		}
	}
	return time.Unix(sec, nsec).UTC()
}
