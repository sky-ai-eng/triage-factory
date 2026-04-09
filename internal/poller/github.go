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
	"github.com/sky-ai-eng/todo-tinder/internal/eventbus"
)

// maxQueryLen is the maximum length of a GitHub search query string.
// GitHub documents 256 as the limit for the q= parameter.
const maxQueryLen = 256

// GitHubPoller fetches tasks from the GitHub API on an interval.
type GitHubPoller struct {
	baseURL  string
	pat      string
	username string
	repos    []string // if non-empty, only poll these repos
	database *sql.DB
	client   *http.Client
	interval time.Duration
	stop     chan struct{}
	bus      *eventbus.Bus
}

// ghItemState is the snapshot we store in poller_state for diffing between polls.
type ghItemState struct {
	CIStatus  string `json:"ci_status"`
	Mergeable string `json:"mergeable_state"`
	Reason    string `json:"reason"` // relevance_reason at time of snapshot
	IsMerged  bool   `json:"is_merged"`
}

// apiBase returns the correct API base URL.
func ghAPIBase(baseURL string) string {
	if baseURL == "https://github.com" {
		return "https://api.github.com"
	}
	return baseURL + "/api/v3"
}

func NewGitHubPoller(baseURL, pat, username string, repos []string, database *sql.DB, interval time.Duration, bus *eventbus.Bus) *GitHubPoller {
	return &GitHubPoller{
		baseURL:  ghAPIBase(strings.TrimRight(baseURL, "/")),
		pat:      pat,
		username: username,
		repos:    repos,
		database: database,
		client:   &http.Client{Timeout: 15 * time.Second},
		interval: interval,
		stop:     make(chan struct{}),
		bus:      bus,
	}
}

func (p *GitHubPoller) Start() {
	go func() {
		p.poll()
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

	// 2. Self-authored open PRs (with CI status)
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

	// 4. Self-authored merged PRs from the last 30 days -> status "done"
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
	eventsEmitted := 0
	for _, t := range tasks {
		// Detect events via state diffing BEFORE upsert
		events := p.diffAndEmit(t)
		eventsEmitted += len(events)

		// Set the event_type on the task to the most recent event (if any)
		if len(events) > 0 {
			t.EventType = events[len(events)-1].EventType
		}

		if err := db.UpsertTask(p.database, t); err != nil {
			log.Printf("[github] error upserting task %s: %v", t.SourceID, err)
			continue
		}
		inserted++

		// Record events and publish to bus
		for _, evt := range events {
			// Resolve task ID (may have been assigned by a previous upsert)
			evt.TaskID = t.ID
			if id, err := db.RecordEvent(p.database, evt); err != nil {
				log.Printf("[github] error recording event: %v", err)
			} else {
				evt.ID = id
			}
			db.SetTaskEventType(p.database, t.ID, evt.EventType)
			p.bus.Publish(evt)
		}

		// Save new state snapshot
		p.saveState(t)
	}

	log.Printf("[github] poll complete: %d tasks processed, %d events emitted (%d review, %d authored, %d mentioned, %d merged)",
		inserted, eventsEmitted, len(reviewPRs), len(authoredPRs), len(mentionedPRs), len(mergedPRs))

	// Emit batch-complete sentinel so subscribers (scorer) know the cycle is done
	if inserted > 0 {
		p.bus.Publish(domain.Event{
			EventType: domain.EventSystemPollCompleted,
			SourceID:  "github",
			Metadata:  mustJSON(map[string]any{"tasks": inserted, "events": eventsEmitted}),
			CreatedAt: time.Now(),
		})
	}
}

// diffAndEmit compares current task state against the stored poller_state snapshot
// and returns typed events for any detected transitions.
func (p *GitHubPoller) diffAndEmit(t domain.Task) []domain.Event {
	prevJSON, err := db.GetPollerState(p.database, "github", t.SourceID)
	if err != nil {
		log.Printf("[github] error loading poller state for %s: %v", t.SourceID, err)
	}

	current := ghItemState{
		CIStatus:  t.CIStatus,
		Mergeable: "", // we don't fetch mergeable_state in the poller (that's dashboard-only)
		Reason:    t.RelevanceReason,
		IsMerged:  t.Status == "done",
	}

	var events []domain.Event
	now := time.Now()

	if prevJSON == "" {
		// First time seeing this item — emit the appropriate "new" event
		eventType := p.initialEventType(t)
		if eventType != "" {
			events = append(events, domain.Event{
				EventType: eventType,
				TaskID:    t.ID,
				SourceID:  t.SourceID,
				Metadata:  mustJSON(map[string]string{"reason": "first_seen"}),
				CreatedAt: now,
			})
		}
		return events
	}

	// Parse previous state
	var prev ghItemState
	if err := json.Unmarshal([]byte(prevJSON), &prev); err != nil {
		log.Printf("[github] error parsing previous state for %s: %v", t.SourceID, err)
		return events
	}

	// Detect CI transitions
	if prev.CIStatus != current.CIStatus && current.CIStatus != "" {
		switch current.CIStatus {
		case "success":
			events = append(events, domain.Event{
				EventType: domain.EventGitHubPRCIPassed,
				TaskID:    t.ID,
				SourceID:  t.SourceID,
				Metadata:  mustJSON(map[string]string{"prev": prev.CIStatus, "new": current.CIStatus}),
				CreatedAt: now,
			})
		case "failure":
			events = append(events, domain.Event{
				EventType: domain.EventGitHubPRCIFailed,
				TaskID:    t.ID,
				SourceID:  t.SourceID,
				Metadata:  mustJSON(map[string]string{"prev": prev.CIStatus, "new": current.CIStatus}),
				CreatedAt: now,
			})
		}
	}

	// Detect merge
	if !prev.IsMerged && current.IsMerged {
		events = append(events, domain.Event{
			EventType: domain.EventGitHubPRMerged,
			TaskID:    t.ID,
			SourceID:  t.SourceID,
			Metadata:  mustJSON(map[string]string{"reason": "merged"}),
			CreatedAt: now,
		})
	}

	return events
}

// initialEventType returns the event type for a newly-seen item based on relevance reason.
func (p *GitHubPoller) initialEventType(t domain.Task) string {
	if t.Status == "done" {
		return domain.EventGitHubPRMerged
	}
	switch t.RelevanceReason {
	case "review_requested":
		return domain.EventGitHubPRReviewRequested
	case "authored":
		return domain.EventGitHubPROpened
	case "mentioned":
		return domain.EventGitHubPRMentioned
	default:
		return domain.EventGitHubPROpened
	}
}

// saveState persists the current snapshot for future diffing.
func (p *GitHubPoller) saveState(t domain.Task) {
	state := ghItemState{
		CIStatus:  t.CIStatus,
		Reason:    t.RelevanceReason,
		IsMerged:  t.Status == "done",
	}
	data, _ := json.Marshal(state)
	if err := db.SetPollerState(p.database, "github", t.SourceID, string(data)); err != nil {
		log.Printf("[github] error saving poller state for %s: %v", t.SourceID, err)
	}
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// scopedQueries takes a base search query and returns one or more queries
// with +repo: qualifiers appended, batched to stay under maxQueryLen.
// If no repos are configured, returns the base query as-is.
func scopedQueries(base string, repos []string) []string {
	if len(repos) == 0 {
		return []string{base}
	}

	var queries []string
	current := base
	for _, repo := range repos {
		term := "+repo:" + repo
		if len(current)+len(term) > maxQueryLen {
			// Current batch is full — flush it and start a new one
			queries = append(queries, current)
			current = base + term
		} else {
			current += term
		}
	}
	queries = append(queries, current)
	return queries
}

// searchPRs runs a GitHub search query and returns tasks tagged with the given relevance reason.
// If repos are configured, scopes the query to those repos (batching if needed).
func (p *GitHubPoller) searchPRs(baseQuery, reason string, fetchCI bool) ([]domain.Task, error) {
	queries := scopedQueries(baseQuery, p.repos)

	var allTasks []domain.Task
	for _, query := range queries {
		tasks, err := p.searchPRsQuery(query, reason, fetchCI)
		if err != nil {
			return nil, err
		}
		allTasks = append(allTasks, tasks...)
	}
	return allTasks, nil
}

func (p *GitHubPoller) searchPRsQuery(query, reason string, fetchCI bool) ([]domain.Task, error) {
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

		repoName := item.repoFullName()
		if item.PullRequest != nil && repoName != "" {
			details, err := p.fetchPRDetails(repoName, item.Number)
			if err != nil {
				log.Printf("[github] could not fetch PR details for %s#%d: %v", repoName, item.Number, err)
			} else {
				t.DiffAdditions = details.Additions
				t.DiffDeletions = details.Deletions
				t.FilesChanged = details.ChangedFiles

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
func (p *GitHubPoller) fetchCIStatusBySHA(repoFullName, sha string) string {
	if repoFullName == "" || sha == "" {
		return ""
	}

	url := fmt.Sprintf("%s/repos/%s/commits/%s/status", p.baseURL, repoFullName, sha)
	body, err := p.get(url)
	if err != nil {
		return p.fetchCheckRunStatus(repoFullName, sha)
	}

	var status struct {
		State    string `json:"state"`
		Statuses []any  `json:"statuses"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		return ""
	}

	if len(status.Statuses) == 0 {
		return p.fetchCheckRunStatus(repoFullName, sha)
	}

	return status.State
}

func (p *GitHubPoller) fetchCheckRunStatus(repoFullName, sha string) string {
	url := fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?per_page=100", p.baseURL, repoFullName, sha)
	body, err := p.get(url)
	if err != nil {
		return ""
	}

	var result struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}

	if len(result.CheckRuns) == 0 {
		return ""
	}

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
