package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// fakeLinear is an in-memory LinearClient. Issues live in byID; a search
// returns whatever the test put under the query's kind and team; GetIssues
// and GetIssue answer from byID. fail lets a test make a call return an
// error; notFound makes both reads answer not-found for an id, and batchOmits
// makes only the batch read leave it out.
type fakeLinear struct {
	mu         sync.Mutex
	viewer     linear.User
	search     map[string][]linear.Issue // "pickup:<team>" / "assigned:<team>"
	byID       map[string]linear.Issue
	calls      []string
	filters    []linear.IssueFilter
	batches    [][]string
	fail       func(call string, n int) error
	notFound   map[string]bool
	batchOmits map[string]bool
}

func newFakeLinear() *fakeLinear {
	return &fakeLinear{
		viewer: linear.User{ID: "u-viewer", Name: "Viewer"},
		search: map[string][]linear.Issue{},
		byID:   map[string]linear.Issue{},
	}
}

// record logs a call and returns the error the test injected for it, if any.
func (f *fakeLinear) record(call string) error {
	f.calls = append(f.calls, call)
	if f.fail != nil {
		return f.fail(call, len(f.calls))
	}
	return nil
}

func (f *fakeLinear) Viewer(context.Context) (linear.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("viewer"); err != nil {
		return linear.User{}, err
	}
	return f.viewer, nil
}

func (f *fakeLinear) SearchIssues(_ context.Context, filter linear.IssueFilter, _ string) (linear.IssuePage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind := "pickup"
	if filter.AssigneeID != "" {
		kind = "assigned"
	}
	key := kind + ":" + filter.TeamID
	if err := f.record("search:" + key); err != nil {
		return linear.IssuePage{}, err
	}
	f.filters = append(f.filters, filter)
	return linear.IssuePage{Items: f.search[key]}, nil
}

func (f *fakeLinear) GetIssues(_ context.Context, ids []string) ([]linear.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, slices.Clone(ids))
	if err := f.record(fmt.Sprintf("getIssues:%d", len(ids))); err != nil {
		return nil, err
	}
	var out []linear.Issue
	for _, id := range ids {
		if is, ok := f.byID[id]; ok && !f.notFound[id] && !f.batchOmits[id] {
			out = append(out, is)
		}
	}
	return out, nil
}

func (f *fakeLinear) GetIssue(_ context.Context, ref string) (*linear.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("getIssue:" + ref); err != nil {
		return nil, err
	}
	if f.notFound[ref] {
		return nil, fmt.Errorf("%w: issue %s", linear.ErrNotFound, ref)
	}
	if is, ok := f.byID[ref]; ok {
		return &is, nil
	}
	for _, is := range f.byID {
		if is.Identifier == ref {
			return &is, nil
		}
	}
	return nil, fmt.Errorf("%w: issue %s", linear.ErrNotFound, ref)
}

func (f *fakeLinear) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// put stores an issue for the batch and individual reads.
func (f *fakeLinear) put(is linear.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[is.ID] = is
}

func rateLimited(reset time.Time) error {
	return &linear.RateLimitError{Reset: reset, Err: errors.New("linear: graphql error RATELIMITED")}
}

const linTeamID = "team-eng"

func linState(r domain.LinearStateRef) linear.WorkflowState {
	return linear.WorkflowState{ID: r.ID, Name: r.Name, Type: r.Type}
}

// linIssue is an open, unassigned issue in team ENG.
func linIssue(n int) linear.Issue {
	return linear.Issue{
		ID:            fmt.Sprintf("uuid-%d", n),
		Identifier:    fmt.Sprintf("ENG-%d", n),
		Title:         fmt.Sprintf("Issue %d", n),
		Description:   "body",
		URL:           fmt.Sprintf("https://linear.app/acme/issue/ENG-%d", n),
		Priority:      3,
		PriorityLabel: "Medium",
		State:         linState(linTodo),
		Team:          linear.Team{ID: linTeamID, Key: "ENG"},
		Labels:        []string{},
		UpdatedAt:     linUpdatedAt,
	}
}

func linRules() LinearRules {
	return LinearRules{{
		ID: linTeamID, Key: "ENG",
		Pickup:     []domain.LinearStateRef{linTodo},
		InProgress: []domain.LinearStateRef{linProgress},
		Done:       linDoneSet,
	}}
}

type linearFixture struct {
	tr     *Tracker
	pub    *recordingPublisher
	stores db.Stores
	client *fakeLinear
}

func newLinearFixture(t *testing.T) *linearFixture {
	t.Helper()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, runmode.LocalDefaultOrgID)
	return &linearFixture{tr: tr, pub: pub, stores: stores, client: newFakeLinear()}
}

// cycle runs one RefreshLinear with a fresh publisher, so each cycle's events
// are its own.
func (fx *linearFixture) cycle(t *testing.T, teams LinearRules) ([]domain.Event, error) {
	t.Helper()
	fx.pub = &recordingPublisher{}
	fx.tr.pub = fx.pub
	_, err := fx.tr.RefreshLinear(context.Background(), fx.client, teams)
	return fx.pub.nonSystemEvents(), err
}

func (fx *linearFixture) pollCompleted() bool {
	fx.pub.mu.Lock()
	defer fx.pub.mu.Unlock()
	for _, e := range fx.pub.events {
		if e.EventType == domain.EventSystemPollCompleted {
			return true
		}
	}
	return false
}

func (fx *linearFixture) entity(t *testing.T, identifier string) *domain.Entity {
	t.Helper()
	e, err := fx.stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "linear", identifier)
	if err != nil || e == nil {
		t.Fatalf("GetBySource(%s): entity=%v err=%v", identifier, e, err)
	}
	return e
}

func (fx *linearFixture) snapshot(t *testing.T, identifier string) domain.LinearSnapshot {
	t.Helper()
	var snap domain.LinearSnapshot
	if err := json.Unmarshal([]byte(fx.entity(t, identifier).SnapshotJSON), &snap); err != nil {
		t.Fatalf("snapshot of %s: %v", identifier, err)
	}
	return snap
}

// seed discovers issues through the pickup query and runs the cycle that
// seeds them, then empties the query so later cycles reach them only through
// the refresh.
func (fx *linearFixture) seed(t *testing.T, issues ...linear.Issue) {
	t.Helper()
	fx.client.search["pickup:"+linTeamID] = issues
	for _, is := range issues {
		fx.client.put(is)
	}
	if evts, err := fx.cycle(t, linRules()); err != nil || len(evts) != 0 {
		t.Fatalf("seed cycle: events=%v err=%v, want a quiet seed", eventTypes(evts), err)
	}
	fx.client.search = map[string][]linear.Issue{}
	fx.client.calls, fx.client.batches, fx.client.filters = nil, nil, nil
}

// TestRefreshLinear_DiscoveryQueries pins both queries' filters: pickup is
// unassigned issues in the pickup states, assigned is the viewer's issues
// outside the done states, the viewer is read once per cycle, and a team with
// no pickup states sends no pickup query rather than an unfiltered one.
func TestRefreshLinear_DiscoveryQueries(t *testing.T) {
	fx := newLinearFixture(t)
	teams := append(linRules(), LinearTeamRule{ID: "team-ops", Key: "OPS", Done: linDoneSet})
	if _, err := fx.cycle(t, teams); err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}

	want := []linear.IssueFilter{
		{TeamID: linTeamID, StateIDsIn: []string{linTodo.ID}, Unassigned: true},
		{TeamID: linTeamID, AssigneeID: "u-viewer", StateIDsNotIn: []string{linDone.ID}},
		{TeamID: "team-ops", AssigneeID: "u-viewer", StateIDsNotIn: []string{linDone.ID}},
	}
	if len(fx.client.filters) != len(want) {
		t.Fatalf("filters = %+v, want %+v", fx.client.filters, want)
	}
	for i := range want {
		got := fx.client.filters[i]
		if got.TeamID != want[i].TeamID || got.Unassigned != want[i].Unassigned || got.AssigneeID != want[i].AssigneeID ||
			!slices.Equal(got.StateIDsIn, want[i].StateIDsIn) || !slices.Equal(got.StateIDsNotIn, want[i].StateIDsNotIn) {
			t.Errorf("filter[%d] = %+v, want %+v", i, got, want[i])
		}
	}
	viewerReads := 0
	for _, c := range fx.client.callLog() {
		if c == "viewer" {
			viewerReads++
		}
	}
	if viewerReads != 1 {
		t.Errorf("viewer read %d times, want once per cycle", viewerReads)
	}
}

// TestRefreshLinear_PickupDiscoverySeedsQuietly: an issue first seen through
// the pickup query is recorded with no event, as a Jira pickup issue is, and
// its snapshot and description are stored.
func TestRefreshLinear_PickupDiscoverySeedsQuietly(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(1)
	is.Children = []linear.ChildIssue{
		{ID: "c1", State: linState(linDone)},
		{ID: "c2", State: linState(linProgress)},
		{ID: "c3"},
	}
	fx.seed(t, is)

	snap := fx.snapshot(t, "ENG-1")
	if snap.ID != "uuid-1" || snap.TeamID != linTeamID || snap.State.ID != linTodo.ID {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.OpenChildCount != 2 {
		t.Errorf("open_child_count = %d, want 2 (an unknown state counts as open)", snap.OpenChildCount)
	}
	if snap.BodyHash == "" {
		t.Error("body hash not recorded")
	}
	if e := fx.entity(t, "ENG-1"); e.Description != "body" || e.URL != is.URL || e.Title != is.Title {
		t.Errorf("entity = title %q url %q description %q", e.Title, e.URL, e.Description)
	}
}

// TestRefreshLinear_FirstDiscoveryAssignedToViewerEmitsAssignment: arriving
// through the assigned-to-viewer query is the assignment, committed with the
// first snapshot; the next cycle does not emit it again.
func TestRefreshLinear_FirstDiscoveryAssignedToViewerEmitsAssignment(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(2)
	is.Assignee = &linear.User{ID: "u-viewer", Name: "Viewer"}
	fx.client.search["assigned:"+linTeamID] = []linear.Issue{is}
	fx.client.put(is)

	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueAssigned}) {
		t.Fatalf("events = %v, want [assigned]", got)
	}
	e := fx.entity(t, "ENG-2")
	queued, err := fx.stores.EventQueue.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, e.ID)
	if err != nil || len(queued) != 1 || queued[0].EventID != evts[0].ID {
		t.Fatalf("queued = %v err=%v, want the assignment committed with the seed", queued, err)
	}
	if fx.snapshot(t, "ENG-2").AssigneeUserID != "u-viewer" {
		t.Error("snapshot was not seeded with the assignment")
	}

	if evts, err := fx.cycle(t, linRules()); err != nil || len(evts) != 0 {
		t.Fatalf("cycle 2: events=%v err=%v, want nothing re-emitted", eventTypes(evts), err)
	}
}

// TestRefreshLinear_RefreshDiffsAndQuietCycle: a tracked issue's change comes
// through the batch refresh; a cycle that observes no change emits nothing.
func TestRefreshLinear_RefreshDiffsAndQuietCycle(t *testing.T) {
	fx := newLinearFixture(t)
	fx.seed(t, linIssue(3))

	is := linIssue(3)
	is.State = linState(linDone)
	is.UpdatedAt = "2026-10-01T11:00:00.000Z"
	fx.client.put(is)
	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueStatusChanged, domain.EventLinearIssueCompleted}) {
		t.Fatalf("events = %v, want status_changed then completed", got)
	}
	if !fx.pollCompleted() {
		t.Error("a completed cycle did not emit the poll-complete sentinel")
	}
	if !slices.Contains(fx.client.callLog(), "getIssues:1") {
		t.Errorf("calls = %v, want the tracked issue read by batch", fx.client.callLog())
	}

	if evts, err := fx.cycle(t, linRules()); err != nil || len(evts) != 0 {
		t.Fatalf("unchanged cycle: events=%v err=%v, want nothing", eventTypes(evts), err)
	}
}

// TestRefreshLinear_RefreshBatchesByFifty: tracked issues are read 50 at a
// time.
func TestRefreshLinear_RefreshBatchesByFifty(t *testing.T) {
	fx := newLinearFixture(t)
	var issues []linear.Issue
	for n := 1; n <= 60; n++ {
		issues = append(issues, linIssue(n))
	}
	fx.seed(t, issues...)
	if _, err := fx.cycle(t, linRules()); err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	var batches []string
	for _, c := range fx.client.callLog() {
		if strings.HasPrefix(c, "getIssues:") {
			batches = append(batches, c)
		}
	}
	if !slices.Equal(batches, []string{"getIssues:50", "getIssues:10"}) {
		t.Errorf("batches = %v, want 50 then 10", batches)
	}
}

// TestRefreshLinear_Unreachable: an issue the batch did not return is asked
// about directly, and only Linear's answer about that issue retires it. Each
// case's entity must retire with one unreachable carrying its last-known
// state, and nothing else.
func TestRefreshLinear_Unreachable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fx *linearFixture, is linear.Issue)
	}{
		{"missing from the batch and not found", func(fx *linearFixture, is linear.Issue) {
			fx.client.notFound = map[string]bool{is.ID: true}
		}},
		{"trashed", func(fx *linearFixture, is linear.Issue) {
			is.Trashed, is.ArchivedAt = true, "2026-10-01T12:00:00Z"
			fx.client.put(is)
		}},
		{"archived outside a done state", func(fx *linearFixture, is linear.Issue) {
			is.ArchivedAt = "2026-10-01T12:00:00Z"
			fx.client.put(is)
		}},
		{"moved to another team", func(fx *linearFixture, is linear.Issue) {
			is.Identifier = "OPS-77"
			is.Team = linear.Team{ID: "team-ops", Key: "OPS"}
			fx.client.put(is)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLinearFixture(t)
			is := linIssue(4)
			is.Assignee = &linear.User{ID: "u-alice", Name: "Alice"}
			fx.seed(t, is)
			tc.mutate(fx, is)

			evts, err := fx.cycle(t, linRules())
			if err != nil {
				t.Fatalf("RefreshLinear: %v", err)
			}
			if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueUnreachable}) {
				t.Fatalf("events = %v, want [unreachable]", got)
			}
			var meta events.LinearIssueUnreachableMetadata
			if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil {
				t.Fatal(err)
			}
			if meta.IssueIdentifier != "ENG-4" || meta.LinearTeamID != linTeamID || meta.AssigneeUserID != "u-alice" || meta.LastStatus != "Todo" {
				t.Errorf("metadata = %+v, want the entity's last-known state", meta)
			}
			if evts[0].EntityID == nil || *evts[0].EntityID != fx.entity(t, "ENG-4").ID {
				t.Errorf("event entity = %v", evts[0].EntityID)
			}
		})
	}
}

// TestRefreshLinear_MissingFromBatchButResolves: absence from a batch is not
// evidence. An issue Linear does give back when asked is diffed like any
// refreshed one.
func TestRefreshLinear_MissingFromBatchButResolves(t *testing.T) {
	fx := newLinearFixture(t)
	fx.seed(t, linIssue(5))

	is := linIssue(5)
	is.Priority, is.PriorityLabel = 1, "Urgent"
	fx.client.put(is)
	fx.client.batchOmits = map[string]bool{is.ID: true}
	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssuePriorityChanged}) {
		t.Fatalf("events = %v, want the change diffed, not unreachable", got)
	}
	if !slices.Contains(fx.client.callLog(), "getIssue:uuid-5") {
		t.Errorf("calls = %v, want the issue asked about by UUID", fx.client.callLog())
	}
}

// TestRefreshLinear_ArchivedDoneIsTheTerminalPath: archived in a done state is
// completion, not unreachability.
func TestRefreshLinear_ArchivedDoneIsTheTerminalPath(t *testing.T) {
	fx := newLinearFixture(t)
	fx.seed(t, linIssue(6))
	is := linIssue(6)
	is.State = linState(linDone)
	is.ArchivedAt = "2026-10-01T12:00:00Z"
	fx.client.put(is)

	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueStatusChanged, domain.EventLinearIssueCompleted}) {
		t.Fatalf("events = %v, want status_changed then completed", got)
	}
}

// TestRefreshLinear_SnapshotlessEntitySeedsQuietly: an entity with no snapshot
// (a stub created outside the poller, or one a source pause cleared) has no
// UUID to batch by, so it is read by identifier and seeded without a diff.
func TestRefreshLinear_SnapshotlessEntitySeedsQuietly(t *testing.T) {
	fx := newLinearFixture(t)
	if _, _, err := fx.stores.Entities.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, "linear", "ENG-7", "issue", "", ""); err != nil {
		t.Fatal(err)
	}
	is := linIssue(7)
	is.Assignee = &linear.User{ID: "u-alice", Name: "Alice"}
	fx.client.put(is)

	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if len(evts) != 0 {
		t.Fatalf("events = %v, want a quiet seed", eventTypes(evts))
	}
	if !slices.Contains(fx.client.callLog(), "getIssue:ENG-7") {
		t.Errorf("calls = %v, want the stub read by identifier", fx.client.callLog())
	}
	if snap := fx.snapshot(t, "ENG-7"); snap.ID != "uuid-7" || snap.AssigneeUserID != "u-alice" {
		t.Errorf("snapshot = %+v", snap)
	}
	if e := fx.entity(t, "ENG-7"); e.Title != is.Title {
		t.Errorf("title = %q, want %q", e.Title, is.Title)
	}
}

// TestRefreshLinear_LosingCASHasNoEffect: a refresh whose snapshot CAS loses
// writes nothing — no event, no queue row, no snapshot.
func TestRefreshLinear_LosingCASHasNoEffect(t *testing.T) {
	fx := newLinearFixture(t)
	fx.seed(t, linIssue(8))
	before := fx.entity(t, "ENG-8").SnapshotJSON

	is := linIssue(8)
	is.State = linState(linProgress)
	fx.client.put(is)
	queue := &failingBatchQueue{EventQueueStore: fx.stores.EventQueue}
	fx.tr.queue = queue

	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if queue.calls == 0 {
		t.Fatal("the refresh never reached the snapshot CAS")
	}
	if len(evts) != 0 {
		t.Errorf("events = %v, want none from a lost CAS", eventTypes(evts))
	}
	if n := countQueueRows(t, fx.tr); n != 0 {
		t.Errorf("queue rows = %d, want 0", n)
	}
	if after := fx.entity(t, "ENG-8").SnapshotJSON; after != before {
		t.Errorf("snapshot moved under a lost CAS:\nbefore %s\nafter  %s", before, after)
	}
}

// TestRefreshLinear_RateLimitedDiscovery: a rate limit on the first request
// ends the cycle there. No second request, no poll-complete, and the error
// carries the reset.
func TestRefreshLinear_RateLimitedDiscovery(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute)
	for _, at := range []string{"viewer", "search:pickup:" + linTeamID} {
		t.Run(at, func(t *testing.T) {
			fx := newLinearFixture(t)
			fx.client.fail = func(call string, _ int) error {
				if call == at {
					return rateLimited(reset)
				}
				return nil
			}
			_, err := fx.cycle(t, linRules())
			var rl *linear.RateLimitError
			if !errors.Is(err, linear.ErrRateLimited) || !errors.As(err, &rl) || !rl.Reset.Equal(reset) {
				t.Fatalf("err = %v, want the rate limit with its reset", err)
			}
			calls := fx.client.callLog()
			if calls[len(calls)-1] != at {
				t.Errorf("calls = %v, want nothing sent after %s", calls, at)
			}
			if fx.pollCompleted() {
				t.Error("a rate-limited cycle emitted poll-complete")
			}
		})
	}
}

// TestRefreshLinear_RateLimitedMidRefresh: a rate limit partway through the
// refresh keeps what the batches before it wrote, and the batch it cut short
// produces nothing: no events, no confirmation reads, no unreachable.
func TestRefreshLinear_RateLimitedMidRefresh(t *testing.T) {
	fx := newLinearFixture(t)
	var issues []linear.Issue
	for n := 1; n <= 60; n++ {
		issues = append(issues, linIssue(n))
	}
	fx.seed(t, issues...)
	// Every issue changes; the second batch is refused.
	for n := 1; n <= 60; n++ {
		is := linIssue(n)
		is.State = linState(linProgress)
		fx.client.put(is)
	}
	batches := 0
	fx.client.fail = func(call string, _ int) error {
		if strings.HasPrefix(call, "getIssues:") {
			batches++
			if batches == 2 {
				return rateLimited(time.Time{})
			}
		}
		return nil
	}
	entities, err := fx.stores.Entities.ListActiveSystem(context.Background(), runmode.LocalDefaultOrgID, "linear")
	if err != nil {
		t.Fatal(err)
	}

	evts, err := fx.cycle(t, linRules())
	if !errors.Is(err, linear.ErrRateLimited) {
		t.Fatalf("err = %v, want the rate limit", err)
	}
	if len(fx.client.batches) != 2 || len(fx.client.batches[0]) != 50 {
		t.Fatalf("batches = %d, want two with the first holding 50", len(fx.client.batches))
	}
	firstBatch := map[string]bool{}
	for _, id := range fx.client.batches[0] {
		firstBatch[id] = true
	}
	byEntity := map[string]domain.LinearSnapshot{}
	for _, e := range entities {
		byEntity[e.ID] = fx.snapshot(t, e.SourceID)
	}
	if len(evts) != 50 {
		t.Errorf("events = %d, want 50: the first batch's transitions, none from the batch cut short", len(evts))
	}
	for _, e := range evts {
		if e.EventType != domain.EventLinearIssueStatusChanged || !firstBatch[byEntity[*e.EntityID].ID] {
			t.Errorf("unexpected event %s on %s", e.EventType, *e.EntityID)
		}
	}
	for _, snap := range byEntity {
		wantState := linTodo.ID
		if firstBatch[snap.ID] {
			wantState = linProgress.ID
		}
		if snap.State.ID != wantState {
			t.Errorf("%s state = %s, want %s", snap.Identifier, snap.State.ID, wantState)
		}
	}
	for _, c := range fx.client.callLog() {
		if strings.HasPrefix(c, "getIssue:") {
			t.Errorf("a rate-limited batch led to an individual read (%s)", c)
		}
	}
	if fx.pollCompleted() {
		t.Error("a rate-limited cycle emitted poll-complete")
	}
}

// TestRefreshLinear_RateLimitedConfirmation: a rate limit on the individual
// read is not evidence that the issue is gone.
func TestRefreshLinear_RateLimitedConfirmation(t *testing.T) {
	fx := newLinearFixture(t)
	fx.seed(t, linIssue(9))
	fx.client.notFound = map[string]bool{"uuid-9": true}
	fx.client.fail = func(call string, _ int) error {
		if strings.HasPrefix(call, "getIssue:") {
			return rateLimited(time.Time{})
		}
		return nil
	}
	evts, err := fx.cycle(t, linRules())
	if !errors.Is(err, linear.ErrRateLimited) {
		t.Fatalf("err = %v, want the rate limit", err)
	}
	if len(evts) != 0 {
		t.Errorf("events = %v, want none", eventTypes(evts))
	}
	if fx.pollCompleted() {
		t.Error("a rate-limited cycle emitted poll-complete")
	}
}

// TestRefreshLinear_ConfirmationBudget: at most linearConfirmBudget issues are
// asked about one at a time in a cycle.
func TestRefreshLinear_ConfirmationBudget(t *testing.T) {
	fx := newLinearFixture(t)
	var issues []linear.Issue
	for n := 1; n <= linearConfirmBudget+5; n++ {
		issues = append(issues, linIssue(n))
	}
	fx.seed(t, issues...)
	fx.client.notFound = map[string]bool{}
	for _, is := range issues {
		fx.client.notFound[is.ID] = true
	}
	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if len(evts) != linearConfirmBudget {
		t.Errorf("unreachable events = %d, want %d", len(evts), linearConfirmBudget)
	}
}

// TestRefreshLinear_DiscoveryUnreachedReportsNoPoll: when every call fails on
// the connection, the cycle returns the failure and does not report a poll.
func TestRefreshLinear_DiscoveryUnreachedReportsNoPoll(t *testing.T) {
	fx := newLinearFixture(t)
	fx.client.fail = func(string, int) error {
		return &linear.StatusError{Status: 401, Class: upstream.Auth}
	}
	if _, err := fx.cycle(t, linRules()); err == nil {
		t.Fatal("RefreshLinear succeeded with every call refused")
	}
	if fx.pollCompleted() {
		t.Error("an unanswered cycle emitted poll-complete")
	}
}
