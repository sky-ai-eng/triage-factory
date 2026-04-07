package poller

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// GitHubPoller fetches tasks from the GitHub API on an interval.
type GitHubPoller struct {
	baseURL    string
	pat        string
	username   string
	database   *sql.DB
	client     *http.Client
	interval   time.Duration
	stop       chan struct{}
	onNewTasks func() // called after new tasks are ingested
}

// apiBase returns the correct API base URL.
// github.com uses api.github.com; GHE uses {host}/api/v3.
func ghAPIBase(baseURL string) string {
	if baseURL == "https://github.com" {
		return "https://api.github.com"
	}
	return baseURL + "/api/v3"
}

func NewGitHubPoller(baseURL, pat, username string, database *sql.DB, interval time.Duration, onNewTasks func()) *GitHubPoller {
	return &GitHubPoller{
		baseURL:    ghAPIBase(strings.TrimRight(baseURL, "/")),
		pat:        pat,
		username:   username,
		database:   database,
		client:     &http.Client{Timeout: 15 * time.Second},
		interval:   interval,
		stop:       make(chan struct{}),
		onNewTasks: onNewTasks,
	}
}

func (p *GitHubPoller) Start() {
	go func() {
		p.poll() // poll immediately on start
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.poll()
			case <-p.stop:
				return
			}
		}
	}()
}

func (p *GitHubPoller) Stop() {
	close(p.stop)
}

func (p *GitHubPoller) poll() {
	log.Println("[github] polling for tasks...")

	var allTasks []domain.Task

	// 1. PRs where the user's review is requested
	reviewPRs, err := p.searchPRs(
		fmt.Sprintf("review-requested:%s+type:pr+state:open", p.username),
		"review_requested", false,
	)
	if err != nil {
		log.Printf("[github] error fetching review-requested PRs: %v", err)
	} else {
		allTasks = append(allTasks, reviewPRs...)
	}

	// 2. Self-authored open PRs (with CI status — no extra API call, uses head SHA from details)
	authoredPRs, err := p.searchPRs(
		fmt.Sprintf("author:%s+type:pr+state:open", p.username),
		"authored", true,
	)
	if err != nil {
		log.Printf("[github] error fetching authored PRs: %v", err)
	} else {
		allTasks = append(allTasks, authoredPRs...)
	}

	// 3. PRs where the user was @mentioned
	mentionedPRs, err := p.searchPRs(
		fmt.Sprintf("mentions:%s+type:pr+state:open", p.username),
		"mentioned", false,
	)
	if err != nil {
		log.Printf("[github] error fetching mentioned PRs: %v", err)
	} else {
		allTasks = append(allTasks, mentionedPRs...)
	}

	// 4. Self-authored merged PRs from the last 30 days → status "done"
	cutoff := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	mergedPRs, err := p.searchPRs(
		fmt.Sprintf("author:%s+type:pr+is:merged+merged:>=%s", p.username, cutoff),
		"authored", false,
	)
	if err != nil {
		log.Printf("[github] error fetching merged PRs: %v", err)
	} else {
		for i := range mergedPRs {
			mergedPRs[i].Status = "done"
		}
		allTasks = append(allTasks, mergedPRs...)
	}

	// Deduplicate by source_id, preferring earlier entries (review_requested > authored > mentioned > merged)
	seen := map[string]bool{}
	var tasks []domain.Task
	for _, t := range allTasks {
		if !seen[t.SourceID] {
			seen[t.SourceID] = true
			tasks = append(tasks, t)
		}
	}

	inserted := 0
	for _, t := range tasks {
		if err := db.UpsertTask(p.database, t); err != nil {
			log.Printf("[github] error upserting task %s: %v", t.SourceID, err)
			continue
		}
		inserted++
	}
	log.Printf("[github] poll complete: %d tasks processed (%d review, %d authored, %d mentioned, %d merged)",
		inserted, len(reviewPRs), len(authoredPRs), len(mentionedPRs), len(mergedPRs))
	if inserted > 0 && p.onNewTasks != nil {
		p.onNewTasks()
	}
}

// searchPRs runs a GitHub search query and returns tasks tagged with the given relevance reason.
// If fetchCI is true, also fetches CI check status using the head SHA from the PR details
// (no extra API call since the details endpoint already returns it).
func (p *GitHubPoller) searchPRs(query, reason string, fetchCI bool) ([]domain.Task, error) {
	url := fmt.Sprintf("%s/search/issues?q=%s&per_page=100", p.baseURL, query)

	body, err := p.get(url)
	if err != nil {
		return nil, err
	}

	var result struct {
		Items []ghSearchItem `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}

	var tasks []domain.Task
	for _, item := range result.Items {
		t := item.toTask()
		t.RelevanceReason = reason

		// Fetch PR details (diff stats + head SHA)
		repoName := item.repoFullName()
		if item.PullRequest != nil && repoName != "" {
			details, err := p.fetchPRDetails(repoName, item.Number)
			if err != nil {
				log.Printf("[github] could not fetch PR details for %s#%d: %v", repoName, item.Number, err)
			} else {
				t.DiffAdditions = details.Additions
				t.DiffDeletions = details.Deletions
				t.FilesChanged = details.ChangedFiles

				// CI status from the same details response — no extra API call
				if fetchCI && details.Head.SHA != "" {
					t.CIStatus = p.fetchCIStatusBySHA(repoName, details.Head.SHA)
				}
			}
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// fetchCIStatusBySHA gets the combined check-run conclusion for a commit SHA.
// Returns "success", "failure", "pending", or "" if unavailable.
func (p *GitHubPoller) fetchCIStatusBySHA(repoFullName, sha string) string {
	if repoFullName == "" || sha == "" {
		return ""
	}

	// Use the combined status endpoint (covers both status checks and check runs)
	url := fmt.Sprintf("%s/repos/%s/commits/%s/status", p.baseURL, repoFullName, sha)
	body, err := p.get(url)
	if err != nil {
		// Try check-runs endpoint as fallback (GitHub Actions use check runs, not statuses)
		return p.fetchCheckRunStatus(repoFullName, sha)
	}

	var status struct {
		State    string `json:"state"` // "success", "failure", "pending"
		Statuses []any  `json:"statuses"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		return ""
	}

	// If no legacy statuses exist, check the check-runs API
	if len(status.Statuses) == 0 {
		return p.fetchCheckRunStatus(repoFullName, sha)
	}

	return status.State
}

// fetchCheckRunStatus gets CI status from the check-runs API (GitHub Actions).
func (p *GitHubPoller) fetchCheckRunStatus(repoFullName, sha string) string {
	url := fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?per_page=100", p.baseURL, repoFullName, sha)
	body, err := p.get(url)
	if err != nil {
		return ""
	}

	var result struct {
		CheckRuns []struct {
			Status     string `json:"status"`     // "queued", "in_progress", "completed"
			Conclusion string `json:"conclusion"` // "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required"
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}

	if len(result.CheckRuns) == 0 {
		return ""
	}

	// Aggregate: any failure → "failure", any pending → "pending", else "success"
	hasFailure := false
	hasPending := false
	for _, cr := range result.CheckRuns {
		if cr.Status != "completed" {
			hasPending = true
			continue
		}
		switch cr.Conclusion {
		case "failure", "timed_out", "cancelled", "action_required":
			hasFailure = true
		}
	}

	if hasFailure {
		return "failure"
	}
	if hasPending {
		return "pending"
	}
	return "success"
}

type prDetails struct {
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
	Head         struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p *GitHubPoller) fetchPRDetails(repoFullName string, number int) (*prDetails, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d", p.baseURL, repoFullName, number)
	body, err := p.get(url)
	if err != nil {
		return nil, err
	}
	var details prDetails
	if err := json.Unmarshal(body, &details); err != nil {
		return nil, err
	}
	return &details, nil
}

func (p *GitHubPoller) get(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.pat)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return body, nil
}

// --- GitHub API types ---

type ghSearchItem struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	User        ghUser     `json:"user"`
	Labels      []ghLabel  `json:"labels"`
	PullRequest *ghPRRef   `json:"pull_request"`
	Repository  *ghRepoRef `json:"repository"`
}

type ghUser struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghPRRef struct {
	URL string `json:"url"`
}

type ghRepoRef struct {
	FullName string `json:"full_name"`
}

func (item ghSearchItem) repoFullName() string {
	if item.Repository != nil {
		return item.Repository.FullName
	}
	// Extract from HTML URL: https://github.example.com/owner/repo/pull/123
	parts := strings.Split(item.HTMLURL, "/")
	if len(parts) >= 5 {
		return parts[len(parts)-4] + "/" + parts[len(parts)-3]
	}
	return ""
}

func (item ghSearchItem) toTask() domain.Task {
	labels := make([]string, len(item.Labels))
	for i, l := range item.Labels {
		labels[i] = l.Name
	}

	return domain.Task{
		ID:        uuid.New().String(),
		Source:    "github",
		SourceID:  fmt.Sprintf("%d", item.Number),
		SourceURL: item.HTMLURL,
		Title:     item.Title,
		Description: item.Body,
		Repo:      item.repoFullName(),
		Author:    item.User.Login,
		Labels:    labels,
		CreatedAt: item.CreatedAt,
		FetchedAt: time.Now(),
		Status:    "queued",
	}
}
