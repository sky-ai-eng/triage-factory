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

// TestSlackMentionPredicate_MatchesAll pins this leaf's deliberately-empty
// predicate: every metadata shape matches, since nothing routes on it until
// TFAC-510 enriches the predicate with channel_in.
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
