package mergeability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	// rateLimited makes GitHub refuse every read for a spent GraphQL budget
	// that resets in an hour.
	rateLimited bool
	resolver    *fixedResolver
}

type recorder struct {
	mu     sync.Mutex
	events []domain.Event
}

func (r *recorder) PublishPreEnqueued(_ context.Context, evt domain.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
		answer, limited := r.answer, r.rateLimited
		r.mu.Unlock()
		if limited {
			w.Header().Set("X-RateLimit-Resource", "graphql")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		nodes := make([]map[string]string, len(body.Variables.IDs))
		for i, id := range body.Variables.IDs {
			nodes[i] = map[string]string{"id": id, "mergeable": answer, "headRefOid": "head-" + id}
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
	r.resolver.err = errors.New("no credential")
	r.drain(domain.MergeableConflicting)

	probes := r.probes(pr.ID)
	if len(probes) != 1 || probes[0].status != workitem.StatusReady || probes[0].outcome != string(workitem.OutcomeDependencyDown) {
		t.Errorf("probes = %+v, want the one probe back to ready as dependency_down", probes)
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

func TestWorker_ARateLimitDefersTheProbesAndHoldsTheCredential(t *testing.T) {
	r := newRig(t)
	first := r.pullRequest("7")
	r.admit(first.ID, domain.MergeabilityProbeNew)
	r.mu.Lock()
	r.rateLimited = true
	r.mu.Unlock()

	r.drain(domain.MergeableConflicting)
	if n := r.readCount(); n != 1 {
		t.Fatalf("GitHub was read %d times, want the one refused read", n)
	}
	if p := r.probes(first.ID); len(p) != 1 || p[0].status != workitem.StatusReady || p[0].outcome != "deferred" || p[0].attempt != 0 {
		t.Errorf("probes = %+v, want the probe deferred with its attempt refunded", p)
	}

	// Another pull request on the same credential waits for the reset too,
	// without a request of its own.
	second := r.pullRequest("8")
	r.admit(second.ID, domain.MergeabilityProbeNew)
	r.drain(domain.MergeableConflicting)
	if n := r.readCount(); n != 1 {
		t.Errorf("GitHub was read %d times, want no read on a rate-limited credential", n)
	}
	if p := r.probes(second.ID); len(p) != 1 || p[0].status != workitem.StatusReady || p[0].outcome != "deferred" || p[0].attempt != 0 {
		t.Errorf("probes = %+v, want the second probe deferred unread", p)
	}
}

func TestWorker_AProbeWhoseReadsKeepFailingIsDroppedNotParked(t *testing.T) {
	r := newRig(t)
	pr := r.pullRequest("9")
	r.admit(pr.ID, domain.MergeabilityProbeNew)
	r.resolver.err = errors.New("no credential")
	for i := 0; i < probeKind.Policy.MaxAttempts; i++ {
		// Each failed attempt backs off; the test stands in for the wait.
		if _, err := r.database.Exec(`UPDATE pr_mergeability_probes SET next_attempt_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'ready'`); err != nil {
			t.Fatalf("ripen retry: %v", err)
		}
		r.drain(domain.MergeableConflicting)
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
