package kinds_test

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// pings keeps the user ids the notify ping was sent to.
type pings struct {
	mu    sync.Mutex
	users []string
}

func (p *pings) Broadcast(e websocket.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users = append(p.users, e.UserID)
}

func (p *pings) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.users
	p.users = nil
	slices.Sort(out)
	return out
}

// TestAutomationConflict_OneRowPerLosingTeam posts a conflict two teams lost
// and checks that each losing team's admins get a row of their own, linking
// to that team's trigger, and that a repeat on the task coalesces.
func TestAutomationConflict_OneRowPerLosingTeam(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, owner, winner := pgtest.SeedOrgWithUser(t, h, "conflict-owner")
	lostA, lostB := pgtest.SeedTeam(t, h, orgID, "lost-a"), pgtest.SeedTeam(t, h, orgID, "lost-b")
	adminA, memberA, adminB := pgtest.SeedUser(t, h, "admin-a"), pgtest.SeedUser(t, h, "member-a"), pgtest.SeedUser(t, h, "admin-b")
	pgtest.AddOrgMember(t, h, adminA, orgID, lostA, "member", "admin")
	pgtest.AddOrgMember(t, h, memberA, orgID, lostA, "member", "member")
	pgtest.AddOrgMember(t, h, adminB, orgID, lostB, "member", "admin")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	hub := &pings{}
	notifytest.Isolate(t)
	notify.Install(notify.Backend{
		Tx: stores.Tx, Notifications: stores.Notifications, Orgs: stores.Orgs,
		LinearTeamRules: stores.LinearTeamRules, Hub: hub,
	})

	triggerOf := map[string]string{lostA: "trigger-a", lostB: "trigger-b"}
	conflict := kinds.Conflict{
		TaskID: "task-1", EventType: domain.EventGitHubPRCICheckFailed, Subject: "acme/widgets#18",
		Winner: kinds.ConflictTeam{TeamID: winner, Name: "Platform"},
		Losers: []kinds.ConflictLoser{{TeamID: lostA, TriggerID: triggerOf[lostA]}, {TeamID: lostB, TriggerID: triggerOf[lostB]}},
	}
	kinds.AutomationConflict.Post(ctx, orgID, conflict)

	got := conflictRows(t, stores, orgID, owner)
	if len(got) != 2 {
		t.Fatalf("rows = %+v; want one per losing team", got)
	}
	for _, n := range got {
		if n.Audience.Scope != domain.NotificationScopeTeam || n.Audience.Role != domain.NotificationRoleAdmins {
			t.Errorf("row audience = %+v; want a team's admins", n.Audience)
		}
		if n.Lifecycle != domain.NotificationNotice || n.Priority != domain.NotificationLow || n.ExpiresAt == nil || n.Key != "task-1" {
			t.Errorf("row = %+v; want a low notice keyed on the task", n)
		}
		want := triggerOf[n.Audience.TeamID]
		if want == "" {
			t.Errorf("a row for team %s, which did not lose", n.Audience.TeamID)
			continue
		}
		var actions []struct{ Href string }
		if err := json.Unmarshal(n.Actions, &actions); err != nil || len(actions) != 1 {
			t.Fatalf("actions %s: %v; want one link", n.Actions, err)
		}
		u, err := url.Parse(actions[0].Href)
		if err != nil {
			t.Fatalf("href %q: %v", actions[0].Href, err)
		}
		if u.Path != "/orgs/"+orgID+"/prompts" || u.Query().Get("trigger") != want {
			t.Errorf("team %s links to %q; want its own trigger %s in the prompts editor", n.Audience.TeamID, actions[0].Href, want)
		}
	}
	if got, want := hub.take(), sortedIDs(adminA, adminB); !slices.Equal(got, want) {
		t.Errorf("pinged %v; want the losing teams' admins %v, and not their members", got, want)
	}

	kinds.AutomationConflict.Post(ctx, orgID, conflict)
	for _, n := range conflictRows(t, stores, orgID, owner) {
		if n.SeenCount != 2 {
			t.Errorf("team %s row seen %d times after a repeat; want it coalesced to 2", n.Audience.TeamID, n.SeenCount)
		}
	}
	if got := hub.take(); len(got) != 0 {
		t.Errorf("a repeat pinged %v; a bump pings nobody", got)
	}
}

func conflictRows(t *testing.T, stores db.Stores, orgID, viewer string) []domain.Notification {
	t.Helper()
	views, _, err := stores.Notifications.ListForOrg(context.Background(), orgID, viewer, db.NotificationOrgListFilter{}, db.ListOpts{Limit: 200})
	if err != nil {
		t.Fatalf("ListForOrg: %v", err)
	}
	var out []domain.Notification
	for _, v := range views {
		if v.Kind == kinds.AutomationConflict.Name() {
			out = append(out, v.Notification)
		}
	}
	return out
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}
