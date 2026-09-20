package routing

import (
	"context"
	"database/sql"
	"testing"
	"time"

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
		st.Orgs, st.Teams, nil, st.JiraStatusRules, nil, nil, noopScorer{}, websocket.NewHub())
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
	if source == "jira" {
		kind = "issue"
	}
	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, source, sourceID, kind, sourceID, "")
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
	if err := sqlitestore.New(database).JiraStatusRules.ReplaceForTeam(t.Context(), teamID, rules); err != nil {
		t.Fatalf("seed jira status rules: %v", err)
	}
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
	r.setTerminalGauges(newTerminalInvariantGauges(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))

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
	a, b, ok := r.checkOrgTerminalInvariant(context.Background(), runmode.LocalDefaultOrgID)
	if !ok {
		t.Fatal("pass reported failure")
	}
	if a != 1 || b != 1 {
		t.Errorf("counts = (%d, %d), want (1, 1)", a, b)
	}
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
	a, b, _ = r.checkOrgTerminalInvariant(context.Background(), runmode.LocalDefaultOrgID)
	if a != 0 || b != 0 {
		t.Errorf("counts after repair = (%d, %d), want zeros", a, b)
	}
	if v, _ := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); v != 0 {
		t.Errorf("entity_terminal_active after repair = %d, want 0", v)
	}
	if v, _ := gaugeValue(t, reader, "tasks_open_on_closed_entity", runmode.LocalDefaultOrgID); v != 0 {
		t.Errorf("tasks_open_on_closed_entity after repair = %d, want 0", v)
	}
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

// TestRunTerminalInvariantChecker_CountsOnItsOwnTicker covers the loop
// itself — the per-org iteration and the ticker — since a checker nothing
// drives is not an alarm.
func TestRunTerminalInvariantChecker_CountsOnItsOwnTicker(t *testing.T) {
	database := newTestDB(t)
	r := newCheckerRouter(t, database)
	reader := sdkmetric.NewManualReader()
	r.setTerminalGauges(newTerminalInvariantGauges(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	seedDivergentEntity(t, database, "github", "owner/repo#ticked", `{"state":"MERGED","merged":true}`, domain.EventGitHubPRCICheckFailed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunTerminalInvariantChecker(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, found := gaugeValue(t, reader, "entity_terminal_active", runmode.LocalDefaultOrgID); found && v == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the gauge never recorded the violation after several ticks")
}
