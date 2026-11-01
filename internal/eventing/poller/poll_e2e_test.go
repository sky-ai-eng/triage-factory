package poller_test

// The GitHub poll path end to end: a real poller.Manager and tracker, over real
// stores, poll a GitHub world while its pull requests change, and each cycle
// must emit exactly the events the tracker's taxonomy
// (specs/tracked-events.md) promises — and none when nothing changed.
//
// The world is served by Orrery (github.com/sky-ai-eng/orrery), a deterministic
// GitHub built from real git, and is authored through its GitHub API. A call
// Orrery answers "not mocked" fails the test, so a poll-path query Orrery does
// not serve surfaces here instead of as a quietly wrong answer.
//
// Opt-in: it runs only when ORRERY_DIR names an Orrery checkout, the variable
// e2e/sso/run.sh reads too, and skips otherwise, so a contributor without one
// never runs it. A relative ORRERY_DIR is read from the repository root. The
// test builds Orrery there with cargo (a no-op when nothing changed) and serves
// the binary it built. CI builds the commit e2e/ORRERY_REF pins, and the test
// logs a warning when the checkout is elsewhere; moving to a newer Orrery is a
// pull request here that changes that file. Locally, with Orrery checked out
// beside this repository:
//
//	ORRERY_DIR=../orrery go test ./internal/eventing/poller -run TestPollEmitsTrackedEvents

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/mergeability"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/poller"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The scenario runs once per shape TF ships in: multi mode on Postgres, and
// local mode on SQLite. The two poll differently — local mode polls as the
// local user, reading their login and teams and running the dashboard
// searches — and every expectation holds for both. The Postgres shape skips
// without Docker, as every pgtest test does.
func TestPollEmitsTrackedEvents(t *testing.T) {
	dir := os.Getenv("ORRERY_DIR")
	if dir == "" {
		t.Skip("ORRERY_DIR is not set; see the comment at the top of this file")
	}
	bin := buildOrrery(t, dir)
	t.Run("multi on Postgres", func(t *testing.T) {
		runPollScenario(t, bin, multiOnPostgres)
	})
	t.Run("local on SQLite", func(t *testing.T) {
		runPollScenario(t, bin, localOnSQLite)
	})
}

func runPollScenario(t *testing.T, bin string, shape deploymentShape) {
	ctx := context.Background()
	w := startOrrery(t, bin)
	w.actor("acme", "org")
	alice := w.actor("alice", "user")
	bob := w.actor("bob", "user")
	carol := w.actor("carol", "user")
	dave := w.actor(knownLogin, "user")
	ci := w.actor("ci", "bot")
	w.githubREST(alice, "POST", "/api/v3/orgs/acme/repos", map[string]any{"name": "webapp"}, 201)

	// Two GitHub teams, both mapped to the TF team, with dave on one of them.
	orgs := "/api/v3/orgs/acme"
	w.githubREST(alice, "PUT", orgs+"/memberships/"+knownLogin, map[string]any{"role": "member"}, 200)
	org := w.githubREST(alice, "GET", orgs, nil, 200)
	reviewers := w.githubREST(alice, "POST", orgs+"/teams", map[string]any{"name": "Reviewers"}, 201)
	release := w.githubREST(alice, "POST", orgs+"/teams", map[string]any{"name": "Release"}, 201)
	groups := []domain.TeamGitHubGroup{
		{GitHubTeamID: githubIDOf(t, reviewers), GitHubOrgID: githubIDOf(t, org), OrgLogin: "acme", TeamSlug: "reviewers"},
		{GitHubTeamID: githubIDOf(t, release), GitHubOrgID: githubIDOf(t, org), OrgLogin: "acme", TeamSlug: "release"},
	}
	w.githubREST(alice, "PUT", orgs+"/teams/reviewers/memberships/"+knownLogin, map[string]any{"role": "member"}, 200)

	// main, then three branches off it: one for checks, one that will
	// conflict with main, one for reviews. main then changes the line the
	// conflict branch changed.
	repo := w.clone(t, alice, "acme/webapp")
	repo.commit("app.txt", "version 1\n", "init")
	repo.push("main")
	repo.branch("checks", "main")
	repo.commit("checks.txt", "checks\n", "add checks")
	repo.branch("conflict", "main")
	repo.commit("app.txt", "version 2 (branch)\n", "bump on a branch")
	repo.branch("review", "main")
	repo.commit("review.txt", "review\n", "add review")
	repo.push("checks", "conflict", "review")
	repo.checkout("main")
	repo.commit("app.txt", "version 2 (main)\n", "bump on main")
	repo.push("main")
	// Branched after main's bump, so it does not conflict.
	repo.branch("draft", "main")
	repo.commit("draft.txt", "draft\n", "add draft")
	repo.push("draft")

	pulls := "/api/v3/repos/acme/webapp/pulls"
	checksPR := w.githubREST(alice, "POST", pulls, map[string]any{"title": "Checks", "head": "checks", "base": "main"}, 201)
	w.githubREST(alice, "POST", pulls, map[string]any{"title": "Conflict", "head": "conflict", "base": "main"}, 201)
	reviewPR := w.githubREST(alice, "POST", pulls, map[string]any{"title": "Review", "head": "review", "base": "main"}, 201)
	draftPR := w.githubREST(alice, "POST", pulls, map[string]any{"title": "Draft", "head": "draft", "base": "main", "draft": true}, 201)
	labels := "/api/v3/repos/acme/webapp/issues/1/labels"
	w.githubRESTList(alice, "POST", labels, map[string]any{"labels": []string{"bug"}}, 200)

	checkRuns := "/api/v3/repos/acme/webapp/check-runs"
	checksHead := sha(checksPR)
	w.githubREST(ci, "POST", checkRuns, map[string]any{"name": "ci/test", "head_sha": checksHead, "status": "completed", "conclusion": "success"}, 201)
	w.githubREST(ci, "POST", checkRuns, map[string]any{"name": "ci/lint", "head_sha": checksHead, "status": "completed", "conclusion": "failure"}, 201)
	running := w.githubREST(ci, "POST", checkRuns, map[string]any{"name": "ci/test", "head_sha": sha(reviewPR), "status": "in_progress"}, 201)
	w.githubREST(bob, "POST", pulls+"/1/reviews", map[string]any{"event": "APPROVE", "body": "ship it"}, 200)
	w.githubREST(bob, "POST", pulls+"/2/reviews", map[string]any{"event": "COMMENT", "body": "this one conflicts"}, 200)
	w.githubREST(carol, "POST", pulls+"/3/reviews", map[string]any{"event": "REQUEST_CHANGES", "body": "needs a test"}, 200)

	p := newPollRig(t, ctx, w, shape, groups)

	// The first sight of each pull request reports what is already on it: its
	// checks, reviews and conflicts, though not its labels or its draft state.
	p.cycle(t, ctx, map[string]int{
		"acme/webapp#1 " + domain.EventGitHubPRCICheckPassed:          1,
		"acme/webapp#1 " + domain.EventGitHubPRCICheckFailed:          1,
		"acme/webapp#1 " + domain.EventGitHubPRReviewApproved:         1,
		"acme/webapp#2 " + domain.EventGitHubPRConflicts:              1,
		"acme/webapp#2 " + domain.EventGitHubPRReviewCommented:        1,
		"acme/webapp#3 " + domain.EventGitHubPRReviewChangesRequested: 1,
	})

	// Nothing changed: no events, and the pull request list answers 304.
	pullsList := "GET /api/v3/repos/acme/webapp/pulls"
	before := p.tr.count(pullsList, http.StatusNotModified)
	p.cycle(t, ctx, map[string]int{})
	if p.tr.count(pullsList, http.StatusNotModified) == before {
		t.Error("the unchanged cycle's pull request list was not a 304")
	}

	// Every kind of change the world can make through GitHub's API today, a
	// minute on. Orrery's clock moves only when told to, and a pull request
	// whose updated_at and head are unchanged is not re-read, so without it a
	// change that bumps only updated_at (#4's) would go unseen.
	w.advance(60)
	w.githubREST(ci, "PATCH", fmt.Sprintf("%s/%v", checkRuns, running["id"]), map[string]any{"status": "completed", "conclusion": "success"}, 200)
	w.githubREST(bob, "POST", pulls+"/3/reviews", map[string]any{"event": "APPROVE", "body": "thanks"}, 200)
	w.githubREST(carol, "POST", "/api/v3/repos/acme/webapp/issues/3/comments", map[string]any{"body": "merging after lunch"}, 201)
	w.githubREST(alice, "PATCH", pulls+"/3", map[string]any{"body": "Adds review.txt."}, 200)
	repo.checkout("checks")
	repo.commit("checks.txt", "checks, again\n", "another commit")
	repo.push("checks")
	w.githubREST(alice, "PATCH", pulls+"/2", map[string]any{"state": "closed"}, 200)
	w.githubRESTList(alice, "POST", labels, map[string]any{"labels": []string{"needs-review"}}, 200)
	w.githubRESTList(alice, "DELETE", labels+"/bug", nil, 200)
	w.githubGraphQL(alice, `mutation($id: ID!) {
		markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } }
	}`, map[string]any{"id": draftPR["node_id"]})
	evts := p.cycle(t, ctx, map[string]int{
		"acme/webapp#1 " + domain.EventGitHubPRNewCommits:                   1,
		"acme/webapp#1 " + domain.EventGitHubPRLabelAdded + " needs-review": 1,
		"acme/webapp#1 " + domain.EventGitHubPRLabelRemoved + " bug":        1,
		"acme/webapp#2 " + domain.EventGitHubPRClosed:                       1,
		"acme/webapp#3 " + domain.EventGitHubPRCICheckPassed:                1,
		"acme/webapp#3 " + domain.EventGitHubPRReviewApproved:               1,
		"acme/webapp#3 " + domain.EventGitHubPRCommented:                    1,
		"acme/webapp#3 " + domain.EventGitHubPRBodyUpdated:                  1,
		"acme/webapp#4 " + domain.EventGitHubPRReadyForReview:               1,
	})
	aliceID := githubIDOf(t, w.githubREST(alice, "GET", "/api/v3/users/alice", nil, 200))
	if m, ok := metadataOf[events.GitHubPRClosedMetadata](t, evts, domain.EventGitHubPRClosed); ok && (m.ClosedBy != "alice" || m.ClosedByID != aliceID) {
		t.Errorf("acme/webapp#2 closed by %s/%s, want alice/%s", m.ClosedBy, m.ClosedByID, aliceID)
	}

	// Review requests and mentions. Only identities TF knows produce events:
	// dave and the mapped acme/reviewers, never carol. A request is keyed by
	// GitHub's id for the requested user or team; a mention, which names a
	// handle, by the handle.
	daveID := githubIDOf(t, w.githubREST(alice, "GET", "/api/v3/users/"+knownLogin, nil, 200))
	w.advance(60)
	w.githubREST(alice, "POST", pulls+"/1/requested_reviewers", map[string]any{
		"reviewers": []string{knownLogin, "carol"}, "team_reviewers": []string{"reviewers"},
	}, 201)
	w.githubREST(carol, "POST", "/api/v3/repos/acme/webapp/issues/4/comments", map[string]any{
		"body": "@" + knownLogin + " and @acme/reviewers, can you look? cc @bob",
	}, 201)
	p.cycle(t, ctx, map[string]int{
		"acme/webapp#1 " + domain.EventGitHubPRReviewRequested + " user_id:" + daveID:                 1,
		"acme/webapp#1 " + domain.EventGitHubPRReviewRequested + " team_id:" + groups[0].GitHubTeamID: 1,
		"acme/webapp#4 " + domain.EventGitHubPRCommented:                                              1,
		"acme/webapp#4 " + domain.EventGitHubPRMentioned + " user:" + knownLogin:                      1,
		"acme/webapp#4 " + domain.EventGitHubPRMentioned + " team:acme/reviewers":                     1,
	})

	// dave's review answers his request; the team's request stays. Deleting
	// acme/release prunes its mapping on the next cycle, and acme/reviewers'
	// mapping stays because the team still exists.
	w.advance(60)
	w.githubREST(dave, "POST", pulls+"/1/reviews", map[string]any{"event": "COMMENT", "body": "looked"}, 200)
	w.send(alice, "DELETE", orgs+"/teams/release", nil, http.StatusNoContent)
	p.cycle(t, ctx, map[string]int{
		"acme/webapp#1 " + domain.EventGitHubPRReviewCommented:                             1,
		"acme/webapp#1 " + domain.EventGitHubPRReviewRequestRemoved + " user_id:" + daveID: 1,
	})
	p.assertMapped(t, ctx, groups[0], true)
	p.assertMapped(t, ctx, groups[1], false)

	w.advance(60)
	w.githubREST(alice, "PUT", pulls+"/3/merge", map[string]any{}, 200)
	evts = p.cycle(t, ctx, map[string]int{
		"acme/webapp#3 " + domain.EventGitHubPRMerged: 1,
	})
	if m, ok := metadataOf[events.GitHubPRMergedMetadata](t, evts, domain.EventGitHubPRMerged); ok && (m.MergedBy != "alice" || m.MergedByID != aliceID) {
		t.Errorf("acme/webapp#3 merged by %s/%s, want alice/%s", m.MergedBy, m.MergedByID, aliceID)
	}

	if n := len(p.tr.notMocked()); n > 0 {
		t.Errorf("%d request(s) reached surface Orrery does not mock: %v", n, p.tr.notMocked())
	}
}

// --- the poller --------------------------------------------------------------

type pollRig struct {
	mgr *poller.Manager
	// prober is the mergeability probe worker, which reads the pull requests
	// each cycle's poll admitted; ripen stands in for the admission delay.
	prober *mergeability.Worker
	ripen  func() error
	stores db.Stores
	orgID  string
	teamID string
	host   string
	pub    *recordingPublisher
	tr     *countingTransport
	errs   []string
	errsMu sync.Mutex
}

// deploymentShape is one way TF is deployed: its run mode, its stores, and the
// org and team those stores poll for.
type deploymentShape func(t *testing.T, ctx context.Context) deployment

type deployment struct {
	database *sql.DB
	// ripen moves every ready mergeability probe's probe_after into the past,
	// in the shape's own SQL.
	ripen   func() error
	stores  db.Stores
	orgID   string
	teamID  string
	adminID string
	// pollerLogin is the GitHub login the poll credential belongs to.
	pollerLogin string
	// identitySource is how the admin's GitHub identity was captured.
	identitySource string
}

// The person TF knows on GitHub: the admin's login in both shapes, and in
// local mode also the poll credential's owner. A review request or mention
// naming anyone else routes to nobody.
const knownLogin = "dave"

// multiOnPostgres is a multi-mode control plane: Postgres stores, a real org
// and team, and no local user, so the poller resolves reviewers against the
// stores. It polls as the org's machine user.
func multiOnPostgres(t *testing.T, ctx context.Context) deployment {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "acme-tf")
	return deployment{
		database: h.AdminDB,
		ripen: func() error {
			_, err := h.AdminDB.Exec(`UPDATE pr_mergeability_probes SET probe_after = statement_timestamp() - interval '1 second' WHERE status = 'ready'`)
			return err
		},
		stores:         pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey),
		orgID:          orgID,
		teamID:         teamID,
		adminID:        owner,
		pollerLogin:    "tf-poller",
		identitySource: "connect_oauth",
	}
}

// localOnSQLite is local mode: one SQLite file's stores, the local default org
// and team, and a local user whose GitHub login is the poll credential's
// owner, as when someone runs TF with their own PAT. The poller resolves
// reviewers against that login and the teams GitHub lists for it.
func localOnSQLite(t *testing.T, ctx context.Context) deployment {
	runmode.SetForTest(t, runmode.ModeLocal)
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	database.SetMaxOpenConns(1)
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatal(err)
	}
	return deployment{
		database: database,
		ripen: func() error {
			_, err := database.Exec(`UPDATE pr_mergeability_probes SET probe_after = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'ready'`)
			return err
		},
		stores:         sqlitestore.New(database),
		orgID:          runmode.LocalDefaultOrgID,
		teamID:         runmode.LocalDefaultTeamID,
		adminID:        runmode.LocalDefaultUserID,
		pollerLogin:    knownLogin,
		identitySource: "pat",
	}
}

// newPollRig wires a poller over the shape's stores, tracking acme/webapp on
// the org whose GitHub is the Orrery world. The admin's GitHub identity is
// knownLogin, and the team is mapped to groups, the GitHub teams
// acme/reviewers and acme/release.
func newPollRig(t *testing.T, ctx context.Context, w *orreryWorld, shape deploymentShape, groups []domain.TeamGitHubGroup) *pollRig {
	t.Helper()
	d := shape(t, ctx)
	stores := d.stores
	// Configured as the org's admin would through the settings routes, under
	// their claims: the host first, then the tracked repository on it.
	if err := stores.Tx.WithTx(ctx, d.orgID, d.adminID, func(tx db.TxStores) error {
		_, err := tx.Orgs.SetSourceBaseURL(ctx, d.orgID, "github", w.base)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	host, err := db.OrgGitHubHostSystem(ctx, stores.Orgs, d.orgID)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Tx.WithTx(ctx, d.orgID, d.adminID, func(tx db.TxStores) error {
		if err := dbtest.TrackRepos(ctx, tx.Repos, tx.TeamGitHubRepos, d.orgID, d.teamID, host,
			[]domain.TeamGitHubRepo{{Owner: "acme", Repo: "webapp"}}); err != nil {
			return err
		}
		if err := tx.Users.UpsertGitHubIdentity(ctx, d.adminID, host, knownLogin, "", "", d.identitySource); err != nil {
			return err
		}
		return tx.TeamGitHubGroups.SetForTeam(ctx, d.teamID, host, groups)
	}); err != nil {
		t.Fatal(err)
	}

	token := w.tokens[d.pollerLogin]
	if token == "" {
		token = w.actor(d.pollerLogin, "user")
	}
	tr := &countingTransport{base: http.DefaultTransport}
	resolver := &orreryResolver{
		client: ghclient.NewClientWithHTTPClient(w.base, token, &http.Client{Transport: tr}),
		base:   w.base,
	}
	r := &pollRig{stores: stores, orgID: d.orgID, teamID: d.teamID, host: host, pub: &recordingPublisher{}, tr: tr, ripen: d.ripen}
	r.prober = mergeability.NewWorker(stores.PRMergeability, stores.Orgs, resolver, r.pub, workitem.Owner{ID: "poll-e2e", Epoch: 1})
	r.mgr = poller.NewManager(d.database, r.pub,
		stores.Users, stores.Tasks, stores.Entities, stores.Repos, stores.Deliverables, stores.ExternalActions, stores.EventQueue, stores.PRMergeability, stores.Orgs,
		stores.JiraStatusRules, stores.JiraProjects, stores.LinearTeamRules, stores.TeamGitHubGroups, stores.Secrets, stores.GitHubApps, stores.PollReadiness,
		resolver, nil)
	r.mgr.OnError = func(source, org string, err error) {
		r.errsMu.Lock()
		r.errs = append(r.errs, fmt.Sprintf("%s: %v", source, err))
		r.errsMu.Unlock()
	}
	return r
}

// cycle runs one GitHub poll cycle and the mergeability probe pass after it,
// and compares the github:* events they emitted, keyed "<source id> <event
// type>" plus " <dedup key>" when the event has one, with want, and returns
// them.
func (r *pollRig) cycle(t *testing.T, ctx context.Context, want map[string]int) []domain.Event {
	t.Helper()
	r.pub.take()
	r.mgr.PollGitHubOnce(ctx, r.orgID)
	if err := r.ripen(); err != nil {
		t.Fatalf("ripen mergeability probes: %v", err)
	}
	if err := r.prober.Drain(ctx); err != nil {
		t.Errorf("mergeability probe pass: %v", err)
	}
	r.errsMu.Lock()
	errs := r.errs
	r.errs = nil
	r.errsMu.Unlock()
	for _, e := range errs {
		t.Errorf("poll cycle error: %s", e)
	}

	evts := r.pub.take()
	got := map[string]int{}
	for _, evt := range evts {
		key := evt.EventType
		if evt.EntityID != nil {
			e, err := r.stores.Entities.GetSystem(ctx, r.orgID, *evt.EntityID)
			if err == nil && e != nil {
				key = e.SourceID + " " + evt.EventType
			}
		}
		if evt.DedupKey != "" {
			key += " " + evt.DedupKey
		}
		got[key]++
	}
	if !equalCounts(got, want) {
		t.Errorf("events:\n got  %s\n want %s", describe(got), describe(want))
	}
	return evts
}

// metadataOf decodes the metadata of the event of eventType among evts, and
// reports false when there is none: the cycle's counts already fail the test
// for a missing event.
func metadataOf[T any](t *testing.T, evts []domain.Event, eventType string) (T, bool) {
	t.Helper()
	var m T
	for _, evt := range evts {
		if evt.EventType == eventType {
			if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
				t.Fatalf("%s metadata: %v", eventType, err)
			}
			return m, true
		}
	}
	return m, false
}

// assertMapped checks whether the GitHub team g names is still mapped to the
// rig's TF team.
func (r *pollRig) assertMapped(t *testing.T, ctx context.Context, g domain.TeamGitHubGroup, want bool) {
	t.Helper()
	teams, err := r.stores.TeamGitHubGroups.TeamsForGitHubTeamSystem(ctx, r.orgID, r.host, g.GitHubTeamID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Contains(teams, r.teamID); got != want {
		t.Errorf("acme/%s mapped to the team: %v, want %v", g.TeamSlug, got, want)
	}
}

// githubIDOf is the id GitHub gave the object a REST call answered with, in
// the form TF stores GitHub's ids.
func githubIDOf(t *testing.T, obj map[string]any) string {
	t.Helper()
	id, ok := obj["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("no numeric id in %v", obj)
	}
	return strconv.FormatFloat(id, 'f', -1, 64)
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func describe(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		keys = append(keys, fmt.Sprintf("%s ×%d", k, v))
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "(none)"
	}
	return strings.Join(keys, ", ")
}

// recordingPublisher keeps every github:* event the tracker emits, on either
// side of its durable write.
type recordingPublisher struct {
	mu     sync.Mutex
	events []domain.Event
}

func (p *recordingPublisher) Publish(_ context.Context, evt domain.Event) {
	if !strings.HasPrefix(evt.EventType, "github:") {
		return
	}
	p.mu.Lock()
	p.events = append(p.events, evt)
	p.mu.Unlock()
}

func (p *recordingPublisher) PublishPreEnqueued(ctx context.Context, evt domain.Event) {
	p.Publish(ctx, evt)
}

func (p *recordingPublisher) take() []domain.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.events
	p.events = nil
	return out
}

// orreryResolver hands every poll-path lookup the one client pointed at the
// world.
type orreryResolver struct {
	client *ghclient.Client
	base   string
}

func (r *orreryResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r *orreryResolver) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	return r.client, nil
}

func (r *orreryResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r *orreryResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func (r *orreryResolver) BaseURLFor(context.Context, string) (string, error) {
	return r.base, nil
}

func (r *orreryResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}

// countingTransport counts responses by request and status, and keeps the
// requests Orrery answered with its 501 not-mocked envelope.
type countingTransport struct {
	base     http.RoundTripper
	mu       sync.Mutex
	counts   map[string]int // "<method> <path> <status>"
	unmocked []string
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	c.mu.Lock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[fmt.Sprintf("%s %s %d", req.Method, req.URL.Path, resp.StatusCode)]++
	if resp.StatusCode == http.StatusNotImplemented {
		c.unmocked = append(c.unmocked, req.Method+" "+req.URL.Path)
	}
	c.mu.Unlock()
	return resp, nil
}

// count is how many responses to request ("<method> <path>") had status.
func (c *countingTransport) count(request string, status int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[fmt.Sprintf("%s %d", request, status)]
}

func (c *countingTransport) notMocked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.unmocked...)
}

// --- the world ---------------------------------------------------------------

type orreryWorld struct {
	t    *testing.T
	base string
	// tokens holds each actor's token by login.
	tokens map[string]string
}

// buildOrrery builds the orrery binary in the checkout at dir and returns its
// path. Having opted in, a missing checkout or toolchain fails the test rather
// than skipping it.
func buildOrrery(t *testing.T, dir string) string {
	t.Helper()
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoRoot(t), dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err != nil {
		t.Fatalf("ORRERY_DIR=%s is not an Orrery checkout: %v", dir, err)
	}
	warnUnpinnedOrrery(t, dir)
	cmd := exec.Command("cargo", "build", "--quiet", "-p", "orrery")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cargo build in %s: %v\n%s", dir, err, out)
	}
	target := os.Getenv("CARGO_TARGET_DIR")
	switch {
	case target == "":
		target = filepath.Join(dir, "target")
	case !filepath.IsAbs(target):
		target = filepath.Join(dir, target)
	}
	return filepath.Join(target, "debug", "orrery")
}

// warnUnpinnedOrrery logs when the checkout at dir is not at the Orrery commit
// e2e/ORRERY_REF pins, which is the one CI builds. A local run against another
// commit is allowed; its result just may not match CI's.
func warnUnpinnedOrrery(t *testing.T, dir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "e2e", "ORRERY_REF"))
	if err != nil {
		t.Fatalf("read the pinned Orrery commit: %v", err)
	}
	pinned := strings.TrimSpace(string(raw))
	head := "unknown"
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
		head = strings.TrimSpace(string(out))
	}
	if head != pinned {
		t.Logf("warning: %s is at %s, not the pinned Orrery commit %s (e2e/ORRERY_REF)", dir, head, pinned)
	}
}

// repoRoot is the directory holding this module's go.mod. go test runs in the
// package's directory, so a relative path is read from there by default.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			t.Fatalf("no go.mod above %s", wd)
		}
	}
}

// startOrrery serves a fresh world on a free loopback port for the length of
// the test. The port is found by binding port 0 and closing it before Orrery
// binds it, so another process can take it in between; Orrery then exits on
// its bind error and the start is retried on a new port.
func startOrrery(t *testing.T, bin string) *orreryWorld {
	t.Helper()
	var lastLog string
	for attempt := 0; attempt < 3; attempt++ {
		w, log, ok := tryStartOrrery(t, bin)
		if ok {
			return w
		}
		lastLog = log
	}
	t.Fatalf("orrery did not start in 3 attempts; last log:\n%s", lastLog)
	return nil
}

func tryStartOrrery(t *testing.T, bin string) (*orreryWorld, string, bool) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "orrery.toml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("[instance]\ndir = %q\n\n[server]\nlisten = %q\n", filepath.Join(dir, "instance"), addr)), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	cmd := exec.Command(bin, "serve", "--config", cfg)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start orrery: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	stop := func() {
		_ = cmd.Process.Kill()
		<-exited
	}

	w := &orreryWorld{t: t, base: "http://" + addr, tokens: map[string]string{}}
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-exited:
			return nil, logs.String(), false
		default:
		}
		resp, err := http.Get(w.base + "/_orrery/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Cleanup(func() {
					stop()
					if t.Failed() {
						t.Logf("orrery log:\n%s", logs.String())
					}
				})
				return w, "", true
			}
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("orrery did not become healthy at %s:\n%s", w.base, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncBuffer is Orrery's log: os/exec copies the process's output into it from
// its own goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// actor mints an actor on the control plane and returns its token.
func (w *orreryWorld) actor(login, kind string) string {
	w.t.Helper()
	out := w.call("", "POST", "/_orrery/actors", map[string]any{"login": login, "kind": kind}, http.StatusCreated)
	token, _ := out["token"].(string)
	w.tokens[login] = token
	return token
}

// githubREST calls the GitHub REST API Orrery serves, as the actor holding
// token.
func (w *orreryWorld) githubREST(token, method, path string, body map[string]any, want int) map[string]any {
	w.t.Helper()
	return w.call(token, method, path, body, want)
}

// githubRESTList is githubREST for a call GitHub answers with a JSON array,
// such as the label writes, which answer with the issue's labels.
func (w *orreryWorld) githubRESTList(token, method, path string, body map[string]any, want int) []any {
	w.t.Helper()
	got := w.send(token, method, path, body, want)
	var out []any
	if err := json.Unmarshal(got, &out); err != nil {
		w.t.Fatalf("%s %s: response is not a JSON array: %v: %s", method, path, err, got)
	}
	return out
}

// githubGraphQL runs a GraphQL operation as the actor holding token and
// returns its data. GraphQL reports a failed operation in a 200, so any entry
// in errors fails the test.
func (w *orreryWorld) githubGraphQL(token, query string, variables map[string]any) map[string]any {
	w.t.Helper()
	out := w.call(token, "POST", "/api/graphql", map[string]any{"query": query, "variables": variables}, http.StatusOK)
	if errs, ok := out["errors"].([]any); ok && len(errs) > 0 {
		w.t.Fatalf("graphql: %v", errs)
	}
	data, ok := out["data"].(map[string]any)
	if !ok {
		w.t.Fatalf("graphql: no data in the response: %v", out)
	}
	return data
}

func (w *orreryWorld) call(token, method, path string, body map[string]any, want int) map[string]any {
	w.t.Helper()
	got := w.send(token, method, path, body, want)
	// Every call this test makes through here answers with a JSON object;
	// anything else is a failure here rather than an empty map the caller
	// reads as absent fields.
	out := map[string]any{}
	if err := json.Unmarshal(got, &out); err != nil {
		w.t.Fatalf("%s %s: response is not a JSON object: %v: %s", method, path, err, got)
	}
	return out
}

// send makes the request and returns the response body, failing the test
// unless the status is want.
func (w *orreryWorld) send(token, method, path string, body map[string]any, want int) []byte {
	w.t.Helper()
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatal(err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, w.base+path, reqBody)
	if err != nil {
		w.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		w.t.Fatalf("%s %s: read response: %v", method, path, err)
	}
	if resp.StatusCode != want {
		w.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, got)
	}
	return got
}

// advance moves the world's clock forward by seconds.
func (w *orreryWorld) advance(seconds int) {
	w.t.Helper()
	w.call("", "POST", "/_orrery/clock/advance", map[string]any{"seconds": seconds}, http.StatusOK)
}

func (w *orreryWorld) clock() int64 {
	w.t.Helper()
	out := w.call("", "GET", "/_orrery/clock", nil, http.StatusOK)
	n, _ := out["clock_unix"].(float64)
	return int64(n)
}

func sha(pull map[string]any) string {
	head, _ := pull["head"].(map[string]any)
	s, _ := head["sha"].(string)
	return s
}

// gitRepo is a working copy whose commits are dated by the world's clock, so
// the world stays deterministic.
type gitRepo struct {
	w      *orreryWorld
	dir    string
	remote string
	// auth is the HTTP Authorization header git sends to Orrery. It reaches
	// git through the environment rather than the remote URL or an argument,
	// so neither a failed command's message nor git's own output carries it.
	auth string
}

func (w *orreryWorld) clone(t *testing.T, token, fullName string) *gitRepo {
	t.Helper()
	r := &gitRepo{
		w:      w,
		dir:    t.TempDir(),
		remote: w.base + "/" + fullName + ".git",
		auth:   "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)),
	}
	r.git("init", "-q", "-b", "main", ".")
	return r
}

func (r *gitRepo) git(args ...string) {
	r.w.t.Helper()
	date := fmt.Sprintf("%d +0000", r.w.clock())
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=alice", "GIT_AUTHOR_EMAIL=alice@orrery.invalid",
		"GIT_COMMITTER_NAME=alice", "GIT_COMMITTER_EMAIL=alice@orrery.invalid",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0="+r.auth)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *gitRepo) commit(file, content, message string) {
	r.w.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, file), []byte(content), 0o644); err != nil {
		r.w.t.Fatal(err)
	}
	r.git("add", file)
	r.git("commit", "-q", "-m", message)
}

func (r *gitRepo) branch(name, from string) { r.git("checkout", "-q", "-b", name, from) }
func (r *gitRepo) checkout(name string)     { r.git("checkout", "-q", name) }
func (r *gitRepo) push(branches ...string) {
	r.git(append([]string{"push", "-q", r.remote}, branches...)...)
}
