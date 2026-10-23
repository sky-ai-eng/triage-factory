package tracker

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// The team visibility tests: Linear answers not-found for an issue the
// credential cannot read as it does for a deleted one, so an armed team the
// credential cannot read holds its missing issues instead of retiring them.

// seedEngAndOps seeds ENG-1 in team ENG and OPS-2 in team OPS, both armed.
func (fx *linearFixture) seedEngAndOps(t *testing.T) (eng, ops linear.Issue) {
	t.Helper()
	eng, ops = linIssue(1), movedToOps(linIssue(2), 2)
	fx.client.search["pickup:"+linTeamID] = []linear.Issue{eng}
	fx.client.search["pickup:"+opsTeamID] = []linear.Issue{ops}
	fx.client.put(eng)
	fx.client.put(ops)
	if evts, err := fx.cycle(t, linRulesWithOps()); err != nil || len(evts) != 0 {
		t.Fatalf("seed cycle: events=%v err=%v, want a quiet seed", eventTypes(evts), err)
	}
	fx.client.search = map[string][]linear.Issue{}
	fx.client.calls = nil
	return eng, ops
}

// TestRefreshLinear_UnreadableTeamIsHeld: issues missing from a team the
// credential cannot read are neither asked about nor retired, while a team it
// does read is confirmed as before; once the credential reads the team again,
// its issues refresh.
func TestRefreshLinear_UnreadableTeamIsHeld(t *testing.T) {
	fx := newLinearFixture(t)
	eng, ops := fx.seedEngAndOps(t)

	fx.client.hiddenTeams = map[string]bool{linTeamID: true}
	fx.client.notFound = map[string]bool{eng.ID: true, ops.ID: true}
	evts, err := fx.cycle(t, linRulesWithOps())
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueUnreachable}) {
		t.Fatalf("events = %v, want only OPS-2's unreachable", got)
	}
	var meta events.LinearIssueUnreachableMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.IssueIdentifier != "OPS-2" || meta.Reason != events.LinearUnreachableNotFound {
		t.Errorf("unreachable = %+v, want OPS-2 not_found", meta)
	}
	if got, want := getIssueCalls(fx.client), []string{"getIssue:" + ops.ID}; !slices.Equal(got, want) {
		t.Errorf("confirmations = %v, want %v: ENG's issue held, not asked about", got, want)
	}
	if !slices.Contains(fx.client.callLog(), "teams:"+linTeamID+","+opsTeamID) {
		t.Errorf("calls = %v, want one read of both candidates' teams", fx.client.callLog())
	}
	if !fx.pollCompleted() {
		t.Error("a cycle that held a team did not report its poll")
	}

	// No router runs here, so OPS-2 is still active; it answers again too,
	// leaving ENG-1's change the only event.
	fx.client.hiddenTeams, fx.client.notFound = nil, nil
	eng.Priority, eng.PriorityLabel = 1, "Urgent"
	fx.client.put(eng)
	evts, err = fx.cycle(t, linRulesWithOps())
	if err != nil {
		t.Fatalf("RefreshLinear after access returned: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssuePriorityChanged}) {
		t.Errorf("events = %v, want ENG-1's change diffed from its held snapshot", got)
	}
}

// TestRefreshLinear_UnarmedTeamIsNotHeld: an issue whose team no rule arms is
// asked about whatever the credential reads, so untracking a team that will
// not come back lets its issues retire.
func TestRefreshLinear_UnarmedTeamIsNotHeld(t *testing.T) {
	fx := newLinearFixture(t)
	is := linIssue(3)
	fx.seed(t, is)

	fx.client.hiddenTeams = map[string]bool{linTeamID: true}
	fx.client.notFound = map[string]bool{is.ID: true}
	opsOnly := linRulesWithOps()[1:]
	evts, err := fx.cycle(t, opsOnly)
	if err != nil {
		t.Fatalf("RefreshLinear: %v", err)
	}
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventLinearIssueUnreachable}) {
		t.Fatalf("events = %v, want the unarmed team's issue retired", got)
	}
	for _, c := range fx.client.callLog() {
		if strings.HasPrefix(c, "teams:") {
			t.Errorf("calls = %v, want no team read with no armed team among the candidates", fx.client.callLog())
		}
	}
}

// TestRefreshLinear_TeamReadFailureHolds: a team read that fails is no
// evidence the credential reads the team, so its missing issues are held this
// cycle and the cycle still completes. A rate limit on it ends the cycle.
func TestRefreshLinear_TeamReadFailureHolds(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantErr  error
		wantPoll bool
	}{
		{"failed", &linear.StatusError{Status: 502, Class: upstream.Transient}, nil, true},
		{"rate limited", rateLimited(time.Time{}), linear.ErrRateLimited, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLinearFixture(t)
			is := linIssue(4)
			fx.seed(t, is)
			fx.client.notFound = map[string]bool{is.ID: true}
			fx.client.fail = func(call string, _ int) error {
				if strings.HasPrefix(call, "teams:") {
					return tc.err
				}
				return nil
			}
			evts, err := fx.cycle(t, linRules())
			if tc.wantErr == nil && err != nil {
				t.Fatalf("RefreshLinear: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if len(evts) != 0 {
				t.Errorf("events = %v, want none", eventTypes(evts))
			}
			if got := getIssueCalls(fx.client); len(got) != 0 {
				t.Errorf("confirmations = %v, want none", got)
			}
			if fx.pollCompleted() != tc.wantPoll {
				t.Errorf("poll completed = %v, want %v", fx.pollCompleted(), tc.wantPoll)
			}
		})
	}
}
