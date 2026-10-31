package routing

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// testRouterDeps is a RouterDeps over database's SQLite stores with every field
// set but the three team↔scope tracking gates (TeamRepos, JiraRules,
// LinearRules). A wired gate drops the handlers of every team that tracks
// nothing, which is every team in a test that seeds no tracking, so a test of
// the gates sets them alongside the tracking it seeds. A test overrides the
// fields it varies and builds the router with newTestRouter.
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
		GitHubGroups:    st.TeamGitHubGroups,
		ReDerive:        st.TaskReDerive,
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
	"TeamRepos":    true,
	"JiraRules":    true,
	"LinearRules":  true,
	"GitHubGroups": true,
	"BootEpoch":    true,
}

// TestNewRouter_RefusesEachMissingRequiredField zeroes one RouterDeps field at
// a time: a required one is refused with an error naming it, a nil-safe one
// builds. Walking the struct means a field added later is tested as required
// unless it is listed nil-safe above.
func TestNewRouter_RefusesEachMissingRequiredField(t *testing.T) {
	database := newTestDB(t)
	st := sqlitestore.New(database)
	full := testRouterDeps(database)
	full.TeamRepos, full.JiraRules, full.LinearRules = st.TeamGitHubRepos, st.JiraStatusRules, st.LinearTeamRules
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

// namesField reports whether err's message lists name as a word of its own,
// so Events is not found inside EventQueue.
func namesField(err error, name string) bool {
	words := strings.FieldsFunc(err.Error(), func(r rune) bool { return r == ' ' || r == ',' })
	for _, w := range words {
		if w == name {
			return true
		}
	}
	return false
}
