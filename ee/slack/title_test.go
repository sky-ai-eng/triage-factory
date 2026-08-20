package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fakeEntityTitleUpdater is a minimal entityTitleUpdater fake: an in-memory
// map of entity id -> title, mirroring permalink_test.go's fakeEntityURLUpdater
// (including its deterministic done signal for the detached resolver goroutine).
type fakeEntityTitleUpdater struct {
	mu     sync.Mutex
	titles map[string]string
	err    error
	done   chan struct{}
}

func newFakeEntityTitleUpdater() *fakeEntityTitleUpdater {
	return &fakeEntityTitleUpdater{titles: map[string]string{}}
}

func (f *fakeEntityTitleUpdater) UpdateTitleSystem(_ context.Context, _, entityID, title string) (domain.Entity, error) {
	f.mu.Lock()
	if f.err == nil {
		f.titles[entityID] = title
	}
	err := f.err
	f.mu.Unlock()
	if f.done != nil {
		f.done <- struct{}{}
	}
	return domain.Entity{ID: entityID, Title: title}, err
}

func (f *fakeEntityTitleUpdater) get(entityID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.titles[entityID]
}

var _ entityTitleUpdater = (*fakeEntityTitleUpdater)(nil)

func TestMentionTitle_StripsSlackMarkup(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<@U0BHY927K34> what can you do?", "what can you do?"},            // leading bot mention dropped
		{"ping <@U1|ada> about <#C2|deploys>", "ping @ada about #deploys"}, // named mentions kept
		{"see <https://x.test|the docs>", "see the docs"},                  // link label
		{"raw <https://x.test/y>", "raw https://x.test/y"},                 // bare link
		{"heads up <!here> and <!subteam^S1|oncall>", "heads up @here and @oncall"},
		{"a &amp; b &lt; c", "a & b < c"}, // escaped entities decoded
	}
	for _, c := range cases {
		if got := mentionTitle(c.in); got != c.want {
			t.Errorf("mentionTitle(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestComposeThreadTitle(t *testing.T) {
	cases := []struct{ name, channel, want string }{
		{"with channel", "general", "New thread messages in #general"},
		{"empty channel falls back to the channel-less form", "", slackThreadTitle},
	}
	for _, c := range cases {
		if got := composeThreadTitle(c.channel); got != c.want {
			t.Errorf("%s: composeThreadTitle(%q) = %q; want %q", c.name, c.channel, got, c.want)
		}
	}

	t.Run("caps a pathologically long channel name", func(t *testing.T) {
		long := strings.Repeat("z", 300)
		got := composeThreadTitle(long)
		if r := []rune(got); len(r) > slackTitleMaxRunes {
			t.Errorf("length = %d runes; want <= %d", len(r), slackTitleMaxRunes)
		}
	})
}

// TestHandleEventCallback_DispatchesTitleResolutionOnCreated is the call-site
// wiring test: a fresh thread entity dispatches resolveTitle, which resolves
// the channel name and rewrites entities.title into "New thread messages in
// #<channel>". Mirrors the identity/channel/permalink dispatch tests' shape;
// the fake Slack API answers conversations.info by path.
func TestHandleEventCallback_DispatchesTitleResolutionOnCreated(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "conversations.info"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"name": "general"}})
		default:
			t.Errorf("unexpected Slack API path %q", r.URL.Path)
		}
	})
	titles := newFakeEntityTitleUpdater()
	titles.done = make(chan struct{}, 1)
	resolver := &TitleResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: titles, client: srv.Client()}
	p := &ingestPipeline{
		entities:   newFakeEntities(),
		deliveries: newFakeDeliveries(),
		publish:    func(context.Context, domain.Event) {},
		title:      resolver,
	}
	ws := testWorkspaceRow("org-1")
	ev := inboundMention{
		Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U0SENDER",
		Text: "<@U0BHY927K34> what can you do?", TS: "1600000000.000100",
	}

	if err := p.handleEventCallback(context.Background(), ws, ev); err != nil {
		t.Fatalf("handleEventCallback: %v", err)
	}

	select {
	case <-titles.done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolveTitle never completed — handleEventCallback did not dispatch it")
	}

	entityID := "entity-org-1/slack/C1/1600000000.000100"
	if got, want := titles.get(entityID), "New thread messages in #general"; got != want {
		t.Errorf("title = %q; want %q", got, want)
	}
}
