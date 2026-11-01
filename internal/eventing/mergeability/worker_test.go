package mergeability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// rig is one worker over a migrated in-memory database and a GitHub that
// answers PRMergeability with whatever answer holds.
type rig struct {
	t        *testing.T
	database *sql.DB
	stores   db.Stores
	worker   *Worker
	pub      *recorder

	mu     sync.Mutex
	answer string
	reads  int
	// refuse, when set, answers every read in place of GitHub's answer: a
	// rate-limit refusal, in the tests that set it.
	refuse   func(w http.ResponseWriter)
	resolver *fixedResolver
}

type recorder struct {
	mu     sync.Mutex
	events []domain.Event
	// panicNext makes the next publish panic instead of recording.
	panicNext bool
}

func (r *recorder) PublishPreEnqueued(_ context.Context, evt domain.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.panicNext {
		r.panicNext = false
		panic("publish failed")
	}
	r.events = append(r.events, evt)
}

func (r *recorder) take() []domain.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// fixedResolver hands out a new client for the one fake GitHub on every
// call, as the production resolver does, so no rate-limit reading carries
// from one resolution to the next.
type fixedResolver struct {
	base  string
	err   error
	repos []string
}

func (f *fixedResolver) ClientForRepo(_ context.Context, _, owner, repo string) (*ghclient.Client, error) {
	f.repos = append(f.repos, owner+"/"+repo)
	if f.err != nil {
		return nil, f.err
	}
	return ghclient.NewClient(f.base, "tok"), nil
}

func newRig(t *testing.T) *rig {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	r := &rig{t: t, database: database, stores: sqlitestore.New(database), pub: &recorder{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Query     string `json:"query"`
			Variables struct {
				IDs []string `json:"ids"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || !strings.Contains(body.Query, "query PRMergeability(") {
			t.Errorf("unexpected request: %s %s %q", req.Method, req.URL.Path, body.Query)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.reads++
		answer, refuse := r.answer, r.refuse
		r.mu.Unlock()
		if refuse != nil {
			refuse(w)
			return
		}
		nodes := make([]map[string]string, len(body.Variables.IDs))
		for i, id := range body.Variables.IDs {
			nodes[i] = map[string]string{"id": id, "mergeable": answer, "headRefOid": "head-" + id, "baseRefName": "main"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes, "rateLimit": map[string]int{"cost": 1}}})
	}))
	t.Cleanup(srv.Close)
	r.resolver = &fixedResolver{base: srv.URL}
	r.worker = NewWorker(r.stores.PRMergeability, r.stores.Orgs, r.resolver, r.pub, workitem.Owner{ID: "test-worker", Epoch: 1})
	return r
}

// pullRequest seeds an open pull request entity in octo/api with a snapshot
// the event metadata is read from.
func (r *rig) pullRequest(number string) *domain.Entity {
	r.t.Helper()
	return r.pullRequestIn("octo/api", number)
}

func (r *rig) pullRequestIn(repo, number string) *domain.Entity {
	r.t.Helper()
	ctx := context.Background()
	e, _, err := r.stores.Entities.FindOrCreateSystem(ctx, runmode.LocalDefaultOrgID, "github", "https://github.com",
		repo+"#"+number, "PR_"+number, domain.EntityKindPullRequest, "PR", "")
	if err != nil {
		r.t.Fatalf("seed entity: %v", err)
	}
	snap, _ := json.Marshal(domain.PRSnapshot{Number: 1, Author: "aidan", Repo: "octo/api", State: "OPEN", HeadSHA: "stored"})
	if ok, err := r.stores.Entities.UpdateSnapshotCASSystem(ctx, runmode.LocalDefaultOrgID, e.ID, string(snap), e.PollSeq); err != nil || !ok {
		r.t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}
	return e
}

// admit queues a probe for the pull request.
func (r *rig) admit(entityID, reason string) {
	r.t.Helper()
	if _, err := r.stores.PRMergeability.AdmitSystem(context.Background(), runmode.LocalDefaultOrgID,
		[]db.PRMergeabilityProbe{{EntityID: entityID, Reason: reason}}); err != nil {
		r.t.Fatalf("AdmitSystem: %v", err)
	}
}

// drain ripens the ready probes, standing in for the admission delay, and
// runs one worker pass with GitHub giving answer, returning the events it
// published.
func (r *rig) drain(answer string) []domain.Event {
	r.t.Helper()
	r.mu.Lock()
	r.answer = answer
	r.mu.Unlock()
	if _, err := r.database.Exec(`UPDATE pr_mergeability_probes SET probe_after = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'ready'`); err != nil {
		r.t.Fatalf("ripen: %v", err)
	}
	if err := r.worker.Drain(context.Background()); err != nil {
		r.t.Fatalf("Drain: %v", err)
	}
	return r.pub.take()
}

func (r *rig) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

type probeRow struct {
	reason, status, outcome string
	attempt                 int
}

func (r *rig) probes(entityID string) []probeRow {
	r.t.Helper()
	rows, err := r.database.Query(`SELECT reason, status, COALESCE(last_outcome, ''), attempt FROM pr_mergeability_probes WHERE entity_id = ? ORDER BY id`, entityID)
	if err != nil {
		r.t.Fatalf("read probes: %v", err)
	}
	defer rows.Close()
	var out []probeRow
	for rows.Next() {
		var p probeRow
		if err := rows.Scan(&p.reason, &p.status, &p.outcome, &p.attempt); err != nil {
			r.t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func types(evts []domain.Event) []string {
	out := make([]string, len(evts))
	for i, e := range evts {
		out[i] = e.EventType
	}
	return out
}

func TestWorker_ANewPullRequestThatConflictsEmitsConflictsOnce(t *testing.T) {
	r := newRig(t)
	pr := r.pullRequest("1")
	r.admit(pr.ID, domain.MergeabilityProbeNew)

	evts := r.drain(domain.MergeableConflicting)
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRConflicts || evts[0].ID == "" {
		t.Fatalf("published %v, want one enqueued github:pr:conflicts", types(evts))
	}
	var meta events.GitHubPRConflictsMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil || meta.HeadSHA != "head-PR_1" || meta.Author != "aidan" || meta.RepositoryID != pr.RepositoryID {
		t.Errorf("metadata = %+v (err %v), want the read's head and the snapshot's author", meta, err)
	}
	if row, err := r.stores.PRMergeability.GetSystem(context.Background(), runmode.LocalDefaultOrgID, pr.ID); err != nil || row == nil ||
		row.MergeableHeadSHA != "head-PR_1" || row.MergeableBaseRef != "main" {
		t.Errorf("row = %+v (err %v), want the answer recorded at the read's head and base", row, err)
	}

	r.admit(pr.ID, domain.MergeabilityProbeSweep)
	if again := r.drain(domain.MergeableConflicting); len(again) != 0 {
		t.Errorf("a second CONFLICTING read published %v, want nothing", types(again))
	}
}

func TestWorker_ConflictsResolvedOnceThenARepeatedMergeableEmitsNothing(t *testing.T) {
	r := newRig(t)
	pr := r.pullRequest("2")
	r.admit(pr.ID, domain.MergeabilityProbeNew)
	r.drain(domain.MergeableConflicting)

	r.admit(pr.ID, domain.MergeabilityProbeHeadMoved)
	if evts := r.drain(domain.MergeableMergeable); len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRConflictsResolved {
		t.Fatalf("published %v, want one github:pr:conflicts_resolved", types(evts))
	}
	r.admit(pr.ID, domain.MergeabilityProbeSweep)
	if evts := r.drain(domain.MergeableMergeable); len(evts) != 0 {
		t.Errorf("a repeated MERGEABLE published %v, want nothing", types(evts))
	}
}

func TestWorker_UnknownRetriesUntilTheLimitThenStops(t *testing.T) {
	r := newRig(t)
	pr := r.pullRequest("3")
	r.admit(pr.ID, domain.MergeabilityProbeNew)
	for i := 0; i < workkinds.PRMergeabilityMaxUnknownReads+2; i++ {
		if evts := r.drain(domain.MergeableUnknown); len(evts) != 0 {
			t.Fatalf("an UNKNOWN read published %v", types(evts))
		}
	}
	if n := r.readCount(); n != workkinds.PRMergeabilityMaxUnknownReads {
		t.Errorf("GitHub was read %d times, want %d", n, workkinds.PRMergeabilityMaxUnknownReads)
	}
	probes := r.probes(pr.ID)
	if len(probes) != workkinds.PRMergeabilityMaxUnknownReads {
		t.Fatalf("%d probes, want %d: %+v", len(probes), workkinds.PRMergeabilityMaxUnknownReads, probes)
	}
	for i, p := range probes {
		want := domain.MergeabilityProbeUnknownRetry
		if i == 0 {
			want = domain.MergeabilityProbeNew
		}
		if p.reason != want || p.status != workitem.StatusDone {
			t.Errorf("probe %d = %+v, want %s and done", i, p, want)
		}
	}
	row, err := r.stores.PRMergeability.GetSystem(context.Background(), runmode.LocalDefaultOrgID, pr.ID)
	if err != nil || row == nil || row.Mergeable != "" || row.MergeableHeadSHA != "head-PR_3" || row.CheckedAt == nil {
		t.Errorf("row = %+v (err %v), want the give-up's read recorded with no answer", row, err)
	}
}

func TestWorker_AnUnresolvableClientFailsTheProbeAsDependencyDown(t *testing.T) {
	r := newRig(t)
	pr := r.pullRequest("4")
	r.admit(pr.ID, domain.MergeabilityProbeNew)
	r.resolver.err = errors.New("the secret store is unreachable")
	r.drain(domain.MergeableConflicting)

	probes := r.probes(pr.ID)
	if len(probes) != 1 || probes[0].status != workitem.StatusReady || probes[0].outcome != string(workitem.OutcomeDependencyDown) {
		t.Errorf("probes = %+v, want the one probe back to ready as dependency_down", probes)
	}
	if n := r.readCount(); n != 0 {
		t.Errorf("GitHub was read %d times without a client", n)
	}
}

// A repository no credential reaches stays that way until someone changes
// the configuration, so its probes are dropped on the first try rather than
// retried, and the next poll admits them again.
func TestWorker_AProbeWhoseRepositoryNoCredentialReachesIsDroppedAtOnce(t *testing.T) {
	r := newRig(t)
	pr := r.pullRequest("5")
	r.admit(pr.ID, domain.MergeabilityProbeNew)
	r.resolver.err = fmt.Errorf("%w: octo/api not in the app installation grant", ghclient.ErrNoGitHubCredentials)
	r.drain(domain.MergeableConflicting)

	if probes := r.probes(pr.ID); len(probes) != 1 || probes[0].status != workitem.StatusDone || probes[0].attempt != 1 {
		t.Errorf("probes = %+v, want the one probe done on its first attempt", probes)
	}
	if n := r.readCount(); n != 0 {
		t.Errorf("GitHub was read %d times without a client", n)
	}
}

func TestWorker_PullRequestsInTwoRepositoriesOnOneCredentialAreReadInOneQuery(t *testing.T) {
	r := newRig(t)
	api, web := r.pullRequestIn("octo/api", "5"), r.pullRequestIn("octo/web", "6")
	r.admit(api.ID, domain.MergeabilityProbeNew)
	r.admit(web.ID, domain.MergeabilityProbeNew)

	if evts := r.drain(domain.MergeableConflicting); len(evts) != 2 {
		t.Fatalf("published %v, want a github:pr:conflicts for each pull request", types(evts))
	}
	if n := r.readCount(); n != 1 {
		t.Errorf("GitHub was read %d times, want one query for both repositories", n)
	}
	if !slices.Equal(r.resolver.repos, []string{"octo/api", "octo/web"}) && !slices.Equal(r.resolver.repos, []string{"octo/web", "octo/api"}) {
		t.Errorf("resolved %v, want a client resolved for each repository", r.resolver.repos)
	}
}

// spentGraphQLBudget sets the headers of a response reporting the GraphQL
// budget spent until reset.
func spentGraphQLBudget(w http.ResponseWriter, reset time.Time) {
	w.Header().Set("X-RateLimit-Resource", "graphql")
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
}

// Every shape GitHub refuses a read in for a rate limit defers the probe,
// whether or not the wait it names is one the client would sleep through
// without a deadline: the read's own deadline is shorter than all of them.
func TestWorker_ARateLimitDefersTheProbesAndHoldsTheCredential(t *testing.T) {
	refusals := map[string]func(w http.ResponseWriter){
		"a spent budget resetting in an hour": func(w http.ResponseWriter) {
			spentGraphQLBudget(w, time.Now().Add(time.Hour))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		},
		"a spent budget resetting after the read's deadline": func(w http.ResponseWriter) {
			spentGraphQLBudget(w, time.Now().Add(2*time.Minute))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		},
		"a secondary limit's Retry-After": func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
		},
		"a RATE_LIMITED error in a 200": func(w http.ResponseWriter) {
			spentGraphQLBudget(w, time.Now().Add(time.Hour))
			_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		},
	}
	for name, refuse := range refusals {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			first := r.pullRequest("7")
			r.admit(first.ID, domain.MergeabilityProbeNew)
			r.mu.Lock()
			r.refuse = refuse
			r.mu.Unlock()

			start := time.Now()
			r.drain(domain.MergeableConflicting)
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Errorf("the drain took %v, want the refusal returned rather than waited out", elapsed)
			}
			if n := r.readCount(); n != 1 {
				t.Fatalf("GitHub was read %d times, want the one refused read", n)
			}
			if p := r.probes(first.ID); len(p) != 1 || p[0].status != workitem.StatusReady || p[0].outcome != "deferred" || p[0].attempt != 0 {
				t.Errorf("probes = %+v, want the probe deferred with its attempt refunded", p)
			}

			// Another pull request on the same credential waits for the reset
			// too, without a request of its own.
			second := r.pullRequest("8")
			r.admit(second.ID, domain.MergeabilityProbeNew)
			r.drain(domain.MergeableConflicting)
			if n := r.readCount(); n != 1 {
				t.Errorf("GitHub was read %d times, want no read on a rate-limited credential", n)
			}
			if p := r.probes(second.ID); len(p) != 1 || p[0].status != workitem.StatusReady || p[0].outcome != "deferred" || p[0].attempt != 0 {
				t.Errorf("probes = %+v, want the second probe deferred unread", p)
			}
		})
	}
}

// A panic completing one probe of a batch returns that probe alone: the ones
// completed after it are recorded as if nothing had happened.
func TestWorker_APanicCompletingOneProbeLeavesTheRestOfTheBatchCompleted(t *testing.T) {
	r := newRig(t)
	a, b := r.pullRequest("11"), r.pullRequest("12")
	r.admit(a.ID, domain.MergeabilityProbeNew)
	r.admit(b.ID, domain.MergeabilityProbeNew)
	r.pub.mu.Lock()
	r.pub.panicNext = true
	r.pub.mu.Unlock()

	published := r.drain(domain.MergeableConflicting)

	// The panic came after the first answer committed, so both probes are
	// done, and only the second answer's event reached the publisher.
	for _, e := range []*domain.Entity{a, b} {
		if p := r.probes(e.ID); len(p) != 1 || p[0].status != workitem.StatusDone {
			t.Errorf("probes of %s = %+v, want one done probe", e.ID, p)
		}
	}
	if len(published) != 1 || published[0].EventType != domain.EventGitHubPRConflicts {
		t.Errorf("published %+v, want the one github:pr:conflicts the panic did not interrupt", published)
	}
}

// ripenRetries stands in for the backoff after a failed attempt.
func (r *rig) ripenRetries() {
	r.t.Helper()
	if _, err := r.database.Exec(`UPDATE pr_mergeability_probes SET next_attempt_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'ready'`); err != nil {
		r.t.Fatalf("ripen retry: %v", err)
	}
}

func TestWorker_AProbeWhoseReadsKeepFailingIsDroppedNotParked(t *testing.T) {
	r := newRig(t)
	pr := r.pullRequest("9")
	r.admit(pr.ID, domain.MergeabilityProbeNew)
	r.resolver.err = errors.New("the secret store is unreachable")
	for i := 1; i <= workkinds.PRMergeabilityMaxFailedReads; i++ {
		r.ripenRetries()
		r.drain(domain.MergeableConflicting)
		if p := r.probes(pr.ID); i < workkinds.PRMergeabilityMaxFailedReads && (len(p) != 1 || p[0].status != workitem.StatusReady) {
			t.Fatalf("after failed attempt %d: probes = %+v, want the probe back to ready", i, p)
		}
	}
	probes := r.probes(pr.ID)
	if len(probes) != 1 || probes[0].status != workitem.StatusDone {
		t.Fatalf("probes = %+v, want the one probe done after its last failed attempt, not parked", probes)
	}

	// Its key is free, so the next poll's admission is a fresh probe that
	// reads once the dependency is back.
	r.admit(pr.ID, domain.MergeabilityProbeNew)
	r.resolver.err = nil
	if evts := r.drain(domain.MergeableConflicting); len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRConflicts {
		t.Errorf("published %v after the dependency recovered, want one github:pr:conflicts", types(evts))
	}
}

// A probe whose lease ran out on its last read, with no terminal write (its
// holder died mid-read, say), is claimed again rather than parked, and its
// next failed read drops it.
func TestWorker_AProbeWhoseLeaseLapsedOnItsLastReadIsDroppedNotParked(t *testing.T) {
	r := newRig(t)
	pr := r.pullRequest("13")
	r.admit(pr.ID, domain.MergeabilityProbeNew)
	r.resolver.err = errors.New("the secret store is unreachable")
	for i := 1; i < workkinds.PRMergeabilityMaxFailedReads; i++ {
		r.ripenRetries()
		r.drain(domain.MergeableConflicting)
	}

	// The last read's holder claims it and dies before writing anything.
	r.ripenRetries()
	ctx := context.Background()
	claim, err := r.stores.PRMergeability.Claim(ctx, workitem.Owner{ID: "dead-worker", Epoch: 1}, runmode.LocalDefaultOrgID, 10)
	if err != nil || len(claim.Items) != 1 || claim.Items[0].Receipt.Attempt != workkinds.PRMergeabilityMaxFailedReads {
		t.Fatalf("claim = %+v, %v; want the probe on its last read", claim, err)
	}
	if _, err := r.database.Exec(`UPDATE pr_mergeability_probes SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'leased'`); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	r.drain(domain.MergeableConflicting)
	if p := r.probes(pr.ID); len(p) != 1 || p[0].status != workitem.StatusDone || p[0].attempt != workkinds.PRMergeabilityMaxFailedReads+1 {
		t.Errorf("probes = %+v, want the probe reclaimed and dropped, not parked", p)
	}
}
