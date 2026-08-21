package jira

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// transitionServer serves a fixed transition list and records which transition
// id was posted, so a test can tell WHICH transition the client picked rather
// than only that it picked one.
func transitionServer(t *testing.T, transitions string) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		took []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"transitions":[`+transitions+`]}`)
			return
		}
		var body struct {
			Transition struct {
				ID string `json:"id"`
			} `json:"transition"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		took = append(took, body.Transition.ID)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return took
	}
}

// The workflow below has two transitions whose target NAMES have both been
// changed in Jira since a rule captured them. Only the ids still line up.
const renamedWorkflow = `{"id":"31","name":"Start","to":{"id":"10001","name":"Doing"}},` +
	`{"id":"41","name":"Finish","to":{"id":"10002","name":"Shipped"}}`

// TestTransitionTo_MatchesByID is the rename that used to break a transition:
// the rule captured "In Progress", Jira now calls that status "Doing", and the
// id is what still resolves it.
func TestTransitionTo_MatchesByID(t *testing.T) {
	srv, took := transitionServer(t, renamedWorkflow)
	err := dcClient(srv.URL).TransitionTo(t.Context(), "SKY-1",
		Status{ID: "10001", Name: "In Progress"})
	if err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	if got := took(); len(got) != 1 || got[0] != "31" {
		t.Errorf("posted transitions = %v, want [31] — the one whose target id matches", got)
	}
}

// TestTransitionTo_IDMismatchIsNotSalvagedByTheName: an id that names no
// transition fails, rather than falling back to a name match that could land
// the ticket somewhere else entirely.
func TestTransitionTo_IDMismatchIsNotSalvagedByTheName(t *testing.T) {
	srv, took := transitionServer(t, renamedWorkflow)
	err := dcClient(srv.URL).TransitionTo(t.Context(), "SKY-1",
		Status{ID: "99999", Name: "Doing"})
	if err == nil {
		t.Fatal("TransitionTo with an unknown id succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "no transition to") {
		t.Errorf("error = %v, want it to say no transition was found", err)
	}
	if got := took(); len(got) != 0 {
		t.Errorf("posted transitions = %v, want none", got)
	}
}

// TestTransitionTo_NameFallbackWhenNoID covers both a status a caller named
// directly (the agent's transition verb) and a rule armed before statuses were
// identified.
func TestTransitionTo_NameFallbackWhenNoID(t *testing.T) {
	srv, took := transitionServer(t, renamedWorkflow)
	// Case-insensitive, as the name match always was.
	err := dcClient(srv.URL).TransitionTo(t.Context(), "SKY-1", Status{Name: "shipped"})
	if err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	if got := took(); len(got) != 1 || got[0] != "41" {
		t.Errorf("posted transitions = %v, want [41]", got)
	}
}
