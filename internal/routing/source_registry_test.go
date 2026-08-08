package routing

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
// purely to exercise HandleEvent end to end. Ownership is declared here too
// (OwnershipOwned) — a registered source's event types classify their
// ownership model the same way core's do, via events.Register, not a
// per-source hook.
const fakeEventType = "fake:thing:happened"

func init() {
	events.Register(events.EventSchema{
		EventType: fakeEventType,
		Ownership: events.OwnershipOwned,
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
		testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
}

// fakeSourceHooks builds SourceHooks for teamID: ResolveOwner always
// resolves to teamID, TracksScope controlled by tracksScope so tests can
// flip the stage-1 gate. Ownership isn't part of SourceHooks — fakeEventType
// declares OwnershipOwned once, in this file's init(), via events.Register.
func fakeSourceHooks(teamID string, tracksScope bool) SourceHooks {
	return SourceHooks{
		ResolveOwner: func(ctx context.Context, orgID string, evt domain.Event, entityID string) (string, []string, error) {
			return teamID, []string{teamID}, nil
		},
		TracksScope: func(ctx context.Context, evt domain.Event, teamID string) bool { return tracksScope },
	}
}

// fakeSourceOwnerLookupFailure is the error a registered source's ResolveOwner
// returns while its store is down — wrapped the way a real hook wraps its own
// read (ee/slack's channel→primary-team lookup), so a test can assert the
// parked row names the read that failed rather than some other stage.
const fakeSourceOwnerLookupFailure = "fake source: primary team for scope"

// outageSourceHooks is fakeSourceHooks with a store behind it: ResolveOwner
// answers teamID normally, and returns an error — not the empty owner — for
// as long as o is down. A registered source's hook stands in for the WHOLE
// owner ladder, so this is the only place a failed read can be reported from;
// there is no lower tier to catch it. TracksScope keeps the opposite posture
// (fail open) throughout, which is the split under test.
func outageSourceHooks(teamID string, o *outage) SourceHooks {
	return SourceHooks{
		ResolveOwner: func(ctx context.Context, orgID string, evt domain.Event, entityID string) (string, []string, error) {
			if o.down() {
				return "", nil, fmt.Errorf("%s: %w", fakeSourceOwnerLookupFailure, errOutage)
			}
			return teamID, []string{teamID}, nil
		},
		TracksScope: func(ctx context.Context, evt domain.Event, teamID string) bool { return true },
	}
}

// noOwnerSourceHooks resolves the genuine no-owner answer: a scope no team has
// claimed. ("", nil, nil) is a RESOLVED result — the event is consumed, not
// retried, because a retry cannot produce a different answer.
func noOwnerSourceHooks() SourceHooks {
	return SourceHooks{
		ResolveOwner: func(ctx context.Context, orgID string, evt domain.Event, entityID string) (string, []string, error) {
			return "", nil, nil
		},
		TracksScope: func(ctx context.Context, evt domain.Event, teamID string) bool { return true },
	}
}

// fakeSourceQueueRouter is fakeSourceRouter plus the durable queue and a
// delegator, so a test can route through the drain worker — the thing that
// decides done vs. requeued — instead of calling HandleEvent directly.
func fakeSourceQueueRouter(database *sql.DB, spawner Delegator) *Router {
	st := sqlitestore.New(database)
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		st.Agents, st.TeamAgents, nil, testTaskStore(database), st.Conversations, st.Entities,
		st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, nil, spawner,
		noopScorer{}, websocket.NewHub())
	r.SetEventQueue(st.EventQueue)
	return r
}

// TestSourceRegistry_OwnershipModelForEvent pins wire point 1
// (ownershipModelForEvent, lifecycle.go): a registered source's event type
// classifies per its events.Register declaration (OwnershipOwned here), and
// EventSupportsWatch (which derives from the same classifier) reports true
// for it.
func TestSourceRegistry_OwnershipModelForEvent(t *testing.T) {
	t.Cleanup(ResetSources)
	RegisterSource("fake", fakeSourceHooks("team-x", true))

	if got := ownershipModelForEvent(fakeEventType); got != events.OwnershipOwned {
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

	fakeSourceRouter(database).HandleEvent(context.Background(), fakeEvent(entityID))

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

	fakeSourceRouter(database).HandleEvent(context.Background(), fakeEvent(entityID))

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task (TracksScope=false drops the only matched handler), got %d", len(active))
	}
}

// Owner-resolution error posture at the registered-source seam. The built-in
// ladders tell a failed read apart from a resolved no-owner; these pin that a
// registered source's hook does too. The seam needs it MORE than the built-in
// ladders, not less: the hook replaces the whole ladder for its source, so a
// swallowed failure has no lower tier left to be caught by, and an ee source's
// events (a Slack mention, say) have no snapshot-diff behind them to re-emit.

// TestSourceRegistry_ResolveOwnerFailure_RequeuesThenRoutesToTheHooksTeam is
// the headline contract: a hook that cannot answer leaves the event on the
// queue with the reason on it and mints nothing, and the retry routes it to the
// team the healthy hook names.
func TestSourceRegistry_ResolveOwnerFailure_RequeuesThenRoutesToTheHooksTeam(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	seedFakeEventCatalog(t, database)

	teamX := seedTeam(t, database, "team-x")
	t.Cleanup(ResetSources)
	RegisterSource("fake", outageSourceHooks(teamX, &outage{remaining: 1}))

	seedFakeRule(t, database, teamX)
	entityID := fakeEntity(t, database, "fake/thing#resolve-err")
	enqueueRoutedEvent(t, database, entityID, fakeEventType, "", map[string]string{})

	r := fakeSourceQueueRouter(database, &stubDelegator{db: database})
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	status, attempts, lastErr := queueRow(t, database)
	if status != domain.QueuedEventStatusPending {
		t.Errorf("row after the failed hook = %q, want pending — an unresolvable owner must not consume the event", status)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !strings.Contains(lastErr, fakeSourceOwnerLookupFailure) {
		t.Errorf("last_error = %q, want it to name the hook's failed read (%q)", lastErr, fakeSourceOwnerLookupFailure)
	}
	if n := len(activeTasksOfType(t, database, entityID, fakeEventType)); n != 0 {
		t.Fatalf("tasks after the failed hook = %d, want 0 — no task at all beats one on the wrong team", n)
	}

	// The source's store recovers: the retry resolves the owner it always had.
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	if status, _, _ = queueRow(t, database); status != domain.QueuedEventStatusDone {
		t.Errorf("row after recovery = %q, want done", status)
	}
	tasks := activeTasksOfType(t, database, entityID, fakeEventType)
	if len(tasks) != 1 {
		t.Fatalf("tasks after recovery = %d, want exactly 1", len(tasks))
	}
	if owner := teamIDValue(&tasks[0]); owner != teamX {
		t.Errorf("owner = %q, want the hook's resolved team %q", owner, teamX)
	}
}

// TestSourceRegistry_ResolveOwnerFailure_WatcherDoesNotCaptureOwnership is the
// outcome that makes a swallowed failure worse than a lost event. Team W only
// WATCHES the source's scope — an applies_to_unowned trigger, no claim on it.
// A hook that degraded to the empty owner handed the orphan branch to W, and
// the fire consolidated ownership onto W: another team's channel, answered and
// owned by the watcher, on the strength of one transient store blip.
func TestSourceRegistry_ResolveOwnerFailure_WatcherDoesNotCaptureOwnership(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	seedFakeEventCatalog(t, database)

	teamOwner := seedTeam(t, database, "owner-team")
	teamWatcher := seedTeam(t, database, "watch-team")
	seedFakeRule(t, database, teamOwner) // gates creation for the owner
	// The watcher's whole configuration: reach unowned entities, and fire on
	// them.
	enableTeamAutoDelegate(t, database, teamWatcher)
	seedImmediateWatchTrigger(t, database, teamWatcher, fakeEventType, "fake-watch")

	t.Cleanup(ResetSources)
	RegisterSource("fake", outageSourceHooks(teamOwner, &outage{remaining: 1}))

	entityID := fakeEntity(t, database, "fake/thing#capture")
	enqueueRoutedEvent(t, database, entityID, fakeEventType, "", map[string]string{})

	stub := &stubDelegator{db: database}
	r := fakeSourceQueueRouter(database, stub)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if status, _, lastErr := queueRow(t, database); status != domain.QueuedEventStatusPending || lastErr == "" {
		t.Errorf("row after the failed hook = (%s, last_error %q), want (pending, non-empty)", status, lastErr)
	}
	if n := len(activeTasksOfType(t, database, entityID, fakeEventType)); n != 0 {
		t.Fatalf("tasks after the failed hook = %d, want 0 — an unreadable owner is not an absent one", n)
	}
	if stub.calls != 0 {
		t.Fatalf("the watcher fired %d time(s) on an owner the source could not read; that fire is what captures ownership", stub.calls)
	}

	// The store recovers: exactly one task, owned by the team the hook names,
	// and the watcher — which never had a claim on it — has run nothing.
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	if status, _, _ := queueRow(t, database); status != domain.QueuedEventStatusDone {
		t.Errorf("row after recovery = %q, want done", status)
	}
	tasks := activeTasksOfType(t, database, entityID, fakeEventType)
	if len(tasks) != 1 {
		t.Fatalf("tasks after recovery = %d, want exactly 1", len(tasks))
	}
	if owner := teamIDValue(&tasks[0]); owner != teamOwner {
		t.Errorf("owner = %q, want the hook's team %q (the watcher must never end up owning it)", owner, teamOwner)
	}
	if stub.calls != 0 {
		t.Errorf("the watcher fired %d time(s) though an owning team holds the entity; the no-steal invariant is unchanged", stub.calls)
	}
}

// TestSourceRegistry_ResolvedNoOwner_ConsumesTheEvent pins the other half of
// the contract, and the reason the error arm had to be a separate value rather
// than a wider "empty means trouble" reading: a hook that resolves ("", nil,
// nil) has ANSWERED — nothing owns this scope — so the event is recorded
// taskless and consumed. A retry could not produce a different answer.
func TestSourceRegistry_ResolvedNoOwner_ConsumesTheEvent(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	seedFakeEventCatalog(t, database)

	teamX := seedTeam(t, database, "team-x")
	t.Cleanup(ResetSources)
	RegisterSource("fake", noOwnerSourceHooks())

	seedFakeRule(t, database, teamX) // a plain rule, no applies_to_unowned reach
	entityID := fakeEntity(t, database, "fake/thing#no-owner")
	enqueueRoutedEvent(t, database, entityID, fakeEventType, "", map[string]string{})

	r := fakeSourceQueueRouter(database, &stubDelegator{db: database})
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	status, _, lastErr := queueRow(t, database)
	if status != domain.QueuedEventStatusDone {
		t.Errorf("row = %q (last_error %q), want done — a resolved no-owner consumes the event", status, lastErr)
	}
	if n := len(activeTasksOfType(t, database, entityID, fakeEventType)); n != 0 {
		t.Errorf("tasks = %d, want 0 — nothing owns this scope and nothing watches it", n)
	}
}

// TestRouterBound pins the durability predicate internal/ingest gates its
// outbox enqueue on: the built-in github:/jira: sources are always
// router-bound, system events never are, and a registered source's prefix
// joins the set (and leaves it again on ResetSources).
func TestRouterBound(t *testing.T) {
	t.Cleanup(ResetSources)
	if !RouterBound("github:pr:opened") {
		t.Error("github: must be router-bound (built-in)")
	}
	if !RouterBound("jira:issue:assigned") {
		t.Error("jira: must be router-bound (built-in)")
	}
	if RouterBound(domain.EventSystemPollCompleted) {
		t.Error("system: must never be router-bound (bus-only)")
	}
	if RouterBound(fakeEventType) {
		t.Error("an unregistered source must not be router-bound")
	}
	RegisterSource("fake", fakeSourceHooks("team-x", true))
	if !RouterBound(fakeEventType) {
		t.Error("a registered source's event type must be router-bound")
	}
	ResetSources()
	if RouterBound(fakeEventType) {
		t.Error("ResetSources must drop registered prefixes from the routed set")
	}
	if !RouterBound("github:pr:opened") {
		t.Error("ResetSources must preserve the built-in routed prefixes")
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

// TestRegisterSource_PanicsOnNilResolveOwner pins that ResolveOwner is
// required unconditionally, not just "for a source with an Owned event type":
// registration can't see what event types this source will add in the
// future, and resolveOwnedRouting calls the hook whenever a type's declared
// model is Owned — a nil ResolveOwner would otherwise be a drain-goroutine
// panic on the first Owned event instead of a boot failure.
func TestRegisterSource_PanicsOnNilResolveOwner(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for a nil ResolveOwner hook")
		}
	}()
	RegisterSource("fake-nil-resolve", SourceHooks{
		TracksScope: func(context.Context, domain.Event, string) bool { return true },
	})
}

// TestRegisterSource_PanicsOnBuiltinSource pins the shadow guard: hooks are
// consulted before the native github/jira paths at every wire point, so
// registering a built-in prefix would silently replace heavily-tested
// resolver behavior — refused at boot instead.
func TestRegisterSource_PanicsOnBuiltinSource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic when registering a built-in source prefix")
		}
	}()
	RegisterSource("github", fakeSourceHooks("team-x", true))
}

// TestRegisterSource_PanicsOnDuplicate pins that a second registration for
// the same prefix fails at boot instead of silently swapping hooks — a
// double-install (build-tag mistake, two ee/ sources colliding on a prefix)
// must be as loud as every other wiring bug this function guards. Mirrors
// events.Register's duplicate panic: registrations are compile-time config.
func TestRegisterSource_PanicsOnDuplicate(t *testing.T) {
	t.Cleanup(ResetSources)
	RegisterSource("fake-dup", fakeSourceHooks("team-x", true))
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	RegisterSource("fake-dup", fakeSourceHooks("team-x", true))
}
