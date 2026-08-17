package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// recordingJiraServer fakes the Jira REST endpoints the mirror touches (myself,
// get-issue, assignee, transitions) and records each mutating call. It is
// stateful: an assignee PUT marks the ticket assigned to the bot and a
// transition POST moves its status, so a follow-up GetClaimState reflects the
// mutation — which is what makes the mirror's idempotency skip exercisable end
// to end. The transition POST also signals transitionPosted so a test can wait
// for a detached mirror goroutine deterministically.
type recordingJiraServer struct {
	srv        *httptest.Server
	myselfName string

	mu          sync.Mutex
	status      string
	assignee    string
	assigns     int
	transitions []string

	// failClaimState / failAssign inject transient HTTP failures so a test can
	// exercise the mirror's read-failure and assign-failure paths.
	failClaimState bool
	failAssign     bool

	transitionPosted chan struct{}
	// claimStateRead fires after the GetClaimState issue read is served — the
	// observable point of a no-op mirror pass (one that reads state and then
	// transitions nothing), so a test can join a detached mirror goroutine even
	// when it makes no write.
	claimStateRead chan struct{}
}

// jiraFakeIDToStatus maps the transition ids the fake serves back to their target
// status, so a POST can record (and apply) which bucket the ticket moved to.
var jiraFakeIDToStatus = map[string]string{"31": "In Progress", "41": "Done"}

// newRecordingJiraServer starts a fake Jira at the given initial status +
// assignee ("" = unassigned). myselfName is the authenticated service account
// the client's currentUser resolves to ("bot").
func newRecordingJiraServer(t *testing.T, status, assignee string) *recordingJiraServer {
	t.Helper()
	r := &recordingJiraServer{
		myselfName:       "bot",
		status:           status,
		assignee:         assignee,
		transitionPosted: make(chan struct{}, 8),
		claimStateRead:   make(chan struct{}, 8),
	}
	r.srv = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recordingJiraServer) handle(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/myself"):
		_, _ = io.WriteString(w, `{"name":"`+r.myselfName+`"}`)
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/transitions"):
		_, _ = io.WriteString(w, `{"transitions":[`+
			`{"id":"31","name":"Start","to":{"name":"In Progress"}},`+
			`{"id":"41","name":"Finish","to":{"name":"Done"}}]}`)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/transitions"):
		var body struct {
			Transition struct {
				ID string `json:"id"`
			} `json:"transition"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		target := jiraFakeIDToStatus[body.Transition.ID]
		r.status = target
		r.transitions = append(r.transitions, target)
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		select {
		case r.transitionPosted <- struct{}{}:
		default:
		}
	case req.Method == http.MethodPut && strings.HasSuffix(path, "/assignee"):
		r.mu.Lock()
		fail := r.failAssign
		if !fail {
			r.assignee = r.myselfName
			r.assigns++
		}
		r.mu.Unlock()
		if fail {
			http.Error(w, `{"err":"assign failed"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case req.Method == http.MethodGet && strings.Contains(path, "/issue/"):
		// GetClaimState: GET issue?fields=assignee,status. A 5xx makes the
		// client's GetClaimState return nil ("unknown").
		r.mu.Lock()
		fail := r.failClaimState
		status, assignee := r.status, r.assignee
		r.mu.Unlock()
		if fail {
			http.Error(w, `{"err":"read failed"}`, http.StatusInternalServerError)
			return
		}
		assigneeJSON := "null"
		if assignee != "" {
			assigneeJSON = `{"name":"` + assignee + `"}`
		}
		_, _ = io.WriteString(w, `{"key":"SKY-1","fields":{"status":{"name":"`+status+`"},"assignee":`+assigneeJSON+`}}`)
		select {
		case r.claimStateRead <- struct{}{}:
		default:
		}
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// client builds a DC-PAT client pointed at the fake.
func (r *recordingJiraServer) client() *jira.Client {
	return jira.NewClient(jira.DataCenterPAT(r.srv.URL, "bot-tok"))
}

func (r *recordingJiraServer) snapshot() (assigns int, transitions []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.assigns, append([]string(nil), r.transitions...)
}

// currentStatus reports the ticket's status after any applied transitions —
// the end-state assertion for the ordering tests.
func (r *recordingJiraServer) currentStatus() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// mirrorRule is the SKY rule the direct runJiraMirror tests act on:
// To Do (pickup) / In Progress / Done.
func mirrorRule() domain.JiraProjectStatusRules {
	return domain.JiraProjectStatusRules{
		TeamID:              runmode.LocalDefaultTeamID,
		ProjectKey:          "SKY",
		PickupMembers:       []string{"To Do"},
		InProgressMembers:   []string{"In Progress"},
		InProgressCanonical: "In Progress",
		DoneMembers:         []string{"Done"},
		DoneCanonical:       "Done",
	}
}

// waitTransition blocks until the fake has serviced one transition POST, or
// fails the test on timeout — the deterministic join for a detached mirror.
func (r *recordingJiraServer) waitTransition(t *testing.T) {
	t.Helper()
	select {
	case <-r.transitionPosted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the mirror's Jira transition")
	}
}

// waitClaimState blocks until the fake has served one GetClaimState issue read,
// or fails on timeout — the deterministic join for a mirror pass that makes no
// write (so waitTransition would never fire). After this returns the mirror
// goroutine has read state and made its (no-op) decision.
func (r *recordingJiraServer) waitClaimState(t *testing.T) {
	t.Helper()
	select {
	case <-r.claimStateRead:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the mirror's Jira claim-state read")
	}
}

// fakeJiraResolver is a jira.Resolver whose ForSystem returns a fixed client
// (the fake server's) and records how many times it was consulted. ForUser is
// unused by the mirror (the bot lane is system-only).
type fakeJiraResolver struct {
	client *jira.Client
	sysErr error

	mu             sync.Mutex
	forSystemCalls int
}

func (r *fakeJiraResolver) ForSystem(_ context.Context, _ string) (*jira.Client, error) {
	r.mu.Lock()
	r.forSystemCalls++
	r.mu.Unlock()
	if r.sysErr != nil {
		return nil, r.sysErr
	}
	return r.client, nil
}

func (r *fakeJiraResolver) ForUser(_ context.Context, _, _ string) (*jira.Client, error) {
	return nil, jira.ErrNoJiraUserCredential
}

func (r *fakeJiraResolver) systemCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forSystemCalls
}

// --- runJiraMirror: the shared synchronous worker -------------------------

// On a fresh ticket the in-progress mirror both assigns the service account and
// transitions into the InProgress bucket.
func TestRunJiraMirror_InProgress_AssignsAndTransitions(t *testing.T) {
	fake := newRecordingJiraServer(t, "To Do", "")
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})

	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), false)

	assigns, transitions := fake.snapshot()
	if assigns != 1 {
		t.Errorf("assigns = %d, want 1 (service account assigned)", assigns)
	}
	if len(transitions) != 1 || transitions[0] != "In Progress" {
		t.Errorf("transitions = %v, want [In Progress]", transitions)
	}
}

// The done mirror transitions into the Done bucket and never re-assigns — a
// finished ticket stays assigned to the bot.
func TestRunJiraMirror_Done_TransitionsOnly_NoAssign(t *testing.T) {
	fake := newRecordingJiraServer(t, "In Progress", "bot")
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})

	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), true)

	assigns, transitions := fake.snapshot()
	if assigns != 0 {
		t.Errorf("assigns = %d, want 0 (done leaves the ticket assigned to the bot)", assigns)
	}
	if len(transitions) != 1 || transitions[0] != "Done" {
		t.Errorf("transitions = %v, want [Done]", transitions)
	}
}

// A ticket already assigned + already in the target bucket triggers no writes —
// the membership-based idempotency that lets the mirror coexist with the
// agent's own cmd/exec/jira moves.
func TestRunJiraMirror_Idempotent_AlreadyInBucket_NoWrites(t *testing.T) {
	fake := newRecordingJiraServer(t, "In Progress", "bot")
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})

	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), false)

	assigns, transitions := fake.snapshot()
	if assigns != 0 || len(transitions) != 0 {
		t.Errorf("assigns=%d transitions=%v, want no writes (already in bucket + assigned)", assigns, transitions)
	}
}

// in_review collapses into In Progress: a bot that parks a run (board →
// in_review) re-targets the SAME InProgress canonical, so the second mirror
// pass finds the ticket already there and makes no distinct Jira move.
func TestRunJiraMirror_InReviewCollapsesToInProgress_NoDistinctMove(t *testing.T) {
	fake := newRecordingJiraServer(t, "To Do", "")
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})

	// First in-progress (board in_progress) — assign + transition.
	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), false)
	// Board bounces to in_review, which also maps to InProgressCanonical.
	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), false)

	assigns, transitions := fake.snapshot()
	if assigns != 1 {
		t.Errorf("assigns = %d, want 1 (assigned once; in_review re-assign is a no-op)", assigns)
	}
	if len(transitions) != 1 || transitions[0] != "In Progress" {
		t.Errorf("transitions = %v, want exactly one In Progress move (no distinct in_review move)", transitions)
	}
}

// A disabled mirror (no resolver wired) is a clean no-op — the local /
// test-fixture path.
func TestRunJiraMirror_NoResolver_NoOp(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	// No SetJiraResolver → getJiraResolver returns nil.
	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), false)
	// Reaching here without a panic / outbound call is the assertion.
}

// Forward-only: a late in-progress mirror must NEVER transition a ticket the
// done mirror already moved to Done — that would drag the terminal ticket back
// to In Progress. With the ticket already in the Done bucket the in-progress
// mirror does nothing (not even an assign).
func TestRunJiraMirror_InProgress_SkipsWhenAlreadyDone(t *testing.T) {
	fake := newRecordingJiraServer(t, "Done", "")
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})

	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), false)

	if assigns, transitions := fake.snapshot(); assigns != 0 || len(transitions) != 0 {
		t.Errorf("assigns=%d transitions=%v, want no writes (forward-only: a Done ticket is never moved back)", assigns, transitions)
	}
	if got := fake.currentStatus(); got != "Done" {
		t.Errorf("final status = %q, want Done (unchanged)", got)
	}
}

// Concurrent in-progress + done mirrors against one ticket always settle on
// Done, whichever wins the per-issue lock: in-progress-first runs In Progress
// then Done; done-first runs Done then the in-progress mirror's forward-only
// guard skips. Run under -race, this also exercises jiraMirrorLocks.
func TestRunJiraMirror_ConcurrentInProgressAndDone_EndsInDone(t *testing.T) {
	fake := newRecordingJiraServer(t, "To Do", "")
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})
	rule := mirrorRule()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", rule, false) }()
	go func() { defer wg.Done(); s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", rule, true) }()
	wg.Wait()

	if got := fake.currentStatus(); got != "Done" {
		t.Errorf("final status = %q, want Done (a late in-progress mirror must not drag the ticket out of Done)", got)
	}
}

// The mirror stays silent for ErrNoJiraSystemCredential (an expected, normal
// state: a GitHub-only org, or creds removed mid-flight) and logs every other
// ForSystem error (a real failure worth surfacing). Tested as a pure decision
// rather than by capturing the global logger — that races with the package's
// many goroutine-spawning tests under -race. The wrapped case matters because
// ForSystem actually returns the sentinel wrapped ("%w: org=...").
func TestShouldLogForSystemErr(t *testing.T) {
	if shouldLogForSystemErr(nil) {
		t.Error("nil error: want no log")
	}
	if shouldLogForSystemErr(jira.ErrNoJiraSystemCredential) {
		t.Error("bare ErrNoJiraSystemCredential: want silent")
	}
	if shouldLogForSystemErr(fmt.Errorf("%w: org=acme", jira.ErrNoJiraSystemCredential)) {
		t.Error("wrapped ErrNoJiraSystemCredential: want silent")
	}
	if !shouldLogForSystemErr(errors.New("vault unreachable")) {
		t.Error("real backend error: want logged")
	}
}

// If GetClaimState can't be read (transient error → nil), the in-progress
// mirror must skip rather than blind-write: blindly transitioning to In Progress
// could drag a ticket a concurrent done mirror already moved to Done back out of
// the terminal bucket. The forward-only invariant must not depend on the read
// succeeding.
func TestRunJiraMirror_InProgress_SkipsOnUnreadableState(t *testing.T) {
	fake := newRecordingJiraServer(t, "Done", "bot") // ticket already Done...
	fake.failClaimState = true                       // ...but the mirror can't read that
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})

	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), false)

	if assigns, transitions := fake.snapshot(); assigns != 0 || len(transitions) != 0 {
		t.Errorf("assigns=%d transitions=%v, want no writes (unreadable state must not regress a possibly-Done ticket)", assigns, transitions)
	}
}

// The done mirror's mirror image: an unreadable state must NOT prevent the Done
// transition. Reaching the done mirror means the work is complete, so moving to
// Done is correct even when GetClaimState fails — the asymmetry with the
// in-progress path (which skips on nil) is deliberate, and this pins it against
// a refactor that accidentally makes both skip.
func TestRunJiraMirror_Done_ProceedsWhenStateUnreadable(t *testing.T) {
	fake := newRecordingJiraServer(t, "In Progress", "bot") // can't confirm current status...
	fake.failClaimState = true                              // ...GetClaimState returns nil
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})

	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), true)

	if _, transitions := fake.snapshot(); len(transitions) != 1 || transitions[0] != "Done" {
		t.Errorf("transitions = %v, want [Done] (unreadable state must not prevent the done transition)", transitions)
	}
}

// assign + transition move together: a failed AssignToSelf skips the status
// transition, so the ticket is never left "In Progress but unassigned". It
// self-heals on the next board transition's mirror pass.
func TestRunJiraMirror_InProgress_AssignFails_SkipsTransition(t *testing.T) {
	fake := newRecordingJiraServer(t, "To Do", "") // unassigned → assign needed
	fake.failAssign = true
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetJiraResolver(&fakeJiraResolver{client: fake.client()})

	s.runJiraMirror(runmode.LocalDefaultOrgID, "SKY-1", "", mirrorRule(), false)

	if _, transitions := fake.snapshot(); len(transitions) != 0 {
		t.Errorf("transitions = %v, want none (a failed assign must skip the transition)", transitions)
	}
	if got := fake.currentStatus(); got != "To Do" {
		t.Errorf("status = %q, want To Do (untouched after a failed assign)", got)
	}
}

// --- lookupJiraRuleForTaskSystem ------------------------------------------

func seedJiraMirrorRule(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO jira_project_status_rules
		   (team_id, project_key, pickup_members, in_progress_members, in_progress_canonical, done_members, done_canonical)
		 VALUES (?, 'SKY', '["To Do"]', '["In Progress"]', 'In Progress', '["Done"]', 'Done')`,
		runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed jira rule: %v", err)
	}
}

func TestLookupJiraRuleForTaskSystem_ResolvesForJiraTask(t *testing.T) {
	database := newDelegateTestDB(t)
	seedJiraMirrorRule(t, database)
	rules := sqlitestore.New(database).JiraStatusRules

	team := runmode.LocalDefaultTeamID
	task := &domain.Task{EntitySource: "jira", EntitySourceID: "SKY-123", TeamID: &team}
	rule := lookupJiraRuleForTaskSystem(context.Background(), rules, task)
	if rule == nil {
		t.Fatal("rule = nil, want the SKY rule")
	}
	if rule.InProgressCanonical != "In Progress" || rule.DoneCanonical != "Done" {
		t.Errorf("rule = %+v, want InProgress/Done canonicals", rule)
	}
}

func TestLookupJiraRuleForTaskSystem_NonJiraTask_Nil(t *testing.T) {
	database := newDelegateTestDB(t)
	seedJiraMirrorRule(t, database)
	rules := sqlitestore.New(database).JiraStatusRules

	team := runmode.LocalDefaultTeamID
	task := &domain.Task{EntitySource: "github", EntitySourceID: "owner/repo#1", TeamID: &team}
	if rule := lookupJiraRuleForTaskSystem(context.Background(), rules, task); rule != nil {
		t.Errorf("rule = %+v, want nil (GitHub task is not rule-backed)", rule)
	}
}

func TestLookupJiraRuleForTaskSystem_NoTeam_Nil(t *testing.T) {
	database := newDelegateTestDB(t)
	seedJiraMirrorRule(t, database)
	rules := sqlitestore.New(database).JiraStatusRules

	task := &domain.Task{EntitySource: "jira", EntitySourceID: "SKY-123"} // TeamID nil
	if rule := lookupJiraRuleForTaskSystem(context.Background(), rules, task); rule != nil {
		t.Errorf("rule = %+v, want nil (no team stamped)", rule)
	}
}

func TestLookupJiraRuleForTaskSystem_NoRuleForProject_Nil(t *testing.T) {
	database := newDelegateTestDB(t)
	seedJiraMirrorRule(t, database) // only SKY configured
	rules := sqlitestore.New(database).JiraStatusRules

	team := runmode.LocalDefaultTeamID
	task := &domain.Task{EntitySource: "jira", EntitySourceID: "OTHER-9", TeamID: &team}
	if rule := lookupJiraRuleForTaskSystem(context.Background(), rules, task); rule != nil {
		t.Errorf("rule = %+v, want nil (no rule for project OTHER)", rule)
	}
}

// --- Hook 1: recomputeTaskBoardColumn -> in-progress mirror ---------------

// setupJiraMirrorFixture seeds a bot-claimable Jira run+task (its 1-step
// blueprint_run starts 'running') plus the SKY status rule, and returns a
// spawner wired to the fake resolver. Mirrors setupAdvanceFixture but Jira-
// sourced so the mirror has a rule + issue key to act on.
func setupJiraMirrorFixture(t *testing.T, suffix string, status, assignee string) (*Spawner, *sql.DB, string, string, *recordingJiraServer, *fakeJiraResolver) {
	t.Helper()
	database := newDelegateTestDB(t)
	// Bot-claim writes need an agents row (FK on claimed_by_agent_id).
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Triage Factory Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO team_agents (team_id, agent_id, enabled) VALUES (?, ?, 1)`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed local team_agents: %v", err)
	}
	seedJiraMirrorRule(t, database)

	runID := "r-jira-" + suffix
	seedJiraRun(t, database, runID, "sess-"+suffix, "/tmp/wt-jira-"+suffix)
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id = ?`, runID).Scan(&taskID); err != nil {
		t.Fatalf("lookup task_id: %v", err)
	}

	fake := newRecordingJiraServer(t, status, assignee)
	res := &fakeJiraResolver{client: fake.client()}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.SetJiraResolver(res)
	return s, database, runID, taskID, fake, res
}

// A bot delegation against a Jira task assigns the service account and
// transitions the ticket to InProgressCanonical on the first in-progress move.
func TestRecomputeBoard_JiraTask_MirrorsInProgress(t *testing.T) {
	s, database, _, taskID, fake, res := setupJiraMirrorFixture(t, "ip", "To Do", "")
	stampBotClaim(t, database, taskID)

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Fatalf("board status = %q, want in_progress", got)
	}
	fake.waitTransition(t)
	if n := res.systemCalls(); n != 1 {
		t.Errorf("ForSystem calls = %d, want 1", n)
	}
	assigns, transitions := fake.snapshot()
	if assigns != 1 {
		t.Errorf("assigns = %d, want 1", assigns)
	}
	if len(transitions) != 1 || transitions[0] != "In Progress" {
		t.Errorf("transitions = %v, want [In Progress]", transitions)
	}
}

// A user takeover flips the claim off the agent; recomputeTaskBoardColumn
// early-returns, so the bot never mirrors (no ForSystem, no Jira write).
func TestRecomputeBoard_UserClaimedJiraTask_NoMirror(t *testing.T) {
	s, database, runID, taskID, fake, res := setupJiraMirrorFixture(t, "user", "To Do", "")
	stampUserClaim(t, database, taskID)
	setRunStatus(t, database, runID, "open")

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if n := res.systemCalls(); n != 0 {
		t.Errorf("ForSystem calls = %d, want 0 (user-claimed task: bot must not mirror)", n)
	}
	if assigns, transitions := fake.snapshot(); assigns != 0 || len(transitions) != 0 {
		t.Errorf("assigns=%d transitions=%v, want no Jira writes", assigns, transitions)
	}
}

// A GitHub-backed task resolves no rule, so the mirror no-ops even when the
// resolver is wired.
func TestRecomputeBoard_GitHubTask_NoMirror(t *testing.T) {
	s, database, _, taskID := setupAdvanceFixture(t, "gh-nomirror")
	stampBotClaim(t, database, taskID)
	fake := newRecordingJiraServer(t, "To Do", "")
	res := &fakeJiraResolver{client: fake.client()}
	s.SetJiraResolver(res)

	s.recomputeTaskBoardColumn(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Fatalf("board status = %q, want in_progress", got)
	}
	if n := res.systemCalls(); n != 0 {
		t.Errorf("ForSystem calls = %d, want 0 (GitHub task: no Jira rule)", n)
	}
}

// --- Hook 2: terminateBlueprint completed -> in-progress mirror -----------

// A clean blueprint completion re-asserts the InProgress bucket — NOT Done. The
// agent has opened its PR and the work is awaiting human review + merge, which is
// still "in progress" to a Jira watcher; the ticket only reaches Done when its
// PR merges (a separate, entity-driven mirror). Starting from To Do/unassigned
// also exercises the self-heal: completion brings a ticket left behind by a
// failed dispatch-time mirror up to In Progress.
func TestTerminateBlueprint_CompletedJiraTask_MirrorsInProgress(t *testing.T) {
	s, database, runID, taskID, fake, res := setupJiraMirrorFixture(t, "complete", "To Do", "")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForRun(t, database, runID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "",
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Fatalf("task status = %q, want done (the board card still completes)", got)
	}
	fake.waitTransition(t)
	if n := res.systemCalls(); n != 1 {
		t.Errorf("ForSystem calls = %d, want 1", n)
	}
	assigns, transitions := fake.snapshot()
	if assigns != 1 {
		t.Errorf("assigns = %d, want 1 (completion assigns the bot + moves to In Progress)", assigns)
	}
	if len(transitions) != 1 || transitions[0] != "In Progress" {
		t.Errorf("transitions = %v, want [In Progress] (completion must not move the ticket to Done)", transitions)
	}
}

// A clean completion never transitions an already-In-Progress ticket — the
// common case (the dispatch-time mirror already moved it) is an idempotent
// no-op, and crucially does NOT advance it to Done.
func TestTerminateBlueprint_CompletedJiraTask_AlreadyInProgress_NoDoneMove(t *testing.T) {
	s, database, runID, taskID, fake, _ := setupJiraMirrorFixture(t, "complete-noop", "In Progress", "bot")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForRun(t, database, runID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "",
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Fatalf("task status = %q, want done", got)
	}
	// Join the detached mirror goroutine on its claim-state read (it makes no
	// write, so there is no transition to wait on) before asserting — otherwise
	// the assertion would win the race and pass even if a bug introduced an
	// erroneous transition.
	fake.waitClaimState(t)
	// The mirror reads claim state but, finding the ticket already In Progress +
	// assigned, makes no write — and crucially never advances it to Done.
	if _, transitions := fake.snapshot(); len(transitions) != 0 {
		t.Errorf("transitions = %v, want none (already In Progress; completion must not move to Done)", transitions)
	}
	if got := fake.currentStatus(); got != "In Progress" {
		t.Errorf("final status = %q, want In Progress (completion never reaches Done)", got)
	}
}

// A failed/aborted blueprint must NOT move the ticket at all — the mirror is
// gated to the completed branch only, so ForSystem is never reached.
func TestTerminateBlueprint_AbortedJiraTask_NoMirror(t *testing.T) {
	s, database, runID, taskID, fake, res := setupJiraMirrorFixture(t, "abort", "In Progress", "bot")
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForRun(t, database, runID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "",
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusAborted, "needs a human", nil, true)

	if n := res.systemCalls(); n != 0 {
		t.Errorf("ForSystem calls = %d, want 0 (aborted run must not move the ticket to Done)", n)
	}
	if _, transitions := fake.snapshot(); len(transitions) != 0 {
		t.Errorf("transitions = %v, want none on abort", transitions)
	}
}

// User takeover before a clean completion: the task is no longer agent-owned,
// so the completion hook skips the Jira mirror (the terminal write is the
// user's).
func TestTerminateBlueprint_CompletedButUserTookOver_NoMirror(t *testing.T) {
	s, database, runID, taskID, fake, res := setupJiraMirrorFixture(t, "takeover", "In Progress", "bot")
	stampUserClaim(t, database, taskID) // takeover flipped the claim to the user
	bpr := blueprintRunIDForRun(t, database, runID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "",
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	if n := res.systemCalls(); n != 0 {
		t.Errorf("ForSystem calls = %d, want 0 (user takeover: bot must not mirror the completion)", n)
	}
	if _, transitions := fake.snapshot(); len(transitions) != 0 {
		t.Errorf("transitions = %v, want none after takeover", transitions)
	}
}
