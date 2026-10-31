package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func linRef(name string) domain.LinearStateRef {
	return domain.LinearStateRef{ID: "st-" + strings.ToLower(name), Name: name}
}

func linRefs(names ...string) []domain.LinearStateRef {
	refs := make([]domain.LinearStateRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, linRef(n))
	}
	return refs
}

// armedLinear is a fully mapped rule for linearTeam, owned by team.
func armedLinear(team, linearTeam string, pickup, done []string) domain.LinearTeamRules {
	return domain.LinearTeamRules{
		TeamID: team, LinearTeamID: linearTeam, LinearTeamKey: strings.ToUpper(linearTeam),
		PickupMembers:     linRefs(pickup...),
		InProgressMembers: linRefs("In Progress"), InProgressCanonical: linRef("In Progress"),
		DoneMembers: linRefs(done...), DoneCanonical: linRef(done[0]),
	}
}

func linearTeamIDs(rules tracker.LinearRules) string {
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	return strings.Join(ids, ",")
}

func linearStateNames(refs []domain.LinearStateRef) string {
	return strings.Join(domain.LinearStateNames(refs), ",")
}

// TestToTrackerLinearRules_SkipsUnarmed: a Linear team reaches the tracker only
// once pickup, in_progress and done are mapped. One watched with nothing
// mapped has no states to ask about, and one mapped in part has issues to
// discover with nowhere to move them, or the reverse.
func TestToTrackerLinearRules_SkipsUnarmed(t *testing.T) {
	pickupOnly := domain.LinearTeamRules{TeamID: "team-a", LinearTeamID: "web", LinearTeamKey: "WEB", PickupMembers: linRefs("Todo")}
	noDone := armedLinear("team-a", "api", []string{"Todo"}, []string{"Done"})
	noDone.DoneMembers, noDone.DoneCanonical = nil, domain.LinearStateRef{}
	got := toTrackerLinearRules([]domain.LinearTeamRules{
		armedLinear("team-a", "eng", []string{"Todo"}, []string{"Done"}),
		{TeamID: "team-a", LinearTeamID: "ops", LinearTeamKey: "OPS"},
		pickupOnly,
		noDone,
	})
	if ids := linearTeamIDs(got); ids != "eng" {
		t.Errorf("merged teams = %q, want eng alone", ids)
	}
}

// TestToTrackerLinearRules_IgnoresInReview: the in_review rule reaches nothing
// the tracker is asked. An armed team that maps it merges exactly as one that
// does not, and a team that maps only it is skipped as unarmed.
func TestToTrackerLinearRules_IgnoresInReview(t *testing.T) {
	plain := armedLinear("team-a", "eng", []string{"Todo"}, []string{"Done"})
	withReview := plain
	withReview.InReviewMembers, withReview.InReviewCanonical = linRefs("In Review", "In Progress"), linRef("In Review")
	onlyReview := domain.LinearTeamRules{
		TeamID: "team-a", LinearTeamID: "ops", LinearTeamKey: "OPS",
		InReviewMembers: linRefs("In Review"), InReviewCanonical: linRef("In Review"),
	}
	got := toTrackerLinearRules([]domain.LinearTeamRules{withReview, onlyReview})
	if want := toTrackerLinearRules([]domain.LinearTeamRules{plain}); !reflect.DeepEqual(got, want) {
		t.Errorf("with in_review mapped =\n%+v\nwant the same as without\n%+v", got, want)
	}
}

// TestToTrackerLinearRules_MergesPerLinearTeam: two TF teams arming one Linear
// team are one entry, every rule's states set-unioned in first-seen order; a
// second Linear team keeps its own entry, in input order.
func TestToTrackerLinearRules_MergesPerLinearTeam(t *testing.T) {
	b := armedLinear("team-b", "eng", []string{"Backlog", "Todo"}, []string{"Canceled", "Done"})
	b.InProgressMembers = linRefs("In Progress", "In Review")
	got := toTrackerLinearRules([]domain.LinearTeamRules{
		armedLinear("team-a", "eng", []string{"Todo"}, []string{"Done"}),
		b,
		armedLinear("team-a", "ops", []string{"Triage"}, []string{"Shipped"}),
	})
	if ids := linearTeamIDs(got); ids != "eng,ops" {
		t.Fatalf("merged teams = %q, want eng,ops", ids)
	}
	eng := got[0]
	if eng.Key != "ENG" {
		t.Errorf("key = %q, want ENG", eng.Key)
	}
	for _, tc := range []struct {
		rule string
		got  []domain.LinearStateRef
		want string
	}{
		{"pickup", eng.Pickup, "Todo,Backlog"},
		{"in_progress", eng.InProgress, "In Progress,In Review"},
		{"done", eng.Done, "Done,Canceled"},
	} {
		if got := linearStateNames(tc.got); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.rule, got, tc.want)
		}
	}
}

// linearSecrets answers the org's Linear service credential and nothing else.
type linearSecrets struct {
	dbpkg.SecretStore
	key string
}

func (s linearSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	if key == integrations.KeyLinearAPIKey {
		return s.key, nil
	}
	return "", nil
}

// linearTestWorkspace is the Linear workspace the poller tests bind.
const linearTestWorkspace = "ws-test"

// linearRulesStore answers the org union with a fixed set, saved under the
// test workspace: a read for any other workspace finds nothing.
type linearRulesStore struct {
	dbpkg.LinearTeamRulesStore
	rules []domain.LinearTeamRules
}

func (s linearRulesStore) ListForOrgSystem(_ context.Context, _, workspaceID string) ([]domain.LinearTeamRules, error) {
	if workspaceID != linearTestWorkspace {
		return []domain.LinearTeamRules{}, nil
	}
	return s.rules, nil
}

// bindLinearWorkspace records the test workspace as the org's, which the
// credential bind does in production.
func bindLinearWorkspace(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`UPDATE org_settings SET linear_workspace_id = ? WHERE org_id = ?`, linearTestWorkspace, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("bind linear workspace: %v", err)
	}
}

// stubLinearResolver hands back one client for every org.
type stubLinearResolver struct{ client *linear.Client }

func (r stubLinearResolver) ForSystem(context.Context, string) (*linear.Client, error) {
	return r.client, nil
}

func (stubLinearResolver) ForUser(context.Context, string, string) (*linear.Client, error) {
	return nil, errors.New("not used by the poller")
}

func (stubLinearResolver) ResolveSystemCredential(context.Context, string) (linear.SystemCredential, error) {
	return linear.SystemCredential{}, errors.New("not used by the poller")
}

// linearStub is a GraphQL endpoint standing in for Linear. With rateLimit
// set, every request is refused as Linear refuses one, carrying reset when it
// is non-zero; otherwise the viewer is answered and every search is empty.
type linearStub struct {
	requests  atomic.Int32
	rateLimit bool
	reset     time.Time
	// hideTeams makes every team read answer that the credential reads none
	// of the teams asked about.
	hideTeams bool
}

func (s *linearStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if s.rateLimit {
			if !s.reset.IsZero() {
				w.Header().Set("X-RateLimit-Requests-Reset", strconv.FormatInt(s.reset.UnixMilli(), 10))
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`))
			return
		}
		var body struct {
			Query     string `json:"query"`
			Variables struct {
				IDs []string `json:"ids"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.Contains(body.Query, "query Viewer"):
			_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u-viewer","name":"Viewer"}}}`))
		case strings.Contains(body.Query, "query SearchIssues"):
			_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`))
		case strings.Contains(body.Query, "query TeamsByID"):
			nodes := []map[string]string{}
			for _, id := range body.Variables.IDs {
				if !s.hideTeams {
					nodes = append(nodes, map[string]string{"id": id, "key": strings.ToUpper(id)})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"teams": map[string]any{"nodes": nodes}}})
		default:
			t.Errorf("unexpected query: %s", body.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordingErrors collects OnError calls.
type recordingErrors struct {
	mu   sync.Mutex
	errs []error
}

func (r *recordingErrors) record(_, _ string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, err)
}

// linearManager is a Manager over a migrated SQLite database with Linear
// configured and one team armed, polling through the stub.
func linearManager(t *testing.T, stub *linearStub) (*Manager, *recordingErrors) {
	t.Helper()
	database := newMigratedSQLiteForPoller(t)
	bindLinearWorkspace(t, database)
	stores := sqlitestore.New(database)
	srv := stub.serve(t)
	errs := &recordingErrors{}
	m := &Manager{
		database: database, pub: busPublisher{bus: newTestBus(t)},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users,
		secrets:        linearSecrets{key: "lin_api_test"},
		linearRules:    linearRulesStore{rules: []domain.LinearTeamRules{armedLinear("team-a", "eng", []string{"Todo"}, []string{"Done"})}},
		linearResolver: stubLinearResolver{client: linear.NewClient(linear.ProxyPlaceholder(srv.URL, "placeholder"))},
		EventSources:   stores.OrgEventSources,
		OnError:        errs.record,
	}
	return m, errs
}

func (m *Manager) linearSlot(orgID string) time.Time {
	m.dueMu.Lock()
	defer m.dueMu.Unlock()
	return m.nextPoll[pollKey("linear", orgID)]
}

// TestRunLinearCycleForOrg_SkipsTurnedOffSource: a turned-off source makes no
// Linear call. The resolver is nil, so reaching it would panic.
func TestRunLinearCycleForOrg_SkipsTurnedOffSource(t *testing.T) {
	recorder := recordSpans(t)
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	turnOffSource(t, database, "linear")
	m := &Manager{
		database: database, pub: busPublisher{bus: newTestBus(t)},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: linearSecrets{key: "lin_api_test"},
		linearRules:  linearRulesStore{rules: []domain.LinearTeamRules{armedLinear("team-a", "eng", []string{"Todo"}, []string{"Done"})}},
		EventSources: stores.OrgEventSources,
	}
	m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, time.Now())
	if got := spanOutcome(t, recorder, "poll.linear.org"); got != "disabled" {
		t.Errorf("span outcome = %q, want disabled", got)
	}
}

// TestRunLinearCycleForOrg_PolicyReadFails_SkipsRatherThanPolls pins the
// fail-closed read of the off switch, as Jira's does.
func TestRunLinearCycleForOrg_PolicyReadFails_SkipsRatherThanPolls(t *testing.T) {
	recorder := recordSpans(t)
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	m := &Manager{
		database: database, pub: busPublisher{bus: newTestBus(t)},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: linearSecrets{key: "lin_api_test"},
		EventSources: erroringSourceStore{},
	}
	m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, time.Now())
	if got := spanOutcome(t, recorder, "poll.linear.org"); got != "policy_unreadable" {
		t.Errorf("span outcome = %q, want policy_unreadable", got)
	}
}

// TestRunLinearCycleForOrg_Skips covers the anticipated skips: an org with no
// Linear credential, one with no rules, and one whose only rule is watched but
// unmapped. Each knows without asking Linear that no armed team is unreadable,
// so each resolves linear.team_unreadable.
func TestRunLinearCycleForOrg_Skips(t *testing.T) {
	recorder := recordSpans(t)
	for _, tc := range []struct {
		name    string
		key     string
		rules   []domain.LinearTeamRules
		outcome string
	}{
		{"no credential", "", []domain.LinearTeamRules{armedLinear("team-a", "eng", []string{"Todo"}, []string{"Done"})}, "unconfigured"},
		{"no rules", "lin_api_test", nil, "unconfigured"},
		{"only unarmed teams", "lin_api_test", []domain.LinearTeamRules{{TeamID: "team-a", LinearTeamID: "eng", LinearTeamKey: "ENG"}}, "no_armed_teams"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newMigratedSQLiteForPoller(t)
			if tc.key != "" {
				bindLinearWorkspace(t, database)
			}
			stores := sqlitestore.New(database)
			m := &Manager{
				database: database, pub: busPublisher{bus: newTestBus(t)},
				tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
				orgs: stores.Orgs, users: stores.Users, secrets: linearSecrets{key: tc.key},
				linearRules: linearRulesStore{rules: tc.rules},
			}
			rec := notifytest.Record(t)
			kinds.LinearTeamUnreadable.Sync(context.Background(), runmode.LocalDefaultOrgID, []kinds.LinearTeam{{LinearTeamID: "eng", TeamKey: "ENG"}})
			m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, time.Now())
			if got := spanOutcome(t, recorder, "poll.linear.org"); got != tc.outcome {
				t.Errorf("span outcome = %q, want %q", got, tc.outcome)
			}
			rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable)
		})
	}
}

// switchedOffMidCycle answers the off switch's first read as on and every
// later one as off: an admin turning Linear off while a cycle runs.
type switchedOffMidCycle struct {
	dbpkg.OrgEventSourceStore
	reads atomic.Int32
}

func (s *switchedOffMidCycle) ListDisabledSystem(context.Context, string) ([]string, error) {
	if s.reads.Add(1) == 1 {
		return nil, nil
	}
	return []string{"linear"}, nil
}

// TestRunLinearCycleForOrg_TurnedOffMidCycleClears: a cycle already past its
// off-switch check when Linear is turned off can raise linear.team_unreadable
// after the switch cleared the source, so the cycle clears it again once its
// refresh is over.
func TestRunLinearCycleForOrg_TurnedOffMidCycleClears(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources dbpkg.OrgEventSourceStore
		open    bool
	}{
		{"stays on", nil, true},
		{"turned off", &switchedOffMidCycle{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := linearManager(t, &linearStub{hideTeams: true})
			if tc.sources != nil {
				m.EventSources = tc.sources
			}
			rec := notifytest.Record(t)
			m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, time.Now())
			if open := len(rec.Open(runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable)) > 0; open != tc.open {
				t.Errorf("open after the cycle = %v, want %v", open, tc.open)
			}
		})
	}
}

// failingLinearRulesStore fails the org union read.
type failingLinearRulesStore struct{ dbpkg.LinearTeamRulesStore }

func (failingLinearRulesStore) ListForOrgSystem(context.Context, string, string) ([]domain.LinearTeamRules, error) {
	return nil, errors.New("rules unreadable")
}

// TestRunLinearCycleForOrg_UnreadableRulesKeepCondition: a cycle that cannot
// read the rules does not know which teams are armed, so it leaves
// linear.team_unreadable as it was rather than resolve it.
func TestRunLinearCycleForOrg_UnreadableRulesKeepCondition(t *testing.T) {
	database := newMigratedSQLiteForPoller(t)
	bindLinearWorkspace(t, database)
	stores := sqlitestore.New(database)
	m := &Manager{
		database: database, pub: busPublisher{bus: newTestBus(t)},
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: linearSecrets{key: "lin_api_test"},
		linearRules: failingLinearRulesStore{},
	}
	rec := notifytest.Record(t)
	kinds.LinearTeamUnreadable.Sync(context.Background(), runmode.LocalDefaultOrgID, []kinds.LinearTeam{{LinearTeamID: "eng", TeamKey: "ENG"}})
	m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, time.Now())
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.LinearTeamUnreadable, "eng")
}

// TestRunLinearCycleForOrg_AnotherWorkspacesRulesDoNotApply: once the org's
// credential belongs to another workspace, the rules saved under the old one
// are not asked about — there is nothing in this workspace to poll — and the
// issues the old workspace left tracked retire with scope_changed, without a
// Linear call. The resolver is nil, so reaching it would panic.
func TestRunLinearCycleForOrg_AnotherWorkspacesRulesDoNotApply(t *testing.T) {
	recorder := recordSpans(t)
	database := newMigratedSQLiteForPoller(t)
	if _, err := database.Exec(`UPDATE org_settings SET linear_workspace_id = 'ws-other' WHERE org_id = ?`, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("bind other workspace: %v", err)
	}
	stores := sqlitestore.New(database)
	stale, _, err := stores.Entities.FindOrCreateLinearSystem(context.Background(), runmode.LocalDefaultOrgID, linearTestWorkspace, "ENG-1", "uuid-1", "team-eng", "Old", "")
	if err != nil {
		t.Fatal(err)
	}
	published := &capturingPublisher{}
	m := &Manager{
		database: database, pub: published,
		tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
		orgs: stores.Orgs, users: stores.Users, secrets: linearSecrets{key: "lin_api_test"},
		linearRules:  linearRulesStore{rules: []domain.LinearTeamRules{armedLinear("team-a", "eng", []string{"Todo"}, []string{"Done"})}},
		EventSources: stores.OrgEventSources,
	}
	m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, time.Now())
	if got := spanOutcome(t, recorder, "poll.linear.org"); got != "unconfigured" {
		t.Errorf("span outcome = %q, want unconfigured: the old workspace's rules were read", got)
	}
	evts := published.ofType(domain.EventLinearIssueUnreachable)
	if len(evts) != 1 || evts[0].EntityID == nil || *evts[0].EntityID != stale.ID {
		t.Fatalf("unreachable events = %+v, want one for the old workspace's issue", evts)
	}
	var meta events.LinearIssueUnreachableMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Reason != events.LinearUnreachableScopeChanged {
		t.Errorf("reason = %q, want scope_changed", meta.Reason)
	}
}

// capturingPublisher records every event a cycle publishes.
type capturingPublisher struct {
	mu   sync.Mutex
	evts []domain.Event
}

func (p *capturingPublisher) Publish(_ context.Context, evt domain.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evts = append(p.evts, evt)
}

func (p *capturingPublisher) PublishPreEnqueued(ctx context.Context, evt domain.Event) {
	p.Publish(ctx, evt)
}

func (p *capturingPublisher) ofType(eventType string) []domain.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []domain.Event
	for _, e := range p.evts {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

// TestRunLinearCycleForOrg_CompletesAndStamps: a cycle Linear answered stamps
// the org's success, emits poll-complete, and schedules the next poll at the
// org's interval.
func TestRunLinearCycleForOrg_CompletesAndStamps(t *testing.T) {
	stub := &linearStub{}
	m, errs := linearManager(t, stub)
	completed := make(chan domain.Event, 1)
	m.pub = completionPublisher{ch: completed}

	now := time.Now()
	m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, now)

	if len(errs.errs) != 0 {
		t.Fatalf("cycle errors: %v", errs.errs)
	}
	select {
	case evt := <-completed:
		if !strings.Contains(evt.MetadataJSON, `"source":"linear"`) {
			t.Errorf("poll-complete metadata = %s, want source linear", evt.MetadataJSON)
		}
	default:
		t.Error("no poll-complete sentinel")
	}
	if _, ok := m.successSnapshot("linear")[runmode.LocalDefaultOrgID]; !ok {
		t.Error("a completed cycle did not stamp the org's Linear success")
	}
	if got, want := m.linearSlot(runmode.LocalDefaultOrgID), now.Add(5*time.Minute); !got.Equal(want) {
		t.Errorf("next poll = %v, want the 5m default interval %v", got, want)
	}
}

// TestRunLinearCycleForOrg_RateLimitSchedulesAtReset: a rate limit on the
// first request ends the cycle with no second request, reports it, stamps no
// success and emits no poll-complete, and moves the org's next poll to the
// reset Linear named. Neither a config save (PollSoon) nor a wake from
// suspend (PollAllSoon) brings that poll forward.
func TestRunLinearCycleForOrg_RateLimitSchedulesAtReset(t *testing.T) {
	recorder := recordSpans(t)
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Millisecond)
	stub := &linearStub{rateLimit: true, reset: reset}
	m, errs := linearManager(t, stub)
	completed := make(chan domain.Event, 1)
	m.pub = completionPublisher{ch: completed}

	m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, time.Now())

	if n := stub.requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1: nothing after the rate limit", n)
	}
	if len(errs.errs) != 1 || !errors.Is(errs.errs[0], linear.ErrRateLimited) {
		t.Fatalf("reported errors = %v, want the rate limit", errs.errs)
	}
	if got := m.linearSlot(runmode.LocalDefaultOrgID); !got.Equal(reset) {
		t.Errorf("next poll = %v, want the reset %v", got, reset)
	}
	m.PollSoon("linear", runmode.LocalDefaultOrgID)
	m.PollAllSoon()
	if m.pollDue("linear", runmode.LocalDefaultOrgID, time.Now()) {
		t.Error("PollSoon and PollAllSoon made the org due before the reset")
	}
	if !m.pollDue("linear", runmode.LocalDefaultOrgID, reset) {
		t.Error("the org is not due at the reset")
	}
	if _, ok := m.successSnapshot("linear")[runmode.LocalDefaultOrgID]; ok {
		t.Error("a rate-limited cycle stamped success")
	}
	select {
	case evt := <-completed:
		t.Errorf("a rate-limited cycle emitted %s", evt.EventType)
	default:
	}
	if got := spanOutcome(t, recorder, "poll.linear.org"); got != "rate_limited" {
		t.Errorf("span outcome = %q, want rate_limited", got)
	}
}

// TestRunLinearCycleForOrg_RateLimitWithoutResetKeepsInterval: when Linear
// did not say when the window resets, the org keeps its interval schedule.
func TestRunLinearCycleForOrg_RateLimitWithoutResetKeepsInterval(t *testing.T) {
	linear.SetRetryBackoffForTest(t, time.Millisecond)
	stub := &linearStub{rateLimit: true}
	m, errs := linearManager(t, stub)

	now := time.Now()
	m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, now)

	var rl *linear.RateLimitError
	if len(errs.errs) != 1 || !errors.As(errs.errs[0], &rl) || !rl.Reset.IsZero() {
		t.Fatalf("reported errors = %v, want a rate limit with no reset", errs.errs)
	}
	if got, want := m.linearSlot(runmode.LocalDefaultOrgID), now.Add(5*time.Minute); !got.Equal(want) {
		t.Errorf("next poll = %v, want the interval %v", got, want)
	}
	if _, ok := m.successSnapshot("linear")[runmode.LocalDefaultOrgID]; ok {
		t.Error("a rate-limited cycle stamped success")
	}
}

// completionPublisher forwards poll-complete sentinels to ch and drops the
// rest.
type completionPublisher struct{ ch chan domain.Event }

func (p completionPublisher) Publish(_ context.Context, evt domain.Event) {
	if evt.EventType == domain.EventSystemPollCompleted {
		select {
		case p.ch <- evt:
		default:
		}
	}
}

func (p completionPublisher) PublishPreEnqueued(ctx context.Context, evt domain.Event) {
	p.Publish(ctx, evt)
}
