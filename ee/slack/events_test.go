package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
)

// TestSlackMessageSchema_Registered pins the registration this package's
// init() performs: slack:message has a schema, it declares OwnershipOwned
// (channel-primary owns; watch semantics apply), and
// routing.EventSupportsWatch derives true from that — the "watch" toggle
// only makes sense for owner-ladder events.
func TestSlackMessageSchema_Registered(t *testing.T) {
	sc, ok := events.Get(domain.EventSlackMessage)
	if !ok {
		t.Fatal("events.Get(slack:message) = not found; want the schema registered by ee/slack's init()")
	}
	if sc.Ownership != events.OwnershipOwned {
		t.Errorf("Ownership = %v; want OwnershipOwned", sc.Ownership)
	}
	if !routing.EventSupportsWatch(domain.EventSlackMessage) {
		t.Error("EventSupportsWatch(slack:message) = false; want true (OwnershipOwned events support the watch flag)")
	}
}

// TestSlackMessagePredicate_MatchesAll pins the empty-predicate case: an
// empty (or absent) channel_in list means "no filter," matching everything
// — the *_in convention shared with GitHubPRCICheckFailedPredicate.AuthorIDIn.
func TestSlackMessagePredicate_MatchesAll(t *testing.T) {
	sc, ok := events.Get(domain.EventSlackMessage)
	if !ok {
		t.Fatal("events.Get(slack:message) = not found")
	}
	meta := SlackMessageMetadata{WorkspaceID: "T123", Channel: "C1", TS: "1.0", SenderID: "U1", Text: "hi", EventID: "Ev1"}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	got, err := sc.Match("", string(metaJSON))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !got {
		t.Error("empty predicate did not match; want match-all")
	}
	got, err = sc.Match(`{}`, string(metaJSON))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !got {
		t.Error("{} predicate did not match; want match-all")
	}
}

// TestSlackMessagePredicate_ChannelIn covers channel_in's membership check:
// it names channels by registry row, so a message matches on its
// channel_row_id, never on the Slack channel id it arrived under.
func TestSlackMessagePredicate_ChannelIn(t *testing.T) {
	const row = "6f1c2a4e-0000-4000-8000-000000000123"
	cases := []struct {
		name string
		pred SlackMessagePredicate
		want bool
	}{
		{"empty list matches any channel", SlackMessagePredicate{}, true},
		{"member channel matches", SlackMessagePredicate{ChannelIn: []string{"6f1c2a4e-0000-4000-8000-000000000999", row}}, true},
		{"non-member channel does not match", SlackMessagePredicate{ChannelIn: []string{"6f1c2a4e-0000-4000-8000-000000000999"}}, false},
		{"the Slack channel id does not match", SlackMessagePredicate{ChannelIn: []string{"C123"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := SlackMessageMetadata{Channel: "C123", ChannelRowID: row}
			if got := tc.pred.Matches(meta); got != tc.want {
				t.Errorf("Matches() = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestSlackMessagePredicate_MentionedOnly covers the MentionedOnly
// narrowing flag: unset (false) matches both mentioned and non-mentioned
// messages, set (true) requires Mentioned=true on the metadata.
func TestSlackMessagePredicate_MentionedOnly(t *testing.T) {
	cases := []struct {
		name      string
		pred      SlackMessagePredicate
		mentioned bool
		want      bool
	}{
		{"unset matches a mention", SlackMessagePredicate{}, true, true},
		{"unset matches a non-mention follow-up", SlackMessagePredicate{}, false, true},
		{"set matches a mention", SlackMessagePredicate{MentionedOnly: true}, true, true},
		{"set excludes a non-mention follow-up", SlackMessagePredicate{MentionedOnly: true}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := SlackMessageMetadata{Channel: "C123", Mentioned: tc.mentioned}
			if got := tc.pred.Matches(meta); got != tc.want {
				t.Errorf("Matches() = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestSlackMessageSource_GatedOnFeatureSlack pins the dormancy wiring this
// leaf's init() performs: every slack:* event type requires FeatureSlack.
func TestSlackMessageSource_GatedOnFeatureSlack(t *testing.T) {
	// GateEventSource is idempotent (a plain map write). Sibling pgtest-backed
	// tests in this package call entitlements.Reset() via t.Cleanup, which —
	// per its doc — also clears every registered event-source gate, not just
	// the stub provider those tests install; re-asserting here keeps this
	// test correct regardless of execution order relative to those.
	entitlements.GateEventSource("slack", entitlements.FeatureSlack)

	f, gated := entitlements.FeatureForEventType(domain.EventSlackMessage)
	if !gated {
		t.Fatal("slack:message is not gated; want entitlements.GateEventSource(\"slack\", FeatureSlack) from init()")
	}
	if f != entitlements.FeatureSlack {
		t.Errorf("gating feature = %q; want %q", f, entitlements.FeatureSlack)
	}
}

// TestSlackMessageCurrentView: the view hands back metadata that already
// names its channel as it is byte for byte, and metadata naming a channel TF
// holds no row for as recorded; it rewrites the channel and its row of one
// that moved, and surfaces a failed read. Registered, it is what lets a
// handler filter on the channel's row match a message recorded under the old
// id — including one recorded with a row a later move absorbed.
func TestSlackMessageCurrentView(t *testing.T) {
	channels := newFakeChannelRegistry()
	c9 := channels.register("org-1", "", "C9")
	channel := channels.register("org-1", "", "G1")
	if _, err := channels.MoveSystem(context.Background(), "org-1", "G1", "C1"); err != nil {
		t.Fatalf("move: %v", err)
	}
	view := slackMessageCurrentView(channels)
	ctx := context.Background()

	unmoved := `{"workspace_id":"T1","channel":"C9","channel_row_id":"` + c9.ID + `","ts":"1.0","mentioned":true}`
	if got, err := view(ctx, "org-1", unmoved); err != nil || got != unmoved {
		t.Errorf("view(unmoved) = %q, %v; want the metadata as recorded", got, err)
	}
	unknown := `{"workspace_id":"T1","channel":"C404","ts":"1.0","mentioned":true}`
	if got, err := view(ctx, "org-1", unknown); err != nil || got != unknown {
		t.Errorf("view(unregistered channel) = %q, %v; want the metadata as recorded", got, err)
	}

	recorded := `{"workspace_id":"T1","api_app_id":"A1","channel":"G1","channel_row_id":"row-absorbed","ts":"2.0","thread_ts":"1.0","sender_id":"U1","text":"hi","event_id":"Ev1","mentioned":true}`
	got, err := view(ctx, "org-1", recorded)
	if err != nil {
		t.Fatalf("view(moved): %v", err)
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte(got), &meta); err != nil {
		t.Fatalf("decode view output: %v", err)
	}
	want := SlackMessageMetadata{WorkspaceID: "T1", APIAppID: "A1", Channel: "C1", ChannelRowID: channel.ID, TS: "2.0", ThreadTS: "1.0", SenderID: "U1", Text: "hi", EventID: "Ev1", Mentioned: true}
	if meta != want {
		t.Errorf("view(moved) = %+v, want %+v", meta, want)
	}

	if got, err := view(ctx, "org-2", recorded); err != nil || got != recorded {
		t.Errorf("view in another org = %q, %v; want the metadata as recorded", got, err)
	}

	channels.resolveErr = errors.New("db down")
	if _, err := view(ctx, "org-1", recorded); err == nil {
		t.Error("view with a failed read = nil error; want the read's error")
	}
	channels.resolveErr = nil

	events.RegisterCurrentView(domain.EventSlackMessage, view)
	t.Cleanup(func() { events.ResetCurrentView(domain.EventSlackMessage) })
	schema, _ := events.Get(domain.EventSlackMessage)
	current, err := events.Current(ctx, "org-1", domain.EventSlackMessage, recorded)
	if err != nil {
		t.Fatalf("events.Current: %v", err)
	}
	if matched, err := schema.Match(`{"channel_in":["`+channel.ID+`"]}`, current); err != nil || !matched {
		t.Errorf("filter on the channel's row against the recorded message = %v, %v; want a match", matched, err)
	}
}
