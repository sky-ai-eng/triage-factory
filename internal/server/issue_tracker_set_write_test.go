package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// landOnFirstCall makes the first upstream call any request sends run save
// before the fake answers it, and hands back what save answered. The save
// stands in for another admin's, landing while a write waits on the gate; it
// goes through the fake as well, and the flag rather than a sync.Once keeps
// its own call from re-entering.
func landOnFirstCall(onCall func(func()), save func() *httptest.ResponseRecorder) <-chan *httptest.ResponseRecorder {
	landed := make(chan *httptest.ResponseRecorder, 1)
	var fired atomic.Bool
	onCall(func() {
		if fired.CompareAndSwap(false, true) {
			landed <- save()
		}
	})
	return landed
}

// requireLanded is the response of the save landOnFirstCall ran, which must
// have run and succeeded.
func requireLanded(t *testing.T, landed <-chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case rec := <-landed:
		if rec.Code != http.StatusOK {
			t.Fatalf("the save landing mid-gate = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	default:
		t.Fatal("no save landed: the write under test never called upstream")
	}
}

// TestLinearTeamsPut_ConflictsWithASaveMidGate: a save that lands while
// another waits on Linear is not reverted by it. The waiting write named ENG
// by id alone, so it would carry ENG's done rule from the read it made before
// the other landed. It answers 409 and stores nothing.
func TestLinearTeamsPut_ConflictsWithASaveMidGate(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng), map[string]any{"id": linearTeamOps})

	doneOnly := armedLinearTeam(linearTeamEng)
	doneOnly["done"] = map[string]any{"member_ids": []string{linearStateDone}, "canonical_id": linearStateDone}
	landed := landOnFirstCall(fake.OnCall, func() *httptest.ResponseRecorder {
		return putLinearTeams(t, s, doneOnly, map[string]any{"id": linearTeamOps})
	})

	rec := putLinearTeams(t, s, map[string]any{"id": linearTeamEng}, armedLinearTeam(linearTeamOps))
	requireLanded(t, landed)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT built from a read another save replaced = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != httpx.ReasonConflict {
		t.Errorf("errors = %+v, want one CONFLICT", items)
	}

	got := storedLinearTeamsRead(t, s)
	if len(got) != 2 || got[0].ID != linearTeamEng || got[1].ID != linearTeamOps {
		t.Fatalf("stored set = %+v, want ENG then OPS as the landed save left them", got)
	}
	if n := stateNames(got[0].Done.Members); n != "Done/completed" {
		t.Errorf("ENG done = %s, want the landed save's Done alone", n)
	}
	if got[1].Armed {
		t.Errorf("OPS = %+v, want it still watched: the refused write stores nothing", got[1])
	}
}

// TestJiraProjectsPut_ConflictsWithASaveMidGate is the Jira route's case: a
// save that lands while another asks Jira about the project it adds is not
// reverted by the done rule the other carries for the project it names by id.
func TestJiraProjectsPut_ConflictsWithASaveMidGate(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := teamJiraRules(t, s)
	sky := stored[0].JiraProjectID

	landed := landOnFirstCall(fake.OnCall, func() *httptest.ResponseRecorder {
		return putJiraProjectElements(t, s, map[string]any{
			"id":   sky,
			"done": map[string]any{"member_ids": []string{statusDoneID, statusInReviewID}, "canonical_id": statusDoneID},
		})
	})

	rec := putJiraProjectElements(t, s,
		map[string]any{"id": sky},
		map[string]any{"jira_id": fakeJiraProjectID("OPS"), "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
	)
	requireLanded(t, landed)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT built from a read another save replaced = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != httpx.ReasonConflict {
		t.Errorf("errors = %+v, want one CONFLICT", items)
	}

	after, _ := teamJiraRules(t, s)
	if len(after) != 1 || after[0].JiraProjectID != sky {
		t.Fatalf("stored rules = %+v, want SKY alone: the refused write stores nothing", after)
	}
	want := []string{statusDoneID, statusInReviewID}
	if got := statusIDs(after[0].DoneMembers); !slices.Equal(got, want) {
		t.Errorf("SKY done = %v, want the landed save's %v", got, want)
	}
}

func statusIDs(refs []domain.JiraStatusRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}
