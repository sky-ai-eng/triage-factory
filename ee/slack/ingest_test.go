package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fakeEntities holds the thread entities a fakeChannelRegistry linked to it
// (newFakeChannelRegistryFor) creates and finds. A thread is found by its
// channel's registry row and its root ts, as the real store finds it; byKey
// indexes the same entities by their display key (domain.SlackSourceID), for
// tests that assert what a thread is displayed as.
type fakeEntities struct {
	byKey    map[string]*domain.Entity
	byThread map[string]*domain.Entity
	err      error
	// channels is the registry linked to this fake, which seedThread
	// registers a seeded thread's channel in.
	channels *fakeChannelRegistry
}

func newFakeEntities() *fakeEntities {
	return &fakeEntities{byKey: map[string]*domain.Entity{}, byThread: map[string]*domain.Entity{}}
}

func fakeEntityKey(orgID, source, scope, sourceID string) string {
	return orgID + "/" + source + "/" + scope + "/" + sourceID
}

func fakeThreadKey(orgID, channelRow, threadTS string) string {
	return orgID + "|" + channelRow + "|" + threadTS
}

func (f *fakeEntities) findOrCreateThread(orgID, channelRow, threadTS, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	if e, ok := f.byThread[fakeThreadKey(orgID, channelRow, threadTS)]; ok {
		return e, false, nil
	}
	key := fakeEntityKey(orgID, "slack", domain.SlackScope, sourceID)
	e := &domain.Entity{ID: "entity-" + key, Source: "slack", Scope: domain.SlackScope, SourceID: sourceID, Kind: kind, Title: title, URL: url, State: "active"}
	f.byKey[key] = e
	f.byThread[fakeThreadKey(orgID, channelRow, threadTS)] = e
	return e, true, nil
}

// seedThread inserts a pre-existing thread entity displayed as sourceID
// ("<channel>/<thread ts>") with the given kind and state — the fixtures the
// engaged-thread tests use to stand up (or deliberately mis-configure) the
// thread a follow-up lands in, without routing through a root-mention first.
// Its channel is registered in the linked registry, and the thread sits on
// that row, as a real thread does.
func (f *fakeEntities) seedThread(orgID, sourceID, kind, state string) {
	channelID, threadTS, _ := strings.Cut(sourceID, "/")
	key := fakeEntityKey(orgID, "slack", domain.SlackScope, sourceID)
	e := &domain.Entity{ID: "entity-" + key, Source: "slack", Scope: domain.SlackScope, SourceID: sourceID, Kind: kind, State: state}
	f.byKey[key] = e
	if f.channels != nil {
		row := f.channels.register(orgID, "", channelID)
		f.byThread[fakeThreadKey(orgID, row.ID, threadTS)] = e
	}
}

// fakeDeliveries is a minimal slackstore.DeliveryStore fake: an in-memory
// set of (workspaceID, eventID) pairs already seen.
type fakeDeliveries struct {
	seen map[string]bool
	err  error

	// onFresh, if set, runs after a delivery is recorded as new — the gap
	// between the dedup insert and the thread write.
	onFresh func()

	// prunes counts PruneDeliveredSystem calls, prunedInTx those made while
	// a fakeIngestTx transaction was open (txOpen); pruneErr fails them.
	prunes, prunedInTx int
	pruneErr           error
	txOpen             bool
}

func newFakeDeliveries() *fakeDeliveries { return &fakeDeliveries{seen: map[string]bool{}} }

func (f *fakeDeliveries) PruneDeliveredSystem(context.Context) error {
	f.prunes++
	if f.txOpen {
		f.prunedInTx++
	}
	return f.pruneErr
}

// fakeIngestTx is the delivery transaction the pipeline tests run in: a
// db.TxRunner whose SystemWithTx hands the body the test's delivery and
// channel fakes, plus an event queue, and when the body fails undoes what it
// wrote through them — the delivery marks, the threads it created, the
// events it enqueued — as a rollback does. Only SystemWithTx is called.
type fakeIngestTx struct {
	db.TxRunner
	// deliveries is the dedup store the transaction marks on — a
	// *fakeDeliveries, whose marks a rollback undoes, unless a test swaps in
	// a store of its own.
	deliveries slackstore.DeliveryStore
	channels   *fakeChannelRegistry

	// enqueueErr fails every enqueue while set.
	enqueueErr error
	// queued is every event a committed transaction enqueued, in order.
	queued []domain.Event
	nextID int
}

// withFakeIngestTx gives p a fakeIngestTx over its own delivery and channel
// fakes — the stores outside the transaction and inside it are one
// database — and returns it.
func withFakeIngestTx(p *ingestPipeline) *fakeIngestTx {
	tx := &fakeIngestTx{deliveries: p.deliveries, channels: p.channels.(*fakeChannelRegistry)}
	p.tx = tx
	return tx
}

func (f *fakeIngestTx) SystemWithTx(_ context.Context, _ string, fn func(db.SystemTxStores) error) error {
	entities := f.channels.entities
	byKey, byThread := maps.Clone(entities.byKey), maps.Clone(entities.byThread)
	marks, _ := f.deliveries.(*fakeDeliveries)
	var seen map[string]bool
	if marks != nil {
		seen = maps.Clone(marks.seen)
		marks.txOpen = true
		defer func() { marks.txOpen = false }()
	}
	q := &fakeEventQueue{tx: f}
	if err := fn(db.SystemTxStores{
		EventQueue: q,
		Ext:        map[string]any{slackstore.ExtKey: &slackstore.Bundle{Deliveries: f.deliveries, Channels: f.channels}},
	}); err != nil {
		entities.byKey, entities.byThread = byKey, byThread
		if marks != nil {
			marks.seen = seen
		}
		return err
	}
	f.queued = append(f.queued, q.events...)
	return nil
}

// fakeEventQueue is one fakeIngestTx transaction's event queue: Enqueue
// mints an id and holds the event until the transaction commits.
type fakeEventQueue struct {
	db.EventQueueStore
	tx     *fakeIngestTx
	events []domain.Event
}

func (q *fakeEventQueue) Enqueue(_ context.Context, orgID string, evt domain.Event, _ string) (string, error) {
	if q.tx.enqueueErr != nil {
		return "", q.tx.enqueueErr
	}
	if orgID != evt.OrgID {
		return "", fmt.Errorf("enqueue for org %q of an event stamped %q", orgID, evt.OrgID)
	}
	q.tx.nextID++
	evt.ID = fmt.Sprintf("evt-%d", q.tx.nextID)
	q.events = append(q.events, evt)
	return evt.ID, nil
}

func (f *fakeDeliveries) MarkDeliveredSystem(_ context.Context, workspaceID, eventID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	key := workspaceID + "/" + eventID
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	if f.onFresh != nil {
		f.onFresh()
	}
	return true, nil
}

var _ slackstore.DeliveryStore = (*fakeDeliveries)(nil)

// fakeChannelRegistry is a minimal slackstore.ChannelRegistryStore fake: an
// in-memory registry keyed by row id, tracking every UpsertSightingSystem
// call so tests can assert dedup (dropped-before-sighting) behavior, and
// creating thread entities in its linked fakeEntities.
type fakeChannelRegistry struct {
	mu       sync.Mutex
	rows     map[string]*slackstore.Channel
	entities *fakeEntities

	upsertErr error
	sawUpsert []string // channelKey values of the id each sighting resolved to

	// moved maps channelKey(org, old id) to the id MoveSystem moved it to;
	// sawMoves records each MoveSystem call as "<channelKey>-><new id>".
	moved      map[string]string
	sawMoves   []string
	moveErr    error
	resolveErr error

	// done, if non-nil, receives whenever SetNameSystem completes — the
	// same deterministic "the detached resolver goroutine finished" signal
	// fakeIdentityStore.done provides in identity_test.go.
	done chan struct{}
}

func newFakeChannelRegistry() *fakeChannelRegistry {
	return newFakeChannelRegistryFor(newFakeEntities())
}

// newFakeChannelRegistryFor links a registry to entities: the threads it
// creates are entities', and entities' seeded threads register their channel
// in it.
func newFakeChannelRegistryFor(entities *fakeEntities) *fakeChannelRegistry {
	f := &fakeChannelRegistry{rows: map[string]*slackstore.Channel{}, entities: entities}
	entities.channels = f
	return f
}

func channelKey(orgID, channelID string) string { return orgID + "/" + channelID }

// current is the id channelID goes by now. Caller holds f.mu.
func (f *fakeChannelRegistry) current(orgID, channelID string) string {
	if moved, ok := f.moved[channelKey(orgID, channelID)]; ok {
		return moved
	}
	return channelID
}

// lookup is the row channelID names now, or nil. Caller holds f.mu.
func (f *fakeChannelRegistry) lookup(orgID, channelID string) *slackstore.Channel {
	channelID = f.current(orgID, channelID)
	for _, row := range f.rows {
		if row.OrgID == orgID && row.ChannelID == channelID {
			return row
		}
	}
	return nil
}

// register returns channelID's row, adding one if absent.
func (f *fakeChannelRegistry) register(orgID, workspaceID, channelID string) *slackstore.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerLocked(orgID, workspaceID, channelID)
}

func (f *fakeChannelRegistry) registerLocked(orgID, workspaceID, channelID string) *slackstore.Channel {
	if row := f.lookup(orgID, channelID); row != nil {
		return row
	}
	channelID = f.current(orgID, channelID)
	row := &slackstore.Channel{ID: "row-" + channelKey(orgID, channelID), OrgID: orgID, ChannelID: channelID, WorkspaceID: workspaceID, FirstSeenAt: time.Now()}
	f.rows[row.ID] = row
	return row
}

// row is the row a Slack channel id names now, or nil.
func (f *fakeChannelRegistry) row(orgID, channelID string) *slackstore.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookup(orgID, channelID)
}

func (f *fakeChannelRegistry) UpsertSightingSystem(_ context.Context, orgID, workspaceID, channelID string, at time.Time) (slackstore.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sawUpsert = append(f.sawUpsert, channelKey(orgID, f.current(orgID, channelID)))
	if f.upsertErr != nil {
		return slackstore.Channel{}, f.upsertErr
	}
	row := f.registerLocked(orgID, workspaceID, channelID)
	row.WorkspaceID = workspaceID
	lastMention := at
	row.LastMentionAt = &lastMention
	return *row, nil
}

func (f *fakeChannelRegistry) EnsureSystem(_ context.Context, orgID, workspaceID, channelID, name string) (slackstore.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.registerLocked(orgID, workspaceID, channelID)
	if name != "" {
		row.Name = name
	}
	return *row, nil
}

func (f *fakeChannelRegistry) SetNameSystem(_ context.Context, orgID, id, name string) error {
	f.mu.Lock()
	if row, ok := f.rows[id]; ok && row.OrgID == orgID {
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

func (f *fakeChannelRegistry) ResolveSystem(_ context.Context, orgID, channelID string) (*slackstore.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	if row := f.lookup(orgID, channelID); row != nil {
		c := *row
		return &c, nil
	}
	return nil, nil
}

func (f *fakeChannelRegistry) GetSystem(_ context.Context, orgID, id string) (*slackstore.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[id]; ok && row.OrgID == orgID {
		c := *row
		return &c, nil
	}
	return nil, nil
}

func (f *fakeChannelRegistry) ListForOrg(context.Context, string) ([]slackstore.Channel, error) {
	return nil, nil
}

// FindOrCreateThreadSystem finds or creates the thread on the channel's row
// in the linked fakeEntities, displayed under the id the row carries.
func (f *fakeChannelRegistry) FindOrCreateThreadSystem(_ context.Context, orgID, channelID, threadTS, kind, title, url string) (slackstore.Thread, error) {
	f.mu.Lock()
	row := f.lookup(orgID, channelID)
	var channel slackstore.Channel
	if row != nil {
		channel = *row
	}
	f.mu.Unlock()
	if row == nil {
		return slackstore.Thread{}, fmt.Errorf("%w: %s", slackstore.ErrChannelNotRegistered, channelID)
	}
	e, created, err := f.entities.findOrCreateThread(orgID, channel.ID, threadTS, domain.SlackSourceID(channel.ChannelID, threadTS), kind, title, url)
	if err != nil {
		return slackstore.Thread{}, err
	}
	return slackstore.Thread{EntityID: e.ID, Created: created, Channel: channel}, nil
}

// GetThreadSystem finds the thread on the channel's row in the linked
// fakeEntities, as the real store does.
func (f *fakeChannelRegistry) GetThreadSystem(_ context.Context, orgID, channel, threadTS string) (*slackstore.ThreadEntity, error) {
	if f.entities.err != nil {
		return nil, f.entities.err
	}
	e, ok := f.entities.byThread[fakeThreadKey(orgID, channel, threadTS)]
	if !ok {
		return nil, nil
	}
	return &slackstore.ThreadEntity{ID: e.ID, Kind: e.Kind, State: e.State}, nil
}

// MoveSystem records the change so a retired id resolves to the channel's
// row, moves the row onto the new id — deleting a row the new id already
// had, as the store absorbs it — and records the call itself in sawMoves;
// moveErr fails it.
func (f *fakeChannelRegistry) MoveSystem(_ context.Context, orgID, oldID, newID string) (slackstore.ChannelMove, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sawMoves = append(f.sawMoves, channelKey(orgID, oldID)+"->"+newID)
	if f.moveErr != nil {
		return slackstore.ChannelMove{}, f.moveErr
	}
	row := f.lookup(orgID, oldID)
	if row != nil {
		if absorbed := f.lookup(orgID, newID); absorbed != nil && absorbed != row {
			delete(f.rows, absorbed.ID)
		}
	}
	if f.moved == nil {
		f.moved = map[string]string{}
	}
	f.moved[channelKey(orgID, oldID)] = newID
	if row != nil {
		row.ChannelID = newID
	}
	return slackstore.ChannelMove{To: newID}, nil
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
		deliveries: deliveries,
		channels:   newFakeChannelRegistryFor(entities),
		publish:    func(_ context.Context, evt domain.Event) { *published = append(*published, evt) },
	}
	withFakeIngestTx(p)
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
	if row := p.channels.(*fakeChannelRegistry).row("org-1", "C1"); row == nil || meta.ChannelRowID != row.ID {
		t.Errorf("ChannelRowID = %q; want the channel's registry row %+v", meta.ChannelRowID, row)
	}
}

// TestHandleEventCallback_MentionSurvivesAMoveAfterDedup: a mention under a
// channel's new id registers a row of its own, and a move committing right
// after the delivery is recorded absorbs that row. The thread resolves the
// delivered id rather than the absorbed row, so the delivery lands on its
// first attempt: the thread and the message land on the surviving row.
func TestHandleEventCallback_MentionSurvivesAMoveAfterDedup(t *testing.T) {
	p, entities, deliveries, published := newTestPipeline()
	channels := p.channels.(*fakeChannelRegistry)
	ws := testWorkspaceRow("org-1")
	survivor := channels.register("org-1", ws.WorkspaceID, "G1")
	deliveries.onFresh = func() {
		if _, err := channels.MoveSystem(context.Background(), "org-1", "G1", "C1"); err != nil {
			t.Errorf("MoveSystem: %v", err)
		}
	}

	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", Text: "hi", TS: "1600000000.000100"}
	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(*published) != 1 {
		t.Fatalf("published %d events; want 1", len(*published))
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte((*published)[0].MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.ChannelRowID != survivor.ID || meta.Channel != "C1" {
		t.Errorf("metadata channel = %q (row %q); want C1 on the surviving row %s", meta.Channel, meta.ChannelRowID, survivor.ID)
	}
	if e := entities.byThread[fakeThreadKey("org-1", survivor.ID, "1600000000.000100")]; e == nil || e.SourceID != "C1/1600000000.000100" {
		t.Errorf("thread on the surviving row = %+v, want one displayed as C1/1600000000.000100", e)
	}
}

// TestHandleEventCallback_ThreadRootVsRootMessage pins the thread-root
// resolution rule (thread_ts when present, else the message's own ts) and
// the thread-engagement kind it implies: a root-message mention is why the
// thread exists ("thread"); a threaded mention is a summons into a thread
// someone else rooted ("message").
func TestHandleEventCallback_ThreadRootVsRootMessage(t *testing.T) {
	cases := []struct {
		name         string
		ts, threadTS string
		wantSourceID string
		wantKind     string
	}{
		{"threaded mention uses thread_ts", "1600000000.000100", "1599999999.000001", "C1/1599999999.000001", "message"},
		{"root-message mention uses its own ts", "1600000000.000100", "", "C1/1600000000.000100", "thread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, entities, _, _ := newTestPipeline()
			ws := testWorkspaceRow("org-1")
			ev := inboundMention{Type: "app_mention", EventID: "Ev-" + tc.name, Channel: "C1", User: "U1", TS: tc.ts, ThreadTS: tc.threadTS}
			if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
				t.Fatalf("handleEventCallback: %v", err)
			}
			key := fakeEntityKey("org-1", "slack", domain.SlackScope, tc.wantSourceID)
			ent, ok := entities.byKey[key]
			if !ok {
				t.Fatalf("no entity created under source_id %q; entities = %v", tc.wantSourceID, entities.byKey)
			}
			if ent.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", ent.Kind, tc.wantKind)
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

// TestHandleEventCallback_UnsupportedTypeDropped: an inner event type this
// pipeline dispatches on neither branch (app_mention / message) is dropped
// defensively, even though the manifest subscribes to nothing else.
func TestHandleEventCallback_UnsupportedTypeDropped(t *testing.T) {
	p, _, _, published := newTestPipeline()
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "reaction_added", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1.0"}
	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(*published) != 0 {
		t.Errorf("published %d events; want 0 (unsupported type dropped)", len(*published))
	}
}

// TestHandleChannelIDChanged_MovesTheChannel: the change moves the channel for
// the delivering workspace's org, publishes nothing, and records no delivery
// row — the move is idempotent, so a redelivery moves (nothing) again rather
// than being dropped.
func TestHandleChannelIDChanged_MovesTheChannel(t *testing.T) {
	p, _, deliveries, published := newTestPipeline()
	channels := p.channels.(*fakeChannelRegistry)
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "channel_id_changed", EventID: "Ev1", OldChannelID: "G1", NewChannelID: "C1"}

	for i := 0; i < 2; i++ {
		if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if want := []string{"org-1/G1->C1", "org-1/G1->C1"}; !reflect.DeepEqual(channels.sawMoves, want) {
		t.Errorf("moves = %v; want %v (each delivery moves)", channels.sawMoves, want)
	}
	if len(*published) != 0 {
		t.Errorf("published %d events; want 0", len(*published))
	}
	if len(deliveries.seen) != 0 {
		t.Errorf("recorded %d deliveries; want 0", len(deliveries.seen))
	}
}

// TestHandleChannelIDChanged_MalformedDropped: a change missing either id, or
// naming the same id twice, moves nothing.
func TestHandleChannelIDChanged_MalformedDropped(t *testing.T) {
	for _, ev := range []inboundMention{
		{Type: "channel_id_changed", EventID: "Ev1", NewChannelID: "C1"},
		{Type: "channel_id_changed", EventID: "Ev2", OldChannelID: "G1"},
		{Type: "channel_id_changed", EventID: "Ev3", OldChannelID: "C1", NewChannelID: "C1"},
	} {
		p, _, _, _ := newTestPipeline()
		channels := p.channels.(*fakeChannelRegistry)
		if err := p.handleEventCallback(context.Background(), testWorkspaceRow("org-1"), ev); err != nil {
			t.Fatalf("%s: %v", ev.EventID, err)
		}
		if len(channels.sawMoves) != 0 {
			t.Errorf("%s: moves = %v; want none", ev.EventID, channels.sawMoves)
		}
	}
}

// TestHandleChannelIDChanged_MoveErrorReturned: a failed move is an error, so
// the transport withholds its ack and Slack redelivers the change.
func TestHandleChannelIDChanged_MoveErrorReturned(t *testing.T) {
	p, _, _, _ := newTestPipeline()
	channels := p.channels.(*fakeChannelRegistry)
	channels.moveErr = errors.New("db down")
	ev := inboundMention{Type: "channel_id_changed", EventID: "Ev1", OldChannelID: "G1", NewChannelID: "C1"}
	if err := p.handleEventCallback(context.Background(), testWorkspaceRow("org-1"), ev); err == nil {
		t.Fatal("handleEventCallback = nil; want the move's error")
	}
}

// TestHandleEventCallback_MentionUnderARetiredChannelID: a mention Slack
// generated before the channel's id changed and delivered after the move
// lands under the new id — the sighting, the thread key and the published
// metadata alike — instead of reviving the old one.
func TestHandleEventCallback_MentionUnderARetiredChannelID(t *testing.T) {
	p, entities, _, published := newTestPipeline()
	channels := p.channels.(*fakeChannelRegistry)
	channels.moved = map[string]string{channelKey("org-1", "G1"): "C1"}
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "G1", User: "U1", Text: "hi", TS: "1600000000.000100", ThreadTS: "1599999999.000001"}

	if err := p.handleEventCallback(context.Background(), testWorkspaceRow("org-1"), ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if _, ok := entities.byKey[fakeEntityKey("org-1", "slack", domain.SlackScope, "C1/1599999999.000001")]; !ok {
		t.Errorf("no entity under the new id; entities = %v", entities.byKey)
	}
	if want := []string{channelKey("org-1", "C1")}; !reflect.DeepEqual(channels.sawUpsert, want) {
		t.Errorf("sightings = %v; want %v", channels.sawUpsert, want)
	}
	if len(*published) != 1 {
		t.Fatalf("published %d events; want 1", len(*published))
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte((*published)[0].MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.Channel != "C1" {
		t.Errorf("metadata channel = %q; want C1", meta.Channel)
	}
	if row := channels.row("org-1", "C1"); row == nil || meta.ChannelRowID != row.ID {
		t.Errorf("metadata channel_row_id = %q; want the channel's row %+v", meta.ChannelRowID, row)
	}
}

// TestHandleThreadMessage_FollowUpUnderARetiredChannelID: a follow-up
// delivered under the old id after the move reaches the thread, now keyed
// under the new id, and publishes under it.
func TestHandleThreadMessage_FollowUpUnderARetiredChannelID(t *testing.T) {
	p, entities, deliveries, published := newTestPipeline()
	channels := p.channels.(*fakeChannelRegistry)
	channels.moved = map[string]string{channelKey("org-1", "G1"): "C1"}
	entities.seedThread("org-1", engagedSourceID, "thread", "active")

	ev := engagedFollowUp()
	ev.Channel = "G1"
	if err := p.handleEventCallback(context.Background(), testWorkspaceRow("org-1"), ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(*published) != 1 {
		t.Fatalf("published %d events; want 1", len(*published))
	}
	got := (*published)[0]
	if got.EntityID == nil || *got.EntityID != engagedEntityID {
		t.Errorf("EntityID = %v; want %s", got.EntityID, engagedEntityID)
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte(got.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.Channel != "C1" {
		t.Errorf("metadata channel = %q; want C1", meta.Channel)
	}
	if row := channels.row("org-1", "C1"); row == nil || meta.ChannelRowID != row.ID {
		t.Errorf("metadata channel_row_id = %q; want the channel's row %+v", meta.ChannelRowID, row)
	}
	if len(deliveries.seen) != 1 {
		t.Errorf("recorded %d deliveries; want 1", len(deliveries.seen))
	}
}

// TestHandleThreadMessage_ChannelResolveErrorBeforeDelivery: when the miss's
// channel id read fails, the reply is an error and no delivery row is
// recorded, so Slack's redelivery can still land it.
func TestHandleThreadMessage_ChannelResolveErrorBeforeDelivery(t *testing.T) {
	p, _, deliveries, published := newTestPipeline()
	channels := p.channels.(*fakeChannelRegistry)
	channels.resolveErr = errors.New("db down")

	if err := p.handleEventCallback(context.Background(), testWorkspaceRow("org-1"), engagedFollowUp()); err == nil {
		t.Fatal("handleEventCallback = nil; want the read's error")
	}
	if len(*published) != 0 || len(deliveries.seen) != 0 {
		t.Errorf("published %d, recorded %d deliveries; want neither", len(*published), len(deliveries.seen))
	}
}

// engagedThreadTS / engagedReplyTS / engagedSourceID are the fixture keys the
// engaged-thread tests share: a follow-up (engagedReplyTS) inside the thread
// rooted at engagedThreadTS in channel C1.
const (
	engagedThreadTS = "1500000000.000001"
	engagedReplyTS  = "1500000000.000100"
	engagedSourceID = "C1/" + engagedThreadTS
	engagedEntityID = "entity-org-1/slack/slack.com/" + engagedSourceID
)

// engagedFollowUp is the canonical valid engaged-thread follow-up: a plain
// (no-subtype) human reply in channel C1's engaged thread that does NOT
// @-mention the bot.
func engagedFollowUp() inboundMention {
	return inboundMention{
		Type: "message", EventID: "Ev-followup", Channel: "C1", User: "U1",
		Text: "here are the PR numbers", TS: engagedReplyTS, ThreadTS: engagedThreadTS,
	}
}

// TestHandleThreadMessage_DropReasonsAndAccept is the table-driven core of the
// engaged-thread branch: one case per drop reason plus the two accept shapes.
// Every case asserts BOTH the published-event count AND the delivery-row count
// — a drop must leave slack_deliveries untouched (the delivery insert is the
// last step, gated behind every filter), so an accidentally-recorded delivery
// on a dropped message is its own regression.
func TestHandleThreadMessage_DropReasonsAndAccept(t *testing.T) {
	seedActive := func(f *fakeEntities) { f.seedThread("org-1", engagedSourceID, "thread", "active") }
	// with returns a copy of the canonical follow-up with fn applied, keeping
	// each case to only its one distinguishing tweak.
	with := func(fn func(*inboundMention)) inboundMention {
		ev := engagedFollowUp()
		fn(&ev)
		return ev
	}

	cases := []struct {
		name      string
		seed      func(*fakeEntities) // entity fixtures; nil = no thread entity
		ev        inboundMention
		wantPub   int
		wantDeliv int
	}{
		{"accept: plain thread reply", seedActive, engagedFollowUp(), 1, 1},
		{"accept: thread_broadcast reply", seedActive, with(func(e *inboundMention) { e.Subtype = "thread_broadcast" }), 1, 1},
		{"drop: unsupported subtype", seedActive, with(func(e *inboundMention) { e.Subtype = "message_changed" }), 0, 0},
		{"drop: not a thread reply (no thread_ts)", seedActive, with(func(e *inboundMention) { e.ThreadTS = "" }), 0, 0},
		{"drop: bot-authored (bot_id present)", seedActive, with(func(e *inboundMention) { e.BotID = "B1" }), 0, 0},
		{"drop: self-authored (sender is the workspace bot)", seedActive, with(func(e *inboundMention) { e.User = "U0BOT" }), 0, 0},
		{"drop: explicit @-mention (owned by app_mention)", seedActive, with(func(e *inboundMention) { e.Text = "hey <@U0BOT> follow up" }), 0, 0},
		{"drop: no entity for the thread", nil, engagedFollowUp(), 0, 0},
		{"drop: entity is a mid-thread summons (kind=message)", func(f *fakeEntities) { f.seedThread("org-1", engagedSourceID, "message", "active") }, engagedFollowUp(), 0, 0},
		{"drop: engaged thread is closed (state=closed)", func(f *fakeEntities) { f.seedThread("org-1", engagedSourceID, "thread", "closed") }, engagedFollowUp(), 0, 0},
		{"drop: malformed (missing event_id)", seedActive, with(func(e *inboundMention) { e.EventID = "" }), 0, 0},
		{"drop: malformed (missing channel)", seedActive, with(func(e *inboundMention) { e.Channel = "" }), 0, 0},
		{"drop: malformed (missing ts)", seedActive, with(func(e *inboundMention) { e.TS = "" }), 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, entities, deliveries, published := newTestPipeline()
			if tc.seed != nil {
				tc.seed(entities)
			}
			ws := testWorkspaceRow("org-1")
			if err := p.handleEventCallback(context.Background(), ws, tc.ev); err != nil {
				t.Fatalf("handleEventCallback: %v", err)
			}
			if len(*published) != tc.wantPub {
				t.Errorf("published %d events; want %d", len(*published), tc.wantPub)
			}
			if len(deliveries.seen) != tc.wantDeliv {
				t.Errorf("recorded %d deliveries; want %d (drops must precede the delivery insert)", len(deliveries.seen), tc.wantDeliv)
			}
		})
	}
}

// TestHandleThreadMessage_AcceptPublishesFollowUpShape pins the accepted
// follow-up's event shape: it hangs off the pre-existing thread entity (no new
// entity minted) and carries Mentioned=false — the honest "addressed the bot
// without an @" the engaged-thread semantics encode.
func TestHandleThreadMessage_AcceptPublishesFollowUpShape(t *testing.T) {
	p, entities, _, published := newTestPipeline()
	entities.seedThread("org-1", engagedSourceID, "thread", "active")
	before := len(entities.byKey)
	ws := testWorkspaceRow("org-1")

	if err := p.handleEventCallback(context.Background(), ws, engagedFollowUp()); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(entities.byKey) != before {
		t.Errorf("entity count changed from %d to %d; a follow-up must reuse the engaged thread, never mint one", before, len(entities.byKey))
	}
	if len(*published) != 1 {
		t.Fatalf("published %d events; want 1", len(*published))
	}
	got := (*published)[0]
	if got.EventType != domain.EventSlackMessage {
		t.Errorf("EventType = %q; want %q", got.EventType, domain.EventSlackMessage)
	}
	if got.EntityID == nil || *got.EntityID != engagedEntityID {
		t.Errorf("EntityID = %v; want the seeded thread entity %q", got.EntityID, engagedEntityID)
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte(got.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata round-trip: %v", err)
	}
	if meta.Mentioned {
		t.Error("Mentioned = true; want false (an engaged-thread follow-up carried no explicit @-mention)")
	}
	if meta.ThreadTS != engagedThreadTS || meta.TS != engagedReplyTS || meta.SenderID != "U1" || meta.EventID != "Ev-followup" {
		t.Errorf("metadata = %+v; want fields carried from the follow-up", meta)
	}
}

// TestHandleThreadMessage_FollowUpThroughAnotherWorkspace: a shared channel has
// one id in every workspace it is in, so a thread engaged through one
// workspace's connection takes a reply delivered through another's.
func TestHandleThreadMessage_FollowUpThroughAnotherWorkspace(t *testing.T) {
	p, entities, _, published := newTestPipeline()
	if err := p.handleEventCallback(context.Background(), testWorkspaceRow("org-1"), inboundMention{
		Type: "app_mention", EventID: "Ev-root", Channel: "C1", User: "U1", Text: "<@U0BOT> look", TS: engagedThreadTS,
	}); err != nil {
		t.Fatalf("root mention: %v", err)
	}
	other := testWorkspaceRow("org-1")
	other.WorkspaceID, other.APIAppID = "T0OTHER01", "A0OTHER01"
	if err := p.handleEventCallback(context.Background(), other, engagedFollowUp()); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if len(entities.byKey) != 1 {
		t.Errorf("entities = %d, want the one thread", len(entities.byKey))
	}
	if len(*published) != 2 || (*published)[1].EntityID == nil || *(*published)[1].EntityID != engagedEntityID {
		t.Fatalf("published %d events; want the follow-up on %s", len(*published), engagedEntityID)
	}
}

// TestHandleThreadMessage_MentionInsideEngagedThread_PublishesOnce is the
// double-delivery dedup: a single user message that @-mentions the bot inside
// an engaged thread fans out into TWO Slack deliveries (an app_mention AND a
// message.channels twin, each with its own event_id). The app_mention owns it;
// the message twin is dropped by the explicit-mention guard — so exactly one
// event publishes and exactly one delivery row is recorded.
func TestHandleThreadMessage_MentionInsideEngagedThread_PublishesOnce(t *testing.T) {
	p, entities, deliveries, published := newTestPipeline()
	entities.seedThread("org-1", engagedSourceID, "thread", "active")
	ws := testWorkspaceRow("org-1")

	const sharedText = "hey <@U0BOT> another one"
	appMention := inboundMention{Type: "app_mention", EventID: "Ev-appmention", Channel: "C1", User: "U1", Text: sharedText, TS: "1500000000.000200", ThreadTS: engagedThreadTS}
	messageTwin := inboundMention{Type: "message", EventID: "Ev-message", Channel: "C1", User: "U1", Text: sharedText, TS: "1500000000.000200", ThreadTS: engagedThreadTS}

	for _, ev := range []inboundMention{appMention, messageTwin} {
		if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
			t.Fatalf("handleEventCallback(%s): %v", ev.Type, err)
		}
	}
	if len(*published) != 1 {
		t.Fatalf("published %d events across the mention + its message twin; want exactly 1", len(*published))
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte((*published)[0].MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata round-trip: %v", err)
	}
	if !meta.Mentioned {
		t.Error("the surviving event is Mentioned=false; want true (the app_mention delivery is the one that published)")
	}
	if len(deliveries.seen) != 1 {
		t.Errorf("recorded %d deliveries; want 1 (only the app_mention; its message twin drops before the delivery insert)", len(deliveries.seen))
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

// TestHandleEventCallback_FailureAfterDedupRollsTheDeliveryBack: a mention
// whose thread write or enqueue fails leaves no delivery recorded, no thread
// and nothing queued or published, so Slack's redelivery runs fresh — and,
// with the mark and the event committed together, publishes the message
// exactly once across every redelivery after it, under its queue row's id.
func TestHandleEventCallback_FailureAfterDedupRollsTheDeliveryBack(t *testing.T) {
	cases := []struct {
		name string
		fail func(entities *fakeEntities, tx *fakeIngestTx, err error)
	}{
		{"thread write", func(entities *fakeEntities, _ *fakeIngestTx, err error) { entities.err = err }},
		{"enqueue", func(_ *fakeEntities, tx *fakeIngestTx, err error) { tx.enqueueErr = err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, entities, deliveries, published := newTestPipeline()
			tx := p.tx.(*fakeIngestTx)
			ws := testWorkspaceRow("org-1")
			ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", Text: "hi", TS: "1600000000.000100"}

			boom := errors.New("boom")
			tc.fail(entities, tx, boom)
			if err := p.handleEventCallback(context.Background(), ws, ev); !errors.Is(err, boom) {
				t.Fatalf("handleEventCallback = %v; want the injected failure", err)
			}
			if len(deliveries.seen) != 0 || len(entities.byThread) != 0 || len(tx.queued) != 0 || len(*published) != 0 {
				t.Fatalf("after the failure: %d deliveries, %d threads, %d queued, %d published; want none",
					len(deliveries.seen), len(entities.byThread), len(tx.queued), len(*published))
			}

			tc.fail(entities, tx, nil)
			for i := range 2 {
				if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
					t.Fatalf("redelivery %d: %v", i+1, err)
				}
			}
			if len(tx.queued) != 1 || len(*published) != 1 || len(entities.byThread) != 1 {
				t.Fatalf("across the redeliveries: %d queued, %d published, %d threads; want 1 each", len(tx.queued), len(*published), len(entities.byThread))
			}
			if got := (*published)[0].ID; got == "" || got != tx.queued[0].ID {
				t.Errorf("published event id = %q; want its queue row's %q", got, tx.queued[0].ID)
			}
		})
	}
}

// TestHandleThreadMessage_EnqueueFailureRollsTheDeliveryBack is the
// engaged-thread twin: a follow-up whose enqueue fails is not recorded as
// delivered, and its redelivery publishes it once.
func TestHandleThreadMessage_EnqueueFailureRollsTheDeliveryBack(t *testing.T) {
	p, entities, deliveries, published := newTestPipeline()
	tx := p.tx.(*fakeIngestTx)
	entities.seedThread("org-1", engagedSourceID, "thread", "active")
	ws := testWorkspaceRow("org-1")

	boom := errors.New("boom")
	tx.enqueueErr = boom
	if err := p.handleEventCallback(context.Background(), ws, engagedFollowUp()); !errors.Is(err, boom) {
		t.Fatalf("handleEventCallback = %v; want the injected failure", err)
	}
	if len(deliveries.seen) != 0 || len(tx.queued) != 0 || len(*published) != 0 {
		t.Fatalf("after the failure: %d deliveries, %d queued, %d published; want none", len(deliveries.seen), len(tx.queued), len(*published))
	}

	tx.enqueueErr = nil
	for i := range 2 {
		if err := p.handleEventCallback(context.Background(), ws, engagedFollowUp()); err != nil {
			t.Fatalf("redelivery %d: %v", i+1, err)
		}
	}
	if len(tx.queued) != 1 || len(*published) != 1 {
		t.Fatalf("across the redeliveries: %d queued, %d published; want 1 each", len(tx.queued), len(*published))
	}
	if got := (*published)[0]; got.ID != tx.queued[0].ID || got.EntityID == nil || *got.EntityID != engagedEntityID {
		t.Errorf("published %+v; want the queued event %q on the engaged thread %q", got, tx.queued[0].ID, engagedEntityID)
	}
}

// TestHandleEventCallback_PrunesAfterTheDeliveryTx: the dedup table's prune
// runs once per delivery, after its transaction rather than inside it, and a
// failing prune does not fail the delivery.
func TestHandleEventCallback_PrunesAfterTheDeliveryTx(t *testing.T) {
	p, _, deliveries, published := newTestPipeline()
	deliveries.pruneErr = errors.New("boom")
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1600000000.000100"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(*published) != 1 {
		t.Errorf("published %d events; want 1 (a failed prune is not the delivery's failure)", len(*published))
	}
	if deliveries.prunes != 1 || deliveries.prunedInTx != 0 {
		t.Errorf("prunes = %d (%d inside the transaction); want 1, outside it", deliveries.prunes, deliveries.prunedInTx)
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
	entities := newFakeEntities()
	p := &ingestPipeline{
		deliveries: newFakeDeliveries(),
		channels:   newFakeChannelRegistryFor(entities),
		publish:    func(context.Context, domain.Event) {},
		identity:   resolver,
	}
	withFakeIngestTx(p)
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

// TestHandleEventCallback_FollowUpDispatchesIdentityResolution is the
// engaged-thread twin of the wiring test above: an accepted un-mentioned
// follow-up dispatches resolveSender for its sender too. Follow-up senders
// are exactly the participants who may never type an @-mention, so capture
// wired only into the app_mention branch would systematically exclude them
// from user_slack_identities.
func TestHandleEventCallback_FollowUpDispatchesIdentityResolution(t *testing.T) {
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
	entities := newFakeEntities()
	published := 0
	p := &ingestPipeline{
		deliveries: newFakeDeliveries(),
		channels:   newFakeChannelRegistryFor(entities),
		publish:    func(context.Context, domain.Event) { published++ },
		identity:   resolver,
	}
	withFakeIngestTx(p)
	ws := testWorkspaceRow("org-1")
	entities.seedThread("org-1", domain.SlackSourceID("C1", "1600000000.000100"), "thread", "active")
	ev := inboundMention{
		Type: "message", EventID: "Ev2", Channel: "C1", User: "U0SENDER1",
		Text: "here are the PR numbers", TS: "1600000000.000200", ThreadTS: "1600000000.000100",
	}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if published != 1 {
		t.Fatalf("published %d events; want 1 (follow-up should have been accepted)", published)
	}

	select {
	case <-identities.done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolveSender never completed — the follow-up branch did not dispatch it")
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
	channels := p.channels.(*fakeChannelRegistry)
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1600000000.000100"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}
	if len(channels.sawUpsert) != 1 {
		t.Fatalf("sighting upserts = %d; want 1", len(channels.sawUpsert))
	}
	if row := channels.row("org-1", "C1"); row == nil {
		t.Fatal("no sighting row recorded")
	}
}

// TestHandleEventCallback_ChannelSightingRecordedBeforeDedup pins that the
// sighting precedes the dedup insert — the message's registry row is what the
// thread and the event reference — so a redelivered event_id records the same
// sighting again and is then dropped, never reaching entity creation or
// publish.
func TestHandleEventCallback_ChannelSightingRecordedBeforeDedup(t *testing.T) {
	p, _, _, _ := newTestPipeline()
	channels := p.channels.(*fakeChannelRegistry)
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev-dup", Channel: "C1", User: "U1", TS: "1.0"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if len(channels.sawUpsert) != 2 {
		t.Errorf("sighting upserts across 2 identical deliveries = %d; want 2 (each resolves its channel before dedup)", len(channels.sawUpsert))
	}
}

// TestHandleEventCallback_ChannelSightingErrorBeforeDelivery: the sighting
// resolves the mention's channel to the registry row its thread and event
// reference, so a failed one is the delivery's error — returned before the
// dedup insert, so Slack's redelivery can still land the mention.
func TestHandleEventCallback_ChannelSightingErrorBeforeDelivery(t *testing.T) {
	p, _, deliveries, published := newTestPipeline()
	channels := p.channels.(*fakeChannelRegistry)
	channels.upsertErr = errors.New("boom")
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U1", TS: "1.0"}

	if err := p.handleEventCallback(context.Background(), ws, ev); err == nil {
		t.Fatal("handleEventCallback = nil; want the sighting's error")
	}
	if len(*published) != 0 || len(deliveries.seen) != 0 {
		t.Errorf("published %d, recorded %d deliveries; want neither", len(*published), len(deliveries.seen))
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
	entities := newFakeEntities()
	channels := newFakeChannelRegistryFor(entities)
	channels.done = make(chan struct{}, 1)
	resolver := &ChannelResolver{secrets: &fakeSecrets{token: "xoxb-test"}, channels: channels, client: srv.Client()}
	p := &ingestPipeline{
		deliveries:  newFakeDeliveries(),
		channels:    channels,
		publish:     func(context.Context, domain.Event) {},
		channelName: resolver,
	}
	withFakeIngestTx(p)
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
	row := channels.row("org-1", "C1")
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
		deliveries: newFakeDeliveries(),
		channels:   newFakeChannelRegistryFor(entities),
		publish:    func(context.Context, domain.Event) {},
		permalink:  resolver,
	}
	withFakeIngestTx(p)
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
	entityID := "entity-org-1/slack/slack.com/C1/1600000000.000100"
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
		deliveries: newFakeDeliveries(),
		channels:   newFakeChannelRegistryFor(entities),
		publish:    func(context.Context, domain.Event) {},
		permalink:  resolver,
	}
	withFakeIngestTx(p)
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
	if r := []rune(got); len(r) != slackTitleMaxRunes {
		t.Errorf("mentionTitle length = %d runes; want %d", len(r), slackTitleMaxRunes)
	}
}
