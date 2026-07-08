package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
)

func testWorkspaceForPermalinkResolver() slackstore.Workspace {
	return slackstore.Workspace{WorkspaceID: "T0PERM0001", OrgID: "org-1", BotTokenRef: "slack_ws_T0PERM0001_bot_token"}
}

// fakeEntityURLUpdater is a minimal entityURLUpdater fake: an in-memory map
// of entity id -> url, tracking every UpdateURLSystem call so tests can
// assert dedup / never-writes-on-failure behavior.
type fakeEntityURLUpdater struct {
	mu   sync.Mutex
	urls map[string]string
	err  error

	// done, if non-nil, receives whenever UpdateURLSystem completes — the
	// same deterministic "the detached resolver goroutine finished" signal
	// fakeChannelRegistry.done provides in ingest_test.go.
	done chan struct{}
}

func newFakeEntityURLUpdater() *fakeEntityURLUpdater {
	return &fakeEntityURLUpdater{urls: map[string]string{}}
}

func (f *fakeEntityURLUpdater) UpdateURLSystem(_ context.Context, _, entityID, url string) error {
	f.mu.Lock()
	if f.err == nil {
		f.urls[entityID] = url
	}
	err := f.err
	f.mu.Unlock()
	if f.done != nil {
		f.done <- struct{}{}
	}
	return err
}

func (f *fakeEntityURLUpdater) get(entityID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.urls[entityID]
}

var _ entityURLUpdater = (*fakeEntityURLUpdater)(nil)

// newPermalinkServer serves a fixed chat.getPermalink JSON body and counts
// hits — mirrors channels_test.go's newConversationsInfoServer.
func newPermalinkServer(t *testing.T, body map[string]any) (*http.Client, *int) {
	t.Helper()
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(body)
	})
	return srv.Client(), &hits
}

// TestResolvePermalink_GoldenPath covers the happy path: the resolved
// permalink lands on the entity's url via UpdateURLSystem.
func TestResolvePermalink_GoldenPath(t *testing.T) {
	client, hits := newPermalinkServer(t, map[string]any{
		"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1700000000000100",
	})
	entities := newFakeEntityURLUpdater()
	ws := testWorkspaceForPermalinkResolver()
	r := &PermalinkResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: entities, client: client}

	r.resolvePermalink(context.Background(), ws, "entity-1", "C1", "1700000000.000100")

	if *hits != 1 {
		t.Fatalf("chat.getPermalink hits = %d; want 1", *hits)
	}
	if got := entities.get("entity-1"); got != "https://acme.slack.com/archives/C1/p1700000000000100" {
		t.Errorf("url = %q; want the resolved permalink", got)
	}
}

// TestResolvePermalink_BotTokenReadFailed never calls chat.getPermalink and
// never writes — a transient secrets-store error should not persist a
// wrong/empty url.
func TestResolvePermalink_BotTokenReadFailed(t *testing.T) {
	client, hits := newPermalinkServer(t, map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1"})
	entities := newFakeEntityURLUpdater()
	ws := testWorkspaceForPermalinkResolver()
	r := &PermalinkResolver{secrets: &fakeSecrets{err: context.DeadlineExceeded}, entities: entities, client: client}

	r.resolvePermalink(context.Background(), ws, "entity-1", "C1", "1.0")

	if *hits != 0 {
		t.Errorf("chat.getPermalink hits = %d; want 0 (bot token read failed)", *hits)
	}
	if got := entities.get("entity-1"); got != "" {
		t.Errorf("url = %q; want empty (never written on bot-token failure)", got)
	}
}

// TestResolvePermalink_NoBotTokenConfigured never calls chat.getPermalink
// when the workspace has no bot token secret at all.
func TestResolvePermalink_NoBotTokenConfigured(t *testing.T) {
	client, hits := newPermalinkServer(t, map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1"})
	entities := newFakeEntityURLUpdater()
	ws := testWorkspaceForPermalinkResolver()
	r := &PermalinkResolver{secrets: &fakeSecrets{token: ""}, entities: entities, client: client}

	r.resolvePermalink(context.Background(), ws, "entity-1", "C1", "1.0")

	if *hits != 0 {
		t.Errorf("chat.getPermalink hits = %d; want 0 (no bot token configured)", *hits)
	}
}

// TestResolvePermalink_APIFailureLeavesURLUntouched pins that a
// chat.getPermalink failure never writes — the entity keeps its empty url
// rather than risk a wrong fallback.
func TestResolvePermalink_APIFailureLeavesURLUntouched(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
	})
	entities := newFakeEntityURLUpdater()
	ws := testWorkspaceForPermalinkResolver()
	r := &PermalinkResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: entities, client: srv.Client()}

	r.resolvePermalink(context.Background(), ws, "entity-1", "C1", "1.0")

	if got := entities.get("entity-1"); got != "" {
		t.Errorf("url = %q; want empty (never written on chat.getPermalink failure)", got)
	}
}

// TestResolvePermalink_StoreWriteFailedIsSwallowed pins the best-effort
// contract for the final write step too: a store error is logged and
// swallowed, not propagated (there is no error return at all).
func TestResolvePermalink_StoreWriteFailedIsSwallowed(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1"})
	})
	entities := newFakeEntityURLUpdater()
	entities.err = context.DeadlineExceeded
	ws := testWorkspaceForPermalinkResolver()
	r := &PermalinkResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: entities, client: srv.Client()}

	// Must not panic and must return (best-effort, no error to check).
	r.resolvePermalink(context.Background(), ws, "entity-1", "C1", "1.0")
}

// TestPermalinkResolverDispatch_DedupsConcurrentResolutions pins the
// thundering-herd guard: while a resolution for an entity is in flight
// (chat.getPermalink deliberately held open here), further dispatch calls
// for the same entity must be no-ops rather than spawning their own
// goroutine and hitting the Slack API again.
func TestPermalinkResolverDispatch_DedupsConcurrentResolutions(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1"})
	})
	entities := newFakeEntityURLUpdater()
	entities.done = make(chan struct{}, 1)
	ws := testWorkspaceForPermalinkResolver()
	r := &PermalinkResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: entities, client: srv.Client()}

	r.dispatch(ws, "entity-1", "C1", "1.0")

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		reached := hits == 1
		mu.Unlock()
		if reached {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first dispatch never reached chat.getPermalink")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Further dispatches while the first is still blocked in
	// chat.getPermalink must not spawn another resolution.
	for i := 0; i < 5; i++ {
		r.dispatch(ws, "entity-1", "C1", "1.0")
	}

	close(release)

	select {
	case <-entities.done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolution never completed")
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("chat.getPermalink hits = %d; want 1 (concurrent dispatches for the same entity must dedup)", hits)
	}
}
