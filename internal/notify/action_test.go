package notify

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

type externalParams struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

var (
	// actionCondition carries a link on every row, "retry" (org admins) on
	// org and user rows and "untrack" (team admins) on team rows.
	actionCondition = Condition(ConditionSpec[testParams]{
		Name: "test.action_condition", Priority: Always(High), Dismissible: true,
		Key: testID, Audience: Audiences(OrgAdmins(), TeamAdmins(testTeamID)),
		Render: func(p testParams, to Recipient) Content {
			c := testRender(p, to)
			c.Actions = []Action{Link("Open settings", Routes.OrgSettings())}
			if to.Scope == domain.NotificationScopeTeam {
				c.Actions = append(c.Actions, Invoke("untrack", "Stop tracking", RequiresTeamAdmin))
			} else {
				c.Actions = append(c.Actions, Invoke("retry", "Retry now", RequiresOrgAdmin, Confirm("Retry every owed memory now?")))
			}
			return c
		},
		Example: testParams{ID: "x", TeamID: exampleTeamID},
	})
	externalCondition = Condition(ConditionSpec[externalParams]{
		Name: "test.external_condition", Priority: Always(High),
		Key: func(p externalParams) string { return p.ID }, Audience: OrgAdmins(),
		Render: func(p externalParams, _ Recipient) Content {
			return Content{Title: "t", Actions: []Action{Link("Open it", External(p.URL))}}
		},
		Example: externalParams{ID: "x", URL: "https://github.com/acme/widgets/pull/7"},
	})
	// unhandledCondition renders "orphan", which nothing handles, and
	// "handled", which TestUnhandledActions registers.
	unhandledCondition = Condition(ConditionSpec[testParams]{
		Name: "test.unhandled_condition", Priority: Always(Low), Key: testID, Audience: OrgAdmins(),
		Render: func(testParams, Recipient) Content {
			return Content{Title: "t", Actions: []Action{
				Invoke("orphan", "Orphan", RequiresOrgAdmin), Invoke("handled", "Handled", RequiresOrgAdmin),
			}}
		},
		Example: testParams{ID: "x"},
	})
	// multiOnlyCondition links a page local mode does not have.
	multiOnlyCondition = Condition(ConditionSpec[testParams]{
		Name: "test.multi_only_condition", Priority: Always(Low), Key: testID, Audience: OrgAdmins(),
		Render: func(testParams, Recipient) Content {
			return Content{Title: "t", Actions: []Action{Link("Open", Href{route: &route{name: "multi_only", multi: "/org"}})}}
		},
		Example: testParams{ID: "x"},
	})
)

// fakeAuthz answers every check from its fields and counts the questions.
type fakeAuthz struct {
	orgAdmin   bool
	teamAdmins map[string]bool
	err        error
	asked      int
}

func (f *fakeAuthz) UserIsOrgAdmin(context.Context, string, string) (bool, error) {
	f.asked++
	return f.orgAdmin, f.err
}

func (f *fakeAuthz) UserIsTeamAdmin(_ context.Context, _, _, teamID string) (bool, error) {
	f.asked++
	return f.teamAdmins[teamID], f.err
}

func TestHref_Render(t *testing.T) {
	const org = "11111111-1111-4111-8111-111111111111"
	none := func(*url.URL) error { return nil }
	if got, err := Routes.OrgSettings().render(org, false, none); err != nil || got != "/orgs/"+org+"/org?tab=settings" {
		t.Errorf("OrgSettings in multi = %q, %v", got, err)
	}
	if got, err := Routes.OrgSettings().render(org, true, none); err != nil || got != "/settings" {
		t.Errorf("OrgSettings in local = %q, %v", got, err)
	}
	const team = "22222222-2222-4222-8222-222222222222"
	if got, err := Routes.TeamJiraRules(team).render(org, false, none); err != nil || got != "/orgs/"+org+"/team?source=jira&team="+team {
		t.Errorf("TeamJiraRules in multi = %q, %v", got, err)
	}
	if got, err := Routes.TeamLinearRules(team).render(org, true, none); err != nil || got != "/settings" {
		t.Errorf("TeamLinearRules in local = %q, %v", got, err)
	}
	if _, err := Routes.TeamJiraRules("").render(org, false, none); err == nil || !strings.Contains(err.Error(), "names no team") {
		t.Errorf("a team route with no team in multi: err = %v; want it refused", err)
	}
	// Local mode's one user is every row's recipient, a recipient that names
	// no team, and its page needs none.
	if got, err := Routes.TeamJiraRules("").render(org, true, none); err != nil || got != "/settings" {
		t.Errorf("a team route with no team in local = %q, %v; want /settings", got, err)
	}
	multiOnly := Href{route: &route{name: "multi_only", multi: "/org"}}
	if _, err := multiOnly.render(org, true, none); err == nil || !strings.Contains(err.Error(), "no page in local mode") {
		t.Errorf("a multi-only route in local: err = %v; want it refused", err)
	}
	if _, err := (Href{}).render(org, false, none); err == nil {
		t.Error("the zero Href rendered")
	}
	trigger := Routes.Trigger("trig-1")
	if got, err := trigger.render(org, false, none); err != nil || got != "/orgs/"+org+"/prompts?trigger=trig-1" {
		t.Errorf("Trigger in multi = %q, %v", got, err)
	}
	if got, err := trigger.render(org, true, none); err != nil || got != "/prompts?trigger=trig-1" {
		t.Errorf("Trigger in local = %q, %v", got, err)
	}
	if _, err := Routes.Trigger("").render(org, false, none); err == nil || !strings.Contains(err.Error(), "query trigger is empty") {
		t.Errorf("a Trigger naming no trigger: err = %v; want it refused", err)
	}
	withQuery := Href{route: orgSettingsRoute, query: url.Values{"source": {"jira"}}}
	if got, err := withQuery.render(org, false, none); err != nil || got != "/orgs/"+org+"/org?tab=settings&source=jira" {
		t.Errorf("a query on a route that has one = %q, %v; want the two joined", got, err)
	}

	settings := domain.OrgSettings{GitHubBaseURL: "https://GHE.example.com/", JiraBaseURL: "https://example.com/jira", LinearWorkspaceID: "ws"}
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"https://ghe.example.com/acme/widgets/pull/7", true},
		{"https://example.com/jira/browse/ENG-1", true},
		{"https://example.com/jira", true},
		{"https://linear.app/acme/issue/ENG-1", true},
		{"https://example.com/jira2/browse/ENG-1", false},
		{"https://example.com/browse/ENG-1", false},
		{"http://ghe.example.com/acme", false},
		{"https://evil.example/acme", false},
		{"https://ghe.example.com@evil.example/", false},
		{"/orgs/x/org", false},
		{"javascript:alert(1)", false},
		{"https://example.com/jira/../admin/reset", false},
		{"https://example.com/jira/%2e%2e/admin", false},
		{"https://example.com/jira/..%5cadmin", false},
		{"https://example.com/jira/./browse/ENG-1", false},
	} {
		got, err := External(c.url).render(org, false, configuredHosts(settings))
		if c.ok && (err != nil || got != c.url) {
			t.Errorf("External(%q) = %q, %v; want it accepted", c.url, got, err)
		}
		if !c.ok && err == nil {
			t.Errorf("External(%q) = %q; want it refused", c.url, got)
		}
	}
	settings.LinearWorkspaceID = ""
	if _, err := External("https://linear.app/acme/issue/ENG-1").render(org, false, configuredHosts(settings)); err == nil {
		t.Error("a Linear link for an org with no Linear workspace was accepted")
	}
}

func TestRenderActions(t *testing.T) {
	orgRow := Recipient{Scope: domain.NotificationScopeOrg, Role: Admins}
	teamRow := Recipient{Scope: domain.NotificationScopeTeam, Role: Admins, TeamID: exampleTeamID}
	none := func(*url.URL) error { return nil }
	link := Link("Open settings", Routes.OrgSettings())

	got, err := renderActions(domain.NotificationCondition, teamRow,
		[]Action{link, Invoke("untrack", "Stop tracking", RequiresTeamAdmin, Confirm("Stop?"))}, exampleOrgID, false, none)
	want := []storedAction{
		{ID: "org_settings", Type: linkAction, Label: "Open settings", Href: "/orgs/" + exampleOrgID + "/org?tab=settings"},
		{ID: "untrack", Type: invokeAction, Label: "Stop tracking", Requires: RequiresTeamAdmin, Confirm: "Stop?"},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("renderActions = %+v, %v; want %+v", got, err, want)
	}

	for _, c := range []struct {
		name      string
		lifecycle domain.NotificationLifecycle
		to        Recipient
		actions   []Action
		want      string
	}{
		{"three actions", domain.NotificationCondition, orgRow,
			[]Action{link, Invoke("a", "A", RequiresOrgAdmin), Invoke("b", "B", RequiresOrgAdmin)}, "at most 2"},
		{"a notice with none", domain.NotificationNotice, orgRow, nil, "a notice carries an action"},
		{"an invoke on a notice", domain.NotificationNotice, orgRow,
			[]Action{Invoke("a", "A", RequiresOrgAdmin)}, "a notice's actions are links"},
		{"a team admin's action on an org row", domain.NotificationCondition, orgRow,
			[]Action{Invoke("a", "A", RequiresTeamAdmin)}, "is for no team"},
		{"an id unfit for a path", domain.NotificationCondition, orgRow,
			[]Action{Invoke("Stop/Track", "A", RequiresOrgAdmin)}, "lowercase letters"},
		{"a permission notify does not check", domain.NotificationCondition, orgRow,
			[]Action{Invoke("a", "A", Permission("owner"))}, "not one notify checks"},
		{"two actions with one id", domain.NotificationCondition, orgRow, []Action{link, link}, "two actions with id"},
		{"no label", domain.NotificationCondition, orgRow, []Action{Invoke("a", " ", RequiresOrgAdmin)}, "no label"},
		{"the zero Action", domain.NotificationCondition, orgRow, []Action{{label: "x"}}, "not built by Link or Invoke"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := renderActions(c.lifecycle, c.to, c.actions, exampleOrgID, false, none); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v; want one naming %q", err, c.want)
			}
		})
	}
}

// Examples carries the write's own verdict, so the kinds conformance test
// refuses what a write would.
func TestExamples_ReportRefusedRenderings(t *testing.T) {
	for _, ex := range Examples(multiOnlyCondition) {
		if ex.Local != (ex.Err != nil) {
			t.Errorf("multi-only link for %+v (local %v): err = %v; want refused in local mode only", ex.Recipient, ex.Local, ex.Err)
		}
	}
	for _, ex := range Examples(actionCondition) {
		if ex.Err != nil {
			t.Errorf("actionCondition for %+v: %v", ex.Recipient, ex.Err)
		}
	}
}

func TestUnhandledActions(t *testing.T) {
	unhandledCondition.HandleAction("handled", func(context.Context, ActionCall[testParams]) (bool, error) { return false, nil })
	missing := UnhandledActions()
	if !slices.Contains(missing, "test.unhandled_condition/orphan") || slices.Contains(missing, "test.unhandled_condition/handled") {
		t.Errorf("UnhandledActions() = %v; want the orphan listed and the handled action not", missing)
	}
}

func TestHandleAction_Refusals(t *testing.T) {
	for _, c := range []struct {
		name, want string
		register   func()
	}{
		{"an id unfit for a path", "lowercase letters", func() {
			actionCondition.HandleAction("Retry Now", func(context.Context, ActionCall[testParams]) (bool, error) { return false, nil })
		}},
		{"no handler", "needs a handler", func() { actionCondition.HandleAction("retry", nil) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if msg, _ := recover().(string); !strings.Contains(msg, c.want) {
					t.Errorf("panic = %q; want one naming %q", msg, c.want)
				}
			}()
			c.register()
		})
	}
}

// Links are stored rendered for the mode, and an external link reaches the
// row only on a host the org has configured.
func TestLocal_ActionsAreStoredRendered(t *testing.T) {
	stores, _, _ := localBackend(t)
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID

	actionCondition.Set(ctx, org, testParams{ID: "a", TeamID: runmode.LocalDefaultTeamID}, true)
	got := open(rows(t, stores, org, user, actionCondition))
	if len(got) != 1 {
		t.Fatalf("rows = %+v; want one", got)
	}
	stored, err := decodeActions(got[0].Actions)
	want := []storedAction{
		{ID: "org_settings", Type: linkAction, Label: "Open settings", Href: "/settings"},
		{ID: "retry", Type: invokeAction, Label: "Retry now", Requires: RequiresOrgAdmin, Confirm: "Retry every owed memory now?"},
	}
	if err != nil || !slices.Equal(stored, want) {
		t.Errorf("stored actions = %+v, %v; want %+v", stored, err, want)
	}

	if _, err := stores.Orgs.SetSourceBaseURL(ctx, org, "github", "https://github.com"); err != nil {
		t.Fatalf("set the GitHub base URL: %v", err)
	}
	count := failureCounter(t)
	externalCondition.Set(ctx, org, externalParams{ID: "ok", URL: "https://github.com/acme/widgets/pull/7"}, true)
	externalCondition.Set(ctx, org, externalParams{ID: "off", URL: "https://elsewhere.example/acme"}, true)
	got = open(rows(t, stores, org, user, externalCondition))
	if len(got) != 1 || got[0].Key != "ok" {
		t.Fatalf("external rows = %+v; want only the one on the org's GitHub", got)
	}
	if !strings.Contains(string(got[0].Actions), `"href":"https://github.com/acme/widgets/pull/7"`) {
		t.Errorf("external row actions = %s", got[0].Actions)
	}
	if n := count()[externalCondition.Name()]; n != 1 {
		t.Errorf("failures counted = %d; want 1 for the unconfigured host", n)
	}
}

// A Sync member whose link is refused is skipped, not the pass: the others
// write, and its key is kept, so its open row is neither bumped nor resolved.
func TestLocal_SyncSkipsAnUnrenderedMember(t *testing.T) {
	stores, _, _ := localBackend(t)
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	if _, err := stores.Orgs.SetSourceBaseURL(ctx, org, "github", "https://github.com"); err != nil {
		t.Fatalf("set the GitHub base URL: %v", err)
	}
	good := func(id string) externalParams { return externalParams{ID: id, URL: "https://github.com/acme/" + id} }
	externalCondition.Sync(ctx, org, []externalParams{good("a"), good("b"), good("c")})

	count := failureCounter(t)
	externalCondition.Sync(ctx, org, []externalParams{good("a"), {ID: "b", URL: "https://elsewhere.example/b"}})
	seen := map[string]int{}
	for _, n := range open(rows(t, stores, org, user, externalCondition)) {
		seen[n.Key] = n.SeenCount
	}
	if want := map[string]int{"a": 2, "b": 1}; !maps.Equal(seen, want) {
		t.Errorf("open rows' seen counts = %v; want %v: a bumped, b left as it was, c resolved", seen, want)
	}
	if n := count()[externalCondition.Name()]; n != 1 {
		t.Errorf("failures counted = %d; want 1 for the refused member", n)
	}
}

// recordingHandler registers a handler for actionCondition's "retry" that
// keeps its calls and answers resolved and err.
func recordingHandler(t *testing.T, resolved bool, err error) *[]ActionCall[testParams] {
	t.Helper()
	var calls []ActionCall[testParams]
	actionCondition.HandleAction("retry", func(_ context.Context, call ActionCall[testParams]) (bool, error) {
		calls = append(calls, call)
		return resolved, err
	})
	return &calls
}

func TestLocal_RunAction(t *testing.T) {
	stores, hub, _ := localBackend(t)
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	actionCondition.Set(ctx, org, testParams{ID: "a", TeamID: runmode.LocalDefaultTeamID}, true)
	hub.take(t)
	row := open(rows(t, stores, org, user, actionCondition))[0]

	// Nothing an unauthorized caller is offered runs, and nothing runs that
	// the row does not declare.
	calls := recordingHandler(t, true, nil)
	denied := NewCaller(&fakeAuthz{}, org, user)
	views, err := denied.Actions(ctx, row)
	if err != nil || len(views) != 2 || views[0].Allowed != nil || views[1].Allowed == nil || *views[1].Allowed {
		t.Fatalf("Actions for a non-admin = %+v, %v; want the link and a disallowed retry", views, err)
	}
	if err := denied.RunAction(ctx, row, "retry"); !errors.Is(err, ErrActionForbidden) {
		t.Errorf("RunAction for a non-admin: err = %v; want ErrActionForbidden", err)
	}
	admin := &fakeAuthz{orgAdmin: true}
	caller := NewCaller(admin, org, user)
	for _, id := range []string{"org_settings", "untrack", "nope"} {
		if err := caller.RunAction(ctx, row, id); !errors.Is(err, ErrNoSuchAction) {
			t.Errorf("RunAction(%q): err = %v; want ErrNoSuchAction", id, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("the handler ran %d times for refused calls", len(*calls))
	}

	views, err = caller.Actions(ctx, row)
	if err != nil || views[1].Allowed == nil || !*views[1].Allowed || views[1].Confirm != "Retry every owed memory now?" {
		t.Fatalf("Actions for an admin = %+v, %v; want retry allowed, with its confirmation", views, err)
	}
	wire, _ := json.Marshal(views)
	if strings.Contains(string(wire), "requires") {
		t.Errorf("the wire shape carries the stored permission: %s", wire)
	}

	// A handler's error is the caller's, and the row stays open.
	boom := errors.New("boom")
	recordingHandler(t, true, boom)
	if err := caller.RunAction(ctx, row, "retry"); !errors.Is(err, boom) {
		t.Errorf("RunAction with a failing handler: err = %v; want it unchanged", err)
	}

	// Not fixed: the handler is given the row, and the row stays open.
	calls = recordingHandler(t, false, nil)
	if err := caller.RunAction(ctx, row, "retry"); err != nil {
		t.Fatalf("RunAction: %v", err)
	}
	wantCall := ActionCall[testParams]{OrgID: org, UserID: user, Recipient: row.Audience, Params: testParams{ID: "a", TeamID: runmode.LocalDefaultTeamID}}
	if len(*calls) != 1 || (*calls)[0] != wantCall {
		t.Errorf("handler calls = %+v; want %+v", *calls, wantCall)
	}
	if got := open(rows(t, stores, org, user, actionCondition)); len(got) != 1 {
		t.Errorf("open rows after an unfixed action = %+v; want the row open", got)
	}
	if pings := hub.take(t); len(pings) != 0 {
		t.Errorf("an unfixed action pinged %v", pings)
	}

	// Fixed: the row resolves at once, and its audience hears.
	recordingHandler(t, true, nil)
	if err := caller.RunAction(ctx, row, "retry"); err != nil {
		t.Fatalf("RunAction: %v", err)
	}
	all := rows(t, stores, org, user, actionCondition)
	if len(all) != 1 || all[0].ResolvedAt == nil || all[0].ResolvedReason != domain.NotificationResolvedCleared {
		t.Errorf("rows after a fixing action = %+v; want the row cleared", all)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{user}) {
		t.Errorf("a fixing action pinged %v; want the local user", pings)
	}
	if admin.asked != 1 {
		t.Errorf("one caller asked the org-admin question %d times; want once", admin.asked)
	}

	// Resolved: the fix no longer applies, to anyone.
	calls = recordingHandler(t, true, nil)
	views, err = caller.Actions(ctx, all[0])
	if err != nil || views[1].Allowed == nil || *views[1].Allowed {
		t.Errorf("Actions on a resolved row = %+v, %v; want retry disallowed", views, err)
	}
	if err := caller.RunAction(ctx, all[0], "retry"); !errors.Is(err, ErrConditionResolved) {
		t.Errorf("RunAction on a resolved row: err = %v; want ErrConditionResolved", err)
	}
	if len(*calls) != 0 {
		t.Errorf("the handler ran %d times on a resolved row", len(*calls))
	}
}

// A team row's action answers to that team's admins, and the answer is the
// same on the read and on the run.
func TestCaller_TeamAdmin(t *testing.T) {
	ctx := context.Background()
	const org, teamA, teamB = "11111111-1111-4111-8111-111111111111", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	var untracked []string
	actionCondition.HandleAction("untrack", func(_ context.Context, call ActionCall[testParams]) (bool, error) {
		untracked = append(untracked, call.Recipient.TeamID)
		return false, nil
	})
	row := func(teamID string) domain.Notification {
		to := Recipient{Scope: domain.NotificationScopeTeam, Role: Admins, TeamID: teamID}
		stored, err := renderActions(domain.NotificationCondition, to, actionCondition.k.render(testParams{ID: "a", TeamID: teamID}, to).Actions, org, false, func(*url.URL) error { return nil })
		if err != nil {
			t.Fatalf("renderActions: %v", err)
		}
		actions, _ := json.Marshal(stored)
		params, _ := json.Marshal(testParams{ID: "a", TeamID: teamID})
		return domain.Notification{ID: "n-" + teamID, OrgID: org, Kind: actionCondition.Name(), Audience: to, Actions: actions, Params: params}
	}
	az := &fakeAuthz{teamAdmins: map[string]bool{teamA: true}}
	caller := NewCaller(az, org, "u")
	for _, c := range []struct {
		team    string
		allowed bool
	}{{teamA, true}, {teamB, false}} {
		views, err := caller.Actions(ctx, row(c.team))
		if err != nil || views[1].ID != "untrack" || *views[1].Allowed != c.allowed {
			t.Errorf("Actions on team %s = %+v, %v; want untrack allowed=%v", c.team, views, err, c.allowed)
		}
		err = caller.RunAction(ctx, row(c.team), "untrack")
		if c.allowed && err != nil || !c.allowed && !errors.Is(err, ErrActionForbidden) {
			t.Errorf("RunAction on team %s: err = %v", c.team, err)
		}
	}
	if !slices.Equal(untracked, []string{teamA}) {
		t.Errorf("untracked %v; want only the team the caller administers", untracked)
	}
	if az.asked != 2 {
		t.Errorf("asked %d questions for two teams; want 2", az.asked)
	}

	// A row in another org, and a permission the row cannot answer, run
	// nothing.
	other := row(teamA)
	other.OrgID = "22222222-2222-4222-8222-222222222222"
	if err := caller.RunAction(ctx, other, "untrack"); !errors.Is(err, ErrNoSuchAction) {
		t.Errorf("RunAction on another org's row: err = %v; want ErrNoSuchAction", err)
	}
	orgRow := row(teamA)
	orgRow.Audience = Recipient{Scope: domain.NotificationScopeOrg, Role: Admins}
	if err := caller.RunAction(ctx, orgRow, "untrack"); !errors.Is(err, ErrActionForbidden) {
		t.Errorf("a team admin's action on an org row: err = %v; want ErrActionForbidden", err)
	}
}

// Multi mode stores each recipient's own actions, its links under the org.
func TestMulti_ActionsPerRecipient(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, owner, team := pgtest.SeedOrgWithUser(t, h, "notify-actions")
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	install(t, Backend{Tx: stores.Tx, Notifications: stores.Notifications, Orgs: stores.Orgs, LinearTeamRules: stores.LinearTeamRules})

	actionCondition.Set(ctx, orgID, testParams{ID: "a", TeamID: team}, true)
	got := map[domain.NotificationScope]string{}
	for _, n := range open(rows(t, stores, orgID, owner, actionCondition)) {
		var stored []storedAction
		if err := json.Unmarshal(n.Actions, &stored); err != nil || len(stored) != 2 || stored[0].Href != "/orgs/"+orgID+"/org?tab=settings" {
			t.Errorf("%s row actions = %s, %v", n.Audience.Scope, n.Actions, err)
			continue
		}
		got[n.Audience.Scope] = stored[1].ID
	}
	if got[domain.NotificationScopeOrg] != "retry" || got[domain.NotificationScopeTeam] != "untrack" {
		t.Errorf("invoke per scope = %v; want retry on the org row and untrack on the team row", got)
	}
}
