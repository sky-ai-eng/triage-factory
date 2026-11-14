package routing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestVulnerablePackage_AlertsFoldIntoOneTaskAndTheResolutionClosesIt runs the
// poll and the router together on one package: the alert open at the first
// listing makes a task, alerts that open after it fold into that task, and the
// last alert closing closes the task and the entity.
func TestVulnerablePackage_AlertsFoldIntoOneTaskAndTheResolutionClosesIt(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	st := sqlitestore.New(database)
	ctx := context.Background()
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, org, team, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "octo", Repo: "api"}}); err != nil {
		t.Fatalf("track octo/api: %v", err)
	}
	repos, _, err := st.Repos.ListTrackedSystem(ctx, org, dbtest.TestGitHubHost, db.Unwindowed)
	if err != nil || len(repos) != 1 {
		t.Fatalf("tracked repositories = %v, %v", repos, err)
	}
	if _, err := st.DependabotAccess.RecordSystem(ctx, org, repos[0].ID, domain.DependabotAlertsReadable); err != nil {
		t.Fatalf("record access: %v", err)
	}
	seedMatchAllRule(t, database, team, domain.EventGitHubVulnerabilityAlertOpened)
	router := firingRouter(database, &stubDelegator{db: database})

	var mu sync.Mutex
	open := []int{1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var objs []string
		for _, n := range open {
			objs = append(objs, fmt.Sprintf(`{"number":%d,"state":"open","dependency":{"package":{"ecosystem":"npm","name":"lodash"},"manifest_path":"package.json"},"security_advisory":{"ghsa_id":"GHSA-%d","severity":"high"},"security_vulnerability":{"package":{"ecosystem":"npm","name":"lodash"},"severity":"high"}}`, n, n))
		}
		_, _ = w.Write([]byte("[" + strings.Join(objs, ",") + "]"))
	}))
	t.Cleanup(srv.Close)
	setOpen := func(numbers ...int) {
		mu.Lock()
		defer mu.Unlock()
		open = numbers
	}
	poll := func() {
		t.Helper()
		a, err := st.DependabotAccess.GetSystem(ctx, org, repos[0].ID)
		if err != nil || a == nil {
			t.Fatalf("read access: %+v %v", a, err)
		}
		tr := tracker.New(database, noopPublisher{}, st.Tasks, st.Entities, st.Repos, st.JiraProjects, st.Deliverables, st.ExternalActions, st.EventQueue, st.PRMergeability, org).
			WithDependabotAccess(st.DependabotAccess)
		if _, _, err := tr.RefreshVulnerablePackages(ctx, dbtest.TestGitHubHost, ghclient.NewClient(srv.URL, "tok"), []tracker.AlertListing{{Repository: repos[0], Access: *a}}); err != nil {
			t.Fatalf("RefreshVulnerablePackages: %v", err)
		}
		mustDrain(t, router)
	}
	entity := func() *domain.Entity {
		t.Helper()
		e, err := st.Entities.GetBySourceSystem(ctx, org, "github", dbtest.TestGitHubHost, "octo/api#npm:lodash")
		if err != nil || e == nil {
			t.Fatalf("read package: %+v %v", e, err)
		}
		return e
	}
	ts := testTaskStore(database)
	activeTasks := func(e *domain.Entity) []domain.Task {
		t.Helper()
		tasks, err := ts.FindActiveByEntity(ctx, org, e.ID)
		if err != nil {
			t.Fatalf("read tasks: %v", err)
		}
		return tasks
	}

	poll()
	tasks := activeTasks(entity())
	if len(tasks) != 1 || tasks[0].EventType != domain.EventGitHubVulnerabilityAlertOpened {
		t.Fatalf("after the first listing: tasks = %+v, want one alert_opened task", tasks)
	}
	taskID := tasks[0].ID

	setOpen(1, 2)
	poll()
	setOpen(1, 2, 3)
	poll()
	if tasks := activeTasks(entity()); len(tasks) != 1 || tasks[0].ID != taskID {
		t.Fatalf("after two more alerts: tasks = %+v, want the same one task", tasks)
	}

	setOpen()
	poll()
	e := entity()
	if e.State != "closed" {
		t.Errorf("after the last alert closed the entity is %s, want closed", e.State)
	}
	if tasks := activeTasks(e); len(tasks) != 0 {
		t.Errorf("after the last alert closed: %d tasks still active, want none", len(tasks))
	}
}

// TestVulnerablePackageTerminators_CloseEveryOtherTask pins the close
// relations the resolved and unreachable events fire: each terminates the
// entity and closes every vulnerable-package task but its own type's.
func TestVulnerablePackageTerminators_CloseEveryOtherTask(t *testing.T) {
	for terminator, want := range map[string][]string{
		domain.EventGitHubVulnerabilityResolved:    {domain.EventGitHubVulnerabilityAlertOpened, domain.EventGitHubVulnerabilityDependabotCannotFix, domain.EventGitHubVulnerabilityUnreachable},
		domain.EventGitHubVulnerabilityUnreachable: {domain.EventGitHubVulnerabilityAlertOpened, domain.EventGitHubVulnerabilityDependabotCannotFix, domain.EventGitHubVulnerabilityResolved},
	} {
		rel := terminatingRelationFor(terminator)
		if rel == nil {
			t.Errorf("%s terminates nothing", terminator)
			continue
		}
		if fmt.Sprint(rel.closes) != fmt.Sprint(want) {
			t.Errorf("%s closes %v, want %v", terminator, rel.closes, want)
		}
		if !domain.TaskMayRideClosedEntity(terminator) {
			t.Errorf("%s's own task may not ride the entity it closed", terminator)
		}
	}
}

// capturePublisher keeps what a tracker published, for a test to route.
type capturePublisher struct{ events []domain.Event }

func (p *capturePublisher) Publish(_ context.Context, evt domain.Event) {
	p.events = append(p.events, evt)
}
func (p *capturePublisher) PublishPreEnqueued(context.Context, domain.Event) {}

// TestVulnerablePackage_AHostChangeClosesThePackageAndItsTasks: once the org's
// GitHub host changes, the retirement's unreachable event closes a package on
// the old host and the task its alert made.
func TestVulnerablePackage_AHostChangeClosesThePackageAndItsTasks(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	st := sqlitestore.New(database)
	ctx := context.Background()
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, org, team, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "octo", Repo: "api"}}); err != nil {
		t.Fatalf("track octo/api: %v", err)
	}
	seedMatchAllRule(t, database, team, domain.EventGitHubVulnerabilityAlertOpened)
	router := firingRouter(database, &stubDelegator{db: database})

	e, _, err := st.Entities.FindOrCreateVulnerablePackageSystem(ctx, org, dbtest.TestGitHubHost, "octo/api#npm:lodash", "lodash (npm)", "")
	if err != nil {
		t.Fatalf("create package: %v", err)
	}
	snap := `{"repo":"octo/api","ecosystem":"npm","package":"lodash","alerts":[{"number":1,"ghsa_id":"GHSA-1","severity":"high","manifest_path":"package.json"}]}`
	if ok, err := st.Entities.UpdateSnapshotCASSystem(ctx, org, e.ID, snap, e.PollSeq); err != nil || !ok {
		t.Fatalf("snapshot: ok=%v err=%v", ok, err)
	}
	opened := tracker.DiffVulnerablePackage(domain.VulnerablePackageSnapshot{}, domain.VulnerablePackageSnapshot{
		Repo: "octo/api", Ecosystem: "npm", Package: "lodash",
		Alerts: []domain.DependabotAlert{{Number: 1, GHSAID: "GHSA-1", Severity: "high", FirstPatchedVersion: "4.17.21"}},
	}, e.ID, e.RepositoryID)
	for _, evt := range opened {
		evt.OrgID = org
		mustHandle(t, router, evt)
	}
	ts := testTaskStore(database)
	if tasks, _ := ts.FindActiveByEntity(ctx, org, e.ID); len(tasks) != 1 {
		t.Fatalf("setup: %d active tasks, want the alert's one", len(tasks))
	}

	pub := &capturePublisher{}
	tr := tracker.New(database, pub, st.Tasks, st.Entities, st.Repos, st.JiraProjects, st.Deliverables, st.ExternalActions, st.EventQueue, st.PRMergeability, org)
	if n := tr.RetireGitHubOutOfScope(ctx, dbtest.TestOtherGitHubHost); n != 1 || len(pub.events) != 1 || pub.events[0].EventType != domain.EventGitHubVulnerabilityUnreachable {
		t.Fatalf("retired %d, published %v; want one unreachable", n, pub.events)
	}
	mustHandle(t, router, pub.events[0])

	after, err := st.Entities.GetSystem(ctx, org, e.ID)
	if err != nil || after == nil || after.State != "closed" {
		t.Errorf("the package after the host change = %+v, %v; want it closed", after, err)
	}
	if tasks, _ := ts.FindActiveByEntity(ctx, org, e.ID); len(tasks) != 0 {
		t.Errorf("%d tasks still active after the host change, want none", len(tasks))
	}
}

// TestVulnerablePackage_DependabotCannotFixMakesATaskOfItsOwn runs the poll
// and the router together on a package whose alerts Dependabot's update
// failed for: the first listing makes an alert_opened task and a
// dependabot_cannot_fix task beside it, so a trigger on the second sees only
// what Dependabot leaves undone. A later alert Dependabot also cannot fix folds
// into that task, and the last alert closing closes both tasks and the entity.
func TestVulnerablePackage_DependabotCannotFixMakesATaskOfItsOwn(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	st := sqlitestore.New(database)
	ctx := context.Background()
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	if err := dbtest.TrackRepos(ctx, st.Repos, st.TeamGitHubRepos, org, team, dbtest.TestGitHubHost, []domain.TeamGitHubRepo{{Owner: "octo", Repo: "api"}}); err != nil {
		t.Fatalf("track octo/api: %v", err)
	}
	repos, _, err := st.Repos.ListTrackedSystem(ctx, org, dbtest.TestGitHubHost, db.Unwindowed)
	if err != nil || len(repos) != 1 {
		t.Fatalf("tracked repositories = %v, %v", repos, err)
	}
	if _, err := st.DependabotAccess.RecordSystem(ctx, org, repos[0].ID, domain.DependabotAlertsReadable); err != nil {
		t.Fatalf("record access: %v", err)
	}
	seedMatchAllRule(t, database, team, domain.EventGitHubVulnerabilityAlertOpened)
	seedMatchAllRule(t, database, team, domain.EventGitHubVulnerabilityDependabotCannotFix)
	router := firingRouter(database, &stubDelegator{db: database})

	const failed = `{"pullRequest":null,"error":{"errorType":"dependency_file_not_resolvable","title":"Dependabot can't resolve your Python dependency files"}}`
	var mu sync.Mutex
	open := []int{1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			var nodes []string
			for _, n := range open {
				nodes = append(nodes, fmt.Sprintf(`{"number":%d,"dependabotUpdate":%s}`, n, failed))
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"vulnerabilityAlerts":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[%s]}}}}`, strings.Join(nodes, ","))
			return
		}
		var objs []string
		for _, n := range open {
			objs = append(objs, fmt.Sprintf(`{"number":%d,"state":"open","dependency":{"package":{"ecosystem":"pip","name":"urllib3"},"manifest_path":"uv.lock"},"security_advisory":{"ghsa_id":"GHSA-%d","severity":"high"},"security_vulnerability":{"severity":"high","first_patched_version":{"identifier":"1.24.2"}}}`, n, n))
		}
		_, _ = w.Write([]byte("[" + strings.Join(objs, ",") + "]"))
	}))
	t.Cleanup(srv.Close)
	poll := func(numbers ...int) {
		t.Helper()
		mu.Lock()
		open = numbers
		mu.Unlock()
		a, err := st.DependabotAccess.GetSystem(ctx, org, repos[0].ID)
		if err != nil || a == nil {
			t.Fatalf("read access: %+v %v", a, err)
		}
		tr := tracker.New(database, noopPublisher{}, st.Tasks, st.Entities, st.Repos, st.JiraProjects, st.Deliverables, st.ExternalActions, st.EventQueue, st.PRMergeability, org).
			WithDependabotAccess(st.DependabotAccess)
		if _, _, err := tr.RefreshVulnerablePackages(ctx, dbtest.TestGitHubHost, ghclient.NewClient(srv.URL, "tok"), []tracker.AlertListing{{Repository: repos[0], Access: *a}}); err != nil {
			t.Fatalf("RefreshVulnerablePackages: %v", err)
		}
		mustDrain(t, router)
	}
	entity := func() *domain.Entity {
		t.Helper()
		e, err := st.Entities.GetBySourceSystem(ctx, org, "github", dbtest.TestGitHubHost, "octo/api#pip:urllib3")
		if err != nil || e == nil {
			t.Fatalf("read package: %+v %v", e, err)
		}
		return e
	}
	ts := testTaskStore(database)
	tasksByType := func(e *domain.Entity) map[string]string {
		t.Helper()
		tasks, err := ts.FindActiveByEntity(ctx, org, e.ID)
		if err != nil {
			t.Fatalf("read tasks: %v", err)
		}
		out := map[string]string{}
		for _, task := range tasks {
			out[task.EventType] = task.ID
		}
		return out
	}

	poll(1)
	first := tasksByType(entity())
	if len(first) != 2 || first[domain.EventGitHubVulnerabilityAlertOpened] == "" || first[domain.EventGitHubVulnerabilityDependabotCannotFix] == "" {
		t.Fatalf("after the first listing: tasks by type = %v, want an alert_opened task and a dependabot_cannot_fix task", first)
	}

	poll(1, 2)
	if got := tasksByType(entity()); fmt.Sprint(got) != fmt.Sprint(first) {
		t.Fatalf("after a second alert Dependabot cannot fix: tasks by type = %v, want the same two %v", got, first)
	}

	poll()
	e := entity()
	if e.State != "closed" {
		t.Errorf("after the last alert closed the entity is %s, want closed", e.State)
	}
	if got := tasksByType(e); len(got) != 0 {
		t.Errorf("after the last alert closed: tasks still active = %v, want none", got)
	}
}
