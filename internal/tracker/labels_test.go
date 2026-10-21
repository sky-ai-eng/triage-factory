package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// labelEvent is an emitted label event reduced to what the label tests check.
type labelEvent struct {
	eventType, dedupKey string
}

func labelEvents(evts []domain.Event) []labelEvent {
	var out []labelEvent
	for _, e := range evts {
		if strings.HasSuffix(e.EventType, ":label_added") || strings.HasSuffix(e.EventType, ":label_removed") {
			out = append(out, labelEvent{e.EventType, e.DedupKey})
		}
	}
	return out
}

// --- Jira ---------------------------------------------------------------------

// jiraLabelSnap is an open, assigned Jira issue with nothing unusual about it.
func jiraLabelSnap(labels ...string) domain.JiraSnapshot {
	if labels == nil {
		labels = []string{}
	}
	return domain.JiraSnapshot{
		ID: "10001", Key: "SKY-1", ProjectID: "10000", ProjectKey: "SKY", Summary: "Fix the thing", Status: "To Do",
		Assignee: testUser, AssigneeAccountID: testUserAccountID,
		Reporter: "Grace", ReporterAccountID: "557058:grace",
		IssueType: "Task", Priority: "High", Labels: labels,
		UpdatedAt: "2026-10-01T10:00:00.000+0000",
	}
}

func TestDiffJira_Labels(t *testing.T) {
	added, removed := domain.EventJiraIssueLabelAdded, domain.EventJiraIssueLabelRemoved
	cases := []struct {
		name       string
		prev, curr domain.JiraSnapshot
		want       []labelEvent
	}{
		{name: "label added", prev: jiraLabelSnap(), curr: jiraLabelSnap("tf-agent"),
			want: []labelEvent{{added, "tf-agent"}}},
		{name: "label removed", prev: jiraLabelSnap("tf-agent"), curr: jiraLabelSnap(),
			want: []labelEvent{{removed, "tf-agent"}}},
		{name: "one added, one removed", prev: jiraLabelSnap("backend", "triage"), curr: jiraLabelSnap("backend", "tf-agent"),
			want: []labelEvent{{added, "tf-agent"}, {removed, "triage"}}},
		{name: "unchanged", prev: jiraLabelSnap("backend"), curr: jiraLabelSnap("backend")},
		{name: "reordered", prev: jiraLabelSnap("a", "b"), curr: jiraLabelSnap("b", "a")},
		{name: "null and empty are the same set", prev: domain.JiraSnapshot{ID: "10001", Key: "SKY-1"}, curr: jiraLabelSnap()},
		{name: "added on a parent with open subtasks",
			prev: func() domain.JiraSnapshot { s := jiraLabelSnap(); s.OpenSubtaskCount = 2; return s }(),
			curr: func() domain.JiraSnapshot { s := jiraLabelSnap("tf-agent"); s.OpenSubtaskCount = 2; return s }(),
			want: []labelEvent{{added, "tf-agent"}}},
		{name: "first discovery with a label", prev: domain.JiraSnapshot{}, curr: jiraLabelSnap("tf-agent")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := labelEvents(DiffJiraSnapshots(tc.prev, tc.curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil))
			if !slices.Equal(got, tc.want) {
				t.Errorf("label events = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiffJira_LabelMetadataAndPredicates(t *testing.T) {
	evts := DiffJiraSnapshots(jiraLabelSnap("backend"), jiraLabelSnap("backend", "tf-agent"), testEntityID, JiraEventProjects{Current: "jp-sky", Previous: "jp-sky"}, testDoneStatuses, nil)
	evt := findEvent(evts, domain.EventJiraIssueLabelAdded)
	if evt == nil {
		t.Fatalf("no label_added in %v", eventTypes(evts))
	}
	assertEntityID(t, *evt)
	meta := decodeMetadata[events.JiraIssueLabelMetadata](t, *evt)
	want := events.JiraIssueLabelMetadata{
		Assignee: testUser, AssigneeAccountID: testUserAccountID,
		Reporter: "Grace", ReporterAccountID: "557058:grace",
		IssueKey: "SKY-1", IssueID: "10001", Project: "SKY", JiraProjectID: "jp-sky", IssueType: "Task",
		Priority: "High", Status: "To Do", Summary: "Fix the thing",
		LabelName: "tf-agent", Labels: []string{"backend", "tf-agent"},
	}
	if !reflect.DeepEqual(meta, want) {
		t.Errorf("metadata = %+v, want %+v", meta, want)
	}
	assertOccurredAt(t, evt, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC))

	for _, tc := range []struct {
		pred string
		want bool
	}{
		{`{"label_name":"tf-agent"}`, true},
		{`{"label_name":"backend"}`, false},
		{`{"has_label":"backend"}`, true},
		{`{"has_label":"frontend"}`, false},
		{`{"project":"SKY"}`, true},
		{`{"project":"OPS"}`, false},
		{allowlist(t, "assignee_in", testUserAccountID), true},
		{allowlist(t, "assignee_in", "someone-else"), false},
	} {
		if got := matchesPredicate(t, *evt, tc.pred); got != tc.want {
			t.Errorf("predicate %s matched = %v, want %v", tc.pred, got, tc.want)
		}
	}

	removedEvts := DiffJiraSnapshots(jiraLabelSnap("backend", "tf-agent"), jiraLabelSnap("backend"), testEntityID, JiraEventProjects{}, testDoneStatuses, nil)
	removedEvt := findEvent(removedEvts, domain.EventJiraIssueLabelRemoved)
	if removedEvt == nil {
		t.Fatalf("no label_removed in %v", eventTypes(removedEvts))
	}
	if !matchesPredicate(t, *removedEvt, `{"label_name":"tf-agent"}`) || matchesPredicate(t, *removedEvt, `{"has_label":"tf-agent"}`) {
		t.Errorf("label_removed carries labels after the removal: %s", removedEvt.MetadataJSON)
	}
}

// TestDiffJira_LabelledAtDiscoveryMatchesHasLabel: a label the issue already
// has when TF first sees it is not a label event, and the event discovery does
// emit carries it for has_label. The same holds for became_atomic.
func TestDiffJira_LabelledAtDiscoveryMatchesHasLabel(t *testing.T) {
	unassigned := jiraLabelSnap("tf-agent")
	unassigned.Assignee, unassigned.AssigneeAccountID = "", ""
	for _, tc := range []struct {
		name       string
		prev, curr domain.JiraSnapshot
		eventType  string
	}{
		{"available", domain.JiraSnapshot{}, unassigned, domain.EventJiraIssueAvailable},
		{"assigned", domain.JiraSnapshot{}, jiraLabelSnap("tf-agent"), domain.EventJiraIssueAssigned},
		{"became_atomic", func() domain.JiraSnapshot { s := jiraLabelSnap("tf-agent"); s.OpenSubtaskCount = 1; return s }(),
			jiraLabelSnap("tf-agent"), domain.EventJiraIssueBecameAtomic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evts := DiffJiraSnapshots(tc.prev, tc.curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil)
			if got := labelEvents(evts); len(got) != 0 {
				t.Errorf("label events = %v, want none", got)
			}
			evt := findEvent(evts, tc.eventType)
			if evt == nil {
				t.Fatalf("no %s in %v", tc.eventType, eventTypes(evts))
			}
			if !matchesPredicate(t, *evt, `{"has_label":"tf-agent"}`) {
				t.Errorf("has_label tf-agent did not match %s", evt.MetadataJSON)
			}
			if matchesPredicate(t, *evt, `{"has_label":"other"}`) {
				t.Errorf("has_label other matched %s", evt.MetadataJSON)
			}
		})
	}
}

// --- Linear diff ----------------------------------------------------------------

// linLabel is a label on a Linear issue.
func linLabel(id, name string) domain.LinearLabelRef {
	return domain.LinearLabelRef{ID: id, Name: name}
}

// withLabels gives a snapshot labels the way linearIssueToState does: the
// names in Labels, and the same labels with their ids in LabelRefs.
func withLabels(s *domain.LinearSnapshot, labels ...domain.LinearLabelRef) {
	s.Labels, s.LabelRefs = []string{}, nil
	for _, l := range labels {
		s.Labels = append(s.Labels, l.Name)
		s.LabelRefs = append(s.LabelRefs, l)
	}
}

var (
	labelAgent  = linLabel("l-agent", "tf-agent")
	labelBug    = linLabel("l-bug", "Bug")
	labelMedium = linLabel("l-medium", "Medium")
	labelHard   = linLabel("l-hard", "Hard")
)

// difficulty is the team's label list read for groups: Medium and Hard sit in
// the Difficulty group.
var difficulty = map[string]string{labelMedium.ID: "Difficulty", labelHard.ID: "Difficulty"}

func TestDiffLinear_Labels(t *testing.T) {
	added, removed := domain.EventLinearIssueLabelAdded, domain.EventLinearIssueLabelRemoved
	cases := []struct {
		name  string
		first bool
		prev  func(*domain.LinearSnapshot)
		curr  func(*domain.LinearSnapshot)
		want  []labelEvent
	}{
		{name: "label added",
			curr: func(s *domain.LinearSnapshot) { withLabels(s, labelAgent) },
			want: []labelEvent{{added, labelAgent.ID}}},
		{name: "label removed",
			prev: func(s *domain.LinearSnapshot) { withLabels(s, labelBug, labelAgent) },
			curr: func(s *domain.LinearSnapshot) { withLabels(s, labelBug) },
			want: []labelEvent{{removed, labelAgent.ID}}},
		{name: "label renamed in Linear",
			prev: func(s *domain.LinearSnapshot) { withLabels(s, labelBug) },
			curr: func(s *domain.LinearSnapshot) { withLabels(s, linLabel(labelBug.ID, "Defect")) }},
		{name: "single-select group switched",
			prev: func(s *domain.LinearSnapshot) { withLabels(s, labelMedium) },
			curr: func(s *domain.LinearSnapshot) { withLabels(s, labelHard) },
			want: []labelEvent{{added, labelHard.ID}, {removed, labelMedium.ID}}},
		{name: "added on a parent with open sub-issues",
			prev: func(s *domain.LinearSnapshot) { s.OpenChildCount = 2 },
			curr: func(s *domain.LinearSnapshot) { s.OpenChildCount = 2; withLabels(s, labelAgent) },
			want: []labelEvent{{added, labelAgent.ID}}},
		{name: "first discovery with a label", first: true,
			curr: func(s *domain.LinearSnapshot) { withLabels(s, labelAgent) }},

		// A snapshot stored before label ids were recorded has names alone.
		{name: "pre-id snapshot, names unchanged",
			prev: func(s *domain.LinearSnapshot) { s.Labels = []string{"Bug", "tf-agent"} },
			curr: func(s *domain.LinearSnapshot) { withLabels(s, labelBug, labelAgent) }},
		{name: "pre-id snapshot, a label added",
			prev: func(s *domain.LinearSnapshot) { s.Labels = []string{"Bug"} },
			curr: func(s *domain.LinearSnapshot) { withLabels(s, labelBug, labelAgent) },
			want: []labelEvent{{added, labelAgent.ID}}},
		{name: "pre-id snapshot, a label removed",
			prev: func(s *domain.LinearSnapshot) { s.Labels = []string{"Bug", "Old"} },
			curr: func(s *domain.LinearSnapshot) { withLabels(s, labelBug) },
			want: []labelEvent{{removed, "Old"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev, curr := linSnap(), linSnap()
			if tc.first {
				prev = domain.LinearSnapshot{}
			} else if tc.prev != nil {
				tc.prev(&prev)
			}
			if tc.curr != nil {
				tc.curr(&curr)
			}
			got := labelEvents(DiffLinearSnapshots(prev, curr, "ent-1", linDoneSet, nil, difficulty))
			if !slices.Equal(got, tc.want) {
				t.Errorf("label events = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDiffLinear_GroupSwitchCarriesTheGroup: switching a single-select group
// is a removal and an addition, each naming the group, so a rule can tell the
// Difficulty group's Medium from another group's.
func TestDiffLinear_GroupSwitchCarriesTheGroup(t *testing.T) {
	prev, curr := linSnap(), linSnap()
	withLabels(&prev, labelBug, labelMedium)
	withLabels(&curr, labelBug, labelHard)
	evts := DiffLinearSnapshots(prev, curr, "ent-1", linDoneSet, nil, difficulty)

	addedEvt := findEvent(evts, domain.EventLinearIssueLabelAdded)
	removedEvt := findEvent(evts, domain.EventLinearIssueLabelRemoved)
	if addedEvt == nil || removedEvt == nil {
		t.Fatalf("events = %v, want one label_added and one label_removed", eventTypes(evts))
	}
	gotAdded := decodeMetadata[events.LinearIssueLabelMetadata](t, *addedEvt)
	wantAdded := events.LinearIssueLabelMetadata{
		LinearIssueIdentity: linearIdentity(curr),
		Priority:            "Medium", Status: "Todo",
		LabelName: "Hard", LabelGroup: "Difficulty", Labels: []string{"Bug", "Hard"},
	}
	if !reflect.DeepEqual(gotAdded, wantAdded) {
		t.Errorf("label_added metadata = %+v, want %+v", gotAdded, wantAdded)
	}
	gotRemoved := decodeMetadata[events.LinearIssueLabelMetadata](t, *removedEvt)
	if gotRemoved.LabelName != "Medium" || gotRemoved.LabelGroup != "Difficulty" || !slices.Equal(gotRemoved.Labels, []string{"Bug", "Hard"}) {
		t.Errorf("label_removed metadata = %+v, want Medium in Difficulty with the labels after the change", gotRemoved)
	}

	for _, tc := range []struct {
		pred string
		want bool
	}{
		{`{"label_group":"Difficulty","label_name":"Hard"}`, true},
		{`{"label_group":"Size","label_name":"Hard"}`, false},
		{`{"label_name":"Medium"}`, false},
		{`{"has_label":"Bug"}`, true},
		{`{"has_label":"Medium"}`, false},
		{`{"linear_team_id_in":["team-eng"]}`, true},
		{`{"linear_team_id_in":["ENG"]}`, false},
		{allowlist(t, "assignee_in", "u-alice"), true},
		{allowlist(t, "assignee_in", "u-bob"), false},
	} {
		if got := matchesPredicate(t, *addedEvt, tc.pred); got != tc.want {
			t.Errorf("predicate %s matched = %v, want %v", tc.pred, got, tc.want)
		}
	}

	ungrouped := DiffLinearSnapshots(linSnap(), func() domain.LinearSnapshot { s := linSnap(); withLabels(&s, labelAgent); return s }(),
		"ent-1", linDoneSet, nil, difficulty)
	if m := decodeMetadata[events.LinearIssueLabelMetadata](t, ungrouped[0]); m.LabelGroup != "" {
		t.Errorf("label in no group reported in group %q", m.LabelGroup)
	}
}

// TestDiffLinear_LabelledAtDiscoveryMatchesHasLabel: the Linear sibling of
// the Jira test of the same name.
func TestDiffLinear_LabelledAtDiscoveryMatchesHasLabel(t *testing.T) {
	labelled := linSnap()
	withLabels(&labelled, labelAgent)
	unassigned := labelled
	unassigned.Assignee, unassigned.AssigneeUserID = "", ""
	parent := labelled
	parent.OpenChildCount = 1
	for _, tc := range []struct {
		name       string
		prev, curr domain.LinearSnapshot
		eventType  string
	}{
		{"available", domain.LinearSnapshot{}, unassigned, domain.EventLinearIssueAvailable},
		{"assigned", domain.LinearSnapshot{}, labelled, domain.EventLinearIssueAssigned},
		{"became_atomic", parent, labelled, domain.EventLinearIssueBecameAtomic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evts := DiffLinearSnapshots(tc.prev, tc.curr, "ent-1", linDoneSet, nil, nil)
			if got := labelEvents(evts); len(got) != 0 {
				t.Errorf("label events = %v, want none", got)
			}
			evt := findEvent(evts, tc.eventType)
			if evt == nil {
				t.Fatalf("no %s in %v", tc.eventType, eventTypes(evts))
			}
			if !matchesPredicate(t, *evt, `{"has_label":"tf-agent"}`) {
				t.Errorf("has_label tf-agent did not match %s", evt.MetadataJSON)
			}
			if matchesPredicate(t, *evt, `{"has_label":"other"}`) {
				t.Errorf("has_label other matched %s", evt.MetadataJSON)
			}
		})
	}
}

// --- Linear refresh -----------------------------------------------------------

// traceRecorder is installed on the OTel global once for the package: the
// package's tracer resolves through the global delegate, which binds to the
// first provider installed, so a recorder installed per test would capture
// nothing after the first.
var (
	traceOnce     sync.Once
	traceRecorder = tracetest.NewSpanRecorder()
)

// recordSpans returns a function reporting the spans ended since it was
// called.
func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	traceOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(traceRecorder)))
	})
	start := len(traceRecorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return traceRecorder.Ended()[start:] }
}

// spanOutcomes is the outcome attribute of each span named name, in order.
func spanOutcomes(spans []sdktrace.ReadOnlySpan, name string) []string {
	var out []string
	for _, s := range spans {
		if s.Name() != name {
			continue
		}
		outcome := ""
		for _, a := range s.Attributes() {
			if a.Key == "outcome" {
				outcome = a.Value.AsString()
			}
		}
		out = append(out, outcome)
	}
	return out
}

func linIssueLabels(labels ...domain.LinearLabelRef) []linear.Label {
	out := make([]linear.Label, 0, len(labels))
	for _, l := range labels {
		out = append(out, linear.Label{ID: l.ID, Name: l.Name})
	}
	return out
}

// teamLabels is ENG's label list, with Medium and Hard in the Difficulty
// group.
func teamLabels() []linear.Label {
	return []linear.Label{
		{ID: labelAgent.ID, Name: labelAgent.Name},
		{ID: labelBug.ID, Name: labelBug.Name},
		{ID: labelHard.ID, Name: labelHard.Name, Group: "Difficulty"},
		{ID: labelMedium.ID, Name: labelMedium.Name, Group: "Difficulty"},
	}
}

func labelListReads(f *fakeLinear) int {
	n := 0
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "labels:") {
			n++
		}
	}
	return n
}

// TestRefreshLinear_LabelChanges: a label added or removed on a tracked issue
// is one event on the next poll, carrying its group. The team's label list is
// read once for the cycle however many of its issues changed labels, and not
// at all in a cycle where none did.
func TestRefreshLinear_LabelChanges(t *testing.T) {
	fx := newLinearFixture(t)
	fx.client.labels[linTeamID] = teamLabels()
	one, two := linIssue(1), linIssue(2)
	one.Labels = linIssueLabels(labelMedium)
	fx.seed(t, one, two)
	if n := labelListReads(fx.client); n != 0 {
		t.Errorf("label list read %d times while seeding, want none", n)
	}

	one.Labels = linIssueLabels(labelHard)
	two.Labels = linIssueLabels(labelAgent)
	fx.client.put(one)
	fx.client.put(two)
	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	// The two issues' events come in the order the issues were refreshed,
	// which this test does not pin.
	cmp := func(a, b labelEvent) int { return strings.Compare(a.eventType+a.dedupKey, b.eventType+b.dedupKey) }
	want := []labelEvent{
		{domain.EventLinearIssueLabelAdded, labelHard.ID},
		{domain.EventLinearIssueLabelRemoved, labelMedium.ID},
		{domain.EventLinearIssueLabelAdded, labelAgent.ID},
	}
	got := labelEvents(evts)
	slices.SortFunc(got, cmp)
	slices.SortFunc(want, cmp)
	if !slices.Equal(got, want) {
		t.Fatalf("label events = %v, want %v", got, want)
	}
	for _, e := range evts {
		m := decodeMetadata[events.LinearIssueLabelMetadata](t, e)
		wantGroup := difficulty[e.DedupKey]
		if m.LabelGroup != wantGroup {
			t.Errorf("%s %s: label_group = %q, want %q", e.EventType, m.LabelName, m.LabelGroup, wantGroup)
		}
	}
	if n := labelListReads(fx.client); n != 1 {
		t.Errorf("label list read %d times, want once for the cycle", n)
	}
	if snap := fx.snapshot(t, "ENG-1"); !slices.Equal(snap.LabelRefs, []domain.LinearLabelRef{labelHard}) {
		t.Errorf("label_refs = %+v, want the new label with its id", snap.LabelRefs)
	}

	fx.client.calls = nil
	if evts, err := fx.cycle(t, linRules()); err != nil || len(evts) != 0 {
		t.Fatalf("unchanged cycle: events=%v err=%v, want nothing", eventTypes(evts), err)
	}
	if n := labelListReads(fx.client); n != 0 {
		t.Errorf("label list read %d times in a cycle with no label change, want none", n)
	}
}

// TestRefreshLinear_LabelRenamed: a label renamed in Linear emits nothing, and
// the snapshot takes the new name.
func TestRefreshLinear_LabelRenamed(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(1)
	is.Labels = linIssueLabels(labelBug)
	fx.seed(t, is)

	is.Labels = linIssueLabels(linLabel(labelBug.ID, "Defect"))
	fx.client.put(is)
	evts, err := fx.cycle(t, linRules())
	if err != nil || len(evts) != 0 {
		t.Fatalf("rename cycle: events=%v err=%v, want nothing", eventTypes(evts), err)
	}
	snap := fx.snapshot(t, "ENG-1")
	if !slices.Equal(snap.Labels, []string{"Defect"}) || !slices.Equal(snap.LabelRefs, []domain.LinearLabelRef{linLabel(labelBug.ID, "Defect")}) {
		t.Errorf("snapshot labels = %v / %+v, want the new name", snap.Labels, snap.LabelRefs)
	}
	if n := labelListReads(fx.client); n != 0 {
		t.Errorf("label list read %d times for a rename, want none", n)
	}
}

// TestRefreshLinear_PreIDSnapshot: a snapshot stored before label ids were
// recorded emits nothing on its first refresh when no label name changed, and
// is stored with the ids from then on.
func TestRefreshLinear_PreIDSnapshot(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(1)
	is.Labels = linIssueLabels(labelBug, labelAgent)
	fx.seed(t, is)

	// Rewrite the stored snapshot as it was stored before label_refs.
	e := fx.entity(t, "ENG-1")
	var raw map[string]any
	if err := json.Unmarshal([]byte(e.SnapshotJSON), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "label_refs")
	old, _ := json.Marshal(raw)
	if ok, err := fx.stores.Entities.UpdateSnapshotCASSystem(context.Background(), runmode.LocalDefaultOrgID, e.ID, string(old), e.PollSeq); err != nil || !ok {
		t.Fatalf("store the pre-id snapshot: ok=%v err=%v", ok, err)
	}

	evts, err := fx.cycle(t, linRules())
	if err != nil || len(evts) != 0 {
		t.Fatalf("first refresh: events=%v err=%v, want nothing", eventTypes(evts), err)
	}
	if snap := fx.snapshot(t, "ENG-1"); len(snap.LabelRefs) != 2 {
		t.Errorf("label_refs = %+v, want both labels with their ids", snap.LabelRefs)
	}
}

// TestRefreshLinear_LabelListReadFails: when the team's label list cannot be
// read, the label change is not reported without its group. The snapshot keeps
// the old labels, and the next cycle reports the change.
func TestRefreshLinear_LabelListReadFails(t *testing.T) {
	fx := newLinearFixture(t)
	fx.client.labels[linTeamID] = teamLabels()
	is, other := linIssue(1), linIssue(2)
	fx.seed(t, is, other)

	is.Labels = linIssueLabels(labelHard)
	is.Priority, is.PriorityLabel = 1, "Urgent"
	other.Labels = linIssueLabels(labelAgent)
	fx.client.put(is)
	fx.client.put(other)
	fx.client.fail = func(call string, _ int) error {
		if strings.HasPrefix(call, "labels:") {
			return errors.New("linear: 502")
		}
		return nil
	}
	spans := recordSpans(t)
	evts, err := fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := spanOutcomes(spans(), "tracker.linear.label_groups"); !slices.Equal(got, []string{"failed"}) {
		t.Errorf("label list read span outcomes = %v, want one failed read", got)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssuePriorityChanged}) {
		t.Fatalf("events = %v, want priority_changed alone", got)
	}
	if snap := fx.snapshot(t, "ENG-1"); len(snap.Labels) != 0 || len(snap.LabelRefs) != 0 || snap.PriorityLabel != "Urgent" {
		t.Errorf("snapshot = %+v, want the old labels kept and the rest moved on", snap)
	}
	if snap := fx.snapshot(t, "ENG-2"); len(snap.Labels) != 0 {
		t.Errorf("ENG-2 labels = %v, want them kept: the team's failed read is reused, not retried", snap.Labels)
	}
	if n := labelListReads(fx.client); n != 1 {
		t.Errorf("label list read %d times, want one failed read for the team", n)
	}
	if !fx.pollCompleted() {
		t.Error("a failed label list read ended the cycle")
	}

	fx.client.fail = nil
	evts, err = fx.cycle(t, linRules())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	cmp := func(a, b labelEvent) int { return strings.Compare(a.dedupKey, b.dedupKey) }
	got := labelEvents(evts)
	slices.SortFunc(got, cmp)
	want := []labelEvent{{domain.EventLinearIssueLabelAdded, labelAgent.ID}, {domain.EventLinearIssueLabelAdded, labelHard.ID}}
	if !slices.Equal(got, want) {
		t.Fatalf("label events = %v, want both deferred label_added events", got)
	}
	for _, e := range evts {
		if m := decodeMetadata[events.LinearIssueLabelMetadata](t, e); m.LabelGroup != difficulty[e.DedupKey] {
			t.Errorf("deferred %s group = %q, want %q", m.LabelName, m.LabelGroup, difficulty[e.DedupKey])
		}
	}
}

// TestRefreshLinear_LabelListRateLimited: a rate limit on the label list ends
// the cycle, as one anywhere else does, and the issue is left as it was.
func TestRefreshLinear_LabelListRateLimited(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(1)
	fx.seed(t, is)

	is.Labels = linIssueLabels(labelAgent)
	fx.client.put(is)
	fx.client.fail = func(call string, _ int) error {
		if strings.HasPrefix(call, "labels:") {
			return rateLimited(time.Now().Add(time.Minute))
		}
		return nil
	}
	spans := recordSpans(t)
	evts, err := fx.cycle(t, linRules())
	if !errors.Is(err, linear.ErrRateLimited) {
		t.Fatalf("err = %v, want the rate limit", err)
	}
	if got := spanOutcomes(spans(), "tracker.linear.label_groups"); !slices.Equal(got, []string{"rate_limited"}) {
		t.Errorf("label list read span outcomes = %v, want one rate-limited read", got)
	}
	if len(evts) != 0 || fx.pollCompleted() {
		t.Errorf("events = %v, poll completed = %v; want nothing", eventTypes(evts), fx.pollCompleted())
	}
	if snap := fx.snapshot(t, "ENG-1"); len(snap.Labels) != 0 {
		t.Errorf("labels = %v, want the issue left as it was", snap.Labels)
	}
}
