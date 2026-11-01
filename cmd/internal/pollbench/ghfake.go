package pollbench

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// ServerConfig tunes the fake GitHub server's behavior.
type ServerConfig struct {
	// Latency is injected per request before responding — models the
	// GHES-in-VPC round trip (the tracker's per-repo sweep is sequential,
	// so this dominates cold-start wall time).
	Latency time.Duration

	// RateLimit is the request budget per RateLimitWindow; 0 disables rate
	// limiting entirely (headers omitted — the "GHES with rate limiting
	// disabled" shape the client treats as unlimited). REST and GraphQL
	// share the one budget — a simplification of GitHub's separate
	// primary/GraphQL budgets that's fine for exercising backoff.
	// Conditional 304s are free, matching GitHub's documented behavior.
	RateLimit int

	// RateLimitWindow is the budget window. Real GitHub/GHES uses an hour;
	// the bench defaults shorter so exhaustion→reset→resume fits in a run.
	RateLimitWindow time.Duration

	// ConflictOnOverlap is the chance that a base move makes a pull request
	// whose paths it overlaps CONFLICTING. It is the only way a pull request
	// becomes CONFLICTING after the dataset is generated.
	ConflictOnOverlap float64
}

// Counters tracks what the server served and did, by class. Deltas between
// snapshots give per-cycle costs; with rate limiting off the counts are fully
// deterministic for a given dataset and scenario.
type Counters struct {
	Total       int // every request that reached the handler
	PullsList   int // GET /pulls page 1, served 200
	PullsPages  int // GET /pulls page ≥2 (pagination overflow)
	Pulls304    int // GET /pulls page 1, served 304 (ETag hit)
	GraphQL     int // POST /graphql served 200
	Points      int // GraphQL points those queries cost, by gqlCost
	Teams       int // GET /orgs/{owner}/teams
	Refs        int // GET /git/ref/heads/{branch}, served 200
	Refs304     int // GET /git/ref/heads/{branch}, served 304 (ETag hit)
	Compares    int // GET /compare/{base}...{head}
	PathReads   int // POST /graphql PRChangedPaths and PRChangedPathsPage, served 200
	FileLists   int // GET /pulls/{n}/files pages
	RateLimited int // requests refused with the exhausted-budget 403
	Other       int // anything the poll path shouldn't touch (bug tripwire)

	// TestMerges counts the test merges GitHub ran to answer PRMergeability
	// reads, split by why the pull request's answer was stale: its head was
	// pushed, a base move overlapping its paths landed, or it was read again
	// after an UNKNOWN and the base moved in between. TestMergesUnaccounted
	// is a test merge none of those explain: a read TF did not need.
	TestMerges            int
	TestMergesHeadPushed  int
	TestMergesOverlap     int
	TestMergesReread      int
	TestMergesUnaccounted int
}

// Server is the fake GitHub API. It serves exactly the surface the poll path
// and the mergeability probe worker touch: the REST open-PR listing (with
// ETag conditional support and >100-PR pagination), the GraphQL nodes(ids:)
// batch refresh, the org-teams listing the per-cycle group reconcile reads,
// the base branches' refs, the compares between their tips, each pull
// request's changed files over GraphQL and REST, and the PRMergeability
// read, plus primary-rate-limit headers and exhaustion 403s when configured.
//
// The world it serves moves (world.go): MoveBase lands a commit on a
// repository's base branch, PushHead moves a pull request's head, and
// mergeability is computed lazily, as GitHub computes it.
type Server struct {
	ds  *Dataset
	cfg ServerConfig
	srv *httptest.Server

	repoIdx map[string]int // repo name → index in ds.Repos

	// mu guards the counters, the rate-limit window, the world, and the
	// dataset's pull requests, which the world moves.
	mu        sync.Mutex
	counters  Counters
	used      int
	windowEnd time.Time
	world     *world
}

// NewServer starts the fake API for ds. Callers must Close it.
func NewServer(ds *Dataset, cfg ServerConfig) *Server {
	s := &Server{ds: ds, cfg: cfg, repoIdx: make(map[string]int, len(ds.Repos)), world: newWorld(ds)}
	for i, r := range ds.Repos {
		s.repoIdx[r.Name] = i
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is the server's base URL — pass it as the GitHub base URL, and the
// client routes REST to /api/v3 and GraphQL to /api/graphql (the GHES path
// mount, which is also the deployment shape being modeled).
func (s *Server) URL() string { return s.srv.URL }

// Close shuts the server down.
func (s *Server) Close() { s.srv.Close() }

// Snapshot returns a copy of the request counters.
func (s *Server) Snapshot() Counters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counters
}

// NextReset reports when the current rate-limit window rolls over — the
// driver sleeps until then between resume cycles once the client has given
// up on the current window. Zero when rate limiting is off or no request
// has started a window yet.
func (s *Server) NextReset() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windowEnd
}

// other counts and refuses a request the fake does not model.
func (s *Server) other(w http.ResponseWriter, msg string) {
	s.mu.Lock()
	s.counters.Other++
	s.mu.Unlock()
	http.Error(w, `{"message":"pollbench fake: `+msg+`"}`, http.StatusNotFound)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Latency > 0 {
		time.Sleep(s.cfg.Latency)
	}
	s.mu.Lock()
	s.counters.Total++
	s.mu.Unlock()

	path := r.URL.Path
	switch {
	case path == "/api/graphql":
		if !s.charge(w) {
			return
		}
		s.handleGraphQL(w, r)
	case strings.HasPrefix(path, "/api/v3/repos/"):
		// /api/v3/repos/{owner}/{repo}/{rest}
		parts := strings.SplitN(strings.TrimPrefix(path, "/api/v3/repos/"), "/", 3)
		if len(parts) != 3 {
			s.other(w, "bad repo path")
			return
		}
		ri, ok := s.repoIdx[parts[1]]
		if !ok {
			s.other(w, "unknown repo")
			return
		}
		switch rest := parts[2]; {
		case rest == "pulls":
			s.handlePulls(w, r, ri)
		case strings.HasPrefix(rest, "pulls/") && strings.HasSuffix(rest, "/files"):
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rest, "pulls/"), "/files"))
			if err != nil || n < 1 || n > len(s.ds.Repos[ri].PRs) {
				s.other(w, "unknown pull request")
				return
			}
			s.handleFiles(w, r, ri, n-1)
		case strings.HasPrefix(rest, "git/ref/heads/"):
			s.handleRef(w, r, ri, strings.TrimPrefix(rest, "git/ref/heads/"))
		case strings.HasPrefix(rest, "compare/"):
			s.handleCompare(w, ri, strings.TrimPrefix(rest, "compare/"))
		default:
			s.other(w, "unexpected endpoint")
		}
	case strings.HasPrefix(path, "/api/v3/orgs/") && strings.HasSuffix(path, "/teams"):
		if !s.charge(w) {
			return
		}
		s.handleTeams(w)
	default:
		s.other(w, "unexpected endpoint")
	}
}

// charge consumes one unit of rate-limit budget and stamps the x-ratelimit-*
// headers. When the budget is exhausted it writes GitHub's classic primary
// exhaustion response — 403 with x-ratelimit-remaining: 0 and the window
// reset — and returns false. With rate limiting disabled it's a no-op
// (headers omitted, like a GHES host with limits off).
func (s *Server) charge(w http.ResponseWriter) bool {
	if s.cfg.RateLimit <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.windowEnd.IsZero() || now.After(s.windowEnd) {
		s.used = 0
		s.windowEnd = now.Add(s.cfg.RateLimitWindow)
	}
	if s.used >= s.cfg.RateLimit {
		s.counters.RateLimited++
		s.rlHeaders(w)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for pollbench"}`))
		return false
	}
	s.used++
	s.rlHeaders(w)
	return true
}

// rlHeadersFree stamps the current budget state without consuming it — for
// 304 responses, which don't count against the primary limit.
func (s *Server) rlHeadersFree(w http.ResponseWriter) {
	if s.cfg.RateLimit <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rlHeaders(w)
}

// rlHeaders writes the x-ratelimit-* trio. Callers hold s.mu.
func (s *Server) rlHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(s.cfg.RateLimit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(s.cfg.RateLimit-s.used))
	h.Set("X-RateLimit-Used", strconv.Itoa(s.used))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(s.windowEnd.Unix(), 10))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// queryInt reads a positive integer query parameter, or def when it is
// absent or malformed.
func queryInt(r *http.Request, name string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// --- REST: open-PR listing -------------------------------------------------

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request, ri int) {
	page := queryInt(r, "page", 1)

	// Conditional fast path before budget accounting: a 304 is free on the
	// primary limit. Only page 1 carries the conditional header (matching
	// the client, which lists further pages unconditionally). The ETag
	// changes whenever a base move or a head push changes the repository.
	s.mu.Lock()
	etag := fmt.Sprintf(`W/"pulls-%d-g%d"`, ri, s.world.repos[ri].listGen)
	s.mu.Unlock()
	if page == 1 && r.Header.Get("If-None-Match") == etag {
		s.mu.Lock()
		s.counters.Pulls304++
		s.mu.Unlock()
		s.rlHeadersFree(w)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if !s.charge(w) {
		return
	}
	s.mu.Lock()
	if page == 1 {
		s.counters.PullsList++
	} else {
		s.counters.PullsPages++
	}
	repo := s.ds.Repos[ri]
	const perPage = 100
	start := min((page-1)*perPage, len(repo.PRs))
	end := min(start+perPage, len(repo.PRs))
	out := make([]map[string]any, 0, end-start)
	for _, pr := range repo.PRs[start:end] {
		out = append(out, restPRJSON(repo, pr))
	}
	s.mu.Unlock()

	if page == 1 {
		w.Header().Set("ETag", etag)
	}
	writeJSON(w, out)
}

// restPRJSON renders one PR as the REST pulls-listing object (the subset the
// tracker's discovery path decodes).
func restPRJSON(repo Repo, pr PR) map[string]any {
	full := repo.FullName()
	reviewers := make([]map[string]string, 0, len(pr.ReviewRequests))
	for _, login := range pr.ReviewRequests {
		reviewers = append(reviewers, map[string]string{"login": login})
	}
	labels := make([]map[string]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]any{
		"number":     pr.Number,
		"node_id":    pr.NodeID,
		"title":      pr.Title,
		"state":      "open",
		"draft":      false,
		"html_url":   fmt.Sprintf("https://github.example.com/%s/pull/%d", full, pr.Number),
		"created_at": pr.CreatedAt,
		"updated_at": pr.UpdatedAt,
		"user":       map[string]string{"login": pr.Author},
		"head": map[string]any{
			"sha":  pr.HeadSHA,
			"ref":  pr.HeadRef,
			"repo": map[string]string{"full_name": full},
		},
		"base": map[string]any{
			"ref":  pr.BaseRef,
			"repo": map[string]string{"full_name": full},
		},
		"requested_reviewers": reviewers,
		"requested_teams":     []any{},
		"labels":              labels,
	}
}

// --- REST: a pull request's files -------------------------------------------

// handleFiles serves GET /pulls/{n}/files with GitHub's limits: per_page
// defaults to 30 and is capped at 100, and the list stops at
// ghclient.MaxPRChangedPaths files whatever the pull request changed.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request, ri, pi int) {
	if !s.charge(w) {
		return
	}
	perPage := min(queryInt(r, "per_page", 30), 100)
	page := queryInt(r, "page", 1)

	s.mu.Lock()
	s.counters.FileLists++
	paths := s.ds.Repos[ri].PRs[pi].Paths
	listed := paths[:min(len(paths), ghclient.MaxPRChangedPaths)]
	start := min((page-1)*perPage, len(listed))
	end := min(start+perPage, len(listed))
	out := make([]map[string]any, 0, end-start)
	for _, p := range listed[start:end] {
		out = append(out, restFileJSON(p))
	}
	s.mu.Unlock()
	writeJSON(w, out)
}

// restFileJSON renders one changed file as REST lists it, with
// previous_filename on a rename or copy.
func restFileJSON(p domain.ChangedPath) map[string]any {
	f := map[string]any{"filename": p.Path, "status": restFileStatus(p.Change)}
	if p.Previous != "" {
		f["previous_filename"] = p.Previous
	}
	return f
}

// restFileStatus is the REST status a GraphQL PatchStatus corresponds to.
func restFileStatus(change string) string {
	switch change {
	case domain.PathAdded:
		return "added"
	case domain.PathDeleted:
		return "removed"
	case domain.PathRenamed:
		return "renamed"
	case domain.PathCopied:
		return "copied"
	case domain.PathModified:
		return "modified"
	}
	return "changed"
}

// --- REST: branch refs -------------------------------------------------------

// handleRef serves a base branch's tip with an ETag of that tip; a matching
// If-None-Match is a free 304, as on GitHub. Every pull request in the
// dataset targets one base branch, so a read of any other counts as other.
func (s *Server) handleRef(w http.ResponseWriter, r *http.Request, ri int, branch string) {
	if branch != baseBranch {
		s.other(w, "unknown branch")
		return
	}
	s.mu.Lock()
	tip := s.world.repos[ri].tip
	s.mu.Unlock()
	etag := fmt.Sprintf(`W/"ref-%s"`, tip)
	if r.Header.Get("If-None-Match") == etag {
		s.mu.Lock()
		s.counters.Refs304++
		s.mu.Unlock()
		s.rlHeadersFree(w)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if !s.charge(w) {
		return
	}
	s.mu.Lock()
	s.counters.Refs++
	s.mu.Unlock()
	w.Header().Set("ETag", etag)
	writeJSON(w, map[string]any{
		"ref":    "refs/heads/" + branch,
		"object": map[string]string{"sha": tip, "type": "commit"},
	})
}

// --- REST: compare -----------------------------------------------------------

// GitHub's compare limits: the files of a comparison stop at compareFileCap
// with no count of the rest, and an unpaginated compare lists the most recent
// compareCommitCap commits beside total_commits.
const (
	compareFileCap   = 300
	compareCommitCap = 250
)

// handleCompare serves GET /compare/{base}...{head}: status ahead, behind,
// diverged or identical; the files changed from the merge base to head, with
// previous_filename on a rename; and the commits, all within GitHub's limits.
// A base or head the repository does not hold is a 404, as on GitHub.
func (s *Server) handleCompare(w http.ResponseWriter, ri int, basehead string) {
	base, head, ok := strings.Cut(basehead, "...")
	if !ok {
		s.other(w, "bad compare range")
		return
	}
	if !s.charge(w) {
		return
	}
	s.mu.Lock()
	s.counters.Compares++
	cmp, found := s.world.repos[ri].compare(base, head)
	s.mu.Unlock()
	if !found {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}

	commits := make([]map[string]any, 0, min(len(cmp.commits), compareCommitCap))
	for _, c := range cmp.commits[max(0, len(cmp.commits)-compareCommitCap):] {
		commits = append(commits, map[string]any{"sha": c.sha, "parents": []map[string]string{{"sha": c.parent}}})
	}
	files := make([]map[string]any, 0, min(len(cmp.files), compareFileCap))
	for _, f := range cmp.files[:min(len(cmp.files), compareFileCap)] {
		files = append(files, restFileJSON(f))
	}
	writeJSON(w, map[string]any{
		"status":        cmp.status,
		"ahead_by":      len(cmp.commits),
		"behind_by":     cmp.behindBy,
		"total_commits": len(cmp.commits),
		"commits":       commits,
		"files":         files,
	})
}

// --- REST: org teams ---------------------------------------------------------

// handleTeams serves the org-teams listing the per-cycle GitHub-group
// reconcile reads. A small fixed set, single page.
func (s *Server) handleTeams(w http.ResponseWriter) {
	s.mu.Lock()
	s.counters.Teams++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`[
		{"slug":"platform","name":"Platform"},
		{"slug":"backend","name":"Backend"},
		{"slug":"frontend","name":"Frontend"}
	]`))
}

// --- GraphQL -----------------------------------------------------------------

// handleGraphQL serves the operations the poll path and the probe worker
// send, told apart by the operation name each document carries: the
// nodes(ids:) refresh documents, PRMergeability, PRChangedPaths and
// PRChangedPathsPage. Every answer carries rateLimit.cost from gqlCost,
// which the counters sum. Anything else (the local-mode dashboard-backfill
// search, say) reaching here is a harness bug, counted as other.
func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.other(w, "undecodable GraphQL request")
		return
	}
	cost, err := gqlCost(req.Query, req.Variables)
	if err != nil {
		s.other(w, "unparseable GraphQL document")
		return
	}

	var data map[string]any
	paths := false
	s.mu.Lock()
	switch upstream.OperationName(req.Query) {
	case "RefreshPRsWithChecks", "RefreshPRsWithoutChecks":
		data = map[string]any{"nodes": s.nodes(req.Variables, gqlPRJSON)}
	case "PRMergeability":
		data = map[string]any{"nodes": s.nodes(req.Variables, s.mergeabilityJSON)}
	case "PRChangedPaths":
		pageSize := filesPageSize(req.Query, req.Variables)
		data = map[string]any{"nodes": s.nodes(req.Variables, func(_ Repo, pr PR) map[string]any {
			node := filesPageJSON(pr, 0, pageSize)
			node["id"], node["changedFiles"] = pr.NodeID, pr.ChangedFiles
			return node
		})}
		paths = true
	case "PRChangedPathsPage":
		id, _ := req.Variables["id"].(string)
		cursor, _ := req.Variables["cursor"].(string)
		var node any
		if loc, ok := s.ds.byNodeID[id]; ok {
			node = filesPageJSON(s.ds.Repos[loc[0]].PRs[loc[1]], decodeCursor(cursor), filesPageSize(req.Query, req.Variables))
		}
		data = map[string]any{"node": node}
		paths = true
	default:
		s.counters.Other++
		s.mu.Unlock()
		writeJSON(w, map[string]any{"data": map[string]any{}})
		return
	}
	s.counters.GraphQL++
	s.counters.Points += cost
	if paths {
		s.counters.PathReads++
	}
	s.mu.Unlock()

	data["rateLimit"] = map[string]int{"cost": cost}
	writeJSON(w, map[string]any{"data": data})
}

// nodes answers a nodes(ids:) document: render for each id the dataset
// holds, null for any other. Callers hold s.mu.
func (s *Server) nodes(vars map[string]any, render func(Repo, PR) map[string]any) []any {
	ids, _ := vars["ids"].([]any)
	out := make([]any, 0, len(ids))
	for _, raw := range ids {
		id, _ := raw.(string)
		loc, ok := s.ds.byNodeID[id]
		if !ok {
			out = append(out, nil)
			continue
		}
		repo := s.ds.Repos[loc[0]]
		out = append(out, render(repo, repo.PRs[loc[1]]))
	}
	return out
}

// mergeabilityJSON answers one pull request's PRMergeability read through
// the world's lazy computation. Callers hold s.mu.
func (s *Server) mergeabilityJSON(_ Repo, pr PR) map[string]any {
	loc := s.ds.byNodeID[pr.NodeID]
	return map[string]any{
		"id":          pr.NodeID,
		"mergeable":   s.world.readMergeability(loc[0], loc[1], &s.counters),
		"headRefOid":  pr.HeadSHA,
		"baseRefName": pr.BaseRef,
	}
}

// filesPageJSON renders one page of a pull request's files connection from
// offset, with the head and base it was read at. The connection stops at
// ghclient.MaxPRChangedPaths files, as GitHub's does.
func filesPageJSON(pr PR, offset, pageSize int) map[string]any {
	listed := pr.Paths[:min(len(pr.Paths), ghclient.MaxPRChangedPaths)]
	start := min(offset, len(listed))
	end := min(start+pageSize, len(listed))
	nodes := make([]map[string]string, 0, end-start)
	for _, p := range listed[start:end] {
		nodes = append(nodes, map[string]string{"path": p.Path, "changeType": p.Change})
	}
	return map[string]any{
		"headRefOid":  pr.HeadSHA,
		"baseRefName": pr.BaseRef,
		"files": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end < len(listed), "endCursor": encodeCursor(end)},
			"nodes":    nodes,
		},
	}
}

// filesPageSize is the first argument the document gives its files
// connection, at most GitHub's 100.
func filesPageSize(query string, vars map[string]any) int {
	if doc, err := parseGQL(query); err == nil {
		if f := doc.find("files"); f != nil {
			if n, ok := f.limit(vars); ok && n > 0 {
				return min(n, 100)
			}
		}
	}
	return 100
}

func encodeCursor(offset int) string {
	return base64.StdEncoding.EncodeToString([]byte("files:" + strconv.Itoa(offset)))
}

func decodeCursor(cursor string) int {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(string(raw), "files:"))
	return n
}

// gqlPRJSON renders one PR as the GraphQL PullRequest node shape the full
// refresh fragment produces. The full-fragment fields are always included —
// the client picks which snapshot form to build, and extra fields are
// ignored when it asked for the lightweight fragment.
func gqlPRJSON(repo Repo, pr PR) map[string]any {
	full := repo.FullName()

	reviewReqs := make([]map[string]any, 0, len(pr.ReviewRequests))
	for _, login := range pr.ReviewRequests {
		reviewReqs = append(reviewReqs, map[string]any{
			"requestedReviewer": map[string]string{"login": login},
		})
	}
	reviews := make([]map[string]any, 0, len(pr.Reviews))
	for _, rev := range pr.Reviews {
		reviews = append(reviews, map[string]any{
			"id":          rev.ID,
			"author":      map[string]string{"login": rev.Author},
			"state":       rev.State,
			"submittedAt": rev.SubmittedAt,
		})
	}
	labels := make([]map[string]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	contexts := make([]map[string]any, 0, len(pr.Checks))
	for _, c := range pr.Checks {
		contexts = append(contexts, map[string]any{
			"__typename":  "CheckRun",
			"databaseId":  c.ID,
			"name":        c.Name,
			"status":      strings.ToUpper(c.Status),
			"conclusion":  strings.ToUpper(c.Conclusion),
			"completedAt": c.CompletedAt,
			"detailsUrl":  fmt.Sprintf("https://github.example.com/%s/runs/%d", full, c.ID),
			"checkSuite":  map[string]any{"workflowRun": map[string]any{"databaseId": c.WorkflowRunID}},
		})
	}

	return map[string]any{
		"id":             pr.NodeID,
		"number":         pr.Number,
		"title":          pr.Title,
		"author":         map[string]string{"login": pr.Author},
		"state":          "OPEN",
		"isDraft":        false,
		"merged":         false,
		"headRefName":    pr.HeadRef,
		"baseRefName":    pr.BaseRef,
		"url":            fmt.Sprintf("https://github.example.com/%s/pull/%d", full, pr.Number),
		"repository":     map[string]string{"nameWithOwner": full},
		"headRepository": map[string]string{"nameWithOwner": full},
		"additions":      pr.Additions,
		"deletions":      pr.Deletions,
		"changedFiles":   pr.ChangedFiles,
		"reviewRequests": map[string]any{
			"pageInfo": map[string]bool{"hasNextPage": false},
			"nodes":    reviewReqs,
		},
		"latestReviews": map[string]any{"nodes": reviews},
		"reviews":       map[string]int{"totalCount": len(pr.Reviews)},
		"commits": map[string]any{
			"nodes": []map[string]any{{
				"commit": map[string]any{
					"oid":           pr.HeadSHA,
					"committedDate": pr.CreatedAt,
					"statusCheckRollup": map[string]any{
						"contexts": map[string]any{
							"pageInfo": map[string]bool{"hasNextPage": false},
							"nodes":    contexts,
						},
					},
				},
			}},
		},
		"labels":        map[string]any{"nodes": labels},
		"comments":      map[string]int{"totalCount": pr.CommentCount},
		"createdAt":     pr.CreatedAt,
		"updatedAt":     pr.UpdatedAt,
		"mergedAt":      "",
		"closedAt":      "",
		"timelineItems": map[string]any{"nodes": []any{}},
	}
}
