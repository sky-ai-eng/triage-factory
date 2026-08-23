package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Owner-resolution error posture. Every read the owning-team ladder makes to
// DECIDE an owner used to degrade on failure — a failed tier fell through to
// the tier below, a failed identity read looked exactly like an external
// author — and the three outcomes (no task; a task NULL-owned on a watcher's
// board; a watcher firing and capturing ownership for good) were all silent
// and all permanent, because the tracker's snapshot-diff never re-emits.
//
// These tests pin the replacement contract per read: inject the failure, assert
// the queue row is requeued rather than consumed, then let the store recover
// and assert the event routes to the team it always should have.
//
// The tracking gates (team↔repo / team↔project) are deliberately NOT here:
// they only narrow a set someone else computed, so they keep failing open — see
// teamTracksEventRepo.

// The store wrappers below each fail exactly one owner-resolution read, so a
// case names the read it is failing rather than leaving it to be inferred from
// a generic error. Each embeds its interface, so every other method passes
// straight through to the real store.

type outageOwningTeamStore struct {
	dbpkg.EntityStore
	o *outage
}

func (s outageOwningTeamStore) OwningTeamForEntitySystem(ctx context.Context, orgID, entityID string) (string, error) {
	if s.o.down() {
		return "", errOutage
	}
	return s.EntityStore.OwningTeamForEntitySystem(ctx, orgID, entityID)
}

type outagePriorTaskOwnerStore struct {
	dbpkg.TaskStore
	o *outage
}

func (s outagePriorTaskOwnerStore) OwnerTeamForLatestTaskInTypesSystem(ctx context.Context, orgID, entityID string, eventTypes []string) (string, error) {
	if s.o.down() {
		return "", errOutage
	}
	return s.TaskStore.OwnerTeamForLatestTaskInTypesSystem(ctx, orgID, entityID, eventTypes)
}

type outageActiveTasksStore struct {
	dbpkg.TaskStore
	o *outage
}

func (s outageActiveTasksStore) FindActiveByEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.Task, error) {
	if s.o.down() {
		return nil, errOutage
	}
	return s.TaskStore.FindActiveByEntitySystem(ctx, orgID, entityID)
}

type outageOrgSettingsStore struct {
	dbpkg.OrgsStore
	o *outage
}

func (s outageOrgSettingsStore) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	if s.o.down() {
		return domain.OrgSettings{}, errOutage
	}
	return s.OrgsStore.GetSettingsSystem(ctx, orgID)
}

type outageIdentityStore struct {
	dbpkg.UsersStore
	o *outage
}

func (s outageIdentityStore) UserIDsForGitHubLoginSystem(ctx context.Context, githubBaseURL, login string) ([]string, error) {
	if s.o.down() {
		return nil, errOutage
	}
	return s.UsersStore.UserIDsForGitHubLoginSystem(ctx, githubBaseURL, login)
}

func (s outageIdentityStore) UserIDsForJiraAccountSystem(ctx context.Context, jiraBaseURL, accountID string) ([]string, error) {
	if s.o.down() {
		return nil, errOutage
	}
	return s.UsersStore.UserIDsForJiraAccountSystem(ctx, jiraBaseURL, accountID)
}

type outageTeamsForUserStore struct {
	dbpkg.TeamsStore
	o *outage
}

func (s outageTeamsForUserStore) TeamIDsForUserInOrgSystem(ctx context.Context, orgID, userID string) ([]string, error) {
	if s.o.down() {
		return nil, errOutage
	}
	return s.TeamsStore.TeamIDsForUserInOrgSystem(ctx, orgID, userID)
}

type outageGitHubGroupsStore struct {
	dbpkg.TeamGitHubGroupsStore
	o *outage
}

func (s outageGitHubGroupsStore) TeamsForGroupSystem(ctx context.Context, orgID, orgLogin, teamSlug string) ([]string, error) {
	if s.o.down() {
		return nil, errOutage
	}
	return s.TeamGitHubGroupsStore.TeamsForGroupSystem(ctx, orgID, orgLogin, teamSlug)
}

// identityQueueRouter wires every store the owning-team ladder reads (Users,
// Teams, Orgs, TeamGitHubGroups) plus the durable queue, so a test can route an
// event through the drain worker — the thing that decides done vs. requeued —
// rather than calling HandleEvent directly. teamRepos/jiraRules stay nil so the
// tracking gates are inert and only the ladder's reads are under test.
func identityQueueRouter(database *sql.DB, spawner Delegator) *Router {
	st := sqlitestore.New(database)
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		st.Agents, st.TeamAgents, st.Users, testTaskStore(database), st.Conversations, st.Entities,
		st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, st.TeamGitHubGroups, spawner,
		noopScorer{}, websocket.NewHub())
	r.SetEventQueue(st.EventQueue)
	return r
}

// enqueueRoutedEvent durably enqueues one event the way the production
// ingestor does, so the drain worker is what routes it.
func enqueueRoutedEvent(t *testing.T, database *sql.DB, entityID, eventType, dedupKey string, meta any) {
	t.Helper()
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal %s metadata: %v", eventType, err)
	}
	eid := entityID
	if _, err := sqlitestore.New(database).EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &eid,
		EventType:    eventType,
		DedupKey:     dedupKey,
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
	}, ""); err != nil {
		t.Fatalf("enqueue %s: %v", eventType, err)
	}
}

func activeTasksOfType(t *testing.T, database *sql.DB, entityID, eventType string) []domain.Task {
	t.Helper()
	tasks, err := testTaskStore(database).FindActiveByEntityAndType(t.Context(), runmode.LocalDefaultOrgID, entityID, eventType)
	if err != nil {
		t.Fatalf("list %s tasks: %v", eventType, err)
	}
	return tasks
}

// TestOwnerResolution_StoreFailure_RequeuesThenRoutesToTheRightTeam is one
// subtest per PROPAGATE row of the ticket's table. Each seeds a scenario whose
// correct owner is unambiguous, breaks exactly one read, and asserts the same
// two-phase contract: the event survives the outage, and the team it lands on
// afterwards is the one the healthy ladder names — not the one the degraded
// ladder would have fallen through to.
func TestOwnerResolution_StoreFailure_RequeuesThenRoutesToTheRightTeam(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		// seed builds the scenario and enqueues the event under test. It
		// returns the entity plus the team the routed task must land on once
		// the store recovers.
		seed func(t *testing.T, database *sql.DB, r *Router) (entityID, wantTeam string)
		// disrupt swaps in the store wrapper that fails the one read.
		disrupt func(r *Router, database *sql.DB, o *outage)
		// wantErr is a fragment of the parked row's last_error. It names the
		// read that failed, so a case can't pass by tripping some other stage.
		wantErr string
		// viaVisibility marks review_requested, whose task is NULL-owned by
		// design — its routing shows up in the visibility set instead.
		viaVisibility bool
	}{
		{
			// Tier 1: a failed authoritative read used to fall through to the
			// author tier, handing the entity to the author's team instead of
			// the pinned one.
			name:      "tier 1 structural owner",
			eventType: domain.EventGitHubPRCICheckFailed,
			seed: func(t *testing.T, database *sql.DB, _ *Router) (string, string) {
				setReviewHost(t, database)
				pinned := seedTeam(t, database, "pinned-team")
				author := seedTeam(t, database, "author-team")
				seedUserOnTeam(t, database, author, "aidan")
				seedSystemCIRule(t, database, pinned)
				seedSystemCIRule(t, database, author)
				entityID := reviewEntity(t, database, "owner/repo#tier1")
				if _, err := database.Exec(`UPDATE entities SET owning_team_id = ? WHERE id = ?`, pinned, entityID); err != nil {
					t.Fatalf("set owning_team_id: %v", err)
				}
				enqueueRoutedEvent(t, database, entityID, domain.EventGitHubPRCICheckFailed, "build",
					events.GitHubPRCICheckFailedMetadata{Author: "aidan", CheckName: "build", Repo: "owner/repo", HeadSHA: "abc123"})
				return entityID, pinned
			},
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.entities = outageOwningTeamStore{EntityStore: sqlitestore.New(database).Entities, o: o}
			},
			wantErr: "resolve owner: structural owner for entity",
		},
		{
			// Tier 2: the entity already has an owned CI task on team A. A
			// failed prior-task read used to demote to the author tier, so the
			// second event routed to the SECOND author's team.
			name:      "tier 2 prior-task owner",
			eventType: domain.EventGitHubPRConflicts,
			seed: func(t *testing.T, database *sql.DB, r *Router) (string, string) {
				setReviewHost(t, database)
				teamA := seedTeam(t, database, "team-a")
				teamB := seedTeam(t, database, "team-b")
				seedUserOnTeam(t, database, teamA, "aidan")
				seedUserOnTeam(t, database, teamB, "bob")
				seedSystemCIRule(t, database, teamA)
				seedSystemRule(t, database, teamA, domain.EventGitHubPRConflicts)
				seedSystemRule(t, database, teamB, domain.EventGitHubPRConflicts)
				entityID := reviewEntity(t, database, "owner/repo#tier3")
				emitCI(r, entityID, "aidan") // establishes team A as the owner
				enqueueRoutedEvent(t, database, entityID, domain.EventGitHubPRConflicts, "",
					events.GitHubPRConflictsMetadata{Author: "bob", Repo: "owner/repo"})
				return entityID, teamA
			},
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.tasks = outagePriorTaskOwnerStore{TaskStore: testTaskStore(database), o: o}
			},
			wantErr: "resolve owner: prior-task owner for entity",
		},
		{
			// Tier 3, first read: without the host the reverse lookup can't
			// run, and an empty owner set is indistinguishable from dependabot.
			name:      "author path org-settings host",
			eventType: domain.EventGitHubPRCICheckFailed,
			seed:      seedAuthorCIScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.orgs = outageOrgSettingsStore{OrgsStore: sqlitestore.New(database).Orgs, o: o}
			},
			wantErr: "resolve owner: read org settings for github host",
		},
		{
			name:      "author path reverse-login lookup",
			eventType: domain.EventGitHubPRCICheckFailed,
			seed:      seedAuthorCIScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.users = outageIdentityStore{UsersStore: sqlitestore.New(database).Users, o: o}
			},
			wantErr: "resolve owner: reverse login lookup for aidan",
		},
		{
			// Per-user read: the loop used to drop the user and carry on, so a
			// two-team author could come back as a one-team author — an
			// unambiguous owner that isn't one.
			name:      "author path teams-for-user",
			eventType: domain.EventGitHubPRCICheckFailed,
			seed:      seedAuthorCIScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.teams = outageTeamsForUserStore{TeamsStore: sqlitestore.New(database).Teams, o: o}
			},
			wantErr: "resolve owner: teams for user",
		},
		{
			// review_requested is the sharpest case: scoped-and-empty DROPS the
			// task, so a failed mapping read used to consume the event outright.
			name:          "review_requested github-team mapping",
			eventType:     domain.EventGitHubPRReviewRequested,
			viaVisibility: true,
			seed: func(t *testing.T, database *sql.DB, _ *Router) (string, string) {
				setReviewHost(t, database)
				backend := seedTeam(t, database, "backend-tf")
				if err := sqlitestore.New(database).TeamGitHubGroups.SetForTeam(t.Context(), backend, []domain.TeamGitHubGroup{
					{OrgLogin: "acme", TeamSlug: "backend"},
				}); err != nil {
					t.Fatalf("map github team: %v", err)
				}
				seedReviewRule(t, database, runmode.LocalDefaultTeamID)
				entityID := reviewEntity(t, database, "owner/repo#revteam")
				enqueueRoutedEvent(t, database, entityID, domain.EventGitHubPRReviewRequested, "team:acme/backend",
					events.GitHubPRReviewRequestedMetadata{Author: "carol", Repo: "owner/repo", PRNumber: 3, RequestedTeam: "acme/backend"})
				return entityID, backend
			},
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.githubGroups = outageGitHubGroupsStore{TeamGitHubGroupsStore: sqlitestore.New(database).TeamGitHubGroups, o: o}
			},
			wantErr: "resolve owner: github-team mapping for acme/backend",
		},
		{
			name:          "review_requested org-settings host",
			eventType:     domain.EventGitHubPRReviewRequested,
			viaVisibility: true,
			seed:          seedReviewRequestedScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.orgs = outageOrgSettingsStore{OrgsStore: sqlitestore.New(database).Orgs, o: o}
			},
			wantErr: "resolve owner: read org settings for github host",
		},
		{
			name:          "review_requested reverse-login lookup",
			eventType:     domain.EventGitHubPRReviewRequested,
			viaVisibility: true,
			seed:          seedReviewRequestedScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.users = outageIdentityStore{UsersStore: sqlitestore.New(database).Users, o: o}
			},
			wantErr: "resolve owner: reverse login lookup for alice",
		},
		{
			name:          "review_requested teams-for-user",
			eventType:     domain.EventGitHubPRReviewRequested,
			viaVisibility: true,
			seed:          seedReviewRequestedScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.teams = outageTeamsForUserStore{TeamsStore: sqlitestore.New(database).Teams, o: o}
			},
			wantErr: "resolve owner: teams for user",
		},
		{
			name:      "jira tier 1 structural owner",
			eventType: domain.EventJiraIssueAssigned,
			seed: func(t *testing.T, database *sql.DB, _ *Router) (string, string) {
				setJiraHost(t, database)
				pinned := seedTeam(t, database, "pinned-team")
				assignee := seedTeam(t, database, "assignee-team")
				seedJiraUserOnTeam(t, database, assignee, "acct-aidan", "aidan")
				seedSystemJiraRule(t, database, pinned, domain.EventJiraIssueAssigned)
				seedSystemJiraRule(t, database, assignee, domain.EventJiraIssueAssigned)
				entityID := jiraEntity(t, database, "SKY-tier1")
				if _, err := database.Exec(`UPDATE entities SET owning_team_id = ? WHERE id = ?`, pinned, entityID); err != nil {
					t.Fatalf("set owning_team_id: %v", err)
				}
				enqueueRoutedEvent(t, database, entityID, domain.EventJiraIssueAssigned, "",
					events.JiraIssueAssignedMetadata{Assignee: "aidan", AssigneeAccountID: "acct-aidan", IssueKey: "SKY-tier1", Project: "SKY", Status: "To Do"})
				return entityID, pinned
			},
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.entities = outageOwningTeamStore{EntityStore: sqlitestore.New(database).Entities, o: o}
			},
			wantErr: "resolve owner: structural owner for entity",
		},
		{
			// The Jira tier-2 anchor reads the entity's ACTIVE tasks; a failed
			// read demoted the ladder to the new assignee's team.
			name:      "jira tier 2 prior-task owner",
			eventType: domain.EventJiraIssueStatusChanged,
			seed: func(t *testing.T, database *sql.DB, r *Router) (string, string) {
				setJiraHost(t, database)
				teamA := seedTeam(t, database, "team-a")
				teamB := seedTeam(t, database, "team-b")
				seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan")
				seedJiraUserOnTeam(t, database, teamB, "acct-bob", "bob")
				seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueAssigned)
				seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueStatusChanged)
				seedSystemJiraRule(t, database, teamB, domain.EventJiraIssueStatusChanged)
				entityID := jiraEntity(t, database, "SKY-tier3")
				emitJiraAssigned(r, entityID, "acct-aidan") // establishes team A as the owner
				enqueueRoutedEvent(t, database, entityID, domain.EventJiraIssueStatusChanged, "In Progress",
					events.JiraIssueStatusChangedMetadata{Assignee: "bob", AssigneeAccountID: "acct-bob", IssueKey: "SKY-tier3", Project: "SKY", NewStatus: "In Progress"})
				return entityID, teamA
			},
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.tasks = outageActiveTasksStore{TaskStore: testTaskStore(database), o: o}
			},
			wantErr: "resolve owner: prior-task owner for entity",
		},
		{
			name:      "jira assignee path org-settings host",
			eventType: domain.EventJiraIssueAssigned,
			seed:      seedJiraAssignedScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.orgs = outageOrgSettingsStore{OrgsStore: sqlitestore.New(database).Orgs, o: o}
			},
			wantErr: "resolve owner: read org settings for jira host",
		},
		{
			name:      "jira assignee path reverse-account lookup",
			eventType: domain.EventJiraIssueAssigned,
			seed:      seedJiraAssignedScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.users = outageIdentityStore{UsersStore: sqlitestore.New(database).Users, o: o}
			},
			wantErr: "resolve owner: reverse jira account lookup for acct-aidan",
		},
		{
			name:      "jira assignee path teams-for-user",
			eventType: domain.EventJiraIssueAssigned,
			seed:      seedJiraAssignedScenario,
			disrupt: func(r *Router, database *sql.DB, o *outage) {
				r.teams = outageTeamsForUserStore{TeamsStore: sqlitestore.New(database).Teams, o: o}
			},
			wantErr: "resolve owner: teams for user",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			seedHandlerFKTargets(t, database)
			r := identityQueueRouter(database, &stubDelegator{db: database})
			entityID, wantTeam := tc.seed(t, database, r)
			tc.disrupt(r, database, &outage{remaining: 1})

			// The read fails. Owner resolution could not answer, so the event
			// is still owed: the row goes back to pending with its attempt
			// spent and the reason on it, and no task was minted on a guess.
			if err := r.drainEventQueue(context.Background()); err != nil {
				t.Fatalf("drainEventQueue: %v", err)
			}
			status, attempts, lastErr := queueRow(t, database)
			if status != domain.QueuedEventStatusPending {
				t.Errorf("row after the failed read = %q, want pending — an unresolvable owner must not consume the event", status)
			}
			if attempts != 1 {
				t.Errorf("attempts = %d, want 1", attempts)
			}
			if !strings.Contains(lastErr, tc.wantErr) {
				t.Errorf("last_error = %q, want it to name the failed read (%q) — otherwise this case is passing on some other stage's failure", lastErr, tc.wantErr)
			}
			if n := len(activeTasksOfType(t, database, entityID, tc.eventType)); n != 0 {
				t.Fatalf("%s tasks after the failed read = %d, want 0 — no task at all beats one on the wrong team", tc.eventType, n)
			}

			// The store recovers and the retry routes the event to the team the
			// healthy ladder names.
			if err := r.drainEventQueue(context.Background()); err != nil {
				t.Fatalf("drainEventQueue after recovery: %v", err)
			}
			if status, _, _ = queueRow(t, database); status != domain.QueuedEventStatusDone {
				t.Errorf("row after recovery = %q, want done", status)
			}
			tasks := activeTasksOfType(t, database, entityID, tc.eventType)
			if len(tasks) != 1 {
				t.Fatalf("%s tasks after recovery = %d, want exactly 1", tc.eventType, len(tasks))
			}
			if tc.viaVisibility {
				// review_requested is NULL-owned at creation; the reviewer's
				// team shows up as the task's sole visible team.
				if vis := visTeamsOf(t, database, tasks[0].ID); len(vis) != 1 || vis[0] != wantTeam {
					t.Errorf("visibility = %v, want [%s] (the requested identity's team)", vis, wantTeam)
				}
				return
			}
			if owner := teamIDValue(&tasks[0]); owner != wantTeam {
				t.Errorf("owner = %q, want %q — the retry must resolve the same owner a healthy store would have", owner, wantTeam)
			}
		})
	}
}

// seedAuthorCIScenario is the tier-3 GitHub fixture the three author-identity
// rows share: one member team, one system rule gating creation, a CI failure
// authored by that member. With the identity reads healthy the task is owned by
// the member's team; with any of them broken the author looks external.
func seedAuthorCIScenario(t *testing.T, database *sql.DB, _ *Router) (string, string) {
	t.Helper()
	setReviewHost(t, database)
	teamA := seedTeam(t, database, "team-a")
	seedUserOnTeam(t, database, teamA, "aidan")
	seedSystemCIRule(t, database, teamA)
	entityID := reviewEntity(t, database, "owner/repo#authortier")
	enqueueRoutedEvent(t, database, entityID, domain.EventGitHubPRCICheckFailed, "build",
		events.GitHubPRCICheckFailedMetadata{Author: "aidan", CheckName: "build", Repo: "owner/repo", HeadSHA: "abc123"})
	return entityID, teamA
}

// seedReviewRequestedScenario is the fixture the three review-identity rows
// share: a review requested from a TF user on their own team.
func seedReviewRequestedScenario(t *testing.T, database *sql.DB, _ *Router) (string, string) {
	t.Helper()
	setReviewHost(t, database)
	teamAlice := seedTeam(t, database, "alice-team")
	seedUserOnTeam(t, database, teamAlice, "alice")
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)
	entityID := reviewEntity(t, database, "owner/repo#revuser")
	enqueueRoutedEvent(t, database, entityID, domain.EventGitHubPRReviewRequested, "user:alice",
		events.GitHubPRReviewRequestedMetadata{Author: "carol", Repo: "owner/repo", PRNumber: 4, RequestedLogin: "alice"})
	return entityID, teamAlice
}

// seedJiraAssignedScenario is the Jira mirror of seedAuthorCIScenario.
func seedJiraAssignedScenario(t *testing.T, database *sql.DB, _ *Router) (string, string) {
	t.Helper()
	setJiraHost(t, database)
	teamA := seedTeam(t, database, "team-a")
	seedJiraUserOnTeam(t, database, teamA, "acct-aidan", "aidan")
	seedSystemJiraRule(t, database, teamA, domain.EventJiraIssueAssigned)
	entityID := jiraEntity(t, database, "SKY-assignee")
	enqueueRoutedEvent(t, database, entityID, domain.EventJiraIssueAssigned, "",
		events.JiraIssueAssignedMetadata{Assignee: "aidan", AssigneeAccountID: "acct-aidan", IssueKey: "SKY-assignee", Project: "SKY", Status: "To Do"})
	return entityID, teamA
}

// TestOwnerResolution_TransientIdentityFailure_WatcherDoesNotCaptureOwnership
// is the worst outcome the ticket names, and the reason a degraded read is not
// merely a lost task. Team C only WATCHES: an applies_to_unowned trigger, no
// membership claim on the PR. A blip in the tier-3 identity read made the
// author look external, which is precisely the state that hands the orphan
// branch to the opted-in watcher — C fires, the fire consolidates ownership
// onto C, and tier 2 then anchors every future event on the entity to C. One
// transient failure, permanent capture.
//
// The contract now: the blip requeues the event with nothing fired, and the
// retry gives the PR to the author's team.
func TestOwnerResolution_TransientIdentityFailure_WatcherDoesNotCaptureOwnership(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamMember := seedTeam(t, database, "member-team")
	teamWatcher := seedTeam(t, database, "watch-team")
	seedUserOnTeam(t, database, teamMember, "aidan")
	seedSystemCIRule(t, database, teamMember) // gates creation for the owner
	// The watcher's whole configuration: reach unowned entities, and fire on
	// them. Exactly the setup that captures the PR when the author's identity
	// can't be read.
	enableTeamAutoDelegate(t, database, teamWatcher)
	seedImmediateWatchTrigger(t, database, teamWatcher, domain.EventGitHubPRCICheckFailed, "ci-watch")

	stub := &stubDelegator{db: database}
	r := identityQueueRouter(database, stub)
	entityID := reviewEntity(t, database, "owner/repo#capture")
	enqueueRoutedEvent(t, database, entityID, domain.EventGitHubPRCICheckFailed, "build",
		events.GitHubPRCICheckFailedMetadata{Author: "aidan", CheckName: "build", Repo: "owner/repo", HeadSHA: "abc123"})

	// One transient failure of the author's reverse-login lookup.
	r.users = outageIdentityStore{UsersStore: sqlitestore.New(database).Users, o: &outage{remaining: 1}}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if status, _, lastErr := queueRow(t, database); status != domain.QueuedEventStatusPending || lastErr == "" {
		t.Errorf("row after the failed identity read = (%s, last_error %q), want (pending, non-empty)", status, lastErr)
	}
	if n := len(activeTasksOfType(t, database, entityID, domain.EventGitHubPRCICheckFailed)); n != 0 {
		t.Fatalf("tasks after the failed identity read = %d, want 0 — an unreadable author is not an external one", n)
	}
	if stub.calls != 0 {
		t.Fatalf("the watcher fired %d time(s) on an author it could not read; that fire is what captures ownership for good", stub.calls)
	}

	// The store recovers: the PR belongs to the author's team, and the watcher
	// — which never had a claim on it — has neither run nor owner.
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	if status, _, _ := queueRow(t, database); status != domain.QueuedEventStatusDone {
		t.Errorf("row after recovery = %q, want done", status)
	}
	tasks := activeTasksOfType(t, database, entityID, domain.EventGitHubPRCICheckFailed)
	if len(tasks) != 1 {
		t.Fatalf("tasks after recovery = %d, want exactly 1", len(tasks))
	}
	if owner := teamIDValue(&tasks[0]); owner != teamMember {
		t.Errorf("owner = %q, want the author's team %q (the watcher must never end up owning it)", owner, teamMember)
	}
	if stub.calls != 0 {
		t.Errorf("the watcher fired %d time(s) though a member team owns the PR; the no-steal invariant is unchanged", stub.calls)
	}
}

// TestAuthorTeams_UnwiredStores_StillDegrade pins the row of the table that
// deliberately did NOT change: a nil identity store is test / pre-ticket
// wiring, not an outage, so it still resolves to "no member owner" rather than
// erroring. Around thirty router constructions in this package depend on it.
func TestAuthorTeams_UnwiredStores_StillDegrade(t *testing.T) {
	database := newTestDB(t)
	// No Users/Teams/Orgs wired — the constructor's documented degrade.
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		nil, nil, nil, testTaskStore(database), nil, sqlitestore.New(database).Entities, nil,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, nil, nil, nil, nil, nil,
		noopScorer{}, websocket.NewHub())
	r.users, r.teams, r.orgs = nil, nil, nil

	evt := domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		MetadataJSON: `{"author":"aidan","repo":"owner/repo"}`,
		OrgID:        runmode.LocalDefaultOrgID,
	}
	teams, err := r.authorTeams(t.Context(), runmode.LocalDefaultOrgID, evt)
	if err != nil {
		t.Fatalf("authorTeams with unwired stores returned an error: %v — unwired is wiring, not an outage", err)
	}
	if len(teams) != 0 {
		t.Errorf("teams = %v, want empty", teams)
	}

	jiraEvt := domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		MetadataJSON: `{"assignee_account_id":"acct-aidan","project":"SKY"}`,
		OrgID:        runmode.LocalDefaultOrgID,
	}
	teams, err = r.assigneeTeams(t.Context(), runmode.LocalDefaultOrgID, jiraEvt)
	if err != nil {
		t.Fatalf("assigneeTeams with unwired stores returned an error: %v", err)
	}
	if len(teams) != 0 {
		t.Errorf("teams = %v, want empty", teams)
	}
}

// TestAuthorTeams_MalformedMetadata_ResolvesEmpty pins the other unchanged row:
// an event whose metadata carries no author is a resolved data state — an empty
// owner set, no error, no retry. A retry could not produce a different answer.
func TestAuthorTeams_MalformedMetadata_ResolvesEmpty(t *testing.T) {
	database := newTestDB(t)
	r := identityQueueRouter(database, nil)

	for _, meta := range []string{`{"author":""}`, `not json`, `{}`} {
		teams, err := r.authorTeams(t.Context(), runmode.LocalDefaultOrgID, domain.Event{
			EventType: domain.EventGitHubPRCICheckFailed, MetadataJSON: meta, OrgID: runmode.LocalDefaultOrgID,
		})
		if err != nil {
			t.Errorf("authorTeams(%s) error = %v, want nil (a data state, not a failure)", meta, err)
		}
		if len(teams) != 0 {
			t.Errorf("authorTeams(%s) = %v, want empty", meta, teams)
		}
	}
}

// TestReviewRequestVisibilityTeams_MalformedTeamHandle_ScopedEmpty pins the
// last unchanged row: a requested github team whose handle isn't "org/slug"
// resolves scoped-and-empty (drop the task), because no store was ever asked.
// This is the outcome a failed mapping read used to borrow.
func TestReviewRequestVisibilityTeams_MalformedTeamHandle_ScopedEmpty(t *testing.T) {
	database := newTestDB(t)
	r := identityQueueRouter(database, nil)

	meta, _ := json.Marshal(events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 1, RequestedTeam: "no-slash",
	})
	teams, scoped, err := r.reviewRequestVisibilityTeams(t.Context(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRReviewRequested, MetadataJSON: string(meta), OrgID: runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("error = %v, want nil (a malformed handle is a data state)", err)
	}
	if !scoped || len(teams) != 0 {
		t.Errorf("(teams, scoped) = (%v, %v), want (empty, true) — scoped-and-empty drops the task", teams, scoped)
	}
}
