package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fakeEntities is a minimal entityFinder fake: one (org, source, sourceID)
// maps to one entity, created on first sight — mirrors FindOrCreateSystem's
// real contract closely enough for the pipeline tests.
type fakeEntities struct {
	byKey map[string]*domain.Entity
	err   error
}

func newFakeEntities() *fakeEntities { return &fakeEntities{byKey: map[string]*domain.Entity{}} }

func (f *fakeEntities) FindOrCreateSystem(_ context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	key := orgID + "/" + source + "/" + sourceID
	if e, ok := f.byKey[key]; ok {
		return e, false, nil
	}
	e := &domain.Entity{ID: "entity-" + key, Source: source, SourceID: sourceID, Kind: kind, Title: title, URL: url, State: "active"}
	f.byKey[key] = e
	return e, true, nil
}

// fakeDeliveries is a minimal slackstore.DeliveryStore fake: an in-memory
// set of (workspaceID, eventID) pairs already seen.
type fakeDeliveries struct {
	seen map[string]bool
	err  error
}

func newFakeDeliveries() *fakeDeliveries { return &fakeDeliveries{seen: map[string]bool{}} }

func (f *fakeDeliveries) MarkDeliveredSystem(_ context.Context, workspaceID, eventID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	key := workspaceID + "/" + eventID
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	return true, nil
}

var _ slackstore.DeliveryStore = (*fakeDeliveries)(nil)

// testWorkspaceRow is a small helper for a slackstore.Workspace fixture.
func testWorkspaceRow(orgID string) slackstore.Workspace {
	return slackstore.Workspace{WorkspaceID: "T0PIPE001", OrgID: orgID, BotUserID: "U0BOT"}
}

func newTestPipeline() (*ingestPipeline, *fakeEntities, *fakeDeliveries, *[]domain.Event) {
	entities := newFakeEntities()
	deliveries := newFakeDeliveries()
	published := &[]domain.Event{}
	p := &ingestPipeline{
		entities:   entities,
		deliveries: deliveries,
		publish:    func(evt domain.Event) { *published = append(*published, evt) },
	}
	return p, entities, deliveries, published
}

// TestHandleEventCallback_PublishesCorrectEventShape covers the happy path:
// EventType/EntityID set, metadata round-trips into SlackMentionMetadata.
func TestHandleEventCallback_PublishesCorrectEventShape(t *testing.T) {
	p, _, _, published := newTestPipeline()
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{
		Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1",
		Text: "hey <@BOT> help", TS: "1600000000.000100", ThreadTS: "1599999999.000001",
	}
	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(*published) != 1 {
		t.Fatalf("published %d events; want 1", len(*published))
	}
	got := (*published)[0]
	if got.OrgID != "org-1" {
		t.Errorf("OrgID = %q; want org-1", got.OrgID)
	}
	if got.EventType != domain.EventSlackMention {
		t.Errorf("EventType = %q; want %q", got.EventType, domain.EventSlackMention)
	}
	if got.EntityID == nil || *got.EntityID == "" {
		t.Fatal("EntityID is nil/empty; want the resolved entity id (router's routableEntity drops nil-entity events)")
	}
	var meta SlackMentionMetadata
	if err := json.Unmarshal([]byte(got.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata did not round-trip into SlackMentionMetadata: %v", err)
	}
	if meta.WorkspaceID != "T0PIPE001" || meta.Channel != "C1" || meta.SenderID != "U1" || meta.EventID != "Ev1" || meta.ThreadTS != "1599999999.000001" {
		t.Errorf("metadata = %+v; want fields carried over from ev/ws", meta)
	}
}

// TestHandleEventCallback_ThreadRootVsRootMessage pins the thread-root
// resolution rule: thread_ts when present, else the message's own ts.
func TestHandleEventCallback_ThreadRootVsRootMessage(t *testing.T) {
	cases := []struct {
		name         string
		ts, threadTS string
		wantSourceID string
	}{
		{"threaded mention uses thread_ts", "1600000000.000100", "1599999999.000001", "C1/1599999999.000001"},
		{"root-message mention uses its own ts", "1600000000.000100", "", "C1/1600000000.000100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, entities, _, _ := newTestPipeline()
			ws := testWorkspaceRow("org-1")
			ev := inboundMention{Type: "app_mention", EventID: "Ev-" + tc.name, Channel: "C1", User: "U1", TS: tc.ts, ThreadTS: tc.threadTS}
			if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
				t.Fatalf("handleEventCallback: %v", err)
			}
			key := "org-1/slack/" + tc.wantSourceID
			if _, ok := entities.byKey[key]; !ok {
				t.Errorf("no entity created under source_id %q; entities = %v", tc.wantSourceID, entities.byKey)
			}
		})
	}
}

// TestHandleEventCallback_DropsSelfAndBotMentions: the loop guard drops a
// mention whose sender is the workspace's own bot user id, or whose payload
// carries a bot_id — neither case publishes.
func TestHandleEventCallback_DropsSelfAndBotMentions(t *testing.T) {
	cases := []struct {
		name string
		ev   inboundMention
	}{
		{"sender is the workspace bot", inboundMention{Type: "app_mention", EventID: "Ev1", User: "U0BOT", Channel: "C1", TS: "1.0"}},
		{"payload carries bot_id", inboundMention{Type: "app_mention", EventID: "Ev2", User: "U1", BotID: "B1", Channel: "C1", TS: "1.0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _, published := newTestPipeline()
			ws := testWorkspaceRow("org-1")
			if err := p.handleEventCallback(context.Background(), ws, tc.ev); err != nil {
				t.Fatalf("handleEventCallback: %v", err)
			}
			if len(*published) != 0 {
				t.Errorf("published %d events; want 0 (self/bot mention dropped)", len(*published))
			}
		})
	}
}

// TestHandleEventCallback_NonAppMentionDropped: anything other than
// app_mention is dropped defensively, even though the manifest subscribes
// to nothing else.
func TestHandleEventCallback_NonAppMentionDropped(t *testing.T) {
	p, _, _, published := newTestPipeline()
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "message", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1.0"}
	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(*published) != 0 {
		t.Errorf("published %d events; want 0 (non-app_mention dropped)", len(*published))
	}
}

// TestHandleEventCallback_DuplicateEventIDPublishedOnce: a redelivered
// event_id (Slack retry) is a no-op the second time, per the dedup table.
func TestHandleEventCallback_DuplicateEventIDPublishedOnce(t *testing.T) {
	p, _, _, published := newTestPipeline()
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev-dup", Channel: "C1", User: "U1", TS: "1.0"}
	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if len(*published) != 1 {
		t.Errorf("published %d events across 2 identical deliveries; want exactly 1", len(*published))
	}
}

// TestHandleEventCallback_DeliveryStoreError propagates a dedup-store
// failure rather than silently dropping (which would look identical to a
// legitimate duplicate).
func TestHandleEventCallback_DeliveryStoreError(t *testing.T) {
	p, _, deliveries, published := newTestPipeline()
	deliveries.err = errors.New("boom")
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1.0"}
	if err := p.handleEventCallback(context.Background(), ws, ev); err == nil {
		t.Fatal("expected an error from a failing delivery store, got nil")
	}
	if len(*published) != 0 {
		t.Errorf("published %d events on a dedup-store error; want 0", len(*published))
	}
}

// TestParseSlackTS covers the fractional-second parsing, including the
// unparseable-input "unknown" contract.
func TestParseSlackTS(t *testing.T) {
	cases := []struct {
		name string
		ts   string
		want time.Time
	}{
		{"whole seconds", "1600000000", time.Unix(1600000000, 0).UTC()},
		{"microsecond fraction", "1600000000.000100", time.Unix(1600000000, 100000).UTC()},
		{"empty is unknown", "", time.Time{}},
		{"garbage is unknown", "not-a-ts", time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSlackTS(tc.ts)
			if !got.Equal(tc.want) {
				t.Errorf("parseSlackTS(%q) = %v; want %v", tc.ts, got, tc.want)
			}
		})
	}
}

// TestMentionTitle covers the single-line-sanitize + truncate contract.
func TestMentionTitle(t *testing.T) {
	if got := mentionTitle("hello\nworld   spaced"); got != "hello world spaced" {
		t.Errorf("mentionTitle = %q; want collapsed single line", got)
	}
	long := ""
	for i := 0; i < 200; i++ {
		long += "a"
	}
	got := mentionTitle(long)
	if r := []rune(got); len(r) != mentionTitleMaxRunes {
		t.Errorf("mentionTitle length = %d runes; want %d", len(r), mentionTitleMaxRunes)
	}
}
