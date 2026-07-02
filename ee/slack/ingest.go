package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

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

	root := ev.ThreadTS
	if root == "" {
		root = ev.TS
	}
	sourceID := domain.SlackSourceID(ev.Channel, root)

	entity, _, err := p.entities.FindOrCreateSystem(ctx, ws.OrgID, "slack", sourceID, "message", mentionTitle(ev.Text), "")
	if err != nil {
		return fmt.Errorf("find or create slack entity: %w", err)
	}

	metaJSON, err := json.Marshal(SlackMentionMetadata{
		WorkspaceID: ws.WorkspaceID,
		Channel:     ev.Channel,
		TS:          ev.TS,
		ThreadTS:    ev.ThreadTS,
		SenderID:    ev.User,
		Text:        ev.Text,
		EventID:     ev.EventID,
	})
	if err != nil {
		return fmt.Errorf("marshal slack mention metadata: %w", err)
	}

	p.publish(domain.Event{
		OrgID:        ws.OrgID,
		EventType:    domain.EventSlackMention,
		EntityID:     &entity.ID,
		MetadataJSON: string(metaJSON),
		OccurredAt:   parseSlackTS(ev.TS),
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

// mentionTitle derives the entity title from a mention's raw text: every
// run of whitespace (including embedded newlines) collapsed to a single
// space, then capped at mentionTitleMaxRunes.
func mentionTitle(text string) string {
	return truncateRunes(strings.Join(strings.Fields(text), " "), mentionTitleMaxRunes)
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
