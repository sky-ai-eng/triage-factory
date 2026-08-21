package agenthost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// An org admin turning Jira off has to reach the agent, not just the poll
// cycle: the whole reason the switch exists next to "unbind the credential" is
// that unbinding was the only way to stop a tenant reaching Jira, and it took
// the configuration with it. These tests pin that a turned-off source refuses
// at the credential funnel every Jira verb goes through, in both placements —
// the in-process client and the sidecar's relayed one.
//
// GitHub has no counterpart here and needs none: it cannot be turned off
// (eventsource.Disableable), which the routing and server suites pin.

// fakeSourcePolicy answers the policy read with a fixed set, or a fixed error.
type fakeSourcePolicy struct {
	db.OrgEventSourceStore
	off     []string
	err     error
	reads   atomic.Int32
	lastOrg atomic.Value
}

func (f *fakeSourcePolicy) ListDisabledSystem(_ context.Context, orgID string) ([]string, error) {
	f.reads.Add(1)
	f.lastOrg.Store(orgID)
	if f.err != nil {
		return nil, f.err
	}
	return f.off, nil
}

// withPolicy returns stores with the source policy attached, leaving whatever
// else the caller populated alone.
func withPolicy(stores db.Stores, policy db.OrgEventSourceStore) db.Stores {
	stores.OrgEventSources = policy
	return stores
}

// unreachable stands a backend up that fails the test if anything reaches it.
// A refusal that still made the call would be the failure this whole change is
// about, and it is invisible in the error alone.
func unreachable(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a turned-off source reached %s: %s %s", name, r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestJiraVerb_SourceTurnedOff_RefusesBeforeCallingJira is the headline: an
// agent mid-run has no Jira access the moment an admin turns Jira off. It runs
// through the LOCAL client, where credentials resolve per verb — the placement
// where an already-running agent is affected immediately.
func TestJiraVerb_SourceTurnedOff_RefusesBeforeCallingJira(t *testing.T) {
	jira := unreachable(t, "jira")
	policy := &fakeSourcePolicy{off: []string{eventsource.KindJira}}
	lc := NewLocal(withPolicy(jiraStores(jira.URL, "org-pat"), policy),
		ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-off"})

	_, err := lc.JiraGetIssue(context.Background(), "PROJ-1")
	if err == nil {
		t.Fatal("JiraGetIssue succeeded against a turned-off source, want a refusal")
	}
	// errors.Is, not a string match: an unbound credential and an admin's
	// switch need opposite responses from whoever reads this, and only one of
	// them is theirs to fix.
	if !errors.Is(err, eventsource.ErrDisabled) {
		t.Errorf("error = %v, want one wrapping eventsource.ErrDisabled", err)
	}
	// The message has to name the source and say what a pause covers. A reader
	// told only "Jira is off" reasonably concludes the credential broke.
	if !strings.Contains(err.Error(), "Jira") {
		t.Errorf("error %q does not name the source", err)
	}
	if !strings.Contains(err.Error(), "agent access") {
		t.Errorf("error %q does not say the pause covers agent access", err)
	}
}

// TestJiraVerb_DifferentSourceTurnedOff_StillWorks is the negative control:
// the switch names one source, and a verb reads only its own.
func TestJiraVerb_DifferentSourceTurnedOff_StillWorks(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"PROJ-1","fields":{"summary":"hello"}}`)
	}))
	defer jira.Close()

	// Another source entirely — github could not appear here, because it has
	// no off switch to appear by.
	policy := &fakeSourcePolicy{off: []string{"slack"}}
	lc := NewLocal(withPolicy(jiraStores(jira.URL, "org-pat"), policy),
		ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-on"})

	issue, err := lc.JiraGetIssue(context.Background(), "PROJ-1")
	if err != nil {
		t.Fatalf("another source's switch cost a Jira verb: %v", err)
	}
	if issue.Key != "PROJ-1" {
		t.Errorf("issue key = %q, want PROJ-1", issue.Key)
	}
}

// TestSourceVerb_PolicyReadFails_RefusesRatherThanResolving pins fail-closed.
// Reading an unanswerable policy as permission would hand out the credential
// exactly while the database is unhealthy — and nothing downstream would ever
// report that it happened.
func TestSourceVerb_PolicyReadFails_RefusesRatherThanResolving(t *testing.T) {
	jira := unreachable(t, "jira")
	policy := &fakeSourcePolicy{err: errors.New("boom: org_event_sources read failed")}
	lc := NewLocal(withPolicy(jiraStores(jira.URL, "org-pat"), policy),
		ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-fault"})

	_, err := lc.JiraGetIssue(context.Background(), "PROJ-1")
	if err == nil {
		t.Fatal("JiraGetIssue succeeded on an unreadable policy, want a refusal")
	}
	if errors.Is(err, eventsource.ErrDisabled) {
		t.Errorf("a failed read reported itself as a deliberate pause: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not carry the underlying fault", err)
	}
}

// TestSourceDisabled_ResolvesOverTheRelay pins the multi-mode placement: the
// agenthost daemon runs in the capless sidecar, which holds no database, so the
// answer has to come back over the supervision channel. Without this the gate
// would be local-mode-only — exactly the deployments where it matters least.
func TestSourceDisabled_ResolvesOverTheRelay(t *testing.T) {
	policy := &fakeSourcePolicy{off: []string{eventsource.KindJira}}
	info := ConversationInfo{OrgID: "org-relay", ConversationID: "conv-relay"}
	srv := NewRelayServer(withPolicy(db.Stores{}, policy), info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)

	off, err := rt.SourceDisabled(context.Background(), eventsource.KindJira)
	if err != nil || !off {
		t.Fatalf("SourceDisabled(jira) over the relay = %v, %v; want true, nil", off, err)
	}
	on, err := rt.SourceDisabled(context.Background(), eventsource.KindGitHub)
	if err != nil || on {
		t.Fatalf("SourceDisabled(github) over the relay = %v, %v; want false, nil", on, err)
	}
	// The org is bound orchestrator-side from the run's identity, never taken
	// off the wire — the sidecar parses hostile agent output and must not be
	// able to ask about another tenant's policy.
	if got := policy.lastOrg.Load(); got != info.OrgID {
		t.Errorf("policy read org = %v, want %q", got, info.OrgID)
	}
}
