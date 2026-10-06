package poller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/tracker"
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

// TestToTrackerLinearRules_SkipsUnarmed: a watched-but-unmapped Linear team has
// no states to ask about, so it never reaches the tracker.
func TestToTrackerLinearRules_SkipsUnarmed(t *testing.T) {
	got := toTrackerLinearRules([]domain.LinearTeamRules{
		armedLinear("team-a", "eng", []string{"Todo"}, []string{"Done"}),
		{TeamID: "team-a", LinearTeamID: "ops", LinearTeamKey: "OPS"},
	})
	if ids := linearTeamIDs(got); ids != "eng" {
		t.Errorf("merged teams = %q, want eng alone", ids)
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

// linearRulesStore answers the org union with a fixed set.
type linearRulesStore struct {
	dbpkg.LinearTeamRulesStore
	rules []domain.LinearTeamRules
}

func (s linearRulesStore) ListForOrgSystem(context.Context, string) ([]domain.LinearTeamRules, error) {
	return s.rules, nil
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
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.Contains(body.Query, "query Viewer"):
			_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u-viewer","name":"Viewer"}}}`))
		case strings.Contains(body.Query, "query SearchIssues"):
			_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`))
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

// TestRunLinearCycleForOrg_Skips covers the two anticipated skips: an org with
// no Linear credential, and one whose only rule is watched but unmapped.
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
			stores := sqlitestore.New(database)
			m := &Manager{
				database: database, pub: busPublisher{bus: newTestBus(t)},
				tasks: stores.Tasks, entities: stores.Entities, repos: stores.Repos, eventQueue: stores.EventQueue,
				orgs: stores.Orgs, users: stores.Users, secrets: linearSecrets{key: tc.key},
				linearRules: linearRulesStore{rules: tc.rules},
			}
			m.runLinearCycleForOrg(context.Background(), runmode.LocalDefaultOrgID, time.Now())
			if got := spanOutcome(t, recorder, "poll.linear.org"); got != tc.outcome {
				t.Errorf("span outcome = %q, want %q", got, tc.outcome)
			}
		})
	}
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
