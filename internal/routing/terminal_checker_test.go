package routing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The checker is read-only by contract: it counts the two violations of the
// terminal-state invariant — an active entity carrying a terminal snapshot
// past the poll grace with no close in flight, and an open task on a closed
// entity — logs them, and records them on gauges. It repairs nothing; the
// poll's close obligation does. So the assertions here are about what it
// counts, what it declines to count, and that the rows it reads are
// byte-identical before and after a pass.

// newCheckerRouter builds a router wired with the Jira status-rules store,
// which the checker needs to know which Jira statuses count as done.
func newCheckerRouter(t *testing.T, database *sql.DB) *Router {
	t.Helper()
	st := sqlitestore.New(database)
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		nil, nil, nil, testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, st.JiraStatusRules, nil, nil, nil, noopScorer{}, websocket.NewHub())
}

// seedDivergentEntity is the state the checker counts: an entity whose
// stored snapshot says the work finished, whose row still says 'active', and
// whose tasks are still sitting in the team's queue. Last polled an hour ago,
// so it is past the grace. It writes the rows directly — going through the
// poll would record the obligation, which is the very thing that failed to
// happen in every scenario this covers.
func seedDivergentEntity(t *testing.T, database *sql.DB, source, sourceID, snapshotJSON string, taskTypes ...string) (entityID string, taskIDs []string) {
	t.Helper()
	ctx := t.Context()
	st := sqlitestore.New(database)
	kind := "pr"
	if source == "jira" || source == "linear" {
		kind = "issue"
	}
	var entity *domain.Entity
	var err error
	switch source {
	case "linear":
		// The snapshot written below moves the team to the one it names.
		entity, _, err = st.Entities.FindOrCreateLinearSystem(ctx, runmode.LocalDefaultOrgID, testScope(source), sourceID, "uuid-"+sourceID, "lt-seed", sourceID, "")
	case "jira":
		entity, _, err = st.Entities.FindOrCreateJiraSystem(ctx, runmode.LocalDefaultOrgID, testScope(source), sourceID, dbseed.JiraIssueID(sourceID), dbseed.JiraIssueProject(sourceID), sourceID, "")
	default:
		entity, _, err = st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, source, testScope(source), sourceID, "", kind, sourceID, "")
	}
	if err != nil {
		t.Fatalf("create entity %s: %v", sourceID, err)
	}
	if ok, err := st.Entities.UpdateSnapshotCASSystem(ctx, runmode.LocalDefaultOrgID, entity.ID, snapshotJSON, entity.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot for %s: ok=%v err=%v", sourceID, ok, err)
	}
	backdatePoll(t, database, entity.ID, time.Hour)
	for _, eventType := range taskTypes {
		evtID, err := st.Events.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.Event{
			OrgID: runmode.LocalDefaultOrgID, EntityID: &entity.ID, EventType: eventType, MetadataJSON: "{}",
		})
		if err != nil {
			t.Fatalf("record event %s: %v", eventType, err)
		}
		task, _, err := testTaskStore(database).FindOrCreateAtSystem(ctx, runmode.LocalDefaultOrgID,
			runmode.LocalDefaultTeamID, entity.ID, eventType, "", evtID, 0.5, time.Now())
		if err != nil {
			t.Fatalf("create task %s: %v", eventType, err)
		}
		taskIDs = append(taskIDs, task.ID)
	}
	return entity.ID, taskIDs
}

// backdatePoll rewinds an entity's last_polled_at, standing in for a poll
// that far in the past.
func backdatePoll(t *testing.T, database *sql.DB, entityID string, age time.Duration) {
	t.Helper()
	if _, err := database.Exec(`UPDATE entities SET last_polled_at = ? WHERE id = ?`, time.Now().UTC().Add(-age), entityID); err != nil {
		t.Fatalf("backdate last_polled_at: %v", err)
	}
}

func taskCloseReason(t *testing.T, database *sql.DB, taskID string) (status, reason string) {
	t.Helper()
	if err := database.QueryRow(`SELECT status, COALESCE(close_reason, '') FROM tasks WHERE id = ?`, taskID).
		Scan(&status, &reason); err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	return status, reason
}

// seedJiraDoneRules configures a team's per-project done statuses — the
// vocabulary that decides whether a Jira snapshot reads terminal.
func seedJiraDoneRules(t *testing.T, database *sql.DB, teamID string, rules ...domain.JiraProjectStatusRules) {
	t.Helper()
	trackJiraProjects(t, database, teamID, rules...)
}

// tableFingerprint renders every entities and tasks row as one string, so a
// pass can be shown to have written nothing.
func tableFingerprint(t *testing.T, database *sql.DB) string {
	t.Helper()
	var out string
	for _, q := range []string{
		`SELECT id, state, COALESCE(closed_at, ''), COALESCE(snapshot_json, ''), poll_seq, COALESCE(last_polled_at, '') FROM entities ORDER BY id`,
		`SELECT id, status, COALESCE(close_reason, ''), COALESCE(closed_at, '') FROM tasks ORDER BY id`,
		`SELECT COUNT(*) FROM task_events`,
		`SELECT COUNT(*) FROM event_queue`,
	} {
		rows, err := database.Query(q)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("fingerprint scan: %v", err)
			}
			for _, v := range vals {
				switch x := v.(type) {
				case []byte:
					out += string(x) + "|"
				default:
					out += toString(x) + "|"
				}
			}
			out += "\n"
		}
		rows.Close()
	}
	return out
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return time.Duration(x).String()
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case nil:
		return ""
	}
	return "?"
}

// gaugeValue reads one org's value of a checker gauge through a manual
// reader.
func gaugeValue(t *testing.T, reader *sdkmetric.ManualReader, name, orgID string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 gauge", name, m.Data)
			}
			for _, dp := range g.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("org.id")); ok && v.AsString() == orgID {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

// TestTerminalChecker_CountsBothViolationsAndWritesNothing is the contract:
// Count A and Count B each nonzero for constructed violations, both on the
// gauges, and every row the pass read byte-identical afterwards.
func TestTerminalChecker_CountsBothViolationsAndWritesNothing(t *testing.T) {
	database := newTestDB(t)
	r := newCheckerRouter(t, database)
	reader := sdkmetric.NewManualReader()
	g := NewTerminalInvariantGauges(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	defer g.Close()

	// Count A: a merged PR still active, unpolled for an hour.
	stranded, strandedTasks := seedDivergentEntity(t, database, "github", "owner/repo#stranded",
		`{"state":"MERGED","merged":true}`, domain.EventGitHubPRCICheckFailed)
	// Count B: a task left open under an entity somebody closed around the
	// guard — the store's own close would have taken the task with it.
	_, orphanTasks := seedDivergentEntity(t, database, "github", "owner/repo#orphan",
		`{"state":"OPEN","merged":false}`, domain.EventGitHubPRReviewChangesRequested)
	if _, err := database.Exec(`UPDATE entities SET state = 'closed', closed_at = ? WHERE source_id = 'owner/repo#orphan'`, time.Now().UTC()); err != nil {
		t.Fatalf("close the entity around the guard: %v", err)
	}
	// Neither: an open PR with live work.
	seedDivergentEntity(t, database, "github", "owner/repo#live", `{"state":"OPEN","merged":false}`, domain.EventGitHubPRCICheckFailed)

	before := tableFingerprint(t, database)
	r.checkTerminalInvariant(context.Background(), g)
	if after := tableFingerprint(t, database); after != before {
		t.Errorf("the checker wrote something\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if v, found := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); !found || v != 1 {
		t.Errorf("entity_terminal_active = (%d, found=%v), want 1", v, found)
	}
	if v, found := gaugeValue(t, reader, "tasks_open_on_closed_entity", runmode.LocalDefaultOrgID); !found || v != 1 {
		t.Errorf("tasks_open_on_closed_entity = (%d, found=%v), want 1", v, found)
	}

	// After repair — the obligation route for one, a plain task close for
	// the other — both counts read zero.
	rel := terminatingRelationFor(domain.EventSystemEntityCloseOwed)
	if closed, err := r.runTerminatingClose(context.Background(), runmode.LocalDefaultOrgID,
		domain.Event{EventType: domain.EventSystemEntityCloseOwed, EntityID: &stranded}, stranded, rel, nil); err != nil || !closed {
		t.Fatalf("repair the stranded entity: closed=%v err=%v", closed, err)
	}
	if status, reason := taskCloseReason(t, database, strandedTasks[0]); status != "done" || reason != closeReasonReconciled {
		t.Errorf("repaired task = (%q, %q), want (done, %q)", status, reason, closeReasonReconciled)
	}
	if _, err := testTaskStore(database).CloseSystem(context.Background(), runmode.LocalDefaultOrgID, orphanTasks[0], "user_done", ""); err != nil {
		t.Fatalf("close the orphan task: %v", err)
	}
	r.checkTerminalInvariant(context.Background(), g)
	if v, found := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); !found || v != 0 {
		t.Errorf("entity_terminal_active after repair = (%d, found=%v), want 0", v, found)
	}
	if v, found := gaugeValue(t, reader, "tasks_open_on_closed_entity", runmode.LocalDefaultOrgID); !found || v != 0 {
		t.Errorf("tasks_open_on_closed_entity after repair = (%d, found=%v), want 0", v, found)
	}
}

// activeOrgsStub answers ListActiveSystem with the orgs a test sets, so a
// pass can see an org stop being active.
type activeOrgsStub struct {
	dbpkg.OrgsStore
	ids []string
}

func (s *activeOrgsStub) ListActiveSystem(context.Context) ([]string, error) { return s.ids, nil }

// failingCandidatesStore fails the checker's candidate read while fail is
// set, standing in for a database error on one org's pass.
type failingCandidatesStore struct {
	dbpkg.EntityStore
	fail bool
}

func (s *failingCandidatesStore) ListActiveTerminalCandidatesSystem(ctx context.Context, orgID string, jiraDone []domain.JiraStatusRef, linearDone []domain.LinearStateRef, unpolledFor time.Duration, limit int) ([]domain.Entity, error) {
	if s.fail {
		return nil, errors.New("candidate read failed")
	}
	return s.EntityStore.ListActiveTerminalCandidatesSystem(ctx, orgID, jiraDone, linearDone, unpolledFor, limit)
}

// TestTerminalChecker_PassReplacesOrgs: each pass reports the orgs it read
// and no others. An org whose read failed keeps the counts it had, and an
// org that is no longer active stops reporting rather than holding its last
// count.
func TestTerminalChecker_PassReplacesOrgs(t *testing.T) {
	database := newTestDB(t)
	r := newCheckerRouter(t, database)
	orgs := &activeOrgsStub{OrgsStore: r.orgs, ids: []string{runmode.LocalDefaultOrgID}}
	entities := &failingCandidatesStore{EntityStore: r.entities}
	r.orgs, r.entities = orgs, entities
	reader := sdkmetric.NewManualReader()
	g := NewTerminalInvariantGauges(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	defer g.Close()

	// A task left open under an entity closed around the guard.
	seedDivergentEntity(t, database, "github", "owner/repo#orphan",
		`{"state":"OPEN","merged":false}`, domain.EventGitHubPRReviewChangesRequested)
	if _, err := database.Exec(`UPDATE entities SET state = 'closed', closed_at = ? WHERE source_id = 'owner/repo#orphan'`, time.Now().UTC()); err != nil {
		t.Fatalf("close the entity around the guard: %v", err)
	}

	// An org whose read fails before it was ever counted has nothing to keep.
	entities.fail = true
	r.checkTerminalInvariant(context.Background(), g)
	if v, found := gaugeValue(t, reader, "tasks_open_on_closed_entity", runmode.LocalDefaultOrgID); found {
		t.Errorf("tasks_open_on_closed_entity = %d after a failed first read, want no series", v)
	}

	entities.fail = false
	r.checkTerminalInvariant(context.Background(), g)
	if v, found := gaugeValue(t, reader, "tasks_open_on_closed_entity", runmode.LocalDefaultOrgID); !found || v != 1 {
		t.Fatalf("tasks_open_on_closed_entity = (%d, found=%v), want 1", v, found)
	}

	entities.fail = true
	r.checkTerminalInvariant(context.Background(), g)
	if v, found := gaugeValue(t, reader, "tasks_open_on_closed_entity", runmode.LocalDefaultOrgID); !found || v != 1 {
		t.Errorf("tasks_open_on_closed_entity = (%d, found=%v) after a failed read, want the previous 1", v, found)
	}

	entities.fail = false
	orgs.ids = nil
	r.checkTerminalInvariant(context.Background(), g)
	for _, name := range []string{"entity_terminal_active", "tasks_open_on_closed_entity"} {
		if v, found := gaugeValue(t, reader, name, runmode.LocalDefaultOrgID); found {
			t.Errorf("%s = %d for an org no longer active, want no series", name, v)
		}
	}
}

// TestTerminalInvariantGauges_CloseStopsReporting is the lease-flap case: a
// pod that lost the brain stops reporting, and the gauges it builds on the
// next acquisition report only what their own passes read.
func TestTerminalInvariantGauges_CloseStopsReporting(t *testing.T) {
	database := newTestDB(t)
	r := newCheckerRouter(t, database)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	seedDivergentEntity(t, database, "github", "owner/repo#stranded", `{"state":"MERGED","merged":true}`, domain.EventGitHubPRCICheckFailed)

	old := NewTerminalInvariantGauges(provider)
	r.checkTerminalInvariant(context.Background(), old)
	if v, found := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); !found || v != 1 {
		t.Fatalf("entity_terminal_active = (%d, found=%v) before demotion, want 1", v, found)
	}

	old.Close()
	if v, found := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); found {
		t.Errorf("entity_terminal_active = %d after Close, want no series", v)
	}

	fresh := NewTerminalInvariantGauges(provider)
	defer fresh.Close()
	if v, found := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); found {
		t.Errorf("entity_terminal_active = %d before the new holder's first pass, want no series", v)
	}
	r.checkTerminalInvariant(context.Background(), fresh)
	if n := gaugePoints(t, reader, "entity_terminal_active"); n != 1 {
		t.Errorf("entity_terminal_active has %d points after a re-acquisition, want 1", n)
	}
}

// gaugePoints counts a checker gauge's points across every org.
func gaugePoints(t *testing.T, reader *sdkmetric.ManualReader, name string) int {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	n := 0
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok && m.Name == name {
				n += len(g.DataPoints)
			}
		}
	}
	return n
}

// TestTerminalChecker_UntrackedRepoIsCountedNotEnforced is the case the
// count exists for: an entity no cycle visits — its repo was untracked —
// carrying a terminal snapshot. Untracking never destroys tasks, so nothing
// enforces it (no poll reaches it, so no obligation is owed), and after the
// grace it IS counted; the explicit dismiss is the answer, not a sweep.
func TestTerminalChecker_UntrackedRepoIsCountedNotEnforced(t *testing.T) {
	database := newTestDB(t)
	r := newCheckerRouter(t, database)
	entityID, taskIDs := seedDivergentEntity(t, database, "github", "untracked/repo#1",
		`{"state":"MERGED","merged":true}`, domain.EventGitHubPRCICheckFailed)

	// Within the grace: one cycle of lag is legitimate, not a violation.
	backdatePoll(t, database, entityID, TerminalCheckGrace/2)
	if a, _, _ := r.checkOrgTerminalInvariant(context.Background(), runmode.LocalDefaultOrgID); a != 0 {
		t.Errorf("count within the grace = %d, want 0", a)
	}
	backdatePoll(t, database, entityID, TerminalCheckGrace+time.Minute)
	if a, _, _ := r.checkOrgTerminalInvariant(context.Background(), runmode.LocalDefaultOrgID); a != 1 {
		t.Errorf("count past the grace = %d, want 1", a)
	}
	// And still nothing touched it.
	if got := entityState(t, database, entityID); got != "active" {
		t.Errorf("entity state = %q, want active — the checker never closes", got)
	}
	if status, _ := taskCloseReason(t, database, taskIDs[0]); status != "queued" {
		t.Errorf("task status = %q, want queued", status)
	}
}

// TestTerminalChecker_JiraDoneIsPerProject keeps the Jira half honest: a
// status that is done in another project's vocabulary is not terminal for
// this one, and the checker counts only the issue in its own done status.
func TestTerminalChecker_JiraDoneIsPerProject(t *testing.T) {
	database := newTestDB(t)
	r := newCheckerRouter(t, database)
	// PROJ calls "Shipped" done; OPS does not, and calls "Done" done.
	seedJiraDoneRules(t, database, runmode.LocalDefaultTeamID,
		domain.JiraProjectStatusRules{
			ProjectKey: "PROJ", PickupMembers: jiraRefs("To Do"),
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
			DoneMembers: jiraRefs("Shipped"), DoneCanonical: jiraRef("Shipped"),
		},
		domain.JiraProjectStatusRules{
			ProjectKey: "OPS", PickupMembers: jiraRefs("To Do"),
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
			DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
		},
	)
	// "Shipped" is done in PROJ but not in OPS — the flat union the store
	// filters on surfaces both, and the per-project recheck is what keeps
	// the OPS issue out of the count.
	seedDivergentEntity(t, database, "jira", "OPS-7", `{"key":"OPS-7","status":"Shipped"}`, domain.EventJiraIssueAssigned)
	seedDivergentEntity(t, database, "jira", "PROJ-7", `{"key":"PROJ-7","status":"Shipped"}`, domain.EventJiraIssueAssigned)

	if a, _, _ := r.checkOrgTerminalInvariant(context.Background(), runmode.LocalDefaultOrgID); a != 1 {
		t.Errorf("count = %d, want 1 — only the PROJ issue is in its own project's done status", a)
	}
}

// TestTerminalChecker_JiraDoneFollowsAProjectKeyRename: the done set an issue
// is judged by is its project's, found by the registry row its entity
// references, so an issue whose key no longer spells the project's old key —
// the project's key was renamed — is still judged by that project's rule.
func TestTerminalChecker_JiraDoneFollowsAProjectKeyRename(t *testing.T) {
	ctx := context.Background()
	database := newTestDB(t)
	r := newCheckerRouter(t, database)
	seedJiraDoneRules(t, database, runmode.LocalDefaultTeamID, domain.JiraProjectStatusRules{
		ProjectKey: "PROJ", PickupMembers: jiraRefs("To Do"),
		InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
		DoneMembers: jiraRefs("Shipped"), DoneCanonical: jiraRef("Shipped"),
	})
	entityID, _ := seedDivergentEntity(t, database, "jira", "PROJ-8", `{"key":"PROJ-8","status":"Shipped"}`, domain.EventJiraIssueAssigned)

	st := sqlitestore.New(database)
	e, err := st.Entities.GetSystem(ctx, runmode.LocalDefaultOrgID, entityID)
	if err != nil || e == nil {
		t.Fatalf("read the entity: %v", err)
	}
	if _, err := st.JiraProjects.RefreshSystem(ctx, runmode.LocalDefaultOrgID, e.JiraProjectID, "id-PROJ", "CORE", "Core"); err != nil {
		t.Fatalf("re-key the registry row: %v", err)
	}
	if out, err := st.Entities.RenameSystem(ctx, runmode.LocalDefaultOrgID, "jira", testScope("jira"), e.ExternalID, "CORE-8", ""); err != nil || !out.Renamed {
		t.Fatalf("rename the issue: renamed=%v err=%v", out.Renamed, err)
	}

	if a, _, _ := r.checkOrgTerminalInvariant(ctx, runmode.LocalDefaultOrgID); a != 1 {
		t.Errorf("count = %d, want 1 — CORE-8 sits in its project's done status", a)
	}
}

// TestRunTerminalInvariantChecker_CountsOnItsOwnTicker covers the loop
// itself — the per-org iteration and the ticker — since a checker nothing
// drives is not an alarm, and that the loop closes its gauges when it stops.
func TestRunTerminalInvariantChecker_CountsOnItsOwnTicker(t *testing.T) {
	database := newTestDB(t)
	r := newCheckerRouter(t, database)
	reader := sdkmetric.NewManualReader()
	g := NewTerminalInvariantGauges(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	seedDivergentEntity(t, database, "github", "owner/repo#ticked", `{"state":"MERGED","merged":true}`, domain.EventGitHubPRCICheckFailed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.RunTerminalInvariantChecker(ctx, 20*time.Millisecond, g); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if v, found := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); found && v == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the gauge never recorded the violation after several ticks")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-done
	if v, found := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); found {
		t.Errorf("entity_terminal_active = %d after the checker stopped, want no series", v)
	}
}

// testScope is the scope a test keys an entity of source under: what
// domain.EntityScope answers for an org with default settings where the
// source has one, and a fixed stand-in where it has none.
func testScope(source string) string {
	switch source {
	case "github":
		return "https://github.com"
	case "jira":
		return jiraTestHost
	case "slack":
		return "slack.com"
	case "linear":
		return "ws-test"
	}
	return "test-scope"
}
