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

func testWorkspaceForChannelResolver() slackstore.Workspace {
	return slackstore.Workspace{WorkspaceID: "T0RESOLVE1", OrgID: "org-1", BotTokenRef: "slack_ws_T0RESOLVE1_bot_token"}
}

// newConversationsInfoServer serves a fixed conversations.info JSON body
// and counts hits — mirrors identity_test.go's newUsersInfoServer.
func newConversationsInfoServer(t *testing.T, body map[string]any) (*http.Client, *int) {
	t.Helper()
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(body)
	})
	return srv.Client(), &hits
}

// TestResolveChannelName_GoldenPath covers the happy path: the resolved
// name lands on the existing registry row.
func TestResolveChannelName_GoldenPath(t *testing.T) {
	client, hits := newConversationsInfoServer(t, map[string]any{
		"ok": true, "channel": map[string]any{"name": "general"},
	})
	channels := newFakeChannelRegistry()
	ws := testWorkspaceForChannelResolver()
	channels.rows[channelKey(ws.OrgID, "C1")] = &slackstore.Channel{OrgID: ws.OrgID, ChannelID: "C1"}
	r := &ChannelResolver{secrets: &fakeSecrets{token: "xoxb-test"}, channels: channels, client: client}

	r.resolveChannelName(context.Background(), ws, "C1")

	if *hits != 1 {
		t.Fatalf("conversations.info hits = %d; want 1", *hits)
	}
	row := channels.rows[channelKey(ws.OrgID, "C1")]
	if row.Name != "general" {
		t.Errorf("Name = %q; want general", row.Name)
	}
	if row.NameResolvedAt == nil {
		t.Error("NameResolvedAt not stamped")
	}
}

// TestResolveChannelName_BotTokenReadFailed never calls conversations.info
// and never writes — a transient secrets-store error should retry on the
// next sighting, not persist a wrong/empty name.
func TestResolveChannelName_BotTokenReadFailed(t *testing.T) {
	client, hits := newConversationsInfoServer(t, map[string]any{"ok": true, "channel": map[string]any{"name": "general"}})
	channels := newFakeChannelRegistry()
	ws := testWorkspaceForChannelResolver()
	channels.rows[channelKey(ws.OrgID, "C1")] = &slackstore.Channel{OrgID: ws.OrgID, ChannelID: "C1"}
	r := &ChannelResolver{secrets: &fakeSecrets{err: context.DeadlineExceeded}, channels: channels, client: client}

	r.resolveChannelName(context.Background(), ws, "C1")

	if *hits != 0 {
		t.Errorf("conversations.info hits = %d; want 0 (bot token read failed)", *hits)
	}
	if row := channels.rows[channelKey(ws.OrgID, "C1")]; row.Name != "" {
		t.Errorf("Name = %q; want empty (never written on bot-token failure)", row.Name)
	}
}

// TestResolveChannelName_NoBotTokenConfigured never calls conversations.info
// when the workspace has no bot token secret at all (empty, not an error).
func TestResolveChannelName_NoBotTokenConfigured(t *testing.T) {
	client, hits := newConversationsInfoServer(t, map[string]any{"ok": true, "channel": map[string]any{"name": "general"}})
	channels := newFakeChannelRegistry()
	ws := testWorkspaceForChannelResolver()
	r := &ChannelResolver{secrets: &fakeSecrets{token: ""}, channels: channels, client: client}

	r.resolveChannelName(context.Background(), ws, "C1")

	if *hits != 0 {
		t.Errorf("conversations.info hits = %d; want 0 (no bot token configured)", *hits)
	}
}

// TestResolveChannelName_ConversationsInfoFailed never writes a name on an
// upstream API failure — an unresolved row must render its raw channel ID
// rather than risk a wrong fallback name.
func TestResolveChannelName_ConversationsInfoFailed(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
	})
	channels := newFakeChannelRegistry()
	ws := testWorkspaceForChannelResolver()
	channels.rows[channelKey(ws.OrgID, "C1")] = &slackstore.Channel{OrgID: ws.OrgID, ChannelID: "C1"}
	r := &ChannelResolver{secrets: &fakeSecrets{token: "xoxb-test"}, channels: channels, client: srv.Client()}

	r.resolveChannelName(context.Background(), ws, "C1")

	if row := channels.rows[channelKey(ws.OrgID, "C1")]; row.Name != "" {
		t.Errorf("Name = %q; want empty (never written on conversations.info failure)", row.Name)
	}
}

// TestResolveChannelName_EmptyNameNeverWrites: a well-formed but nameless
// response (ok:true, no channel.name) must not stamp an empty name over
// whatever the row already carries.
func TestResolveChannelName_EmptyNameNeverWrites(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{}})
	})
	channels := newFakeChannelRegistry()
	ws := testWorkspaceForChannelResolver()
	resolvedAt := time.Now().Add(-8 * 24 * time.Hour)
	channels.rows[channelKey(ws.OrgID, "C1")] = &slackstore.Channel{OrgID: ws.OrgID, ChannelID: "C1", Name: "old-name", NameResolvedAt: &resolvedAt}
	r := &ChannelResolver{secrets: &fakeSecrets{token: "xoxb-test"}, channels: channels, client: srv.Client()}

	r.resolveChannelName(context.Background(), ws, "C1")

	if row := channels.rows[channelKey(ws.OrgID, "C1")]; row.Name != "old-name" {
		t.Errorf("Name = %q; want old-name preserved (empty response name must not overwrite)", row.Name)
	}
}

// TestChannelResolverDispatch_DedupsConcurrentResolutions pins the
// thundering-herd guard: while a resolution for a channel is in flight
// (conversations.info deliberately held open here), further dispatch calls
// for the same channel must be no-ops rather than spawning their own
// goroutine and hitting the Slack API again.
func TestChannelResolverDispatch_DedupsConcurrentResolutions(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"name": "general"}})
	})
	channels := newFakeChannelRegistry()
	channels.done = make(chan struct{}, 1)
	ws := testWorkspaceForChannelResolver()
	channels.rows[channelKey(ws.OrgID, "C1")] = &slackstore.Channel{OrgID: ws.OrgID, ChannelID: "C1"}
	r := &ChannelResolver{secrets: &fakeSecrets{token: "xoxb-test"}, channels: channels, client: srv.Client()}

	r.dispatch(ws, "C1")

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
			t.Fatal("first dispatch never reached conversations.info")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Further dispatches while the first is still blocked in
	// conversations.info must not spawn another resolution.
	for i := 0; i < 5; i++ {
		r.dispatch(ws, "C1")
	}

	close(release)

	select {
	case <-channels.done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolution never completed")
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("conversations.info hits = %d; want 1 (concurrent dispatches for the same channel must dedup)", hits)
	}
}
