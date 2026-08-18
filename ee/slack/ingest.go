package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// slackTitleMaxRunes bounds a Slack entity's title — enough to be
// recognizable in a task list without pulling in a whole message body (that
// stays in the event's metadata_json). Applies to both the thread title
// (composeThreadTitle) and the bot-outbound post title (mentionTitle).
const slackTitleMaxRunes = 120

// slackThreadTitle is the channel-less form of a thread entity's title,
// written synchronously at ingest before the channel name resolves. A Slack
// thread is a live conversation with no inherent name, and a single message's
// text is the wrong grain for a card that outlives it (a thread accrues
// follow-ups, each of which bumps the same task) — so the title names the
// situation, not the opening message, and the async resolver (title.go)
// upgrades it to "<this> in #<channel>" once the channel name is known. The
// per-message content lives on each event's metadata_json; the message count
// rides the task card as a badge.
const slackThreadTitle = "New thread messages"

// composeThreadTitle appends the resolved channel name to slackThreadTitle:
// "New thread messages in #general". An empty channelName (its lookup failed
// or hasn't run) returns the channel-less form unchanged, so a caller can
// detect "nothing to enrich" and skip the redundant write. Capped at
// slackTitleMaxRunes so a pathologically long channel name can't unbound
// the title.
func composeThreadTitle(channelName string) string {
	if channelName == "" {
		return slackThreadTitle
	}
	return truncateRunes(slackThreadTitle+" in #"+channelName, slackTitleMaxRunes)
}

// inboundMention is the transport-neutral shape both the Events API
// receiver (this leaf, webhook.go) and the Socket Mode client (the next
// leaf) parse their payload into before calling handleEventCallback —
// Slack's inner "event" object carries the same fields on both transports.
type inboundMention struct {
	Type string // the inner event's own "type", e.g. "app_mention" or "message"
	// Subtype is the inner message event's "subtype" — empty on an app_mention
	// and on a plain thread reply, "thread_broadcast" on a reply also sent to
	// the channel, and one of Slack's edit/delete/system/bot values otherwise.
	// The engaged-thread branch (handleThreadMessage) admits only "" and
	// "thread_broadcast".
	Subtype  string
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
	// GetBySourceSystem resolves an entity by its natural (source, source_id)
	// key without creating one — the engaged-thread branch's engagement gate
	// (handleThreadMessage) needs to check a thread entity's existence, kind,
	// and state before deciding to ingest a follow-up, never mint one.
	GetBySourceSystem(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error)
}

// ingestPipeline is the transport-neutral core both the Events API receiver
// and (the next leaf) Socket Mode feed through handleEventCallback.
// Constructed once at install with dependencies off ExtensionAPI
// (Stores(), PublishEvent) — see install.go.
type ingestPipeline struct {
	entities   entityFinder
	deliveries slackstore.DeliveryStore
	publish    func(context.Context, domain.Event)
	// identity resolves a message's sender to a TF user — both ingest branches
	// dispatch it, since an engaged-thread follow-up's sender may never appear
	// in an app_mention. Best-effort and detached — see resolveSender's doc.
	// nil-safe: tests and any future caller that doesn't need identity capture
	// simply construct ingestPipeline without it.
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
	// title upgrades a freshly-created thread entity's title into
	// "New thread messages in #<channel>" once the channel name resolves,
	// best-effort and detached — see TitleResolver.dispatch. Nil-safe for the
	// same reason as identity.
	title *TitleResolver
	// stats records each delivery's outcome (stats.go) — the message-volume
	// counters TFAC-650's channel-firehose subscriptions made necessary.
	// Nil-safe for the same reason as identity.
	stats *ingestStats
}

// handleEventCallback is the one function both transports call for an inbound
// Slack event. It dispatches on the inner event type: an explicit @-mention
// (app_mention) mints/re-derives the thread entity and always publishes; an
// un-mentioned message.channels / message.groups delivery is an engaged-thread
// follow-up that publishes only if it lands in a thread the bot already owns.
// Any other type is dropped defensively. Returns an error only for a genuine
// failure (store error, marshal failure); every "nothing to do" case is a
// nil-error early return, logged at debug — none of those are failures the
// caller should retry or 5xx on.
//
// Exactly one outcome is recorded per call — each branch names its return
// path (accepted, or the drop reason), and an error return overrides it as
// outcomeError — so tf_slack_ingest_events_total's sum over outcomes IS the
// received total.
func (p *ingestPipeline) handleEventCallback(ctx context.Context, ws slackstore.Workspace, ev inboundMention) error {
	var outcome string
	var err error
	switch ev.Type {
	case "app_mention":
		outcome, err = p.handleAppMention(ctx, ws, ev)
	case "message":
		outcome, err = p.handleThreadMessage(ctx, ws, ev)
	default:
		slackLog.Debug("dropping unsupported slack event type", "type", ev.Type, "workspace", ws.WorkspaceID)
		outcome = dropUnsupportedType
	}
	if err != nil {
		outcome = outcomeError
	}
	p.stats.recordIngest(ctx, ws.APIAppID, outcome)
	return err
}

// handleAppMention ingests an explicit @-mention. It is deliberately
// synchronous: one dedup insert, one entity find-or-create, one publish — well
// inside Slack's 3-second webhook ack budget. Publishes slack:message with
// Mentioned=true. Returns the delivery's outcome label for
// handleEventCallback's single recordIngest.
func (p *ingestPipeline) handleAppMention(ctx context.Context, ws slackstore.Workspace, ev inboundMention) (string, error) {
	// EventID feeds the dedup key and Channel/TS feed the entity source_id
	// (domain.SlackSourceID) — an empty value in any of them (a malformed
	// or unexpectedly-shaped payload) would dedup unrelated deliveries
	// together or collide entities across channels, so treat it as
	// malformed and drop rather than deriving a garbage key from it.
	if ev.EventID == "" || ev.Channel == "" || ev.TS == "" {
		slackLog.Debug("dropping malformed app_mention: missing event_id/channel/ts", "workspace", ws.WorkspaceID)
		return dropMalformed, nil
	}
	if ev.BotID != "" || (ws.BotUserID != "" && ev.User == ws.BotUserID) {
		slackLog.Debug("dropping self/bot-authored slack mention", "workspace", ws.WorkspaceID)
		return dropSelfOrBot, nil
	}

	fresh, err := p.deliveries.MarkDeliveredSystem(ctx, ws.APIAppID, ev.EventID)
	if err != nil {
		return outcomeError, fmt.Errorf("mark slack delivery: %w", err)
	}
	if !fresh {
		slackLog.Debug("dropping duplicate slack delivery", "workspace", ws.WorkspaceID, "app", ws.APIAppID, "event_id", ev.EventID)
		return dropDuplicate, nil
	}

	occurredAt := parseSlackTS(ev.TS)
	p.captureChannelSighting(ctx, ws, ev.Channel, occurredAt)

	root := ev.ThreadTS
	// kind encodes thread engagement: an empty ThreadTS means this mention
	// IS the thread's root message, so the bot is the reason the thread
	// exists ("thread"). A non-empty ThreadTS is a mid-thread summons into
	// a thread someone else rooted ("message").
	kind := "thread"
	if root != "" {
		kind = "message"
	} else {
		root = ev.TS
	}
	sourceID := domain.SlackSourceID(ev.Channel, root)

	entity, created, err := p.entities.FindOrCreateSystem(ctx, ws.OrgID, "slack", sourceID, kind, slackThreadTitle, "")
	if err != nil {
		return outcomeError, fmt.Errorf("find or create slack entity: %w", err)
	}
	// Only on create: the thread-root permalink is stable once minted, and
	// repeat mentions on an already-known thread shouldn't re-resolve it.
	if created && p.permalink != nil {
		p.permalink.dispatch(ws, entity.ID, ev.Channel, root)
	}
	// Only on create, for the same reason: the channel name is resolved once
	// and folded into the thread title, not re-derived per re-mention.
	if created && p.title != nil {
		p.title.dispatch(ws, entity.ID, ev.Channel)
	}

	if err := p.publishMessage(ctx, ws, ev, entity.ID, true, occurredAt); err != nil {
		return outcomeError, err
	}

	// Best-effort sender identity capture (TFAC-531), detached from the
	// publish above: it must never make a real Slack mention wait on a
	// users.info round-trip, and its own internal timeout bounds the whole
	// chain regardless of this context.Background() being long-lived.
	if p.identity != nil {
		go p.identity.resolveSender(context.Background(), ws, ev.User)
	}

	return outcomeAccepted, nil
}

// handleThreadMessage ingests an engaged-thread follow-up — an un-mentioned
// human message.channels / message.groups delivery inside a thread the bot
// already owns. Every drop reason returns BEFORE MarkDeliveredSystem so
// slack_deliveries stays bounded: the vast majority of channel messages are
// not in an engaged thread, and recording a delivery row for each would let
// the firehose grow the table without bound. Publishes slack:message with
// Mentioned=false — in an engaged thread every message is addressed to the
// bot, the @ being transport detail the thread doesn't require. Returns the
// delivery's outcome label for handleEventCallback's single recordIngest.
func (p *ingestPipeline) handleThreadMessage(ctx context.Context, ws slackstore.Workspace, ev inboundMention) (string, error) {
	// (1) subtype gate: a plain reply ("") or a reply also broadcast to the
	// channel ("thread_broadcast") is a real human message; every other
	// subtype (message_changed, message_deleted, channel_join, bot_message, …)
	// is an edit/system/bot variant this pipeline has no use for.
	if ev.Subtype != "" && ev.Subtype != "thread_broadcast" {
		slackLog.Debug("dropping slack message: unsupported subtype", "subtype", ev.Subtype, "workspace", ws.WorkspaceID)
		return dropUnsupportedSubtype, nil
	}
	// (2) thread reply gate: only a message inside a thread can be an
	// engaged-thread follow-up. Root-channel chatter (no thread_ts) never
	// ingests — the bot listens only to threads it owns.
	if ev.ThreadTS == "" {
		slackLog.Debug("dropping slack message: not a thread reply", "workspace", ws.WorkspaceID)
		return dropNotThreadReply, nil
	}
	// EventID feeds the dedup key, Channel + ThreadTS feed the entity source_id
	// (domain.SlackSourceID); TS feeds the occurred-at. Drop a payload missing
	// any of them rather than deriving a garbage key — same discipline as the
	// app_mention path.
	if ev.EventID == "" || ev.Channel == "" || ev.TS == "" {
		slackLog.Debug("dropping malformed slack message: missing event_id/channel/ts", "workspace", ws.WorkspaceID)
		return dropMalformed, nil
	}
	// (3) bot/self-authored: TF's own thread posts (and any other bot's)
	// arrive here too — never ingest them, or a conversation's own reply would feed
	// itself back in.
	if ev.BotID != "" || (ws.BotUserID != "" && ev.User == ws.BotUserID) {
		slackLog.Debug("dropping self/bot-authored slack message", "workspace", ws.WorkspaceID)
		return dropSelfOrBot, nil
	}
	// (4) explicit-mention dedup: a threaded message that @-mentions the bot
	// ALSO arrives as an app_mention delivery (a distinct event_id), and that
	// delivery owns it. Dropping here keeps a single mention from publishing
	// twice. BotUserID unset (a workspace whose bot id we haven't captured)
	// can't build the token, so this guard is skipped — the app_mention path
	// still owns the mention regardless.
	if ws.BotUserID != "" && strings.Contains(ev.Text, "<@"+ws.BotUserID+">") {
		slackLog.Debug("dropping slack message: explicit mention owned by app_mention delivery", "workspace", ws.WorkspaceID)
		return dropMentionDedup, nil
	}
	// (5) engagement gate: the thread's entity must already exist, be a
	// bot-owned thread (kind="thread", not a mid-thread "message" summons into
	// someone else's thread), and still be active. An unknown thread, someone
	// else's thread, or a closed one is chatter the bot doesn't listen to.
	sourceID := domain.SlackSourceID(ev.Channel, ev.ThreadTS)
	entity, err := p.entities.GetBySourceSystem(ctx, ws.OrgID, "slack", sourceID)
	if err != nil {
		return outcomeError, fmt.Errorf("get slack thread entity: %w", err)
	}
	if entity == nil || entity.Kind != "thread" || entity.State != "active" {
		slackLog.Debug("dropping slack message: no active engaged thread", "workspace", ws.WorkspaceID, "source_id", sourceID)
		return dropNotEngaged, nil
	}

	// (6) Past every drop: record the delivery (dedup Slack's redeliveries)
	// and publish. The delivery insert lands only here so the table tracks
	// exactly the messages TF acts on.
	fresh, err := p.deliveries.MarkDeliveredSystem(ctx, ws.APIAppID, ev.EventID)
	if err != nil {
		return outcomeError, fmt.Errorf("mark slack delivery: %w", err)
	}
	if !fresh {
		slackLog.Debug("dropping duplicate slack delivery", "workspace", ws.WorkspaceID, "app", ws.APIAppID, "event_id", ev.EventID)
		return dropDuplicate, nil
	}

	if err := p.publishMessage(ctx, ws, ev, entity.ID, false, parseSlackTS(ev.TS)); err != nil {
		return outcomeError, err
	}

	// Same best-effort sender identity capture as the app_mention path, for
	// the same reasons (detached, never gating the ack). Follow-up senders
	// are the ones who never type an @-mention — an engaged thread is the
	// only place many participants ever address the bot — so capturing only
	// on app_mention would systematically exclude them from
	// user_slack_identities. resolveSender's resolved-row / negative-cache
	// early exit keeps the per-message cost to one indexed read.
	if p.identity != nil {
		go p.identity.resolveSender(context.Background(), ws, ev.User)
	}
	return outcomeAccepted, nil
}

// publishMessage marshals the durable audit metadata and publishes the
// slack:message event both ingest branches emit — identical but for the
// Mentioned flag (an explicit @-mention vs an engaged-thread follow-up) and
// the resolved entity the event hangs off.
func (p *ingestPipeline) publishMessage(ctx context.Context, ws slackstore.Workspace, ev inboundMention, entityID string, mentioned bool, occurredAt time.Time) error {
	metaJSON, err := json.Marshal(SlackMessageMetadata{
		WorkspaceID: ws.WorkspaceID,
		APIAppID:    ws.APIAppID,
		Channel:     ev.Channel,
		TS:          ev.TS,
		ThreadTS:    ev.ThreadTS,
		SenderID:    ev.User,
		Text:        ev.Text,
		EventID:     ev.EventID,
		Mentioned:   mentioned,
	})
	if err != nil {
		return fmt.Errorf("marshal slack message metadata: %w", err)
	}
	p.publish(ctx, domain.Event{
		OrgID:        ws.OrgID,
		EventType:    domain.EventSlackMessage,
		EntityID:     &entityID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   occurredAt,
	})
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
// collapsed and the result capped at slackTitleMaxRunes.
func mentionTitle(text string) string {
	return truncateRunes(strings.Join(strings.Fields(humanizeSlackText(text)), " "), slackTitleMaxRunes)
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
