package pollbench

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// newTestServer serves a one-repo dataset of n PRs through the real client.
func newTestServer(t *testing.T, n int, cfg ServerConfig) (*Server, *ghclient.Client, Repo) {
	t.Helper()
	ds := Generate(DatasetConfig{Repos: 1, PRsPerRepo: n, FilesMean: 2, Seed: 1})
	srv := NewServer(ds, cfg)
	t.Cleanup(srv.Close)
	return srv, ghclient.NewClient(srv.URL(), "bench-pat"), ds.Repos[0]
}

func TestRefReadIsAFree304UntilTheBaseMoves(t *testing.T) {
	ctx := context.Background()
	srv, client, repo := newTestServer(t, 1, ServerConfig{RateLimit: 10, RateLimitWindow: time.Hour})

	tip, etag, notModified, err := client.BranchTip(ctx, repo.Owner, repo.Name, baseBranch, "")
	if err != nil || notModified || tip == "" {
		t.Fatalf("first read = %q, %v, %v", tip, notModified, err)
	}
	if _, _, notModified, err := client.BranchTip(ctx, repo.Owner, repo.Name, baseBranch, etag); err != nil || !notModified {
		t.Fatalf("conditional read = 304 %v, %v; want a 304", notModified, err)
	}
	if c := srv.Snapshot(); c.Refs != 1 || c.Refs304 != 1 || srv.used != 1 {
		t.Fatalf("refs = %d, ref304 = %d, budget used = %d; want 1, 1, 1 (a 304 is free)", c.Refs, c.Refs304, srv.used)
	}
	srv.MoveBase(0)
	moved, _, notModified, err := client.BranchTip(ctx, repo.Owner, repo.Name, baseBranch, etag)
	if err != nil || notModified || moved == tip {
		t.Fatalf("read after a move = %q (was %q), 304 %v, %v; want the new tip", moved, tip, notModified, err)
	}
}

func TestCompareCapsAndStatuses(t *testing.T) {
	ctx := context.Background()
	srv, client, repo := newTestServer(t, 1, ServerConfig{})
	rw := srv.world.repos[0]
	land := func(parent string, changes ...domain.ChangedPath) string {
		c := &commit{sha: commitSHA(0, len(rw.commits)), parent: parent, changes: changes}
		rw.commits[c.sha] = c
		return c.sha
	}
	root := rw.tip
	var line []string
	head := root
	for i := 0; i < 260; i++ {
		head = land(head,
			domain.ChangedPath{Path: fmt.Sprintf("d%03d/a.go", i), Change: domain.PathAdded},
			domain.ChangedPath{Path: fmt.Sprintf("d%03d/b.go", i), Change: domain.PathAdded})
		line = append(line, head)
	}
	renamed := land(head,
		domain.ChangedPath{Path: "moved/a.go", Change: domain.PathRenamed, Previous: "d000/a.go"},
		domain.ChangedPath{Path: "copied/b.go", Change: domain.PathCopied, Previous: "d000/b.go"})
	fork := land(line[0], domain.ChangedPath{Path: "fork.go", Change: domain.PathAdded})
	rw.tip = renamed

	cmp, err := client.Compare(ctx, repo.Owner, repo.Name, root, head)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Status != "ahead" || cmp.TotalCommits != 260 || len(cmp.Commits) != compareCommitCap || len(cmp.Files) != compareFileCap {
		t.Errorf("root...head = %s, %d total, %d listed, %d files; want ahead, 260, %d, %d",
			cmp.Status, cmp.TotalCommits, len(cmp.Commits), len(cmp.Files), compareCommitCap, compareFileCap)
	}
	if last := cmp.Commits[len(cmp.Commits)-1]; last.SHA != head || last.Parents[0] != line[258] {
		t.Errorf("last listed commit = %s (parent %v); want the most recent, %s", last.SHA, last.Parents, head)
	}
	cmp, err = client.Compare(ctx, repo.Owner, repo.Name, head, baseBranch)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ChangedPath{
		{Path: "copied/b.go", Change: domain.PathCopied, Previous: "d000/b.go"},
		{Path: "moved/a.go", Change: domain.PathRenamed, Previous: "d000/a.go"},
	}
	if cmp.Status != "ahead" || fmt.Sprint(cmp.Files) != fmt.Sprint(want) {
		t.Errorf("head...main = %s, %v; want ahead, %v", cmp.Status, cmp.Files, want)
	}
	for _, c := range []struct{ base, head, status string }{
		{head, head, "identical"},
		{head, line[0], "behind"},
		{head, fork, "diverged"},
	} {
		cmp, err := client.Compare(ctx, repo.Owner, repo.Name, c.base, c.head)
		if err != nil || cmp.Status != c.status {
			t.Errorf("compare %s...%s = %+v, %v; want %s", c.base, c.head, cmp, err, c.status)
		}
	}
	if _, err := client.Compare(ctx, repo.Owner, repo.Name, "f00d", head); !ghclient.IsHTTP404(err) {
		t.Errorf("compare from an unknown sha: %v; want a 404", err)
	}
}

func TestFileListsAreCappedAsGitHubCapsThem(t *testing.T) {
	ctx := context.Background()
	srv, client, repo := newTestServer(t, 1, ServerConfig{})
	pr := &srv.ds.Repos[0].PRs[0]
	pr.Paths = nil
	for i := 0; i < 3100; i++ {
		pr.Paths = append(pr.Paths, domain.ChangedPath{Path: fmt.Sprintf("f%04d.go", i), Change: domain.PathModified})
	}
	pr.Paths[5] = domain.ChangedPath{Path: "new.go", Change: domain.PathRenamed, Previous: "old.go"}
	pr.ChangedFiles = len(pr.Paths)

	read := func(query string) []map[string]any {
		t.Helper()
		data, err := client.Get(ctx, fmt.Sprintf("/repos/%s/pulls/1/files?%s", repo.FullName(), query))
		var files []map[string]any
		if err == nil {
			err = json.Unmarshal(data, &files)
		}
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	if n := len(read("")); n != 30 {
		t.Errorf("default page = %d files; want 30", n)
	}
	if n := len(read("per_page=500")); n != 100 {
		t.Errorf("per_page=500 = %d files; want 100", n)
	}
	if n := len(read("per_page=100&page=31")); n != 0 {
		t.Errorf("page 31 = %d files; want none past %d", n, ghclient.MaxPRChangedPaths)
	}
	previous, _, err := client.PreviousPaths(ctx, repo.Owner, repo.Name, 1)
	if err != nil || len(previous) != 1 || previous["new.go"] != "old.go" {
		t.Errorf("previous paths = %v, %v; want new.go from old.go", previous, err)
	}

	reads, requests, err := client.PRChangedPaths(ctx, []string{pr.NodeID})
	got := reads[pr.NodeID]
	if err != nil || len(got.Paths) != ghclient.MaxPRChangedPaths || got.Complete || requests != 30 {
		t.Errorf("GraphQL read = %d paths, complete %v, %d requests, %v; want %d paths over 30 pages, incomplete",
			len(got.Paths), got.Complete, requests, err, ghclient.MaxPRChangedPaths)
	}
	if got.Paths[5].Change != domain.PathRenamed || got.HeadSHA != pr.HeadSHA || got.BaseRef != baseBranch {
		t.Errorf("GraphQL read = %+v at %s on %s", got.Paths[5], got.HeadSHA, got.BaseRef)
	}
}

func TestMergeabilityIsComputedLazily(t *testing.T) {
	ctx := context.Background()
	srv, client, repo := newTestServer(t, 1, ServerConfig{})
	id := repo.PRs[0].NodeID
	read := func() string {
		t.Helper()
		reads, err := client.PRMergeability(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		return reads[id].Mergeable
	}

	if got := read(); got != domain.MergeableMergeable || srv.Snapshot().TestMerges != 0 {
		t.Fatalf("fresh read = %s with %d test merges; want MERGEABLE with none", got, srv.Snapshot().TestMerges)
	}
	srv.MoveBase(0)
	srv.world.prs[0][0].truth = domain.MergeableConflicting
	if got := read(); got != domain.MergeableUnknown || srv.Snapshot().TestMerges != 1 {
		t.Fatalf("read after the base moved = %s with %d test merges; want UNKNOWN with 1", got, srv.Snapshot().TestMerges)
	}
	if got := read(); got != domain.MergeableConflicting || srv.Snapshot().TestMerges != 1 {
		t.Fatalf("read after the UNKNOWN = %s with %d test merges; want CONFLICTING, still 1", got, srv.Snapshot().TestMerges)
	}
	srv.PushHead(0, 0)
	if got := read(); got != domain.MergeableUnknown || srv.Snapshot().TestMergesHeadPushed != 1 {
		t.Fatalf("read after a push = %s with %d head-pushed test merges; want UNKNOWN with 1", got, srv.Snapshot().TestMergesHeadPushed)
	}
}

func TestGraphQLCostFollowsGitHubsRule(t *testing.T) {
	ids := make([]any, 100)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	for _, c := range []struct {
		name, query string
		want        int
	}{
		// GitHub's documented example: 1 + 100 + 5,000 requests.
		{"nested connections", `query { viewer { login repositories(first: 100) { edges { node { id issues(first: 50) {
			edges { node { id labels(first: 60) { edges { node { id name } } } } } } } } } } }`, 51},
		{"a connection per id", `query PRChangedPaths($ids: [ID!]!) { nodes(ids: $ids) { ... on PullRequest {
			files(first: 100) { nodes { path } } } } rateLimit { cost } }`, 1},
		{"no connection", `query PRMergeability($ids: [ID!]!) { nodes(ids: $ids) { ... on PullRequest { id mergeable } } }`, 1},
		{"a totalCount per item, through a fragment", `query Q($ids: [ID!]!) { nodes(ids: $ids) { ...F } }
			fragment F on PullRequest { latestReviews(first: 20) { nodes { comments { totalCount } } } }`, 21},
	} {
		if got, err := gqlCost(c.query, map[string]any{"ids": ids}); err != nil || got != c.want {
			t.Errorf("%s: cost = %d, %v; want %d", c.name, got, err, c.want)
		}
	}
}

func TestMonoFileCountsMatchKubernetes(t *testing.T) {
	ds := Generate(DatasetConfig{Repos: 1, PRsPerRepo: 1500, FileCounts: KubernetesFileCounts, Tree: MonorepoTree, Seed: 42})
	counts := make([]int, 0, ds.TotalPRs)
	for _, pr := range ds.Repos[0].PRs {
		if len(pr.Paths) != pr.ChangedFiles {
			t.Fatalf("PR %d lists %d paths for %d changed files", pr.Number, len(pr.Paths), pr.ChangedFiles)
		}
		counts = append(counts, pr.ChangedFiles)
	}
	sort.Ints(counts)
	within := func(got int, want float64) bool { return float64(got) >= 0.9*want && float64(got) <= 1.1*want }
	median, p90 := counts[len(counts)/2], counts[len(counts)*9/10]
	if !within(median, 3) || !within(p90, 17) {
		t.Errorf("median = %d, p90 = %d; want within 10%% of 3 and 17", median, p90)
	}
}
