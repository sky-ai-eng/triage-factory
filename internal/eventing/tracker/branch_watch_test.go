package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// watchWorld is an org tracking octo/repo with three open pull requests on
// main, PR_1..PR_3 at heads h1..h3. Each one's stored snapshot is current and
// its mergeability was read at its head just now, so a cycle refreshes nothing
// and admits nothing but the branch watch's probes. The fake GitHub answers
// main's tip, the compares and the changed paths the test sets.
type watchWorld struct {
	t      *testing.T
	db     *sql.DB
	stores db.Stores
	tr     *Tracker
	client *ghclient.Client
	repoID string
	prs    []domain.Entity

	mu sync.Mutex
	// tip is main's tip.
	tip string
	// listing304 makes the open-PR listing answer 304 to its stored ETag.
	listing304 bool
	// refFails makes main's ref read answer 404, as GitHub does to a
	// credential that cannot read the repository's contents.
	refFails bool
	// compares maps "base...head" to a compare body, or to "404", "422" or
	// "rate-limited".
	compares map[string]string
	// paths maps a node id to its PRChangedPaths node; a pull request with
	// none answers that it changes nothing, at its head.
	paths map[string]string
	// previous maps a pull request number to its REST files listing, or to
	// "rate-limited".
	previous map[int]string
	// refReads, compareReads and pathReads record the watch's requests:
	// each ref read, each compare's base...head, and each node id whose
	// paths were asked for.
	refReads     int
	compareReads []string
	pathReads    []string
}

func newWatchWorld(t *testing.T) *watchWorld {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	w := &watchWorld{t: t, db: newMigratedSQLite(t), tip: "old", compares: map[string]string{}, paths: map[string]string{}, previous: map[int]string{}}
	w.stores = sqlitestore.New(w.db)
	srv := httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(srv.Close)
	w.client = ghclient.NewClient(srv.URL, "tok")

	trackRepos(t, w.stores, org, []string{"octo/repo"})
	for n := 1; n <= 3; n++ {
		e, _, err := w.stores.Entities.FindOrCreateSystem(ctx, org, "github", cursorTestHost,
			fmt.Sprintf("octo/repo#%d", n), fmt.Sprintf("PR_%d", n), "pr", "PR", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		raw, _ := json.Marshal(domain.PRSnapshot{
			NodeID: e.ExternalID, Number: n, Title: "PR", Author: "alice", Repo: "octo/repo",
			State: "OPEN", HeadRef: "feature", BaseRef: "main", HeadSHA: fmt.Sprintf("h%d", n),
			CreatedAt: "2026-05-20T10:00:00Z", UpdatedAt: "2026-05-29T12:00:00Z", CheckRuns: []domain.CheckRun{},
		})
		if ok, err := w.stores.Entities.UpdateSnapshotCASSystem(ctx, org, e.ID, string(raw), e.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		if _, err := w.db.Exec(`INSERT INTO pull_request_mergeability (entity_id, mergeable, mergeable_head_sha, checked_at) VALUES (?, 'MERGEABLE', ?, ?)`,
			e.ID, fmt.Sprintf("h%d", n), time.Now().UTC()); err != nil {
			t.Fatalf("seed answer: %v", err)
		}
		w.prs = append(w.prs, *e)
		w.repoID = e.RepositoryID
	}
	w.tr = New(w.db, &recordingPublisher{}, w.stores.Tasks, w.stores.Entities, w.stores.Repos, w.stores.JiraProjects,
		w.stores.Deliverables, w.stores.ExternalActions, w.stores.EventQueue, w.stores.PRMergeability, org)
	return w
}

func (w *watchWorld) serve(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	switch {
	case path == "/api/graphql":
		w.serveGraphQL(rw, r)
	case path == "/repos/octo/repo/pulls":
		if w.listing304 && r.Header.Get("If-None-Match") == `"listing"` {
			rw.WriteHeader(http.StatusNotModified)
			return
		}
		rw.Header().Set("ETag", `"listing"`)
		var prs []map[string]any
		for n := 1; n <= 3; n++ {
			prs = append(prs, map[string]any{
				"number": n, "node_id": fmt.Sprintf("PR_%d", n), "title": "PR", "state": "open",
				"html_url":   fmt.Sprintf("https://github.com/octo/repo/pull/%d", n),
				"created_at": "2026-05-20T10:00:00Z", "updated_at": "2026-05-29T12:00:00Z",
				"user": map[string]string{"login": "alice"},
				"head": map[string]any{"sha": fmt.Sprintf("h%d", n), "ref": "feature", "repo": map[string]string{"full_name": "octo/repo"}},
				"base": map[string]any{"ref": "main", "repo": map[string]string{"full_name": "octo/repo"}},
			})
		}
		_ = json.NewEncoder(rw).Encode(prs)
	case path == "/repos/octo/repo/git/ref/heads/main":
		w.refReads++
		if w.refFails {
			http.NotFound(rw, r)
			return
		}
		etag := `"ref-` + w.tip + `"`
		if r.Header.Get("If-None-Match") == etag {
			rw.WriteHeader(http.StatusNotModified)
			return
		}
		rw.Header().Set("ETag", etag)
		_, _ = fmt.Fprintf(rw, `{"ref":"refs/heads/main","object":{"sha":%q}}`, w.tip)
	case strings.HasPrefix(path, "/repos/octo/repo/compare/"):
		key := strings.TrimPrefix(path, "/repos/octo/repo/compare/")
		w.compareReads = append(w.compareReads, key)
		body, ok := w.compares[key]
		switch {
		case !ok || body == "404":
			http.NotFound(rw, r)
			return
		case body == "422":
			http.Error(rw, `{"message":"this comparison could not be generated"}`, http.StatusUnprocessableEntity)
			return
		case body == "rate-limited":
			rw.Header().Set("X-RateLimit-Resource", "core")
			rw.Header().Set("X-RateLimit-Remaining", "0")
			rw.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			http.Error(rw, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		_, _ = rw.Write([]byte(body))
	case strings.HasPrefix(path, "/repos/octo/repo/pulls/") && strings.HasSuffix(path, "/files"):
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/octo/repo/pulls/"), "/files"))
		body, ok := w.previous[n]
		if !ok {
			body = `[]`
		}
		if body == "rate-limited" {
			rw.Header().Set("X-RateLimit-Resource", "core")
			rw.Header().Set("X-RateLimit-Remaining", "0")
			rw.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			http.Error(rw, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		_, _ = rw.Write([]byte(body))
	default:
		w.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(rw, r)
	}
}

func (w *watchWorld) serveGraphQL(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string `json:"query"`
		Variables struct {
			IDs []string `json:"ids"`
		} `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !strings.Contains(req.Query, "query PRChangedPaths(") {
		// The refresh of a pull request no listing returned: GitHub no longer
		// answers for it, which leaves its stored snapshot alone.
		nulls := strings.TrimSuffix(strings.Repeat("null,", len(req.Variables.IDs)), ",")
		_, _ = fmt.Fprintf(rw, `{"data":{"nodes":[%s]}}`, nulls)
		return
	}
	nodes := make([]string, len(req.Variables.IDs))
	for i, id := range req.Variables.IDs {
		w.pathReads = append(w.pathReads, id)
		node, ok := w.paths[id]
		if !ok {
			node = fmt.Sprintf(`{"id":%q,"headRefOid":"h%s","baseRefName":"main","changedFiles":0,"files":{"pageInfo":{"hasNextPage":false},"nodes":[]}}`, id, strings.TrimPrefix(id, "PR_"))
		}
		nodes[i] = node
	}
	_, _ = fmt.Fprintf(rw, `{"data":{"nodes":[%s],"rateLimit":{"cost":1}}}`, strings.Join(nodes, ","))
}

// set changes the fake under its lock.
func (w *watchWorld) set(f func(*watchWorld)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

// cycle runs one poll cycle and returns the probes it admitted, by pull
// request number, each a reason. The ready probes are then read as the worker
// would read them (settle), so the next cycle's admissions are its own.
func (w *watchWorld) cycle() map[int]string {
	w.t.Helper()
	before := w.probeCount()
	refreshRepos(w.t, w.tr, w.client, "octo/repo")
	rows, err := w.db.Query(`SELECT entity_id, reason FROM pr_mergeability_probes WHERE id > ? ORDER BY id`, before)
	if err != nil {
		w.t.Fatalf("read probes: %v", err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var entityID, reason string
		if err := rows.Scan(&entityID, &reason); err != nil {
			w.t.Fatalf("scan probe: %v", err)
		}
		for i, e := range w.prs {
			if e.ID == entityID {
				got[i+1] = reason
			}
		}
	}
	w.settle()
	return got
}

// settle claims every ready probe and completes it with its pull request's
// stored answer, as a read that comes after the cycle's base moves does, which
// settles the moves recorded on the rows. A probe someone else holds leased is
// left alone.
func (w *watchWorld) settle() {
	w.t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	if _, err := w.db.Exec(`UPDATE pr_mergeability_probes SET probe_after = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'ready'`); err != nil {
		w.t.Fatalf("ripen probes: %v", err)
	}
	claim, err := w.stores.PRMergeability.Claim(ctx, workitem.Owner{ID: "watch-world", Epoch: 1}, org, 100)
	if err != nil {
		w.t.Fatalf("claim probes: %v", err)
	}
	for _, item := range claim.Items {
		row, err := w.stores.PRMergeability.GetSystem(ctx, org, item.EntityID)
		if err != nil || row == nil {
			w.t.Fatalf("read the answer of %s: %+v (err %v)", item.EntityID, row, err)
		}
		if _, err := w.stores.PRMergeability.CompleteProbe(ctx, item.Receipt, db.PRMergeabilityAnswer{
			Found: true, Mergeable: domain.MergeableMergeable, HeadSHA: row.MergeableHeadSHA, BaseRef: row.MergeableBaseRef,
		}, ""); err != nil {
			w.t.Fatalf("settle probe %d: %v", item.Receipt.ItemID, err)
		}
	}
}

func (w *watchWorld) probeCount() int64 {
	var n sql.NullInt64
	if err := w.db.QueryRow(`SELECT MAX(id) FROM pr_mergeability_probes`).Scan(&n); err != nil {
		w.t.Fatalf("count probes: %v", err)
	}
	return n.Int64
}

// storedTip is main's branch_tips row, or nil.
func (w *watchWorld) storedTip() *domain.BranchTip {
	w.t.Helper()
	tips, err := w.stores.PRMergeability.ListBranchTipsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		w.t.Fatalf("ListBranchTipsSystem: %v", err)
	}
	for _, tip := range tips {
		if tip.Branch == "main" {
			return &tip
		}
	}
	return nil
}

// ageTip moves main's last check back by age.
func (w *watchWorld) ageTip(age time.Duration) {
	w.t.Helper()
	tip := w.storedTip()
	if tip == nil {
		w.t.Fatal("no tip to age")
	}
	tip.ObservedAt = time.Now().Add(-age)
	if _, err := w.stores.PRMergeability.UpsertBranchTipSystem(context.Background(), runmode.LocalDefaultOrgID, *tip); err != nil {
		w.t.Fatalf("age tip: %v", err)
	}
}

// pathsNode is a PRChangedPaths node for PR_n at head, changing files.
func pathsNode(n int, head string, files ...domain.ChangedPath) string {
	nodes := make([]string, len(files))
	for i, f := range files {
		nodes[i] = fmt.Sprintf(`{"path":%q,"changeType":%q}`, f.Path, f.Change)
	}
	return fmt.Sprintf(`{"id":"PR_%d","headRefOid":%q,"baseRefName":"main","changedFiles":%d,"files":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}`,
		n, head, len(files), strings.Join(nodes, ","))
}

func modified(p string) domain.ChangedPath {
	return domain.ChangedPath{Path: p, Change: domain.PathModified}
}

// compareBody is a compare of status listing files, with total commits and
// the commits listed.
func compareBody(status string, total int, commits string, files ...string) string {
	entries := make([]string, len(files))
	for i, f := range files {
		entries[i] = fmt.Sprintf(`{"filename":%q,"status":"modified"}`, f)
	}
	return fmt.Sprintf(`{"status":%q,"total_commits":%d,"commits":[%s],"files":[%s]}`, status, total, commits, strings.Join(entries, ","))
}

// filesOf is n distinct files under dir.
func filesOf(dir string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s/f%d.go", dir, i)
	}
	return out
}

// seenOnce runs the first-sight cycle on tip old and moves main to "new", so
// the next cycle processes the move old...new. PR_1 changes src/a.go, PR_2
// docs/b.md and PR_3 lib/c.go.
func (w *watchWorld) seenOnce() {
	w.t.Helper()
	w.cycle()
	w.set(func(w *watchWorld) {
		w.tip = "new"
		w.paths["PR_1"] = pathsNode(1, "h1", modified("src/a.go"))
		w.paths["PR_2"] = pathsNode(2, "h2", modified("docs/b.md"))
		w.paths["PR_3"] = pathsNode(3, "h3", modified("lib/c.go"))
	})
}

func TestBranchWatch_FirstSightResetsTheBaseOnceAndStoresTheTip(t *testing.T) {
	w := newWatchWorld(t)
	if got := w.cycle(); len(got) != 3 || got[1] != domain.MergeabilityProbeBaseReset || got[2] != domain.MergeabilityProbeBaseReset || got[3] != domain.MergeabilityProbeBaseReset {
		t.Errorf("first sight admitted %v, want base_reset for every pull request on main", got)
	}
	if tip := w.storedTip(); tip == nil || tip.TipSHA != "old" || tip.ETag != `"ref-old"` {
		t.Fatalf("stored tip = %+v, want old with its etag", tip)
	}
	if got := w.cycle(); len(got) != 0 {
		t.Errorf("the next cycle admitted %v, want nothing: the tip did not move", got)
	}
}

func TestBranchWatch_CompareStatuses(t *testing.T) {
	twoSteps := `{"sha":"mid","parents":[{"sha":"old"}]},{"sha":"new","parents":[{"sha":"mid"},{"sha":"side"}]}`
	// chain is a truncated move of n first-parent steps from old to new, each
	// step's compare listing one file, the last step's src/a.go.
	chain := func(n int) map[string]string {
		shas := []string{"old"}
		for i := 1; i < n; i++ {
			shas = append(shas, fmt.Sprintf("c%d", i))
		}
		shas = append(shas, "new")
		commits := make([]string, n)
		out := map[string]string{}
		for i := 1; i <= n; i++ {
			commits[i-1] = fmt.Sprintf(`{"sha":%q,"parents":[{"sha":%q}]}`, shas[i], shas[i-1])
			file := fmt.Sprintf("docs/s%d.md", i)
			if i == n {
				file = "src/a.go"
			}
			out[shas[i-1]+"..."+shas[i]] = compareBody("ahead", 1, "", file)
		}
		out["old...new"] = compareBody("ahead", n, strings.Join(commits, ","), filesOf("gen", ghclient.CompareFileCap)...)
		return out
	}
	cases := []struct {
		name     string
		compares map[string]string
		want     map[int]string
		reads    []string
		// noPaths is a move whose pull requests' paths need not be read.
		noPaths bool
	}{
		{
			name:     "ahead uses the files",
			compares: map[string]string{"old...new": compareBody("ahead", 1, "", "src/a.go", "other/z.go")},
			want:     map[int]string{1: domain.MergeabilityProbeBaseMoved},
			reads:    []string{"old...new"},
		},
		{
			name:     "identical probes nothing",
			compares: map[string]string{"old...new": compareBody("identical", 0, "")},
			want:     map[int]string{},
		},
		{
			name:     "an ahead move that changed no file probes nothing",
			compares: map[string]string{"old...new": compareBody("ahead", 1, "")},
			want:     map[int]string{},
			noPaths:  true,
		},
		{
			name:     "an ahead compare with no file list resets the base",
			compares: map[string]string{"old...new": `{"status":"ahead","total_commits":40,"commits":[]}`},
			want:     map[int]string{1: domain.MergeabilityProbeBaseReset, 2: domain.MergeabilityProbeBaseReset, 3: domain.MergeabilityProbeBaseReset},
		},
		{
			name:     "behind resets the base",
			compares: map[string]string{"old...new": compareBody("behind", 0, "", "src/a.go")},
			want:     map[int]string{1: domain.MergeabilityProbeBaseReset, 2: domain.MergeabilityProbeBaseReset, 3: domain.MergeabilityProbeBaseReset},
		},
		{
			name:     "diverged resets the base",
			compares: map[string]string{"old...new": compareBody("diverged", 2, "", "src/a.go")},
			want:     map[int]string{1: domain.MergeabilityProbeBaseReset, 2: domain.MergeabilityProbeBaseReset, 3: domain.MergeabilityProbeBaseReset},
		},
		{
			name:     "a previous tip that is gone resets the base",
			compares: map[string]string{"old...new": "404"},
			want:     map[int]string{1: domain.MergeabilityProbeBaseReset, 2: domain.MergeabilityProbeBaseReset, 3: domain.MergeabilityProbeBaseReset},
		},
		{
			name: "a truncated list within the commit cap walks the first-parent steps",
			compares: map[string]string{
				"old...new": compareBody("ahead", 2, twoSteps, filesOf("gen", ghclient.CompareFileCap)...),
				"old...mid": compareBody("ahead", 1, "", "docs/b.md"),
				"mid...new": compareBody("ahead", 1, "", "gen/x.go"),
			},
			want:  map[int]string{2: domain.MergeabilityProbeBaseMoved},
			reads: []string{"old...new", "mid...new", "old...mid"},
		},
		{
			name:     "a truncated list over the commit cap resets the base",
			compares: map[string]string{"old...new": compareBody("ahead", ghclient.CompareCommitCap+1, twoSteps, filesOf("gen", ghclient.CompareFileCap)...)},
			want:     map[int]string{1: domain.MergeabilityProbeBaseReset, 2: domain.MergeabilityProbeBaseReset, 3: domain.MergeabilityProbeBaseReset},
			reads:    []string{"old...new"},
		},
		{
			name:     "a walk of maxMoveSteps steps uses their files",
			compares: chain(maxMoveSteps),
			want:     map[int]string{1: domain.MergeabilityProbeBaseMoved},
		},
		{
			name:     "a walk longer than maxMoveSteps resets the base",
			compares: chain(maxMoveSteps + 1),
			want:     map[int]string{1: domain.MergeabilityProbeBaseReset, 2: domain.MergeabilityProbeBaseReset, 3: domain.MergeabilityProbeBaseReset},
		},
		{
			name: "a truncated step resets the base",
			compares: map[string]string{
				"old...new": compareBody("ahead", 2, twoSteps, filesOf("gen", ghclient.CompareFileCap)...),
				"mid...new": compareBody("ahead", 1, "", filesOf("gen", ghclient.CompareFileCap)...),
			},
			want:  map[int]string{1: domain.MergeabilityProbeBaseReset, 2: domain.MergeabilityProbeBaseReset, 3: domain.MergeabilityProbeBaseReset},
			reads: []string{"old...new", "mid...new"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWatchWorld(t)
			w.seenOnce()
			w.set(func(w *watchWorld) { w.compares = c.compares })
			if got := w.cycle(); !mapsEqual(got, c.want) {
				t.Errorf("admitted %v, want %v", got, c.want)
			}
			if c.reads != nil && !slices.Equal(w.compareReads, c.reads) {
				t.Errorf("compares read = %v, want %v", w.compareReads, c.reads)
			}
			if c.noPaths && len(w.pathReads) != 0 {
				t.Errorf("changed paths read for %v, want none", w.pathReads)
			}
			if tip := w.storedTip(); tip == nil || tip.TipSHA != "new" {
				t.Errorf("stored tip = %+v, want new", tip)
			}
		})
	}
}

func mapsEqual(a, b map[int]string) bool {
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

func TestBranchWatch_AListingThatAnswered304WaitsForTheFloor(t *testing.T) {
	w := newWatchWorld(t)
	w.set(func(w *watchWorld) { w.listing304 = true })
	w.cycle()
	if w.refReads != 1 {
		t.Fatalf("first cycle read main's tip %d times, want once", w.refReads)
	}

	w.cycle()
	if w.refReads != 1 {
		t.Errorf("a cycle whose listing answered 304 read a tip checked just now")
	}
	w.ageTip(staleRefreshFloor + time.Minute)
	w.cycle()
	if w.refReads != 2 {
		t.Errorf("a cycle whose listing answered 304 left a tip last checked past the floor unchecked")
	}

	w.set(func(w *watchWorld) { w.listing304 = false })
	w.cycle()
	if w.refReads != 3 {
		t.Errorf("a cycle whose listing answered 200 left main unchecked")
	}
}

// failingMark refuses the branch watch's records of a base move while fail
// is set.
type failingMark struct {
	db.PRMergeabilityStore
	fail bool
}

func (f *failingMark) MarkBaseMovedSystem(ctx context.Context, orgID string, entityIDs []string, reason string) (int, error) {
	if f.fail {
		return 0, errors.New("write refused")
	}
	return f.PRMergeabilityStore.MarkBaseMovedSystem(ctx, orgID, entityIDs, reason)
}

func TestBranchWatch_TheTipIsStoredOnlyAfterTheMoveIsRecorded(t *testing.T) {
	w := newWatchWorld(t)
	store := &failingMark{PRMergeabilityStore: w.stores.PRMergeability}
	w.tr.mergeability = store
	w.seenOnce()
	w.set(func(w *watchWorld) { w.compares["old...new"] = compareBody("ahead", 1, "", "src/a.go") })

	store.fail = true
	if got := w.cycle(); len(got) != 0 {
		t.Fatalf("admitted %v through a refusing store", got)
	}
	if tip := w.storedTip(); tip == nil || tip.TipSHA != "old" {
		t.Fatalf("stored tip after a failed record = %+v, want old", tip)
	}

	store.fail = false
	if got := w.cycle(); !mapsEqual(got, map[int]string{1: domain.MergeabilityProbeBaseMoved}) {
		t.Errorf("the next cycle admitted %v, want the move processed again", got)
	}
	if tip := w.storedTip(); tip == nil || tip.TipSHA != "new" {
		t.Errorf("stored tip = %+v, want new", tip)
	}
}

// A probe a worker was reading when the move was recorded may have read
// GitHub before it, so its answer leaves the move owed and the next cycle
// admits a fresh probe, with no second look at the branch.
func TestBranchWatch_AMoveWhoseProbeWasBeingReadIsProbedAgain(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	w := newWatchWorld(t)
	w.seenOnce()
	w.set(func(w *watchWorld) { w.compares["old...new"] = compareBody("ahead", 1, "", "src/a.go") })

	if _, err := w.stores.PRMergeability.AdmitSystem(ctx, org, []db.PRMergeabilityProbe{{EntityID: w.prs[0].ID, Reason: domain.MergeabilityProbeSweep}}); err != nil {
		t.Fatalf("AdmitSystem: %v", err)
	}
	if _, err := w.db.Exec(`UPDATE pr_mergeability_probes SET probe_after = strftime('%Y-%m-%d %H:%M:%f','now','-1 seconds') WHERE status = 'ready'`); err != nil {
		t.Fatalf("ripen: %v", err)
	}
	claim, err := w.stores.PRMergeability.Claim(ctx, workitem.Owner{ID: "reader", Epoch: 1}, org, 10)
	if err != nil || len(claim.Items) != 1 {
		t.Fatalf("Claim = %d items (err %v), want PR_1's probe", len(claim.Items), err)
	}
	// Apart on SQLite's millisecond clock from the record the cycle writes.
	time.Sleep(5 * time.Millisecond)

	if got := w.cycle(); len(got) != 0 {
		t.Fatalf("admitted %v, want PR_1's probe collapsed into the one being read", got)
	}
	if tip := w.storedTip(); tip == nil || tip.TipSHA != "new" {
		t.Fatalf("stored tip = %+v, want new: the move is recorded on PR_1", tip)
	}

	if _, err := w.stores.PRMergeability.CompleteProbe(ctx, claim.Items[0].Receipt,
		db.PRMergeabilityAnswer{Found: true, Mergeable: domain.MergeableMergeable, HeadSHA: "h1"}, ""); err != nil {
		t.Fatalf("CompleteProbe: %v", err)
	}
	reads := len(w.compareReads)
	if got := w.cycle(); !mapsEqual(got, map[int]string{1: domain.MergeabilityProbeBaseMoved}) {
		t.Errorf("the next cycle admitted %v, want a fresh base_moved for PR_1", got)
	}
	if len(w.compareReads) != reads {
		t.Errorf("the next cycle compared %v again, want the owed move read off PR_1's row", w.compareReads[reads:])
	}
	if got := w.cycle(); len(got) != 0 {
		t.Errorf("a cycle after the fresh read admitted %v, want nothing", got)
	}
}

// A compare that keeps failing is tried again, and the base is reset once the
// failures have gone on for unreadableMoveAfter, counted from the first one: a
// branch last checked long ago fails once without being reset. The reset
// clears the base's stored paths, as the other resets that can rewind it do.
func TestBranchWatch_ACompareThatKeepsFailingResetsTheBase(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	w := newWatchWorld(t)
	w.seenOnce()
	if _, err := w.stores.PRMergeability.UpsertPathsSystem(ctx, org, domain.PRPaths{
		EntityID: w.prs[0].ID, Paths: []domain.ChangedPath{modified("src/a.go")}, HeadSHA: "h1", BaseRef: "main", Complete: true,
	}); err != nil {
		t.Fatalf("seed paths: %v", err)
	}
	w.ageTip(3 * unreadableMoveAfter)
	w.set(func(w *watchWorld) { w.compares["old...new"] = "422" })
	if got := w.cycle(); len(got) != 0 {
		t.Fatalf("a first failed compare admitted %v, want nothing yet", got)
	}
	tip := w.storedTip()
	if tip == nil || tip.TipSHA != "old" || tip.CompareFailingSince == nil || time.Since(*tip.CompareFailingSince) > time.Minute {
		t.Fatalf("stored tip = %+v, want old, failing since now", tip)
	}

	failing := time.Now().Add(-unreadableMoveAfter - time.Minute)
	tip.CompareFailingSince = &failing
	if _, err := w.stores.PRMergeability.UpsertBranchTipSystem(ctx, org, *tip); err != nil {
		t.Fatalf("age the failure: %v", err)
	}
	all := map[int]string{1: domain.MergeabilityProbeBaseReset, 2: domain.MergeabilityProbeBaseReset, 3: domain.MergeabilityProbeBaseReset}
	if got := w.cycle(); !mapsEqual(got, all) {
		t.Errorf("admitted %v, want %v", got, all)
	}
	if tip := w.storedTip(); tip == nil || tip.TipSHA != "new" || tip.CompareFailingSince != nil {
		t.Errorf("stored tip = %+v, want new with no failure", tip)
	}
	rows, err := w.stores.PRMergeability.ListPathsSystem(ctx, org, []string{w.prs[0].ID})
	if err != nil || len(rows) != 1 || rows[0].Paths != nil {
		t.Errorf("PR_1's paths after the reset = %+v (err %v), want cleared", rows, err)
	}
}

// withWatchMetrics points the branch watch's instruments at a reader the test
// collects, for the test's duration.
func withWatchMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := stats.Swap(newInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { stats.Store(prev) })
	return reader
}

// readFailures is mergeability.base_read_failures by read, as reader last
// collected it.
func readFailures(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != baseReadFailuresMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				read, _ := dp.Attributes.Value(attribute.Key("read"))
				out[read.AsString()] = dp.Value
			}
		}
	}
	return out
}

// Every read the watch could not make is counted by what it read, except a
// rate limit, which is waited out: a ref read that fails leaves the move
// unprocessed, and the count is how an operator learns of it.
func TestBranchWatch_FailedReadsAreCounted(t *testing.T) {
	reader := withWatchMetrics(t)
	w := newWatchWorld(t)
	w.seenOnce()

	w.set(func(w *watchWorld) { w.refFails = true })
	if got := w.cycle(); len(got) != 0 {
		t.Fatalf("a failed ref read admitted %v, want nothing", got)
	}
	if tip := w.storedTip(); tip == nil || tip.TipSHA != "old" {
		t.Fatalf("stored tip = %+v, want old: the move is not processed", tip)
	}
	w.set(func(w *watchWorld) {
		w.refFails = false
		w.compares["old...new"] = "422"
	})
	w.cycle()
	if got, want := readFailures(t, reader), map[string]int64{baseReadTip: 1, baseReadCompare: 1}; !maps.Equal(got, want) {
		t.Errorf("read failures = %v, want %v", got, want)
	}

	// GitHub cannot list PR_1's files: it is probed, and its list is not
	// stored, so the next move reads it again.
	w.set(func(w *watchWorld) {
		w.compares["old...new"] = compareBody("ahead", 1, "", "src/a.go")
		w.paths["PR_1"] = `{"id":"PR_1","headRefOid":"h1","baseRefName":"main","changedFiles":1,"files":null}`
	})
	want := map[int]string{1: domain.MergeabilityProbeBaseMoved}
	if got := w.cycle(); !mapsEqual(got, want) {
		t.Errorf("admitted %v, want %v", got, want)
	}
	if got := readFailures(t, reader)[baseReadPaths]; got != 1 {
		t.Errorf("paths read failures = %d, want 1", got)
	}
	rows, err := w.stores.PRMergeability.ListPathsSystem(context.Background(), runmode.LocalDefaultOrgID, []string{w.prs[0].ID})
	if err != nil || len(rows) != 1 || rows[0].Paths != nil {
		t.Errorf("PR_1's stored paths = %+v (err %v), want none", rows, err)
	}

	// Last, since the client then refuses every request until the limit
	// resets.
	w.set(func(w *watchWorld) {
		w.tip = "newer"
		w.compares["new...newer"] = "rate-limited"
	})
	w.cycle()
	if !slices.Contains(w.compareReads, "new...newer") {
		t.Fatalf("compares sent %v, want new...newer among them", w.compareReads)
	}
	if got, want := readFailures(t, reader), map[string]int64{baseReadTip: 1, baseReadCompare: 1, baseReadPaths: 1}; !maps.Equal(got, want) {
		t.Errorf("read failures = %v, want %v: a rate limit is not one", got, want)
	}
}

// A rate-limited compare is waited out: it neither starts the failure clock
// nor resets the base, however long ago the branch was last checked.
func TestBranchWatch_ARateLimitedCompareIsWaitedOut(t *testing.T) {
	w := newWatchWorld(t)
	w.seenOnce()
	w.ageTip(3 * unreadableMoveAfter)
	w.set(func(w *watchWorld) { w.compares["old...new"] = "rate-limited" })
	if got := w.cycle(); len(got) != 0 {
		t.Fatalf("a rate-limited compare admitted %v, want nothing", got)
	}
	if tip := w.storedTip(); tip == nil || tip.TipSHA != "old" || tip.CompareFailingSince != nil {
		t.Errorf("stored tip = %+v, want old with no failure recorded", tip)
	}
}

// A rewrite can move a pull request's merge base back at an unchanged head,
// so the reset clears the base's stored lists; a move that only went forward
// keeps them.
func TestBranchWatch_ARewriteClearsTheStoredPaths(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	for _, c := range []struct {
		name    string
		compare string
		cleared bool
	}{
		{"rewritten", compareBody("diverged", 2, "", "src/a.go"), true},
		{"previous tip gone", "404", true},
		{"truncated", compareBody("ahead", ghclient.CompareCommitCap+1, "", filesOf("gen", ghclient.CompareFileCap)...), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWatchWorld(t)
			w.seenOnce()
			if _, err := w.stores.PRMergeability.UpsertPathsSystem(ctx, org, domain.PRPaths{
				EntityID: w.prs[0].ID, Paths: []domain.ChangedPath{modified("src/a.go")}, HeadSHA: "h1", BaseRef: "main", Complete: true,
			}); err != nil {
				t.Fatalf("seed paths: %v", err)
			}
			w.set(func(w *watchWorld) { w.compares["old...new"] = c.compare })
			w.cycle()
			rows, err := w.stores.PRMergeability.ListPathsSystem(ctx, org, []string{w.prs[0].ID})
			if err != nil || len(rows) != 1 {
				t.Fatalf("ListPathsSystem = %+v (err %v)", rows, err)
			}
			if cleared := rows[0].Paths == nil; cleared != c.cleared {
				t.Errorf("paths after the reset = %+v, want cleared %v", rows[0], c.cleared)
			}
		})
	}
}

func TestBranchWatch_StalePathsAreReadAgainAndIncompleteOnesProbed(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	w := newWatchWorld(t)
	w.seenOnce()
	write := func(i int, head string, complete bool, paths ...domain.ChangedPath) {
		t.Helper()
		if _, err := w.stores.PRMergeability.UpsertPathsSystem(ctx, org, domain.PRPaths{EntityID: w.prs[i].ID, Paths: paths, HeadSHA: head, BaseRef: "main", Complete: complete}); err != nil {
			t.Fatalf("seed paths: %v", err)
		}
	}
	// PR_1's list was read at an earlier head and names another file; its
	// current list touches the move. PR_2's is current and does not. PR_3's
	// is current and incomplete.
	write(0, "h1-earlier", true, modified("docs/old.md"))
	write(1, "h2", true, modified("docs/b.md"))
	write(2, "h3", false, modified("lib/c.go"))
	w.set(func(w *watchWorld) { w.compares["old...new"] = compareBody("ahead", 1, "", "src/a.go") })

	want := map[int]string{1: domain.MergeabilityProbeBaseMoved, 3: domain.MergeabilityProbeBaseMoved}
	if got := w.cycle(); !mapsEqual(got, want) {
		t.Errorf("admitted %v, want %v", got, want)
	}
	if !slices.Equal(w.pathReads, []string{"PR_1"}) {
		t.Errorf("paths read for %v, want PR_1's alone", w.pathReads)
	}
	rows, err := w.stores.PRMergeability.ListPathsSystem(ctx, org, []string{w.prs[0].ID})
	if err != nil || len(rows) != 1 || rows[0].HeadSHA != "h1" || len(rows[0].Paths) != 1 || rows[0].Paths[0].Path != "src/a.go" || !rows[0].Complete {
		t.Errorf("PR_1's stored paths = %+v (err %v), want src/a.go read at h1", rows, err)
	}
}

// A pull request's changed paths are the difference between its head and its
// base, so a list read against another base is read again, even at the same
// head.
func TestBranchWatch_PathsReadAgainstAnotherBaseAreReadAgain(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	w := newWatchWorld(t)
	w.seenOnce()
	for i, base := range []string{"release", "main", "main"} {
		if _, err := w.stores.PRMergeability.UpsertPathsSystem(ctx, org, domain.PRPaths{
			EntityID: w.prs[i].ID, Paths: []domain.ChangedPath{modified("docs/old.md")}, HeadSHA: fmt.Sprintf("h%d", i+1), BaseRef: base, Complete: true,
		}); err != nil {
			t.Fatalf("seed paths: %v", err)
		}
	}
	w.set(func(w *watchWorld) { w.compares["old...new"] = compareBody("ahead", 1, "", "src/a.go") })

	if got := w.cycle(); !mapsEqual(got, map[int]string{1: domain.MergeabilityProbeBaseMoved}) {
		t.Errorf("admitted %v, want PR_1 alone: its list read against main touches the move", got)
	}
	if !slices.Equal(w.pathReads, []string{"PR_1"}) {
		t.Errorf("paths read for %v, want PR_1's alone", w.pathReads)
	}
}

func TestBranchWatch_RenamesTakeTheirPreviousPathFromREST(t *testing.T) {
	w := newWatchWorld(t)
	w.seenOnce()
	w.set(func(w *watchWorld) {
		w.paths["PR_2"] = pathsNode(2, "h2", domain.ChangedPath{Path: "docs/new.md", Change: domain.PathRenamed})
		w.previous[2] = `[{"filename":"docs/new.md","status":"renamed","previous_filename":"src/a.go"}]`
		w.compares["old...new"] = compareBody("ahead", 1, "", "src/a.go")
	})
	want := map[int]string{1: domain.MergeabilityProbeBaseMoved, 2: domain.MergeabilityProbeBaseMoved}
	if got := w.cycle(); !mapsEqual(got, want) {
		t.Errorf("admitted %v, want %v: PR_2 renamed the file the move changed", got, want)
	}
}

// GitHub meters REST and GraphQL apart, so a REST rate limit on one pull
// request's renamed files costs only that pull request its read: the others,
// whose paths GraphQL already returned, are decided on them.
func TestBranchWatch_ARESTRateLimitOnRenamesProbesOnlyThePullRequestsItLeftUnread(t *testing.T) {
	w := newWatchWorld(t)
	w.seenOnce()
	w.set(func(w *watchWorld) {
		w.paths["PR_1"] = pathsNode(1, "h1", domain.ChangedPath{Path: "src/new.go", Change: domain.PathRenamed})
		w.previous[1] = "rate-limited"
		w.compares["old...new"] = compareBody("ahead", 1, "", "other/z.go")
	})
	want := map[int]string{1: domain.MergeabilityProbeBaseMoved}
	if got := w.cycle(); !mapsEqual(got, want) {
		t.Errorf("admitted %v, want %v: only PR_1's paths went unread", got, want)
	}
}

// countingDeletes counts the branch watch's deletes of stale tips.
type countingDeletes struct {
	db.PRMergeabilityStore
	deletes int
}

func (c *countingDeletes) DeleteBranchTipsExceptSystem(ctx context.Context, orgID string, keep []domain.BranchRef) (int, error) {
	c.deletes++
	return c.PRMergeabilityStore.DeleteBranchTipsExceptSystem(ctx, orgID, keep)
}

// A tip no open pull request's base names is deleted, and a cycle with none
// to delete sends no delete, which is a write transaction.
func TestBranchWatch_DeletesTheTipsOfBranchesNoOpenPRTargets(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	w := newWatchWorld(t)
	store := &countingDeletes{PRMergeabilityStore: w.stores.PRMergeability}
	w.tr.mergeability = store
	if _, err := w.stores.PRMergeability.UpsertBranchTipSystem(ctx, org, domain.BranchTip{
		RepositoryID: w.repoID, Branch: "release", TipSHA: "r1", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed tip: %v", err)
	}
	w.cycle()
	tips, err := w.stores.PRMergeability.ListBranchTipsSystem(ctx, org)
	if err != nil || len(tips) != 1 || tips[0].Branch != "main" {
		t.Errorf("tips after the cycle = %+v (err %v), want main's alone", tips, err)
	}
	w.cycle()
	if store.deletes != 1 {
		t.Errorf("deletes sent = %d, want 1: the second cycle had no tip to delete", store.deletes)
	}
}

// The poller runs one pass per App installation, each over its own
// repositories, while every pass sees the org's open pull requests. A pass
// reads only its repositories' branches, and keeps the tips of the others.
func TestBranchWatch_APassChecksOnlyItsOwnRepositoriesAndKeepsTheOthersTips(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	w := newWatchWorld(t)
	trackRepos(t, w.stores, org, []string{"octo/repo", "octo/other"})
	other, _, err := w.stores.Entities.FindOrCreateSystem(ctx, org, "github", cursorTestHost, "octo/other#9", "PR_other", "pr", "PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	raw, _ := json.Marshal(domain.PRSnapshot{
		NodeID: "PR_other", Number: 9, Repo: "octo/other", State: "OPEN", BaseRef: "main", HeadSHA: "h9",
		CreatedAt: "2026-05-20T10:00:00Z", UpdatedAt: "2026-05-29T12:00:00Z", CheckRuns: []domain.CheckRun{},
	})
	if ok, err := w.stores.Entities.UpdateSnapshotCASSystem(ctx, org, other.ID, string(raw), other.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}
	if _, err := w.db.Exec(`INSERT INTO pull_request_mergeability (entity_id, mergeable, mergeable_head_sha, checked_at) VALUES (?, 'MERGEABLE', 'h9', ?)`,
		other.ID, time.Now().UTC()); err != nil {
		t.Fatalf("seed answer: %v", err)
	}
	if _, err := w.stores.PRMergeability.UpsertBranchTipSystem(ctx, org, domain.BranchTip{
		RepositoryID: other.RepositoryID, Branch: "main", TipSHA: "o1", ObservedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed tip: %v", err)
	}

	// The fake serves octo/repo alone, so a read of octo/other fails the test.
	w.cycle()
	tips, err := w.stores.PRMergeability.ListBranchTipsSystem(ctx, org)
	if err != nil || len(tips) != 2 {
		t.Fatalf("tips after the pass = %+v (err %v), want octo/repo's main and octo/other's", tips, err)
	}
	for _, tip := range tips {
		if tip.RepositoryID == other.RepositoryID && tip.TipSHA != "o1" {
			t.Errorf("octo/other's tip = %+v, want it left as stored", tip)
		}
	}
}
