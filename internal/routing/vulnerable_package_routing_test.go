package routing

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// vulnFixture is a database where teams A and B track octo/api, team C
// tracks only octo/web, and team D tracks nothing, with an active vulnerable
// package lodash (npm) in octo/api. Its router reads the real team↔repo gate
// and delegates to stub. None of the teams is runmode.LocalDefaultTeamID,
// which the router reads as an org-visible handler's team and maps to the
// task's owner, and their ids sort A, B, C, D.
type vulnFixture struct {
	db                      *sql.DB
	router                  *Router
	stub                    *stubDelegator
	entity                  *domain.Entity
	teamA, teamB, teamC, tD string
}

func newVulnFixture(t *testing.T) *vulnFixture {
	t.Helper()
	database := newGateDB(t)
	st := sqlitestore.New(database)
	ctx := context.Background()
	team := func(id, slug string) string {
		t.Helper()
		if _, err := database.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`, id, runmode.LocalDefaultOrgID, slug, slug); err != nil {
			t.Fatalf("seed team %s: %v", slug, err)
		}
		return id
	}
	f := &vulnFixture{
		db:    database,
		teamA: team("00000000-0000-0000-0000-0000000000a1", "team-a"),
		teamB: team("00000000-0000-0000-0000-0000000000b1", "team-b"),
		teamC: team("00000000-0000-0000-0000-0000000000c1", "team-c"),
		tD:    team("00000000-0000-0000-0000-0000000000d1", "team-d"),
	}
	track := func(team string, repos ...domain.TeamGitHubRepo) {
		t.Helper()
		if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, runmode.LocalDefaultOrgID, team, dbtest.TestGitHubHost, repos); err != nil {
			t.Fatalf("track repos for %s: %v", team, err)
		}
	}
	track(f.teamA, domain.TeamGitHubRepo{Owner: "octo", Repo: "api"})
	track(f.teamB, domain.TeamGitHubRepo{Owner: "octo", Repo: "api"})
	track(f.teamC, domain.TeamGitHubRepo{Owner: "octo", Repo: "web"})

	e, _, err := st.Entities.FindOrCreateVulnerablePackageSystem(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, "octo/api#npm:lodash", "lodash (npm)", "")
	if err != nil {
		t.Fatalf("create package: %v", err)
	}
	if e.RepositoryID == "" {
		t.Fatal("the package names no repository")
	}
	f.entity = e
	f.stub = &stubDelegator{db: database}
	deps := gateRouterDeps(database)
	deps.Spawner = f.stub
	f.router = newTestRouter(deps)
	return f
}

// seedVulnHandler inserts an enabled alert_opened handler of kind on teamID,
// filtered by predicate ("" matches every alert), with applies_to_unowned set
// to watch. A rule carries priority; a trigger fires immediately, on its own
// team's blueprint, and returns its id.
func seedVulnHandler(t *testing.T, database *sql.DB, kind, teamID, predicate string, priority float64, watch bool) string {
	t.Helper()
	id := uuid.New().String()
	var pred any
	if predicate != "" {
		pred = predicate
	}
	now := time.Now()
	var err error
	switch kind {
	case domain.EventHandlerKindRule:
		_, err = database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source, applies_to_unowned,
				 name, default_priority, sort_order, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'rule', ?, ?, 1, 'user', ?, ?, ?, 100, ?, ?)
		`, id, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, domain.EventGitHubVulnerabilityAlertOpened,
			pred, watch, "vuln rule "+id[:8], priority, now, now)
	case domain.EventHandlerKindTrigger:
		promptID := "p-" + id[:8]
		insertPromptForTeam(t, database, promptID, teamID)
		bp := insertBlueprintForTeam(t, database, "bp-"+id[:8], promptID, teamID)
		_, err = database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source, applies_to_unowned,
				 blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'trigger', ?, ?, 1, 'user', ?, ?, 4, 0, ?, ?)
		`, id, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, domain.EventGitHubVulnerabilityAlertOpened,
			pred, watch, bp, now, now)
	case domain.EventHandlerKindNotify:
		_, err = database.Exec(`
			INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type,
				 scope_predicate_json, enabled, source, name, notify_audience, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'notify', ?, ?, 1, 'user', ?, ?, ?, ?)
		`, id, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, domain.EventGitHubVulnerabilityAlertOpened,
			pred, "vuln notify "+id[:8], domain.NotifyAudienceTeam, now, now)
	}
	if err != nil {
		t.Fatalf("seed %s for %s: %v", kind, teamID, err)
	}
	return id
}

func vulnAlert(number int, severity string) domain.DependabotAlert {
	return domain.DependabotAlert{
		Number: number, GHSAID: fmt.Sprintf("GHSA-%d", number), Severity: severity,
		ManifestPath: "package.json", Scope: domain.DependabotScopeRuntime, FirstPatchedVersion: "4.17.21",
	}
}

func lodashSnapshot(alerts ...domain.DependabotAlert) domain.VulnerablePackageSnapshot {
	return domain.VulnerablePackageSnapshot{Repo: "octo/api", Ecosystem: "npm", Package: "lodash", Alerts: alerts}
}

// listing routes the events the package's snapshot moving from prev to curr
// implies, built by the tracker's own diff.
func (f *vulnFixture) listing(t *testing.T, prev, curr domain.VulnerablePackageSnapshot) {
	t.Helper()
	evts := tracker.DiffVulnerablePackage(prev, curr, f.entity.ID, f.entity.RepositoryID, nil)
	if len(evts) == 0 {
		t.Fatal("the listing implies no event")
	}
	for _, evt := range evts {
		evt.OrgID = runmode.LocalDefaultOrgID
		mustHandle(t, f.router, evt)
	}
}

func (f *vulnFixture) activeTasks(t *testing.T) []domain.Task {
	t.Helper()
	tasks, err := testTaskStore(f.db).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, f.entity.ID)
	if err != nil {
		t.Fatalf("read active tasks: %v", err)
	}
	return tasks
}

func (f *vulnFixture) visibility(t *testing.T, taskID string) []string {
	t.Helper()
	vis, err := testTaskStore(f.db).VisibilityTeams(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if err != nil {
		t.Fatalf("read visibility: %v", err)
	}
	slices.Sort(vis)
	return vis
}

// An alert on a repository two teams track makes one task both teams see,
// with no applies_to_unowned handler anywhere. A team that tracks another
// repository sees nothing, and so does one whose rule opts into unowned
// entities: the flag reaches nothing a vulnerable package routes by. The same
// gate holds the notify rules: only a tracking team's rule is raised. The
// resolution closes the task and the package.
func TestVulnerablePackage_RoutesToEveryTrackingTeam(t *testing.T) {
	rec := notifytest.Record(t)
	f := newVulnFixture(t)
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamA, "", 0.6, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamB, "", 0.6, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamC, "", 0.6, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.tD, "", 0.6, true)
	notifyB := seedVulnHandler(t, f.db, domain.EventHandlerKindNotify, f.teamB, "", 0, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindNotify, f.teamC, "", 0, false)

	f.listing(t, lodashSnapshot(), lodashSnapshot(vulnAlert(1, domain.DependabotSeverityHigh)))

	tasks := f.activeTasks(t)
	if len(tasks) != 1 || tasks[0].EventType != domain.EventGitHubVulnerabilityAlertOpened {
		t.Fatalf("tasks = %+v, want one alert_opened task", tasks)
	}
	want := []string{f.teamA, f.teamB}
	slices.Sort(want)
	if vis := f.visibility(t, tasks[0].ID); !slices.Equal(vis, want) {
		t.Errorf("visibility = %v, want the two tracking teams %v", vis, want)
	}
	if owner := teamIDValue(&tasks[0]); owner != f.teamA && owner != f.teamB {
		t.Errorf("owner = %q, want one of the tracking teams", owner)
	}
	raised := rec.Events(runmode.LocalDefaultOrgID)
	if len(raised) != 1 || len(raised[0].Raise.Rules) != 1 || raised[0].Raise.Rules[0].HandlerID != notifyB {
		t.Errorf("raised %+v, want one occurrence naming team B's notify rule alone", raised)
	}

	f.listing(t, lodashSnapshot(vulnAlert(1, domain.DependabotSeverityHigh)), lodashSnapshot())
	if tasks := f.activeTasks(t); len(tasks) != 0 {
		t.Errorf("after the resolution: %d tasks active, want none", len(tasks))
	}
	e, err := sqlitestore.New(f.db).Entities.GetSystem(t.Context(), runmode.LocalDefaultOrgID, f.entity.ID)
	if err != nil || e == nil || e.State != "closed" {
		t.Errorf("package after the resolution = %+v, %v; want it closed", e, err)
	}
}

// A trigger filtered to high severity does not fire for a low-severity alert,
// and fires on the package's same task when a high-severity alert opens on it
// later: the trigger is evaluated on the event that bumps the task, not only on
// the one that made it.
func TestVulnerablePackage_SeverityFloorTriggerFiresOnALaterAlert(t *testing.T) {
	f := newVulnFixture(t)
	enableTeamAutoDelegate(t, f.db, f.teamA)
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamA, "", 0.6, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindTrigger, f.teamA, `{"min_severity":"high"}`, 0, false)

	low := vulnAlert(1, domain.DependabotSeverityLow)
	f.listing(t, lodashSnapshot(), lodashSnapshot(low))
	tasks := f.activeTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("after the low alert: %d tasks, want the rule's one", len(tasks))
	}
	if f.stub.calls != 0 {
		t.Fatalf("the trigger fired %d times for a low-severity alert, want none", f.stub.calls)
	}
	taskID := tasks[0].ID

	f.listing(t, lodashSnapshot(low), lodashSnapshot(low, vulnAlert(2, domain.DependabotSeverityHigh)))
	tasks = f.activeTasks(t)
	if len(tasks) != 1 || tasks[0].ID != taskID {
		t.Fatalf("after the high alert: tasks = %+v, want the same task", tasks)
	}
	if f.stub.calls != 1 {
		t.Errorf("the trigger fired %d times after the high-severity alert, want once", f.stub.calls)
	}
	if tasks[0].ClaimedByAgentID == "" {
		t.Error("the task is not claimed by the bot after the trigger fired")
	}
}

// When both tracking teams' triggers match, they fire in the order the
// router ranks pool teams by, the highest matched-rule priority first, and
// the first to fire claims the task: one run, the claim and the owner on that
// team, and no firing left for the other.
func TestVulnerablePackage_TwoTeamsTriggers_TheHigherRankedTeamClaims(t *testing.T) {
	f := newVulnFixture(t)
	enableTeamAutoDelegate(t, f.db, f.teamA, f.teamB)
	// Team A's id sorts first, so team B ranks first only by its priority.
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamA, "", 0.5, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamB, "", 0.9, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindTrigger, f.teamA, "", 0, false)
	seedVulnHandler(t, f.db, domain.EventHandlerKindTrigger, f.teamB, "", 0, false)

	f.listing(t, lodashSnapshot(), lodashSnapshot(vulnAlert(1, domain.DependabotSeverityCritical)))

	tasks := f.activeTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("%d tasks, want one", len(tasks))
	}
	if f.stub.calls != 1 {
		t.Errorf("%d runs started, want one", f.stub.calls)
	}
	if owner := teamIDValue(&tasks[0]); owner != f.teamB {
		t.Errorf("owner = %q, want team B %q, whose rule ranks it first", owner, f.teamB)
	}
	if tasks[0].ClaimedByAgentID == "" {
		t.Error("the task is not claimed by the bot")
	}
	firings, err := sqlitestore.New(f.db).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, f.entity.ID)
	if err != nil {
		t.Fatalf("list firings: %v", err)
	}
	if len(firings) != 0 {
		t.Errorf("%d firings left queued, want none", len(firings))
	}
}

// Untracking a repository creates no task for a later alert and closes none:
// the task made while the team tracked it stays open, and the package's
// resolution still closes it.
func TestVulnerablePackage_UntrackingIsForwardOnly(t *testing.T) {
	f := newVulnFixture(t)
	seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamA, "", 0.6, false)
	first := vulnAlert(1, domain.DependabotSeverityHigh)
	f.listing(t, lodashSnapshot(), lodashSnapshot(first))
	tasks := f.activeTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("setup: %d tasks, want one", len(tasks))
	}
	taskID := tasks[0].ID

	st := sqlitestore.New(f.db)
	if err := st.TeamGitHubRepos.ReplaceForTeam(t.Context(), runmode.LocalDefaultOrgID, f.teamA, dbtest.TestGitHubHost, nil); err != nil {
		t.Fatalf("untrack: %v", err)
	}
	second := vulnAlert(2, domain.DependabotSeverityCritical)
	f.listing(t, lodashSnapshot(first), lodashSnapshot(first, second))
	tasks = f.activeTasks(t)
	if len(tasks) != 1 || tasks[0].ID != taskID {
		t.Fatalf("after untracking: tasks = %+v, want the task made before, still open", tasks)
	}
	var bumps int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ?`, taskID).Scan(&bumps); err != nil {
		t.Fatalf("count task events: %v", err)
	}
	if bumps != 1 {
		t.Errorf("the task has %d task events after untracking, want only the one that made it", bumps)
	}

	f.listing(t, lodashSnapshot(first, second), lodashSnapshot())
	if tasks := f.activeTasks(t); len(tasks) != 0 {
		t.Errorf("after the resolution: %d tasks active, want none", len(tasks))
	}
}

// A predicate on alert_opened filters the alert in routing: a rule narrowed to
// the development scope, to pip, or to a fix not yet released makes no task for
// a runtime npm alert that has one, and a rule naming the package in another
// case does.
func TestVulnerablePackage_RulePredicatesFilterTheAlert(t *testing.T) {
	for _, tc := range []struct {
		predicate string
		want      int
	}{
		{`{"scope":"development"}`, 0},
		{`{"ecosystem_in":["pip"]}`, 0},
		{`{"has_patched_version":false}`, 0},
		{`{"min_severity":"critical"}`, 0},
		{`{"package_in":["Lodash"],"ecosystem_in":["npm"],"min_severity":"medium","scope":"runtime","has_patched_version":true}`, 1},
	} {
		t.Run(tc.predicate, func(t *testing.T) {
			f := newVulnFixture(t)
			seedVulnHandler(t, f.db, domain.EventHandlerKindRule, f.teamA, tc.predicate, 0.6, false)
			f.listing(t, lodashSnapshot(), lodashSnapshot(vulnAlert(1, domain.DependabotSeverityHigh)))
			if got := len(f.activeTasks(t)); got != tc.want {
				t.Errorf("%d tasks, want %d", got, tc.want)
			}
		})
	}
}
