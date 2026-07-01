package routing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// fakeEventType is the out-of-core event type these tests route through the
// source registry — standing in for an ee/ package (e.g. Slack) registering
// "slack:" the same way. Registered here (not in a shipped events package
// file) because the registry seam is orthogonal to the predicate-schema
// seam: matchPredicate still requires a registered schema to evaluate a
// rule's predicate, so this test package registers its own match-all schema
// purely to exercise HandleEvent end to end.
const fakeEventType = "fake:thing:happened"

func init() {
	events.Register(events.EventSchema{
		EventType: fakeEventType,
		Match: func(predJSON, metaJSON string) (bool, error) {
			return true, nil // match-all; these tests don't exercise predicate filtering
		},
	})
}

// seedFakeEventCatalog inserts the events_catalog row fakeEventType needs to
// satisfy the event_handlers.event_type / events.event_type FKs — production
// registers this via the gated catalog-upsert (a sibling ticket); tests seed
// it directly, same as other FK-target fixtures in this package.
func seedFakeEventCatalog(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT OR IGNORE INTO events_catalog (id, source, category, label, description)
		VALUES (?, 'fake', 'thing', 'Fake Thing Happened', 'fake-source test fixture')
	`, fakeEventType); err != nil {
		t.Fatalf("seed events_catalog for %s: %v", fakeEventType, err)
	}
}

// seedFakeRule seeds a match-all fakeEventType rule owned by teamID — gates
// task creation the same way seedSystemRule does for the built-in sources.
func seedFakeRule(t *testing.T, database *sql.DB, teamID string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', ?, 0.6, 100, ?, ?)
	`, "fake-rule-"+teamID[:8], runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		fakeEventType, "fake rule "+teamID[:8], time.Now(), time.Now()); err != nil {
		t.Fatalf("seed fake rule for team %s: %v", teamID, err)
	}
}

func fakeEntity(t *testing.T, database *sql.DB, sourceID string) string {
	t.Helper()
	e, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "fake", sourceID, "thing", "Fake Thing", "https://example.com/"+sourceID)
	if err != nil {
		t.Fatalf("create fake entity: %v", err)
	}
	return e.ID
}

func fakeEvent(entityID string) domain.Event {
	return domain.Event{
		EventType:    fakeEventType,
		EntityID:     &entityID,
		MetadataJSON: `{}`,
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	}
}

// fakeSourceRouter wires a router with just the stores the fake-source
// routing path needs: entity lifecycle, task dedup/visibility, event
// handlers, and the audit log. Identity-resolution stores (Users,
// TeamGitHubGroups, ...) are nil — the fake source's ResolveOwner hook
// replaces the built-in author/assignee resolution entirely.
func fakeSourceRouter(database *sql.DB) *Router {
	st := sqlitestore.New(database)
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), st.AgentRuns, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
}

// fakeSourceHooks builds SourceHooks for teamID: Ownership always Owned,
// ResolveOwner always resolves to teamID, TracksScope controlled by
// tracksScope so tests can flip the stage-1 gate.
func fakeSourceHooks(teamID string, tracksScope bool) SourceHooks {
	return SourceHooks{
		Ownership: func(eventType string) OwnershipModel { return OwnershipOwned },
		ResolveOwner: func(ctx context.Context, orgID string, evt domain.Event, entityID string) (string, []string) {
			return teamID, []string{teamID}
		},
		TracksScope: func(ctx context.Context, evt domain.Event, teamID string) bool { return tracksScope },
	}
}

// TestSourceRegistry_OwnershipModelForEvent pins wire point 1
// (ownershipModelForEvent, lifecycle.go): a registered Owned source classifies
// its event type as Owned, and EventSupportsWatch (which derives from the
// same classifier) reports true for it.
func TestSourceRegistry_OwnershipModelForEvent(t *testing.T) {
	t.Cleanup(ResetSources)
	RegisterSource("fake", fakeSourceHooks("team-x", true))

	if got := ownershipModelForEvent(fakeEventType); got != OwnershipOwned {
		t.Errorf("ownershipModelForEvent(%q) = %v, want OwnershipOwned", fakeEventType, got)
	}
	if !EventSupportsWatch(fakeEventType) {
		t.Errorf("EventSupportsWatch(%q) = false, want true for a registered Owned source", fakeEventType)
	}
}

// TestSourceRegistry_HandleEvent_OwnedRoutingCreatesTask is the full
// HandleEvent integration (wire points 1+2): a matched rule on the fake
// source's event type creates a task owned by the hook's resolved team, with
// the right visibility — mirroring the existing owned-routing acceptance
// tests (author_routing_test.go) but driven entirely by registered hooks
// instead of the built-in github/jira resolvers.
func TestSourceRegistry_HandleEvent_OwnedRoutingCreatesTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	seedFakeEventCatalog(t, database)

	teamX := seedTeam(t, database, "team-x")

	t.Cleanup(ResetSources)
	RegisterSource("fake", fakeSourceHooks(teamX, true))

	seedFakeRule(t, database, teamX)
	entityID := fakeEntity(t, database, "fake/thing#1")

	fakeSourceRouter(database).HandleEvent(fakeEvent(entityID))

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if teamIDValue(&active[0]) != teamX {
		t.Errorf("owner = %q, want the hook's resolved team %q", teamIDValue(&active[0]), teamX)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamX {
		t.Errorf("visibility = %v, want [%s]", vis, teamX)
	}
}

// TestSourceRegistry_HandleEvent_TracksScopeFalse_DropsHandler pins wire
// point 3 (teamTracksEventScope, team_routing.go): when the hook's
// TracksScope reports false, the only matched handler is gated out before it
// ever reaches ownership resolution, so no task is created at all — the same
// shape as TestGate_DisjointRepos_DropsUntrackingTeam for the built-in
// github gate.
func TestSourceRegistry_HandleEvent_TracksScopeFalse_DropsHandler(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	seedFakeEventCatalog(t, database)

	teamX := seedTeam(t, database, "team-x")

	t.Cleanup(ResetSources)
	RegisterSource("fake", fakeSourceHooks(teamX, false)) // TracksScope=false

	seedFakeRule(t, database, teamX)
	entityID := fakeEntity(t, database, "fake/thing#2")

	fakeSourceRouter(database).HandleEvent(fakeEvent(entityID))

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task (TracksScope=false drops the only matched handler), got %d", len(active))
	}
}

// TestSourceRegistry_RequestedPartyDowngradedToPool pins the documented
// behavior for an unsupported Ownership return: registered sources can't use
// OwnershipRequestedParty (the requested-party resolver is GitHub-specific),
// so ownershipModelForEvent downgrades it to OwnershipPool.
func TestSourceRegistry_RequestedPartyDowngradedToPool(t *testing.T) {
	t.Cleanup(ResetSources)
	RegisterSource("fake", SourceHooks{
		Ownership: func(eventType string) OwnershipModel { return OwnershipRequestedParty },
		ResolveOwner: func(ctx context.Context, orgID string, evt domain.Event, entityID string) (string, []string) {
			return "", nil
		},
		TracksScope: func(ctx context.Context, evt domain.Event, teamID string) bool { return true },
	})

	if got := ownershipModelForEvent(fakeEventType); got != OwnershipPool {
		t.Errorf("ownershipModelForEvent(%q) = %v, want OwnershipPool (RequestedParty is unsupported for registered sources)", fakeEventType, got)
	}
}

// TestSourceRegistered pins SourceRegistered — the predicate internal/ingest's
// routerBound consumes (Part 2) — for both a registered and an unregistered
// prefix.
func TestSourceRegistered(t *testing.T) {
	t.Cleanup(ResetSources)
	if SourceRegistered(fakeEventType) {
		t.Error("SourceRegistered should be false before any registration")
	}
	RegisterSource("fake", fakeSourceHooks("team-x", true))
	if !SourceRegistered(fakeEventType) {
		t.Error("SourceRegistered should be true for a registered source's event type")
	}
	if SourceRegistered("github:pr:opened") {
		t.Error("SourceRegistered should be false for a built-in source (never migrated into the registry)")
	}
}

// TestRegisterSource_PanicsOnEmptySource and
// TestRegisterSource_PanicsOnMissingHooks pin the fail-at-boot wiring-bug
// guards: RegisterSource panics before writing to the registry, so neither
// call below needs a ResetSources cleanup.
func TestRegisterSource_PanicsOnEmptySource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for an empty source prefix")
		}
	}()
	RegisterSource("", fakeSourceHooks("team-x", true))
}

func TestRegisterSource_PanicsOnMissingHooks(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for missing required hooks")
		}
	}()
	RegisterSource("fake-missing-hooks", SourceHooks{})
}
