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
	cases := []struct{ name, author, channel, want string }{
		{"author and channel address the thread", "Ada Lovelace", "general", "Ada Lovelace in #general"},
		{"channel alone keeps the anonymous subject", "", "general", "New thread messages in #general"},
		{"author alone keeps the anonymous form — half an address is worse", "Ada Lovelace", "", slackThreadTitle},
		{"neither resolved", "", "", slackThreadTitle},
	}
	for _, c := range cases {
		if got := composeThreadTitle(c.author, c.channel); got != c.want {
			t.Errorf("%s: composeThreadTitle(%q, %q) = %q; want %q", c.name, c.author, c.channel, got, c.want)
		}
	}

	for _, c := range []struct{ name, author, channel string }{
		{"caps a pathologically long channel name", "", strings.Repeat("z", 300)},
		{"caps a pathologically long author name", strings.Repeat("q", 300), "general"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := composeThreadTitle(c.author, c.channel)
			if r := []rune(got); len(r) > slackTitleMaxRunes {
				t.Errorf("length = %d runes; want <= %d", len(r), slackTitleMaxRunes)
			}
		})
	}
}

// fakeTitleAPI answers the three endpoints resolveTitle can reach and records
// what each was asked, so a test can assert the call pattern that produced a
// title as well as the title itself — the summons arm's extra
// conversations.replies hop, and the root arm's absence of one.
type fakeTitleAPI struct {
	mu sync.Mutex

	channelName string
	channelErr  bool

	rootUser   string // the user conversations.replies attributes the root to
	rootBotID  string
	repliesErr bool

	displayName string
	realName    string
	isBot       bool
	deleted     bool
	usersErr    bool
	// displayNameByUser, when set, answers users.info's display_name per user
	// id instead of displayName — the wiring test needs two distinct people to
	// tell the two author arms apart by the title each produces.
	displayNameByUser map[string]string

	infoCalls    int
	repliesCalls int
	usersCalls   int
	repliesTS    string
	usersUserID  string
}

func (f *fakeTitleAPI) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "conversations.info"):
			f.infoCalls++
			if f.channelErr {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"name": f.channelName}})
		case strings.Contains(r.URL.Path, "conversations.replies"):
			f.repliesCalls++
			f.repliesTS = r.URL.Query().Get("ts")
			if f.repliesErr {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "thread_not_found"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{
				{"user": f.rootUser, "bot_id": f.rootBotID, "ts": f.repliesTS},
			}})
		case strings.Contains(r.URL.Path, "users.info"):
			f.usersCalls++
			f.usersUserID = r.URL.Query().Get("user")
			if f.usersErr {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
				return
			}
			displayName := f.displayName
			if f.displayNameByUser != nil {
				displayName = f.displayNameByUser[f.usersUserID]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{
				"is_bot":  f.isBot,
				"deleted": f.deleted,
				"profile": map[string]any{"display_name": displayName, "real_name": f.realName},
			}})
		default:
			t.Errorf("unexpected Slack API path %q", r.URL.Path)
		}
	}
}

// TestResolveTitle covers the composed title and the calls behind it for every
// arm resolveTitle has. It drives resolveTitle directly rather than through
// dispatch — the detached goroutine is the wiring tests' subject below, and
// calling the method synchronously keeps these assertions free of timing.
func TestResolveTitle(t *testing.T) {
	const (
		entityID    = "entity-1"
		mentionUser = "U0MENTIONER"
		rootAuthor  = "U0AUTHOR"
	)
	cases := []struct {
		name        string
		api         *fakeTitleAPI
		ref         threadTitleRef
		wantTitle   string // "" means the resolver must not write at all
		wantReplies int
		wantUsers   int
		wantUserID  string
	}{
		{
			name: "root mention: the mentioning user IS the author, no replies lookup",
			api:  &fakeTitleAPI{channelName: "chan", displayName: "ada"},
			ref:  threadTitleRef{entityID: entityID, channel: "C1", rootTS: "1.1", mentionUser: mentionUser, isRoot: true},

			wantTitle: "ada in #chan", wantReplies: 0, wantUsers: 1, wantUserID: mentionUser,
		},
		{
			name: "summons: the root's author, not the mentioner, names the thread",
			api:  &fakeTitleAPI{channelName: "chan", rootUser: rootAuthor, displayName: "grace"},
			ref:  threadTitleRef{entityID: entityID, channel: "C1", rootTS: "1.1", mentionUser: mentionUser, isRoot: false},

			wantTitle: "grace in #chan", wantReplies: 1, wantUsers: 1, wantUserID: rootAuthor,
		},
		{
			name: "display_name unset falls back to the profile's real name",
			api:  &fakeTitleAPI{channelName: "chan", realName: "Ada Lovelace"},
			ref:  threadTitleRef{entityID: entityID, channel: "C1", mentionUser: mentionUser, isRoot: true},

			wantTitle: "Ada Lovelace in #chan", wantUsers: 1, wantUserID: mentionUser,
		},
		{
			name: "users.info failure keeps the channel-only title",
			api:  &fakeTitleAPI{channelName: "chan", usersErr: true},
			ref:  threadTitleRef{entityID: entityID, channel: "C1", mentionUser: mentionUser, isRoot: true},

			wantTitle: "New thread messages in #chan", wantUsers: 1, wantUserID: mentionUser,
		},
		{
			name: "a bot-authored root has no human to name",
			api:  &fakeTitleAPI{channelName: "chan", rootBotID: "B1", displayName: "ada"},
			ref:  threadTitleRef{entityID: entityID, channel: "C1", rootTS: "1.1", mentionUser: mentionUser, isRoot: false},

			wantTitle: "New thread messages in #chan", wantReplies: 1, wantUsers: 0,
		},
		{
			name: "a bot profile is not an author either",
			api:  &fakeTitleAPI{channelName: "chan", isBot: true, displayName: "some-app"},
			ref:  threadTitleRef{entityID: entityID, channel: "C1", mentionUser: mentionUser, isRoot: true},

			wantTitle: "New thread messages in #chan", wantUsers: 1, wantUserID: mentionUser,
		},
		{
			name: "conversations.replies failure keeps the channel-only title",
			api:  &fakeTitleAPI{channelName: "chan", repliesErr: true},
			ref:  threadTitleRef{entityID: entityID, channel: "C1", rootTS: "1.1", mentionUser: mentionUser, isRoot: false},

			wantTitle: "New thread messages in #chan", wantReplies: 1, wantUsers: 0,
		},
		{
			// The skip rule: without a channel the composed title equals the
			// anonymous one already on the entity whoever the author is, so the
			// resolver writes nothing — and doesn't spend the author's calls
			// learning that.
			name: "an unresolved channel writes nothing, author or not",
			api:  &fakeTitleAPI{channelErr: true, displayName: "ada"},
			ref:  threadTitleRef{entityID: entityID, channel: "C1", rootTS: "1.1", mentionUser: mentionUser, isRoot: false},

			wantTitle: "", wantReplies: 0, wantUsers: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withFakeSlackAPI(t, c.api.handler(t))
			titles := newFakeEntityTitleUpdater()
			r := &TitleResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: titles, client: http.DefaultClient}

			r.resolveTitle(context.Background(), testWorkspaceRow("org-1"), c.ref)

			if got := titles.get(entityID); got != c.wantTitle {
				t.Errorf("title = %q; want %q", got, c.wantTitle)
			}
			c.api.mu.Lock()
			defer c.api.mu.Unlock()
			if c.api.repliesCalls != c.wantReplies {
				t.Errorf("conversations.replies calls = %d; want %d", c.api.repliesCalls, c.wantReplies)
			}
			if c.api.usersCalls != c.wantUsers {
				t.Errorf("users.info calls = %d; want %d", c.api.usersCalls, c.wantUsers)
			}
			if c.wantUserID != "" && c.api.usersUserID != c.wantUserID {
				t.Errorf("users.info resolved %q; want %q", c.api.usersUserID, c.wantUserID)
			}
		})
	}
}

// TestHandleEventCallback_DispatchesTitleResolutionOnCreated is the call-site
// wiring test: a fresh thread entity dispatches resolveTitle, which resolves
// both display names and rewrites entities.title into "<author> in #<channel>".
// Both mention kinds are covered, because the kind is what decides where the
// thread's author comes from — and it reaches the resolver only if the dispatch
// carries the root ts and isRoot correctly. Mirrors the identity/channel/
// permalink dispatch tests' shape; the fake Slack API answers by path.
func TestHandleEventCallback_DispatchesTitleResolutionOnCreated(t *testing.T) {
	const (
		rootTS    = "1600000000.000050"
		mentionTS = "1600000000.000100"
	)
	cases := []struct {
		name          string
		threadTS      string // the mention's thread_ts: empty = the mention IS the root
		wantEntityID  string
		wantTitle     string
		wantRepliesTS string // "" = conversations.replies must not be called
	}{
		{
			name:         "a root mention names its own sender",
			threadTS:     "",
			wantEntityID: "entity-org-1/slack/C1/" + mentionTS,
			wantTitle:    "ada in #general",
		},
		{
			name:          "a summons names whoever rooted the thread",
			threadTS:      rootTS,
			wantEntityID:  "entity-org-1/slack/C1/" + rootTS,
			wantTitle:     "grace in #general",
			wantRepliesTS: rootTS,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The mentioning user resolves to "ada" and the root's author to
			// "grace", so the written title names which one the arm picked.
			api := &fakeTitleAPI{
				channelName:       "general",
				rootUser:          "U0ROOTAUTHOR",
				displayNameByUser: map[string]string{"U0SENDER": "ada", "U0ROOTAUTHOR": "grace"},
			}
			withFakeSlackAPI(t, api.handler(t))

			titles := newFakeEntityTitleUpdater()
			titles.done = make(chan struct{}, 1)
			p := &ingestPipeline{
				entities:   newFakeEntities(),
				deliveries: newFakeDeliveries(),
				publish:    func(context.Context, domain.Event) {},
				title:      &TitleResolver{secrets: &fakeSecrets{token: "xoxb-test"}, entities: titles, client: http.DefaultClient},
			}
			ev := inboundMention{
				Type: "app_mention", EventID: "Ev1", Channel: "C1", User: "U0SENDER",
				Text: "<@U0BHY927K34> what can you do?", TS: mentionTS, ThreadTS: c.threadTS,
			}

			if err := p.handleEventCallback(context.Background(), testWorkspaceRow("org-1"), ev); err != nil {
				t.Fatalf("handleEventCallback: %v", err)
			}

			select {
			case <-titles.done:
			case <-time.After(2 * time.Second):
				t.Fatal("resolveTitle never completed — handleEventCallback did not dispatch it")
			}

			if got := titles.get(c.wantEntityID); got != c.wantTitle {
				t.Errorf("title = %q; want %q", got, c.wantTitle)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.repliesTS != c.wantRepliesTS {
				t.Errorf("conversations.replies ts = %q; want %q", api.repliesTS, c.wantRepliesTS)
			}
		})
	}
}
