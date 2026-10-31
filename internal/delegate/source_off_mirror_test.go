package delegate

import (
	"context"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// orgSecrets answers the org-credential reads the resolvers make from a map.
type orgSecrets struct {
	db.SecretStore
	vals map[string]string
}

func (s orgSecrets) GetSystem(_ context.Context, _, key string) (string, error) {
	return s.vals[key], nil
}

// endpointLinearResolver is the production Linear resolver with the client it
// resolves pointed at a fake rather than at Linear.
type endpointLinearResolver struct {
	linear.Resolver
	endpoint string
}

func (r endpointLinearResolver) ForSystem(ctx context.Context, orgID string) (*linear.Client, error) {
	cred, err := r.ResolveSystemCredential(ctx, orgID)
	if err != nil {
		return nil, err
	}
	cfg := cred.Config()
	cfg.Endpoint = r.endpoint
	return linear.NewClient(cfg).WithOrg(orgID).WithCredential(string(cred.Method)), nil
}

func turnSourceForTest(t *testing.T, store db.OrgEventSourceStore, kind string, off bool) {
	t.Helper()
	if _, err := store.SetDisabled(t.Context(), runmode.LocalDefaultOrgID, kind, off, ""); err != nil {
		t.Fatalf("turn %s disabled=%v: %v", kind, off, err)
	}
}

// With Jira turned off, the board's lifecycle mirror resolves no client
// through the production resolver, so Jira hears nothing; turned back on, the
// mirror moves the ticket again with no rebind.
func TestRunJiraMirror_TurnedOffJiraIsLeftAlone(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	fake := newRecordingJiraServer(t, "To Do", "")
	if _, err := stores.Orgs.SetSourceBaseURL(t.Context(), runmode.LocalDefaultOrgID, eventsource.KindJira, fake.srv.URL); err != nil {
		t.Fatalf("set jira site: %v", err)
	}
	s := NewSpawner(database, stores, nil, nil, "")
	s.SetJiraResolver(jira.NewResolver(orgSecrets{vals: map[string]string{integrations.KeyJiraPAT: "bot-tok"}}, stores.Orgs,
		eventsource.ResolverGate(stores.OrgEventSources, eventsource.KindJira)))

	turnSourceForTest(t, stores.OrgEventSources, eventsource.KindJira, true)
	s.runJiraMirror(runmode.LocalDefaultOrgID, "", "SKY-1", "", mirrorRule(), false)
	fake.mu.Lock()
	requests := len(fake.paths)
	fake.mu.Unlock()
	if requests != 0 {
		t.Errorf("Jira received %d requests while turned off, want none", requests)
	}

	turnSourceForTest(t, stores.OrgEventSources, eventsource.KindJira, false)
	s.runJiraMirror(runmode.LocalDefaultOrgID, "", "SKY-1", "", mirrorRule(), false)
	if _, transitions := fake.snapshot(); !slices.Equal(transitions, []string{"In Progress"}) {
		t.Errorf("transitions once Jira is back on = %v, want [In Progress]", transitions)
	}
}

// The Linear sibling: no request while Linear is off, and no failure counted,
// since leaving the issue alone is the switch working; turned back on, the
// mirror moves the issue again.
func TestRunLinearMirror_TurnedOffLinearIsLeftAlone(t *testing.T) {
	read := linearMirrorFailureCounter(t)
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	stores := testSpawnerStores(database)
	fake := newRecordingLinearServer(t, linearTodo, nil)
	s := NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")
	s.SetLinearResolver(endpointLinearResolver{
		Resolver: linear.NewResolver(orgSecrets{vals: map[string]string{integrations.KeyLinearAPIKey: "lin_org"}}, stores.Orgs,
			eventsource.ResolverGate(stores.OrgEventSources, eventsource.KindLinear)),
		endpoint: fake.srv.URL,
	})

	turnSourceForTest(t, stores.OrgEventSources, eventsource.KindLinear, true)
	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())
	fake.mu.Lock()
	requests := len(fake.addressed)
	fake.mu.Unlock()
	if requests != 0 {
		t.Errorf("Linear received %d requests while turned off, want none", requests)
	}
	if got := read(); len(got) != 0 {
		t.Errorf("failures = %v while Linear is off, want none", got)
	}

	turnSourceForTest(t, stores.OrgEventSources, eventsource.KindLinear, false)
	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())
	if got := fake.transitions(); !slices.Equal(got, []string{linearDoing.ID}) {
		t.Errorf("transitions once Linear is back on = %v, want [%s]", got, linearDoing.ID)
	}
}
