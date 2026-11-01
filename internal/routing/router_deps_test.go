package routing

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// testRouterDeps is a RouterDeps with every field set, over database's SQLite
// stores. The three tracking stores answer that every team tracks every
// repository, Jira project and Linear team (see tracksEveryRepo), so a test
// that seeds no tracking routes through the gates without being dropped by
// them; a test of the gates sets the SQLite stores and seeds what each team
// tracks. A test overrides the fields it varies and builds the router with
// newTestRouter.
func testRouterDeps(database *sql.DB) RouterDeps {
	st := sqlitestore.New(database)
	return RouterDeps{
		Prompts:         st.Prompts,
		Blueprints:      st.Blueprints,
		Handlers:        st.EventHandlers,
		Agents:          st.Agents,
		TeamAgents:      st.TeamAgents,
		Users:           st.Users,
		Tasks:           st.Tasks,
		Conversations:   st.Conversations,
		Entities:        st.Entities,
		Firings:         st.PendingFirings,
		Events:          st.Events,
		EventQueue:      st.EventQueue,
		ExternalActions: st.ExternalActions,
		Orgs:            st.Orgs,
		Teams:           st.Teams,
		TeamRepos:       tracksEveryRepo{st.TeamGitHubRepos},
		JiraRules:       tracksEveryJiraProject{st.JiraStatusRules},
		LinearRules:     tracksEveryLinearTeam{st.LinearTeamRules},
		GitHubGroups:    st.TeamGitHubGroups,
		ReDerive:        st.TaskReDerive,
		Mergeability:    st.PRMergeability,
		EventSources:    st.OrgEventSources,
		Spawner:         refusingDelegator{},
		Scorer:          noopScorer{},
		WS:              websocket.NewHub(),
		Publisher:       discardPublisher{},
		ExecutorID:      testExecutorID,
		BootEpoch:       1,
	}
}

// newTestRouter builds a router from deps. It panics on a refused RouterDeps,
// which in a test is a fixture bug, so the fixture helpers that take no
// *testing.T can call it.
func newTestRouter(deps RouterDeps) *Router {
	r, err := NewRouter(deps)
	if err != nil {
		panic(err)
	}
	return r
}

// tracksEveryRepo, tracksEveryJiraProject and tracksEveryLinearTeam are the
// tracking stores of a test that seeds no tracking: the gate's lookup answers
// that the team tracks what the event names, and every other method is the
// SQLite store's. The gate still decides everything before the lookup — an
// event naming no entity, repository or project is undecided, and a Linear
// event naming no Linear team is refused.
type tracksEveryRepo struct{ dbpkg.TeamGitHubReposStore }

func (tracksEveryRepo) TracksRepositorySystem(context.Context, string, string) (bool, error) {
	return true, nil
}

type tracksEveryJiraProject struct{ dbpkg.JiraStatusRulesStore }

func (tracksEveryJiraProject) TracksJiraProjectSystem(context.Context, string, string, string) (bool, error) {
	return true, nil
}

type tracksEveryLinearTeam struct{ dbpkg.LinearTeamRulesStore }

func (tracksEveryLinearTeam) TracksTeamSystem(context.Context, string, string, string) (bool, error) {
	return true, nil
}

// refusingDelegator is the Spawner of a test that starts no run: Delegate
// refuses, the stops succeed with nothing to stop, and an additive event is
// never delivered.
type refusingDelegator struct{}

var errNoTestDelegator = errors.New("test router: no delegator for this test")

func (refusingDelegator) Delegate(domain.Task, delegate.DelegateOpts) (string, error) {
	return "", errNoTestDelegator
}

func (refusingDelegator) StopConversationAndCancelBlueprint(string, string, string, delegate.StopCause) error {
	return nil
}

func (refusingDelegator) StopBlueprintRun(string, string, delegate.StopCause) error { return nil }

func (refusingDelegator) TeardownTaskDeliverablesSystem(context.Context, string, string) {}
func (refusingDelegator) TaskHasUnresolvedDeliverablesSystem(context.Context, string, string) bool {
	return false
}

func (refusingDelegator) StageOrDeliverAdditiveEvent(context.Context, string, string, string, string, domain.NoteProvenance, delegate.AdditiveFiringRef) delegate.InjectOutcome {
	return delegate.InjectNotDelivered
}

// discardPublisher is the Publisher of a test that does not read the
// disposition sentinel.
type discardPublisher struct{}

func (discardPublisher) Publish(domain.Event) {}

// nilSafeRouterDeps are the RouterDeps fields NewRouter accepts unset. Every
// other field is required.
var nilSafeRouterDeps = map[string]bool{
	"Agents":       true,
	"TeamAgents":   true,
	"Users":        true,
	"GitHubGroups": true,
	"BootEpoch":    true,
}

// TestNewRouter_RefusesEachMissingRequiredField zeroes one RouterDeps field at
// a time: a required one is refused with an error naming it, a nil-safe one
// builds. Walking the struct means a field added later is tested as required
// unless it is listed nil-safe above.
func TestNewRouter_RefusesEachMissingRequiredField(t *testing.T) {
	full := testRouterDeps(newTestDB(t))
	typ := reflect.TypeOf(full)
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		t.Run(name, func(t *testing.T) {
			deps := full
			v := reflect.ValueOf(&deps).Elem().Field(i)
			v.Set(reflect.Zero(v.Type()))

			r, err := NewRouter(deps)
			if nilSafeRouterDeps[name] {
				if err != nil || r == nil {
					t.Fatalf("NewRouter without nil-safe %s = (%v, %v); want a router", name, r, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewRouter without required %s = nil error; want a refusal", name)
			}
			if r != nil {
				t.Errorf("NewRouter without required %s returned a router beside its error", name)
			}
			if !namesField(err, name) {
				t.Errorf("error %q does not name the missing field %s", err, name)
			}
		})
	}
}

// TestNewRouter_NamesEveryMissingField pins that one refusal lists every
// missing required field rather than the first.
func TestNewRouter_NamesEveryMissingField(t *testing.T) {
	_, err := NewRouter(RouterDeps{})
	if err == nil {
		t.Fatal("NewRouter(RouterDeps{}) = nil error; want a refusal")
	}
	typ := reflect.TypeOf(RouterDeps{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if nilSafeRouterDeps[name] {
			continue
		}
		if !namesField(err, name) {
			t.Errorf("error %q does not name the missing field %s", err, name)
		}
	}
}

// TestNewRouter_RefusesATypedNil pins that a required field holding a nil
// pointer is refused like an unset one: production wires concrete pointers, and
// a nil one compares unequal to nil once it is in the interface.
func TestNewRouter_RefusesATypedNil(t *testing.T) {
	deps := testRouterDeps(newTestDB(t))
	deps.Spawner = (*stubDelegator)(nil)
	deps.Publisher = (*fakeDispositionPublisher)(nil)
	_, err := NewRouter(deps)
	if err == nil {
		t.Fatal("NewRouter with a typed-nil Spawner and Publisher = nil error; want a refusal")
	}
	for _, name := range []string{"Spawner", "Publisher"} {
		if !namesField(err, name) {
			t.Errorf("error %q does not name the typed-nil field %s", err, name)
		}
	}
}

// namesField reports whether err's message lists name as a word of its own,
// so Events is not found inside EventQueue.
func namesField(err error, name string) bool {
	return slices.Contains(strings.FieldsFunc(err.Error(), func(r rune) bool { return r == ' ' || r == ',' }), name)
}
