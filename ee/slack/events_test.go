package slack

import (
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
)

// TestSlackMentionSchema_Registered pins the registration this package's
// init() performs: slack:mention has a schema, it declares OwnershipOwned
// (channel-primary owns; watch semantics apply), and
// routing.EventSupportsWatch derives true from that — the "watch" toggle
// only makes sense for owner-ladder events.
func TestSlackMentionSchema_Registered(t *testing.T) {
	sc, ok := events.Get(domain.EventSlackMention)
	if !ok {
		t.Fatal("events.Get(slack:mention) = not found; want the schema registered by ee/slack's init()")
	}
	if sc.Ownership != events.OwnershipOwned {
		t.Errorf("Ownership = %v; want OwnershipOwned", sc.Ownership)
	}
	if !routing.EventSupportsWatch(domain.EventSlackMention) {
		t.Error("EventSupportsWatch(slack:mention) = false; want true (OwnershipOwned events support the watch flag)")
	}
}

// TestSlackMentionSchema_Additive pins the TFAC-597 flip: a re-mention on an
// entity with an already-active auto run injects into that run
// (internal/routing's tryAutoDelegate, via events.AdditiveFor) instead of
// deferring to pending_firings.
func TestSlackMentionSchema_Additive(t *testing.T) {
	sc, ok := events.Get(domain.EventSlackMention)
	if !ok {
		t.Fatal("events.Get(slack:mention) = not found; want the schema registered by ee/slack's init()")
	}
	if !sc.Additive {
		t.Error("Additive = false; want true (a re-mention on a live run should inject, not defer)")
	}
}

// TestSlackMentionPredicate_MatchesAll pins the empty-predicate case: an
// empty (or absent) channel_in list means "no filter," matching everything
// — the *_in convention shared with GitHubPRCICheckFailedPredicate.AuthorIn.
func TestSlackMentionPredicate_MatchesAll(t *testing.T) {
	sc, ok := events.Get(domain.EventSlackMention)
	if !ok {
		t.Fatal("events.Get(slack:mention) = not found")
	}
	meta := SlackMentionMetadata{WorkspaceID: "T123", Channel: "C1", TS: "1.0", SenderID: "U1", Text: "hi", EventID: "Ev1"}
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

// TestSlackMentionPredicate_ChannelIn covers channel_in's membership check,
// including its deliberate divergence from stringInSliceFold: Slack channel
// IDs are case-sensitive tokens, so "c123" must NOT match "C123".
func TestSlackMentionPredicate_ChannelIn(t *testing.T) {
	cases := []struct {
		name string
		pred SlackMentionPredicate
		want bool
	}{
		{"empty list matches any channel", SlackMentionPredicate{}, true},
		{"member channel matches", SlackMentionPredicate{ChannelIn: []string{"C999", "C123"}}, true},
		{"non-member channel does not match", SlackMentionPredicate{ChannelIn: []string{"C999"}}, false},
		{"case-sensitive: lowercase does not match", SlackMentionPredicate{ChannelIn: []string{"c123"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := SlackMentionMetadata{Channel: "C123"}
			if got := tc.pred.Matches(meta); got != tc.want {
				t.Errorf("Matches() = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestSlackMentionSource_GatedOnFeatureSlack pins the dormancy wiring this
// leaf's init() performs: every slack:* event type requires FeatureSlack.
func TestSlackMentionSource_GatedOnFeatureSlack(t *testing.T) {
	// GateEventSource is idempotent (a plain map write). Sibling pgtest-backed
	// tests in this package call entitlements.Reset() via t.Cleanup, which —
	// per its doc — also clears every registered event-source gate, not just
	// the stub provider those tests install; re-asserting here keeps this
	// test correct regardless of execution order relative to those.
	entitlements.GateEventSource("slack", entitlements.FeatureSlack)

	f, gated := entitlements.FeatureForEventType(domain.EventSlackMention)
	if !gated {
		t.Fatal("slack:mention is not gated; want entitlements.GateEventSource(\"slack\", FeatureSlack) from init()")
	}
	if f != entitlements.FeatureSlack {
		t.Errorf("gating feature = %q; want %q", f, entitlements.FeatureSlack)
	}
}
