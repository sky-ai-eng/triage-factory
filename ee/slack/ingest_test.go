package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
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

// fakeChannelRegistry is a minimal slackstore.ChannelRegistryStore fake: an
// in-memory map keyed (orgID, channelID), tracking every UpsertSightingSystem
// call so tests can assert dedup (dropped-before-sighting) behavior.
type fakeChannelRegistry struct {
	mu   sync.Mutex
	rows map[string]*slackstore.Channel

	upsertErr error
	sawUpsert []string // channelKey values, one per UpsertSightingSystem call

	// done, if non-nil, receives whenever SetNameSystem completes — the
	// same deterministic "the detached resolver goroutine finished" signal
	// fakeIdentityStore.done provides in identity_test.go.
	done chan struct{}
}

func newFakeChannelRegistry() *fakeChannelRegistry {
	return &fakeChannelRegistry{rows: map[string]*slackstore.Channel{}}
}

func channelKey(orgID, channelID string) string { return orgID + "/" + channelID }

func (f *fakeChannelRegistry) UpsertSightingSystem(_ context.Context, orgID, workspaceID, channelID string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := channelKey(orgID, channelID)
	f.sawUpsert = append(f.sawUpsert, key)
	if f.upsertErr != nil {
		return false, f.upsertErr
	}
	if row, ok := f.rows[key]; ok {
		row.WorkspaceID = workspaceID
		lastMention := at
		row.LastMentionAt = &lastMention
		return false, nil
	}
	firstSeen := at
	f.rows[key] = &slackstore.Channel{OrgID: orgID, ChannelID: channelID, WorkspaceID: workspaceID, FirstSeenAt: at, LastMentionAt: &firstSeen}
	return true, nil
}

func (f *fakeChannelRegistry) EnsureSystem(context.Context, string, string, string, string) error {
	return nil
}

func (f *fakeChannelRegistry) SetNameSystem(_ context.Context, orgID, channelID, name string) error {
	f.mu.Lock()
	if row, ok := f.rows[channelKey(orgID, channelID)]; ok {
		row.Name = name
		resolvedAt := time.Now()
		row.NameResolvedAt = &resolvedAt
	}
	f.mu.Unlock()
	if f.done != nil {
		f.done <- struct{}{}
	}
	return nil
}

func (f *fakeChannelRegistry) GetSystem(_ context.Context, orgID, channelID string) (*slackstore.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[channelKey(orgID, channelID)], nil
}

func (f *fakeChannelRegistry) ListForOrg(context.Context, string) ([]slackstore.Channel, error) {
	return nil, nil
}

var _ slackstore.ChannelRegistryStore = (*fakeChannelRegistry)(nil)

// testWorkspaceRow is a small helper for a slackstore.Workspace fixture.
func testWorkspaceRow(orgID string) slackstore.Workspace {
	return slackstore.Workspace{WorkspaceID: "T0PIPE001", APIAppID: "A0PIPE001", OrgID: orgID, BotUserID: "U0BOT"}
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
// EventType/EntityID set, metadata round-trips into SlackMessageMetadata.
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
	if got.EventType != domain.EventSlackMessage {
		t.Errorf("EventType = %q; want %q", got.EventType, domain.EventSlackMessage)
	}
	if got.EntityID == nil || *got.EntityID == "" {
		t.Fatal("EntityID is nil/empty; want the resolved entity id (router's routableEntity drops nil-entity events)")
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte(got.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata did not round-trip into SlackMessageMetadata: %v", err)
	}
	if meta.WorkspaceID != "T0PIPE001" || meta.APIAppID != "A0PIPE001" || meta.Channel != "C1" || meta.SenderID != "U1" || meta.EventID != "Ev1" || meta.ThreadTS != "1599999999.000001" {
		t.Errorf("metadata = %+v; want fields carried over from ev/ws", meta)
	}
	if !meta.Mentioned {
		t.Error("Mentioned = false; want true (every app_mention delivery is an explicit @-mention)")
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

// TestHandleEventCallback_MalformedFieldsDropped: an app_mention missing
// event_id, channel, or ts is dropped rather than flowing an empty string
// into the dedup key or the entity source_id — an empty event_id would
// dedup every malformed delivery from a workspace together, and an empty
// channel/ts would collide entities across otherwise-unrelated mentions.
func TestHandleEventCallback_MalformedFieldsDropped(t *testing.T) {
	cases := []struct {
		name string
		ev   inboundMention
	}{
		{"missing event_id", inboundMention{Type: "app_mention", Channel: "C1", User: "U1", TS: "1.0"}},
		{"missing channel", inboundMention{Type: "app_mention", EventID: "Ev1", User: "U1", TS: "1.0"}},
		{"missing ts", inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, entities, deliveries, published := newTestPipeline()
			ws := testWorkspaceRow("org-1")
			if err := p.handleEventCallback(context.Background(), ws, tc.ev); err != nil {
				t.Fatalf("handleEventCallback: %v", err)
			}
			if len(*published) != 0 {
				t.Errorf("published %d events for a malformed mention; want 0", len(*published))
			}
			if len(entities.byKey) != 0 {
				t.Errorf("created %d entities for a malformed mention; want 0", len(entities.byKey))
			}
			if len(deliveries.seen) != 0 {
				t.Errorf("recorded %d deliveries for a malformed mention; want 0 (dropped before dedup)", len(deliveries.seen))
			}
		})
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

// TestHandleEventCallback_DispatchesIdentityResolution is the TFAC-531 call-
// site wiring test: a fresh app_mention both publishes its event AND
// dispatches resolveSender for ev.User (the mentioning human's Slack user
// id) against ws — the one line deferred from TFAC-531 until this ticket
// (TFAC-530) merged. Runs the real IdentityResolver against fakes rather
// than a nil identity (every other test in this file), so this is an
// end-to-end check of the wiring itself; resolveSender's own branch logic
// is covered exhaustively in identity_test.go.
func TestHandleEventCallback_DispatchesIdentityResolution(t *testing.T) {
	client, hits := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": false,
			"profile": map[string]any{"email": "ada@example.com", "real_name": "Ada Lovelace"},
		},
	})
	identities := newFakeIdentityStore()
	identities.done = make(chan struct{}, 1)
	resolver := &IdentityResolver{
		secrets:    &fakeSecrets{token: "xoxb-test"},
		users:      &fakeUsers{ids: []string{"user-1"}},
		identities: identities,
		client:     client,
	}
	p := &ingestPipeline{
		entities:   newFakeEntities(),
		deliveries: newFakeDeliveries(),
		publish:    func(domain.Event) {},
		identity:   resolver,
	}
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{
		Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U0SENDER1",
		Text: "hey <@BOT>", TS: "1600000000.000100",
	}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}

	select {
	case <-identities.done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolveSender never completed — handleEventCallback did not dispatch it")
	}

	if *hits != 1 {
		t.Errorf("users.info hits = %d; want 1 (resolveSender should have called it)", *hits)
	}
	row := identities.rows[identityKey(ws.WorkspaceID, "U0SENDER1")]
	if row == nil || row.UserID == nil || *row.UserID != "user-1" {
		t.Fatalf("row = %+v; want resolved to user-1", row)
	}
}

// TestHandleEventCallback_ChannelSightingUpsertedOnFreshDelivery pins the
// TFAC-542 ingest-time capture: a fresh delivery records exactly one
// sighting for its channel.
func TestHandleEventCallback_ChannelSightingUpsertedOnFreshDelivery(t *testing.T) {
	p, _, _, _ := newTestPipeline()
	channels := newFakeChannelRegistry()
	p.channels = channels
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1600000000.000100"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(channels.sawUpsert) != 1 {
		t.Fatalf("sighting upserts = %d; want 1", len(channels.sawUpsert))
	}
	if row := channels.rows[channelKey("org-1", "C1")]; row == nil {
		t.Fatal("no sighting row recorded")
	}
}

// TestHandleEventCallback_ChannelSightingNotUpsertedOnDuplicate pins that a
// redelivered event_id never reaches sighting capture — it's dropped by the
// dedup check first, same as it never reaches entity creation or publish.
func TestHandleEventCallback_ChannelSightingNotUpsertedOnDuplicate(t *testing.T) {
	p, _, _, _ := newTestPipeline()
	channels := newFakeChannelRegistry()
	p.channels = channels
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev-dup", Channel: "C1", User: "U1", TS: "1.0"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if len(channels.sawUpsert) != 1 {
		t.Errorf("sighting upserts across 2 identical deliveries = %d; want 1 (dropped before sighting capture)", len(channels.sawUpsert))
	}
}

// TestHandleEventCallback_ChannelSightingUpsertErrorDoesNotFailPipeline pins
// the best-effort contract: a sighting-registry hiccup must never 5xx the
// webhook or withhold the mention's own publish.
func TestHandleEventCallback_ChannelSightingUpsertErrorDoesNotFailPipeline(t *testing.T) {
	p, _, _, published := newTestPipeline()
	channels := newFakeChannelRegistry()
	channels.upsertErr = errors.New("boom")
	p.channels = channels
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1.0"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v (a sighting-store error must not fail the pipeline)", err)
	}
	if len(*published) != 1 {
		t.Errorf("published %d events; want 1 (mention still publishes despite sighting error)", len(*published))
	}
}

// TestHandleEventCallback_DispatchesChannelNameResolutionOnCreated is the
// TFAC-542 call-site wiring test for channel name resolution: a fresh
// (first-sighted) channel dispatches resolveChannelName, which writes the
// resolved name back into the registry. Runs a real ChannelResolver against
// fakes rather than a nil channelName (every other test in this file), so
// this is an end-to-end check of the wiring itself — mirrors
// TestHandleEventCallback_DispatchesIdentityResolution's shape.
func TestHandleEventCallback_DispatchesChannelNameResolutionOnCreated(t *testing.T) {
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": map[string]any{"name": "general"},
		})
	})
	channels := newFakeChannelRegistry()
	channels.done = make(chan struct{}, 1)
	resolver := &ChannelResolver{secrets: &fakeSecrets{token: "xoxb-test"}, channels: channels, client: srv.Client()}
	p := &ingestPipeline{
		entities:    newFakeEntities(),
		deliveries:  newFakeDeliveries(),
		channels:    channels,
		publish:     func(domain.Event) {},
		channelName: resolver,
	}
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1600000000.000100"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}

	select {
	case <-channels.done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolveChannelName never completed — handleEventCallback did not dispatch it")
	}

	if hits != 1 {
		t.Errorf("conversations.info hits = %d; want 1 (resolveChannelName should have called it)", hits)
	}
	row := channels.rows[channelKey("org-1", "C1")]
	if row == nil || row.Name != "general" {
		t.Fatalf("row = %+v; want name resolved to general", row)
	}
}

// TestHandleEventCallback_DispatchesPermalinkResolutionOnCreated is the
// TFAC-595 call-site wiring test for permalink enrichment: a fresh
// (first-sighted) thread entity dispatches resolvePermalink, which writes
// the resolved permalink into the entity's url. Runs a real
// PermalinkResolver against fakes rather than a nil permalink (every other
// test in this file), so this is an end-to-end check of the wiring itself
// — mirrors TestHandleEventCallback_DispatchesChannelNameResolutionOnCreated's
// shape.
func TestHandleEventCallback_DispatchesPermalinkResolutionOnCreated(t *testing.T) {
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.URL.Query().Get("channel"); got != "C1" {
			t.Errorf("channel query param = %q; want C1", got)
		}
		if got := r.URL.Query().Get("message_ts"); got != "1600000000.000100" {
			t.Errorf("message_ts query param = %q; want the thread root ts", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1600000000000100",
		})
	})
	entities := newFakeEntities()
	entityURLs := newFakeEntityURLUpdater()
	entityURLs.done = make(chan struct{}, 1)
	resolver := &PermalinkResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: entityURLs, client: srv.Client()}
	p := &ingestPipeline{
		entities:   entities,
		deliveries: newFakeDeliveries(),
		publish:    func(domain.Event) {},
		permalink:  resolver,
	}
	ws := testWorkspaceRow("org-1")
	// Root-message mention (no ThreadTS): the resolved thread root is the
	// message's own ts, "1600000000.000100".
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1600000000.000100"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}

	select {
	case <-entityURLs.done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolvePermalink never completed — handleEventCallback did not dispatch it")
	}

	if hits != 1 {
		t.Errorf("chat.getPermalink hits = %d; want 1 (resolvePermalink should have called it)", hits)
	}
	entityID := "entity-org-1/slack/C1/1600000000.000100"
	if got := entityURLs.get(entityID); got != "https://acme.slack.com/archives/C1/p1600000000000100" {
		t.Errorf("url = %q; want the resolved permalink", got)
	}
}

// TestHandleEventCallback_DoesNotDispatchPermalinkResolutionOnReMention pins
// the "only on create" rule: a second mention on an already-known thread
// (created=false) must not re-dispatch resolvePermalink — the thread-root
// permalink is stable once minted.
func TestHandleEventCallback_DoesNotDispatchPermalinkResolutionOnReMention(t *testing.T) {
	var hits int32
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1"})
	})
	entities := newFakeEntities()
	entityURLs := newFakeEntityURLUpdater()
	entityURLs.done = make(chan struct{}, 1)
	resolver := &PermalinkResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: entityURLs, client: srv.Client()}
	p := &ingestPipeline{
		entities:   entities,
		deliveries: newFakeDeliveries(),
		publish:    func(domain.Event) {},
		permalink:  resolver,
	}
	ws := testWorkspaceRow("org-1")

	// First mention creates the thread entity and dispatches resolution.
	first := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1600000000.000100"}
	if err := p.handleEventCallback(context.Background(), ws, first); err != nil {
		t.Fatalf("first handleEventCallback: %v", err)
	}
	// Wait for the first dispatch to fully complete via the resolver's own
	// deterministic completion signal — NOT a sleep/poll on shared state
	// (which would itself race the detached goroutine, and risks a
	// still-running goroutine reading slackAPIBase out from under
	// withFakeSlackAPI's t.Cleanup restore once this test returns).
	select {
	case <-entityURLs.done:
	case <-time.After(2 * time.Second):
		t.Fatal("first mention never dispatched resolvePermalink")
	}

	// Second mention on the SAME thread (same channel/thread_ts) re-resolves
	// the same entity (created=false), so handleEventCallback never calls
	// dispatch again — no new goroutine, nothing further to wait for before
	// checking hits below.
	second := inboundMention{Type: "app_mention", EventID: "Ev2", Channel: "C1", User: "U2", TS: "1600000000.000200", ThreadTS: "1600000000.000100"}
	if err := p.handleEventCallback(context.Background(), ws, second); err != nil {
		t.Fatalf("second handleEventCallback: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("chat.getPermalink hits = %d; want 1 (no re-dispatch on re-mention)", got)
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
		{"garbage fraction is unknown", "1600000000.not-a-fraction", time.Time{}},
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
