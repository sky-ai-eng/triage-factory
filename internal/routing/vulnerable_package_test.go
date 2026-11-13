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
// poll and the router together on one package: the baseline makes no task, an
// alert that opens after it makes one, a second alert folds into it, and the
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
		if _, err := tr.RefreshVulnerablePackages(ctx, dbtest.TestGitHubHost, ghclient.NewClient(srv.URL, "tok"), []tracker.AlertListing{{Repository: repos[0], Access: *a}}); err != nil {
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
	if got := activeTasks(entity()); len(got) != 0 {
		t.Fatalf("the baseline made %d tasks, want none", len(got))
	}

	setOpen(1, 2)
	poll()
	tasks := activeTasks(entity())
	if len(tasks) != 1 || tasks[0].EventType != domain.EventGitHubVulnerabilityAlertOpened {
		t.Fatalf("after an alert opened: tasks = %+v, want one alert_opened task", tasks)
	}
	taskID := tasks[0].ID

	setOpen(1, 2, 3)
	poll()
	if tasks := activeTasks(entity()); len(tasks) != 1 || tasks[0].ID != taskID {
		t.Fatalf("after a second alert: tasks = %+v, want the same one task", tasks)
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

// TestVulnerablePackageResolved_TerminatesAndClosesAlertTasks pins the close
// relation the resolved event fires.
func TestVulnerablePackageResolved_TerminatesAndClosesAlertTasks(t *testing.T) {
	rel := terminatingRelationFor(domain.EventGitHubVulnerabilityResolved)
	if rel == nil {
		t.Fatal("github:vulnerability:resolved terminates nothing")
	}
	if fmt.Sprint(rel.closes) != fmt.Sprint([]string{domain.EventGitHubVulnerabilityAlertOpened}) {
		t.Errorf("resolved closes %v, want the alert_opened tasks", rel.closes)
	}
	if !domain.TaskMayRideClosedEntity(domain.EventGitHubVulnerabilityResolved) {
		t.Error("the resolved event's own task may not ride the entity it closed")
	}
}
