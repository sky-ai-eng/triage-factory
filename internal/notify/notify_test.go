package notify

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/internal/sink"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

type testParams struct {
	ID     string   `json:"id"`
	TeamID string   `json:"team_id,omitempty"`
	Level  Priority `json:"level,omitempty"`
}

func testID(p testParams) string     { return p.ID }
func testTeamID(p testParams) string { return p.TeamID }

func testLevel(p testParams) Priority {
	if p.Level != "" {
		return p.Level
	}
	return Low
}

func testRender(p testParams, to Recipient) Content {
	return Content{Title: "thing " + p.ID + " is wrong", Body: "for " + string(to.Scope)}
}

var (
	// teamCondition is for the org's admins and the admins of the team its
	// params name.
	teamCondition = Condition(ConditionSpec[testParams]{
		Name: "test.team_condition", Source: "linear", Priority: PriorityOf(testLevel), Dismissible: true,
		Key: testID, Audience: Audiences(OrgAdmins(), TeamAdmins(testTeamID)),
		Render: testRender, Example: testParams{ID: "x", TeamID: exampleTeamID},
	})
	// linearCondition is for the org's admins and the admins of every team
	// tracking the Linear team its ID names.
	linearCondition = Condition(ConditionSpec[testParams]{
		Name: "test.linear_condition", Source: "linear", Priority: Always(High),
		Key: testID, Audience: Audiences(OrgAdmins(), TeamsTrackingLinearTeam(testID, Admins)),
		Render: testRender, Example: testParams{ID: "lt-1"},
	})
	// jiraCondition is for the admins of every team whose rules arm the Jira
	// project its ID names.
	jiraCondition = Condition(ConditionSpec[testParams]{
		Name: "test.jira_condition", Source: "jira", Priority: Always(High),
		Key: testID, Audience: TeamsArmingJiraProject(testID, Admins),
		Render: testRender, Example: testParams{ID: "jp-1"},
	})
	onlyTeamCondition = Condition(ConditionSpec[testParams]{
		Name: "test.only_team_condition", Priority: Always(High),
		Key: testID, Audience: TeamAdmins(testTeamID), Render: testRender, Example: testParams{ID: "x", TeamID: exampleTeamID},
	})
	membersCondition = Condition(ConditionSpec[testParams]{
		Name: "test.members_condition", Source: "jira", Priority: Always(High),
		Key: testID, Audience: OrgMembers(), Render: testRender, Example: testParams{ID: "x"},
	})
	testNotice = Notice(NoticeSpec[testParams]{
		Name: "test.notice", TTL: DefaultNoticeTTL, Key: testID, Audience: OrgMembers(),
		Render: func(p testParams, to Recipient) Content {
			c := testRender(p, to)
			c.Actions = []Action{Link("Open settings", Routes.OrgSettings())}
			return c
		},
		Example: testParams{ID: "x"},
	})
	panicCondition = Condition(ConditionSpec[testParams]{
		Name: "test.panic_condition", Priority: Always(Low),
		Key: testID, Audience: OrgMembers(),
		Render:  func(p testParams, _ Recipient) Content { panic("render " + p.ID) },
		Example: testParams{ID: "x"},
	})
)

// hubRecorder is a Broadcaster that keeps what it was sent.
type hubRecorder struct {
	mu     sync.Mutex
	events []websocket.Event
}

func (h *hubRecorder) Broadcast(e websocket.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
}

// take returns the envelopes sent since the last take, as "org" for an
// org-wide one and the user id otherwise, sorted.
func (h *hubRecorder) take(t *testing.T) []string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.events {
		if e.Type != EventType || e.Data != nil {
			t.Errorf("sent %+v; want a payload-free %s", e, EventType)
		}
		if e.UserID == "" {
			out = append(out, "org")
		} else {
			out = append(out, e.UserID)
		}
	}
	h.events = nil
	slices.Sort(out)
	return out
}

// install installs b for the test, restoring the previous sink after it.
func install(t *testing.T, b Backend) {
	prev := sink.Swap(nil)
	Install(b)
	t.Cleanup(func() { sink.Swap(prev) })
}

// failureCounter backs the failure counter with a manual reader for the
// test and returns a reader of the count per kind.
func failureCounter(t *testing.T) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := stats.Swap(newCounters(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { stats.Store(prev) })
	return func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "notify.write_failures" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					k, _ := dp.Attributes.Value("kind")
					out[k.AsString()] += dp.Value
				}
			}
		}
		return out
	}
}

// rows lists the kind's rows in the org, open and resolved, as an org admin
// may read them.
func rows(t *testing.T, stores db.Stores, orgID, viewer string, k Kind) []domain.Notification {
	t.Helper()
	views, _, err := stores.Notifications.ListForOrg(context.Background(), orgID, viewer, db.NotificationOrgListFilter{}, db.ListOpts{Limit: 200})
	if err != nil {
		t.Fatalf("ListForOrg: %v", err)
	}
	var out []domain.Notification
	for _, v := range views {
		if v.Kind == k.Name() {
			out = append(out, v.Notification)
		}
	}
	return out
}

func open(ns []domain.Notification) []domain.Notification {
	var out []domain.Notification
	for _, n := range ns {
		if n.ResolvedAt == nil {
			out = append(out, n)
		}
	}
	return out
}

func localBackend(t *testing.T) (db.Stores, *hubRecorder, *sql.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	stores := sqlitestore.New(conn)
	hub := &hubRecorder{}
	install(t, Backend{
		Tx: stores.Tx, Notifications: stores.Notifications, Orgs: stores.Orgs,
		LinearTeamRules: stores.LinearTeamRules, Hub: hub, Local: true,
	})
	return stores, hub, conn
}

func TestVerbs_NoOpBeforeInstall(t *testing.T) {
	prev := sink.Swap(nil)
	t.Cleanup(func() { sink.Swap(prev) })
	if Installed() {
		t.Fatal("Installed() before Install")
	}
	ctx := context.Background()
	teamCondition.Set(ctx, "org", testParams{ID: "a"}, true)
	teamCondition.Sync(ctx, "org", []testParams{{ID: "a"}})
	testNotice.Post(ctx, "org", testParams{ID: "a"})
	ClearSource(ctx, "org", "linear")
}

// Local is N=1: a kind with two audiences writes one row, for the one user,
// and every ping is one envelope for them. A bump of last_seen_at pings
// nobody.
func TestLocal_SetCoalescesAndPingsOnChangeOnly(t *testing.T) {
	stores, hub, _ := localBackend(t)
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	p := testParams{ID: "a", TeamID: runmode.LocalDefaultTeamID}

	teamCondition.Set(ctx, org, p, true)
	got := open(rows(t, stores, org, user, teamCondition))
	want := Recipient{Scope: domain.NotificationScopeUser, UserID: user}
	if len(got) != 1 || got[0].Audience != want || got[0].Title != "thing a is wrong" || got[0].Source != "linear" || !got[0].Dismissible {
		t.Fatalf("after Set(true): %+v; want one dismissible row for the local user", got)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{user}) {
		t.Errorf("open pinged %v; want one envelope for the local user", pings)
	}

	teamCondition.Set(ctx, org, p, true)
	if got := open(rows(t, stores, org, user, teamCondition)); len(got) != 1 || got[0].SeenCount != 2 {
		t.Fatalf("after a second Set(true): %+v; want the row bumped", got)
	}
	if pings := hub.take(t); len(pings) != 0 {
		t.Errorf("a bump pinged %v; want nothing", pings)
	}

	p.Level = Urgent
	teamCondition.Set(ctx, org, p, true)
	if got := open(rows(t, stores, org, user, teamCondition)); len(got) != 1 || got[0].Priority != Urgent || !got[0].EscalatedAt.After(got[0].FirstSeenAt) {
		t.Fatalf("after a raise at urgent: %+v; want the row escalated", got)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{user}) {
		t.Errorf("an escalation pinged %v; want one envelope for the local user", pings)
	}

	teamCondition.Set(ctx, org, p, false)
	all := rows(t, stores, org, user, teamCondition)
	if len(all) != 1 || all[0].ResolvedAt == nil || all[0].ResolvedReason != domain.NotificationResolvedCleared {
		t.Fatalf("after Set(false): %+v; want the row cleared", all)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{user}) {
		t.Errorf("a resolve pinged %v; want one envelope for the local user", pings)
	}
	teamCondition.Set(ctx, org, p, false)
	if pings := hub.take(t); len(pings) != 0 {
		t.Errorf("resolving nothing pinged %v", pings)
	}
}

// An archived team's audience is sent to nobody in local mode too, so a kind
// whose only live audience is one writes nothing.
func TestLocal_ArchivedTeamIsNobody(t *testing.T) {
	stores, hub, conn := localBackend(t)
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	const archived = "00000000-0000-4000-8000-00000000a7c1"
	if _, err := conn.Exec(`INSERT INTO teams (id, org_id, slug, name, deleted_at) VALUES (?, ?, 'archived', 'Archived', ?)`,
		archived, org, time.Now().UTC()); err != nil {
		t.Fatalf("seed archived team: %v", err)
	}
	onlyTeamCondition.Set(ctx, org, testParams{ID: "a", TeamID: archived}, true)
	if got := rows(t, stores, org, user, onlyTeamCondition); len(got) != 0 {
		t.Errorf("rows for an archived team's admins = %+v; want none", got)
	}
	onlyTeamCondition.Set(ctx, org, testParams{ID: "a", TeamID: runmode.LocalDefaultTeamID}, true)
	if got := open(rows(t, stores, org, user, onlyTeamCondition)); len(got) != 1 {
		t.Errorf("rows for the active team's admins = %+v; want one", got)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{user}) {
		t.Errorf("pinged %v; want one envelope for the local user", pings)
	}
}

func TestLocal_SyncResolvesWhatIsNotActive(t *testing.T) {
	stores, hub, _ := localBackend(t)
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	team := runmode.LocalDefaultTeamID

	teamCondition.Sync(ctx, org, []testParams{{ID: "a", TeamID: team}, {ID: "b", TeamID: team}, {ID: "b", TeamID: team}})
	if got := open(rows(t, stores, org, user, teamCondition)); len(got) != 2 {
		t.Fatalf("after Sync([a b b]): %d open rows; want 2", len(got))
	}
	hub.take(t)

	teamCondition.Sync(ctx, org, []testParams{{ID: "b", TeamID: team}})
	got := open(rows(t, stores, org, user, teamCondition))
	if len(got) != 1 || got[0].Key != "b" {
		t.Fatalf("after Sync([b]): %+v; want only b open", got)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{user}) {
		t.Errorf("resolving a pinged %v; want one envelope for the local user", pings)
	}

	teamCondition.Sync(ctx, org, nil)
	if got := open(rows(t, stores, org, user, teamCondition)); len(got) != 0 {
		t.Fatalf("after Sync(nil): %+v; want nothing open", got)
	}
}

// A kept member is one the pass has no answer for: its open row is left as it
// was, neither bumped nor resolved, and one that is not open stays closed.
func TestLocal_SyncLeavesWhatItKeeps(t *testing.T) {
	stores, hub, _ := localBackend(t)
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	team := runmode.LocalDefaultTeamID

	teamCondition.Sync(ctx, org, []testParams{{ID: "a", TeamID: team}, {ID: "b", TeamID: team}})
	hub.take(t)

	teamCondition.Sync(ctx, org, []testParams{{ID: "a", TeamID: team}}, testParams{ID: "b", TeamID: team}, testParams{ID: "c", TeamID: team})
	got := open(rows(t, stores, org, user, teamCondition))
	slices.SortFunc(got, func(x, y domain.Notification) int { return strings.Compare(x.Key, y.Key) })
	if len(got) != 2 || got[0].Key != "a" || got[0].SeenCount != 2 || got[1].Key != "b" || got[1].SeenCount != 1 {
		t.Fatalf("after Sync([a], keep b c): %+v; want a bumped, b open and untouched, and c never opened", got)
	}
	if pings := hub.take(t); len(pings) != 0 {
		t.Errorf("a bump and a kept row pinged %v; want nothing", pings)
	}

	teamCondition.Sync(ctx, org, nil)
	if got := open(rows(t, stores, org, user, teamCondition)); len(got) != 0 {
		t.Fatalf("after Sync(nil): %+v; want the kept row resolved once nothing keeps it", got)
	}
}

func TestLocal_PostAndClearSource(t *testing.T) {
	stores, _, _ := localBackend(t)
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID

	before := time.Now()
	testNotice.Post(ctx, org, testParams{ID: "n"})
	testNotice.Post(ctx, org, testParams{ID: "n"})
	notices := rows(t, stores, org, user, testNotice)
	if len(notices) != 1 || notices[0].Lifecycle != domain.NotificationNotice || notices[0].SeenCount != 2 ||
		notices[0].Priority != Low || !notices[0].Dismissible || notices[0].ExpiresAt == nil ||
		notices[0].ExpiresAt.Before(before.Add(DefaultNoticeTTL)) {
		t.Fatalf("after two Posts: %+v; want one low, dismissible notice expiring a TTL from now", notices)
	}

	membersCondition.Set(ctx, org, testParams{ID: "j"}, true)
	teamCondition.Set(ctx, org, testParams{ID: "l", TeamID: runmode.LocalDefaultTeamID}, true)
	ClearSource(ctx, org, "jira")
	if got := rows(t, stores, org, user, membersCondition); len(got) != 1 || got[0].ResolvedReason != domain.NotificationResolvedSourceOff {
		t.Errorf("jira condition after ClearSource(jira): %+v; want it resolved source_off", got)
	}
	if got := open(rows(t, stores, org, user, teamCondition)); len(got) != 1 {
		t.Errorf("linear condition after ClearSource(jira): %+v; want it still open", got)
	}
	if got := rows(t, stores, org, user, testNotice); len(got) != 1 || got[0].ResolvedAt != nil {
		t.Errorf("notice after ClearSource(jira): %+v; want it untouched", got)
	}
}

// A kind's own bug is counted and logged, never raised in the producer.
func TestPanickingKindIsCountedNotRaised(t *testing.T) {
	localBackend(t)
	count := failureCounter(t)
	panicCondition.Set(context.Background(), runmode.LocalDefaultOrgID, testParams{ID: "x"}, true)
	if got := count()[panicCondition.Name()]; got != 1 {
		t.Errorf("failures counted for %s = %d; want 1", panicCondition.Name(), got)
	}
}

// failingOrgs fails the settings read the Linear tracking resolver makes.
type failingOrgs struct{ db.OrgsStore }

func (failingOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, errors.New("settings unreadable")
}

// TestMulti_AudiencesAndPings runs against Postgres: the audience is resolved
// again on every write, a team that stops tracking the subject has its row
// resolved audience_removed, archived teams are left out, a ping reaches the
// users each changed row is sent to, and a resolution failure writes nothing.
func TestMulti_AudiencesAndPings(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, h, "notify-multi")
	tracking, archived, untracked := pgtest.SeedTeam(t, h, orgID, "tracking"), pgtest.SeedTeam(t, h, orgID, "archived"), pgtest.SeedTeam(t, h, orgID, "untracked")
	admin, member := pgtest.SeedUser(t, h, "team-admin"), pgtest.SeedUser(t, h, "team-member")
	pgtest.AddOrgMember(t, h, admin, orgID, tracking, "member", "admin")
	pgtest.AddOrgMember(t, h, member, orgID, tracking, "member", "member")
	archivedAdmin, untrackedAdmin := pgtest.SeedUser(t, h, "archived-admin"), pgtest.SeedUser(t, h, "untracked-admin")
	pgtest.AddOrgMember(t, h, archivedAdmin, orgID, archived, "member", "admin")
	pgtest.AddOrgMember(t, h, untrackedAdmin, orgID, untracked, "member", "admin")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := stores.Orgs.SetLinearWorkspace(ctx, orgID, "ws-1", "eng"); err != nil {
		t.Fatalf("SetLinearWorkspace: %v", err)
	}
	rules := []domain.LinearTeamRules{{LinearTeamID: "lt-1", LinearTeamKey: "ENG"}}
	for _, team := range []string{tracking, archived} {
		if _, err := stores.LinearTeamRules.ReplaceForTeam(ctx, team, "ws-1", rules); err != nil {
			t.Fatalf("track lt-1 on %s: %v", team, err)
		}
	}
	pgtest.MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, archived)

	hub := &hubRecorder{}
	backend := Backend{
		Tx: stores.Tx, Notifications: stores.Notifications, Orgs: stores.Orgs,
		LinearTeamRules: stores.LinearTeamRules, Hub: hub,
	}
	install(t, backend)
	p := testParams{ID: "lt-1"}
	orgAdmins := Recipient{Scope: domain.NotificationScopeOrg, Role: Admins}
	trackingAdmins := Recipient{Scope: domain.NotificationScopeTeam, Role: Admins, TeamID: tracking}

	linearCondition.Set(ctx, orgID, p, true)
	got := open(rows(t, stores, orgID, owner, linearCondition))
	audiences := []Recipient{}
	for _, n := range got {
		audiences = append(audiences, n.Audience)
	}
	if len(audiences) != 2 || !slices.Contains(audiences, orgAdmins) || !slices.Contains(audiences, trackingAdmins) {
		t.Fatalf("open audiences = %+v; want the org's admins and the tracking team's admins", audiences)
	}
	if pings, want := hub.take(t), sorted(owner, admin); !slices.Equal(pings, want) {
		t.Errorf("open pinged %v; want the org admin and the tracking team's admin %v", pings, want)
	}

	// The team stops tracking lt-1: its row resolves on the next write, and
	// only its recipients hear about it.
	if _, err := stores.LinearTeamRules.ReplaceForTeam(ctx, tracking, "ws-1", nil); err != nil {
		t.Fatalf("untrack: %v", err)
	}
	linearCondition.Set(ctx, orgID, p, true)
	for _, n := range rows(t, stores, orgID, owner, linearCondition) {
		switch n.Audience {
		case orgAdmins:
			if n.ResolvedAt != nil || n.SeenCount != 2 {
				t.Errorf("org admins row = %+v; want it open and bumped", n)
			}
		case trackingAdmins:
			if n.ResolvedReason != domain.NotificationResolvedAudienceRemoved {
				t.Errorf("former tracking team's row = %+v; want it resolved audience_removed", n)
			}
		}
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{admin}) {
		t.Errorf("audience removal pinged %v; want only the former tracking team's admin %s", pings, admin)
	}

	// An org members row is one org envelope.
	membersCondition.Set(ctx, orgID, testParams{ID: "m"}, true)
	if pings := hub.take(t); !slices.Equal(pings, []string{"org"}) {
		t.Errorf("an org members row pinged %v; want one org envelope", pings)
	}

	// A resolution failure writes nothing: it neither opens nor resolves.
	count := failureCounter(t)
	broken := backend
	broken.Orgs = failingOrgs{}
	install(t, broken)
	linearCondition.Set(ctx, orgID, testParams{ID: "lt-2"}, true)
	linearCondition.Sync(ctx, orgID, []testParams{{ID: "lt-2"}})
	if got := open(rows(t, stores, orgID, owner, linearCondition)); len(got) != 1 || got[0].Audience != orgAdmins || got[0].Key != "lt-1" {
		t.Errorf("open rows after failed resolutions = %+v; want only lt-1's org admins row", got)
	}
	if got := count()[linearCondition.Name()]; got != 2 {
		t.Errorf("failures counted = %d; want 2", got)
	}
	if pings := hub.take(t); len(pings) != 0 {
		t.Errorf("failed resolutions pinged %v", pings)
	}
}

// TestMulti_JiraTrackingAudience runs against Postgres: a Jira project's
// tracking audience is the admins of the active teams whose rules arm it, not
// a team that only watches it, and a team that stops arming it has its row
// resolved audience_removed on the next write.
func TestMulti_JiraTrackingAudience(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, h, "notify-jira")
	arming, archived, watching := pgtest.SeedTeam(t, h, orgID, "arming"), pgtest.SeedTeam(t, h, orgID, "archived"), pgtest.SeedTeam(t, h, orgID, "watching")
	armingAdmin, watchingAdmin := pgtest.SeedUser(t, h, "arming-admin"), pgtest.SeedUser(t, h, "watching-admin")
	pgtest.AddOrgMember(t, h, armingAdmin, orgID, arming, "member", "admin")
	pgtest.AddOrgMember(t, h, watchingAdmin, orgID, watching, "member", "admin")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	settings, err := stores.Orgs.SetSourceBaseURL(ctx, orgID, "jira", "https://acme.atlassian.net")
	if err != nil {
		t.Fatalf("SetSourceBaseURL: %v", err)
	}
	site := domain.EntityScope("jira", settings)
	project, err := stores.JiraProjects.FindOrCreate(ctx, orgID, site, "10000", "SKY", "Sky")
	if err != nil {
		t.Fatalf("register SKY: %v", err)
	}
	ref := func(name string) domain.JiraStatusRef { return domain.JiraStatusRef{ID: "st-" + name, Name: name} }
	armed := []domain.JiraProjectStatusRules{{
		JiraProjectID: project.ID, PickupMembers: []domain.JiraStatusRef{ref("To Do")},
		InProgressMembers: []domain.JiraStatusRef{ref("Doing")}, InProgressCanonical: ref("Doing"),
		DoneMembers: []domain.JiraStatusRef{ref("Done")}, DoneCanonical: ref("Done"),
	}}
	watched := []domain.JiraProjectStatusRules{{JiraProjectID: project.ID}}
	for team, rules := range map[string][]domain.JiraProjectStatusRules{arming: armed, archived: armed, watching: watched} {
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, team, site, rules); err != nil {
			t.Fatalf("rules for %s: %v", team, err)
		}
	}
	pgtest.MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, archived)

	hub := &hubRecorder{}
	install(t, Backend{
		Tx: stores.Tx, Notifications: stores.Notifications, Orgs: stores.Orgs,
		LinearTeamRules: stores.LinearTeamRules, JiraStatusRules: stores.JiraStatusRules, Hub: hub,
	})
	armingAdmins := Recipient{Scope: domain.NotificationScopeTeam, Role: Admins, TeamID: arming}
	p := testParams{ID: project.ID}

	jiraCondition.Set(ctx, orgID, p, true)
	if got := open(rows(t, stores, orgID, owner, jiraCondition)); len(got) != 1 || got[0].Audience != armingAdmins {
		t.Fatalf("open rows = %+v; want one for the arming team's admins", got)
	}
	if pings := hub.take(t); !slices.Equal(pings, []string{armingAdmin}) {
		t.Errorf("open pinged %v; want only the arming team's admin %s", pings, armingAdmin)
	}

	if err := stores.JiraStatusRules.ReplaceForTeam(ctx, arming, site, watched); err != nil {
		t.Fatalf("unarm: %v", err)
	}
	jiraCondition.Set(ctx, orgID, p, true)
	got := rows(t, stores, orgID, owner, jiraCondition)
	if len(got) != 1 || got[0].ResolvedReason != domain.NotificationResolvedAudienceRemoved {
		t.Errorf("rows after the team stops arming SKY = %+v; want its row resolved audience_removed", got)
	}
}

func sorted(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func TestRegistration_Refusals(t *testing.T) {
	render := func(testParams, Recipient) Content { return Content{Title: "t"} }
	for _, c := range []struct {
		name, want string
		register   func()
	}{
		{"duplicate name", "already registered", func() {
			Condition(ConditionSpec[testParams]{Name: teamCondition.Name(), Priority: Always(Low), Key: testID, Audience: OrgMembers(), Render: render})
		}},
		{"notice without a TTL", "needs a TTL", func() {
			Notice(NoticeSpec[testParams]{Name: "test.no_ttl", Key: testID, Audience: OrgMembers(), Render: render})
		}},
		{"condition without a priority", "needs a Priority", func() {
			Condition(ConditionSpec[testParams]{Name: "test.no_priority", Key: testID, Audience: OrgMembers(), Render: render})
		}},
		{"no audience", "Audience is required", func() {
			Condition(ConditionSpec[testParams]{Name: "test.no_audience", Priority: Always(Low), Key: testID, Render: render})
		}},
		{"audience reads other params", "audience team admins reads", func() {
			Condition(ConditionSpec[testParams]{Name: "test.wrong_audience", Priority: Always(Low), Key: testID, Render: render,
				Audience: TeamAdmins(func(s struct{ T string }) string { return s.T })})
		}},
		{"priority reads other params", "Priority reads", func() {
			Condition(ConditionSpec[testParams]{Name: "test.wrong_priority", Key: testID, Audience: OrgMembers(), Render: render,
				Priority: PriorityOf(func(string) Priority { return Low })})
		}},
		{"params not an object", "JSON object", func() {
			Condition(ConditionSpec[string]{Name: "test.string_params", Priority: Always(Low), Audience: OrgMembers(),
				Key: func(s string) string { return s }, Render: func(string, Recipient) Content { return Content{Title: "t"} }})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if msg, _ := r.(string); !strings.Contains(msg, c.want) {
					t.Errorf("panic = %v; want one naming %q", r, c.want)
				}
			}()
			c.register()
		})
	}
}

// countingStore signals each DeleteRetiredSystem call.
type countingStore struct {
	db.NotificationStore
	swept chan struct{}
}

func (s countingStore) DeleteRetiredSystem(context.Context, time.Time) (int64, error) {
	s.swept <- struct{}{}
	return 0, nil
}

// A newly promoted brain sweeps at once rather than an interval later.
func TestRunRetentionGC_SweepsOnStart(t *testing.T) {
	store := countingStore{swept: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunRetentionGC(ctx, store, time.Hour)
	select {
	case <-store.swept:
	case <-time.After(5 * time.Second):
		t.Fatal("no sweep within 5s of starting with an hour's interval")
	}
}

// Examples renders a kind for every recipient its audience can produce in
// multi mode and for local mode's one user: what the kinds conformance test
// walks.
func TestExamples_EveryRecipientShape(t *testing.T) {
	var got []Recipient
	for _, ex := range Examples(linearCondition) {
		if ex.Content.Title != "thing lt-1 is wrong" || ex.Priority != High {
			t.Errorf("example for %+v = %+v", ex.Recipient, ex)
		}
		got = append(got, ex.Recipient)
	}
	want := []Recipient{
		{Scope: domain.NotificationScopeOrg, Role: Admins},
		{Scope: domain.NotificationScopeTeam, Role: Admins, TeamID: exampleTeamID},
		{Scope: domain.NotificationScopeUser, UserID: runmode.LocalDefaultUserID},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Examples recipients = %+v; want %+v", got, want)
	}
	if !slices.ContainsFunc(Kinds(), func(k Kind) bool { return k.Name() == linearCondition.Name() }) {
		t.Errorf("Kinds() is missing %s", linearCondition.Name())
	}
}
