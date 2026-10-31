package notify

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// testEvent renders every event row with the rule it names, so a test can
// read which rule a row was written for.
var testEvent = Event(EventSpec{
	Render: func(p EventParams, _ Recipient) Content {
		return Content{Title: p.EventType + " " + p.EntityKey, Body: p.RuleName,
			Actions: []Action{Link("Open", External(p.EntityURL))}}
	},
})

// eventRowsOf lists the org's event rows of eventType, open and resolved:
// the ones an org admin may read, which are every row but another user's, and
// each of users' own.
func eventRowsOf(t *testing.T, stores db.Stores, orgID, admin, eventType string, users ...string) []domain.Notification {
	t.Helper()
	ctx := context.Background()
	views, _, err := stores.Notifications.ListForOrg(ctx, orgID, admin, db.NotificationOrgListFilter{}, db.ListOpts{Limit: 200})
	if err != nil {
		t.Fatalf("ListForOrg: %v", err)
	}
	for _, u := range users {
		own, _, err := stores.Notifications.ListForViewer(ctx, orgID, u, db.NotificationListFilter{ScopeIn: []domain.NotificationScope{domain.NotificationScopeUser}}, db.ListOpts{Limit: 200})
		if err != nil {
			t.Fatalf("ListForViewer(%s): %v", u, err)
		}
		views = append(views, own...)
	}
	var out []domain.Notification
	for _, v := range views {
		if v.Origin == domain.NotificationOriginEvent && v.Kind == eventType &&
			!slices.ContainsFunc(out, func(n domain.Notification) bool { return n.ID == v.ID }) {
			out = append(out, v.Notification)
		}
	}
	return out
}

// TestMulti_EventRaise runs against Postgres. A team rule tells its team's
// members; a personal rule tells its creator, unless the creator has left the
// rule's team or a matched team rule's team already reaches them. A condition
// coalesces quietly, a notice resurfaces, and the close phase's resolve
// reaches every audience of the key.
func TestMulti_EventRaise(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, h, "notify-event")
	infra, web := pgtest.SeedTeam(t, h, orgID, "infra"), pgtest.SeedTeam(t, h, orgID, "web")
	alice, bob, carol, dave := pgtest.SeedUser(t, h, "alice"), pgtest.SeedUser(t, h, "bob"), pgtest.SeedUser(t, h, "carol"), pgtest.SeedUser(t, h, "dave")
	pgtest.AddOrgMember(t, h, alice, orgID, infra, "member", "admin")
	pgtest.AddOrgMember(t, h, bob, orgID, infra, "member", "member")
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, bob, web)
	pgtest.AddOrgMember(t, h, carol, orgID, web, "member", "member")
	pgtest.AddOrgMember(t, h, dave, orgID, web, "member", "member")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, domain.ProviderGitHub, "https://github.com", "acme/widgets#18", "", "pr",
		"Retry the webhook", "https://github.com/acme/widgets/pull/18")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	hub := &hubRecorder{}
	install(t, Backend{Tx: stores.Tx, Notifications: stores.Notifications, Orgs: stores.Orgs, Hub: hub})

	rules := []EventRule{
		{HandlerID: "h-infra", TeamID: infra, Name: "Infra CI", Audience: domain.NotifyAudienceTeam},
		// bob is on Infra, whose team row reaches him.
		{HandlerID: "h-bob", TeamID: web, Name: "Bob's", Audience: domain.NotifyAudienceCreator, CreatorUserID: bob},
		// carol's rule is on Infra, a team she is not on.
		{HandlerID: "h-carol", TeamID: infra, Name: "Carol's", Audience: domain.NotifyAudienceCreator, CreatorUserID: carol},
		{HandlerID: "h-dave", TeamID: web, Name: "Dave's", Audience: domain.NotifyAudienceCreator, CreatorUserID: dave},
		{HandlerID: "h-dave-2", TeamID: web, Name: "Dave's second", Audience: domain.NotifyAudienceCreator, CreatorUserID: dave},
	}
	failed := EventRaise{EventID: "e1", EventType: domain.EventGitHubPRCICheckFailed, Source: "github", Entity: *entity, Condition: true, Rules: rules}
	testEvent.Raise(ctx, orgID, failed)

	got := eventRowsOf(t, stores, orgID, owner, domain.EventGitHubPRCICheckFailed, alice, bob, carol, dave)
	byAudience := map[Recipient]domain.Notification{}
	for _, n := range got {
		byAudience[n.Audience] = n
	}
	infraRow := Recipient{Scope: domain.NotificationScopeTeam, Role: Members, TeamID: infra}
	daveRow := Recipient{Scope: domain.NotificationScopeUser, UserID: dave}
	if len(got) != 2 || byAudience[infraRow].Body != "Infra CI" || byAudience[daveRow].Body != "Dave's" {
		t.Fatalf("rows = %+v; want Infra's members (its rule) and dave (his first rule), and no one else", got)
	}
	for _, n := range got {
		if n.Lifecycle != domain.NotificationCondition || n.Priority != High || !n.Dismissible || n.EntityID != entity.ID ||
			n.Key != EventKey(entity.ID, "") || n.Source != "github" {
			t.Errorf("row %+v; want a dismissible high condition about the entity, keyed by it", n)
		}
	}
	if pings, want := hub.take(t), sorted(alice, bob, dave); !slices.Equal(pings, want) {
		t.Errorf("open pinged %v; want %v", pings, want)
	}

	testEvent.Raise(ctx, orgID, failed)
	for _, n := range eventRowsOf(t, stores, orgID, owner, domain.EventGitHubPRCICheckFailed, alice, bob, carol, dave) {
		if n.SeenCount != 2 || !n.EscalatedAt.Equal(n.FirstSeenAt) {
			t.Errorf("a recurring condition = %+v; want it bumped and not resurfaced", n)
		}
	}
	if pings := hub.take(t); len(pings) != 0 {
		t.Errorf("a quiet bump pinged %v", pings)
	}

	commented := EventRaise{EventID: "e2", EventType: domain.EventGitHubPRReviewCommented, Source: "github", Entity: *entity, Rules: rules[3:4]}
	testEvent.Raise(ctx, orgID, commented)
	testEvent.Raise(ctx, orgID, commented)
	notices := eventRowsOf(t, stores, orgID, owner, domain.EventGitHubPRReviewCommented, dave)
	if len(notices) != 1 || notices[0].Lifecycle != domain.NotificationNotice || notices[0].ExpiresAt == nil ||
		notices[0].SeenCount != 2 || !notices[0].EscalatedAt.After(notices[0].FirstSeenAt) {
		t.Fatalf("notice rows = %+v; want one notice, resurfaced by its second event", notices)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{dave, dave}) {
		t.Errorf("a notice and its resurfacing pinged %v; want dave twice", pings)
	}

	targets, err := OpenEventConditions(ctx, orgID, entity.ID, []string{domain.EventGitHubPRCICheckFailed, domain.EventGitHubPRReviewCommented})
	if err != nil || !slices.Equal(targets, []EventTarget{{EventType: domain.EventGitHubPRCICheckFailed}}) {
		t.Fatalf("OpenEventConditions = %+v, %v; want only the CI condition", targets, err)
	}
	ResolveEvents(ctx, orgID, entity.ID, targets)
	for _, n := range eventRowsOf(t, stores, orgID, owner, domain.EventGitHubPRCICheckFailed, alice, bob, carol, dave) {
		if n.ResolvedAt == nil || n.ResolvedReason != domain.NotificationResolvedCleared {
			t.Errorf("after ResolveEvents = %+v; want every audience's row cleared", n)
		}
	}
	if pings, want := hub.take(t), sorted(alice, bob, dave); !slices.Equal(pings, want) {
		t.Errorf("the resolve pinged %v; want %v", pings, want)
	}

	// A raise whose rules reach nobody writes nothing.
	testEvent.Raise(ctx, orgID, EventRaise{EventType: domain.EventGitHubPRConflicts, Source: "github", Entity: *entity, Rules: rules[2:3]})
	if got := eventRowsOf(t, stores, orgID, owner, domain.EventGitHubPRConflicts, carol); len(got) != 0 {
		t.Errorf("rows for a rule whose creator left its team = %+v; want none", got)
	}
}

// Local is N=1: whichever rules match, an event raises one row for the one
// user, named by the first rule.
func TestLocal_EventRaise(t *testing.T) {
	stores, hub, conn := localBackend(t)
	ctx := context.Background()
	org, user, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID
	if _, err := conn.Exec(`INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', 'https://github.com')`, org); err != nil {
		t.Fatalf("seed github host: %v", err)
	}
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, org, domain.ProviderGitHub, "https://github.com", "acme/widgets#18", "", "pr",
		"Retry the webhook", "https://github.com/acme/widgets/pull/18")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	testEvent.Raise(ctx, org, EventRaise{EventType: domain.EventGitHubPRReviewRequested, Source: "github", DedupKey: "user:1", Entity: *entity, Condition: true,
		Rules: []EventRule{
			{HandlerID: "h1", TeamID: team, Name: "Mine", Audience: domain.NotifyAudienceCreator, CreatorUserID: user},
			{HandlerID: "h2", TeamID: team, Name: "The team's", Audience: domain.NotifyAudienceTeam},
		}})
	got := eventRowsOf(t, stores, org, user, domain.EventGitHubPRReviewRequested)
	if len(got) != 1 || got[0].Audience != (Recipient{Scope: domain.NotificationScopeUser, UserID: user}) ||
		got[0].Body != "The team's" || got[0].Key != EventKey(entity.ID, "user:1") {
		t.Fatalf("rows = %+v; want one row for the local user, keyed by the dedup key", got)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{user}) {
		t.Errorf("pinged %v; want the local user", pings)
	}
	targets, err := OpenEventConditions(ctx, org, entity.ID, []string{domain.EventGitHubPRReviewRequested})
	if err != nil || !slices.Equal(targets, []EventTarget{{EventType: domain.EventGitHubPRReviewRequested, DedupKey: "user:1"}}) {
		t.Fatalf("OpenEventConditions = %+v, %v; want the request, with its dedup key", targets, err)
	}
}

// An event row whose entity link cannot be shown keeps its other actions
// rather than telling nobody, and an entity closed by a path that routes no
// event has its conditions resolved by the closed-entity pass.
func TestLocal_EventRowOutlivesItsLinkAndItsEntity(t *testing.T) {
	stores, hub, conn := localBackend(t)
	ctx := context.Background()
	org, user, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID
	if _, err := conn.Exec(`INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', 'https://github.com')`, org); err != nil {
		t.Fatalf("seed github host: %v", err)
	}
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, org, domain.ProviderGitHub, "https://github.com", "acme/widgets#19", "", "pr",
		"Retry the webhook", "https://elsewhere.example/acme/widgets/pull/19")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	linked := Event(EventSpec{Render: func(p EventParams, _ Recipient) Content {
		return Content{Title: p.EntityKey, Body: p.RuleName, Actions: []Action{
			Link("Open", External(p.EntityURL)), Link("Edit your rule", Routes.NotifyRule(p.HandlerID))}}
	}})
	linked.Raise(ctx, org, EventRaise{EventType: domain.EventGitHubPRCICheckFailed, Source: "github", Entity: *entity, Condition: true,
		Rules: []EventRule{{HandlerID: "h1", TeamID: team, Name: "Mine", Audience: domain.NotifyAudienceCreator, CreatorUserID: user}}})
	got := eventRowsOf(t, stores, org, user, domain.EventGitHubPRCICheckFailed)
	if len(got) != 1 {
		t.Fatalf("rows = %+v; want the row, without the link its entity cannot give", got)
	}
	actions, err := decodeActions(got[0].Actions)
	if err != nil || len(actions) != 1 || actions[0].Label != "Edit your rule" {
		t.Errorf("actions = %+v, %v; want only the rule link", actions, err)
	}
	hub.take(t)

	ResolveClosedEntities(ctx, org)
	if got := eventRowsOf(t, stores, org, user, domain.EventGitHubPRCICheckFailed); len(got) != 1 || got[0].ResolvedAt != nil {
		t.Fatalf("after a pass with the entity active = %+v; want the condition open", got)
	}
	if _, err := stores.Entities.CloseSystem(ctx, org, entity.ID); err != nil {
		t.Fatalf("close entity: %v", err)
	}
	ResolveClosedEntities(ctx, org)
	if got := eventRowsOf(t, stores, org, user, domain.EventGitHubPRCICheckFailed); len(got) != 1 || got[0].ResolvedAt == nil {
		t.Errorf("after the entity closed = %+v; want the condition resolved", got)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{user}) {
		t.Errorf("the resolve pinged %v; want the local user", pings)
	}
}

// failingSettings is an OrgsStore whose settings read fails.
type failingSettings struct{ db.OrgsStore }

func (failingSettings) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, errors.New("settings unreadable")
}

// An unreadable org setting is not a refused link: the host check could not
// run, so the raise writes nothing and counts the failure rather than storing
// the row without its link for as long as it stays open.
func TestLocal_EventRaiseFailsOnUnreadableSettings(t *testing.T) {
	stores, hub, _ := localBackend(t)
	install(t, Backend{Tx: stores.Tx, Notifications: stores.Notifications, Orgs: failingSettings{stores.Orgs}, Hub: hub, Local: true})
	failures := failureCounter(t)
	ctx := context.Background()
	org, user, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, org, domain.ProviderGitHub, "https://github.com", "acme/widgets#20", "", "pr",
		"Retry the webhook", "https://github.com/acme/widgets/pull/20")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	linked := Event(EventSpec{Render: func(p EventParams, _ Recipient) Content {
		return Content{Title: p.EntityKey, Body: p.RuleName, Actions: []Action{
			Link("Open", External(p.EntityURL)), Link("Edit your rule", Routes.NotifyRule(p.HandlerID))}}
	}})
	linked.Raise(ctx, org, EventRaise{EventType: domain.EventGitHubPRCICheckFailed, Source: "github", Entity: *entity, Condition: true,
		Rules: []EventRule{{HandlerID: "h1", TeamID: team, Name: "Mine", Audience: domain.NotifyAudienceCreator, CreatorUserID: user}}})
	if got := eventRowsOf(t, stores, org, user, domain.EventGitHubPRCICheckFailed); len(got) != 0 {
		t.Errorf("rows = %+v; want none while the host check cannot run", got)
	}
	if got := failures()[eventLabel]; got != 1 {
		t.Errorf("event failures = %d; want 1", got)
	}
}

// A newly promoted brain sweeps at once: an entity closed outside the router
// before the brain started has its conditions resolved on the first pass.
func TestRunClosedEntitySweep_ResolvesOnStart(t *testing.T) {
	stores, _, conn := localBackend(t)
	ctx := context.Background()
	org, user, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID
	if _, err := conn.Exec(`INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', 'https://github.com')`, org); err != nil {
		t.Fatalf("seed github host: %v", err)
	}
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, org, domain.ProviderGitHub, "https://github.com", "acme/widgets#21", "", "pr",
		"Retry the webhook", "https://github.com/acme/widgets/pull/21")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	testEvent.Raise(ctx, org, EventRaise{EventType: domain.EventGitHubPRCICheckFailed, Source: "github", Entity: *entity, Condition: true,
		Rules: []EventRule{{HandlerID: "h1", TeamID: team, Name: "Mine", Audience: domain.NotifyAudienceCreator, CreatorUserID: user}}})
	if got := eventRowsOf(t, stores, org, user, domain.EventGitHubPRCICheckFailed); len(got) != 1 {
		t.Fatalf("rows = %+v; want the condition", got)
	}
	if _, err := stores.Entities.CloseSystem(ctx, org, entity.ID); err != nil {
		t.Fatalf("close entity: %v", err)
	}

	sweepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go RunClosedEntitySweep(sweepCtx, stores.Notifications, time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := eventRowsOf(t, stores, org, user, domain.EventGitHubPRCICheckFailed)
		if len(got) == 1 && got[0].ResolvedAt != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rows = %+v 5s after starting with an hour's interval; want the condition resolved", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
