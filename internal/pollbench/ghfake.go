package pollbench

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
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
}

// Counters tracks requests served, by class. Deltas between snapshots give
// per-cycle request costs; with rate limiting off the counts are fully
// deterministic for a given dataset.
type Counters struct {
	Total       int // every request that reached the handler
	PullsList   int // GET /pulls page 1, served 200
	PullsPages  int // GET /pulls page ≥2 (pagination overflow)
	Pulls304    int // GET /pulls page 1, served 304 (ETag hit)
	GraphQL     int // POST /graphql served 200
	Teams       int // GET /orgs/{owner}/teams
	RateLimited int // requests refused with the exhausted-budget 403
	Other       int // anything the poll path shouldn't touch (bug tripwire)
}

// Server is the fake GitHub API. It serves exactly the surface the tracker's
// cold-start path touches — the REST open-PR listing (with ETag conditional
// support and >100-PR pagination), the GraphQL nodes(ids:) batch refresh,
// and the org-teams listing the per-cycle group reconcile reads — plus
// primary-rate-limit headers and exhaustion 403s when configured.
type Server struct {
	ds  *Dataset
	cfg ServerConfig
	srv *httptest.Server

	repoIdx map[string]int // repo name → index in ds.Repos

	mu        sync.Mutex
	counters  Counters
	used      int
	windowEnd time.Time
}

// NewServer starts the fake API for ds. Callers must Close it.
func NewServer(ds *Dataset, cfg ServerConfig) *Server {
	s := &Server{ds: ds, cfg: cfg, repoIdx: make(map[string]int, len(ds.Repos))}
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
	case strings.HasPrefix(path, "/api/v3/repos/") && strings.HasSuffix(path, "/pulls"):
		s.handlePulls(w, r)
	case strings.HasPrefix(path, "/api/v3/orgs/") && strings.HasSuffix(path, "/teams"):
		if !s.charge(w) {
			return
		}
		s.handleTeams(w)
	default:
		s.mu.Lock()
		s.counters.Other++
		s.mu.Unlock()
		http.Error(w, `{"message":"pollbench fake: unexpected endpoint"}`, http.StatusNotFound)
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

// --- REST: open-PR listing -------------------------------------------------

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request) {
	// /api/v3/repos/{owner}/{repo}/pulls
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v3/repos/"), "/")
	if len(parts) != 3 {
		s.mu.Lock()
		s.counters.Other++
		s.mu.Unlock()
		http.Error(w, `{"message":"bad pulls path"}`, http.StatusNotFound)
		return
	}
	idx, ok := s.repoIdx[parts[1]]
	if !ok {
		s.mu.Lock()
		s.counters.Other++
		s.mu.Unlock()
		http.Error(w, `{"message":"unknown repo"}`, http.StatusNotFound)
		return
	}
	repo := s.ds.Repos[idx]

	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}

	// Conditional fast path before budget accounting: a 304 is free on the
	// primary limit. Only page 1 carries the conditional header (matching
	// the client, which lists further pages unconditionally).
	etag := fmt.Sprintf(`W/"pulls-%d-g0"`, idx)
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
	s.mu.Unlock()

	const perPage = 100
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(repo.PRs) {
		start = len(repo.PRs)
	}
	if end > len(repo.PRs) {
		end = len(repo.PRs)
	}

	out := make([]map[string]any, 0, end-start)
	for _, pr := range repo.PRs[start:end] {
		out = append(out, restPRJSON(repo, pr))
	}
	if page == 1 {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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

// --- GraphQL: nodes(ids:) batch refresh --------------------------------------

func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string `json:"query"`
		Variables struct {
			IDs []string `json:"ids"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Variables.IDs) == 0 {
		// The only GraphQL the multi-mode poll path issues is the nodes(ids:)
		// refresh; anything else (e.g. the local-mode dashboard-backfill
		// search) reaching here is a harness bug — count it visibly.
		s.mu.Lock()
		s.counters.Other++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
		return
	}

	s.mu.Lock()
	s.counters.GraphQL++
	s.mu.Unlock()

	nodes := make([]any, 0, len(req.Variables.IDs))
	for _, id := range req.Variables.IDs {
		loc, ok := s.ds.byNodeID[id]
		if !ok {
			nodes = append(nodes, nil)
			continue
		}
		repo := s.ds.Repos[loc[0]]
		nodes = append(nodes, gqlPRJSON(repo, repo.PRs[loc[1]]))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]any{"nodes": nodes},
	})
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
		"mergeable":      pr.Mergeable,
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
